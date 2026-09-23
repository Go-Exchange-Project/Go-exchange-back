package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/ws"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shortHTTPTimeouts는 H1~H5의 공통 베이스다 — 테스트 대상 필드만 각 테스트가
// 짧게 덮어쓴다(설계·계획: 테스트 전용 짧은 값, 50~200ms).
func shortHTTPTimeouts() httpTimeoutConfig {
	return httpTimeoutConfig{
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       5 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// startTestServiceServer는 실제 loopback 리스너로 서비스 서버를 띄운다.
func startTestServiceServer(t *testing.T, handler http.Handler, cfg httpTimeoutConfig) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := newServiceHTTPServer(ln.Addr().String(), handler, cfg)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return ln.Addr().String()
}

// H1: 헤더를 천천히(완성하지 않고) 보내면 ReadHeaderTimeout 근처에서 연결이 닫힌다.
func TestServiceServerClosesSlowlorisConnection(t *testing.T) {
	cfg := shortHTTPTimeouts()
	cfg.ReadHeaderTimeout = 100 * time.Millisecond
	addr := startTestServiceServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), cfg)

	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n"))
	require.NoError(t, err)
	go func() {
		for i := 0; i < 10; i++ {
			if _, werr := conn.Write([]byte("X-Slow: a\r\n")); werr != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, 1)
	_, rerr := conn.Read(buf)
	require.Error(t, rerr, "ReadHeaderTimeout 근처에서 연결이 닫혀야 한다(EOF)")
}

// H2: 헤더는 완성하지만 body를 천천히 보내면 ReadTimeout에 끊긴다.
func TestServiceServerClosesSlowBodyConnection(t *testing.T) {
	cfg := shortHTTPTimeouts()
	cfg.ReadTimeout = 100 * time.Millisecond
	addr := startTestServiceServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// body 읽기가 ReadTimeout에 걸려 에러가 나면, 핸들러가 정상 반환해
		// 암묵적 200을 보내는 대신 연결 자체를 조용히 끊는다(net/http가 인식하는
		// 관용구) — "응답이 온다/안 온다"가 아니라 "연결이 끊긴다"가 판정 대상이다.
		buf := make([]byte, 1000)
		if _, err := io.ReadFull(r.Body, buf); err != nil {
			panic(http.ErrAbortHandler)
		}
		w.WriteHeader(http.StatusOK)
	}), cfg)

	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer conn.Close()

	req := "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\n"
	_, err = conn.Write([]byte(req))
	require.NoError(t, err)
	go func() {
		for i := 0; i < 20; i++ {
			if _, werr := conn.Write([]byte("a")); werr != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, 1)
	_, rerr := conn.Read(buf)
	require.Error(t, rerr, "ReadTimeout 초과 시 연결이 끊겨야 한다(EOF)")
}

// H3: 핸들러가 WriteTimeout을 넘겨서 쓰면 응답이 끊긴다(늦게 쓴 본문이 도달하면 안 된다).
func TestServiceServerCutsResponseWhenHandlerExceedsWriteTimeout(t *testing.T) {
	cfg := shortHTTPTimeouts()
	cfg.WriteTimeout = 100 * time.Millisecond
	addr := startTestServiceServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("late-body-should-not-arrive"))
	}), cfg)

	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
	require.NoError(t, err)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, 512)
	n, _ := conn.Read(buf)
	assert.NotContains(t, string(buf[:n]), "late-body-should-not-arrive",
		"WriteTimeout을 넘긴 뒤 쓴 본문이 클라이언트에 도달하면 안 된다")
}

// H4: idle(keep-alive로 요청·응답을 마친 뒤 아무 것도 안 보냄)이 IdleTimeout을
// 넘기면 다음 read에서 EOF다. http.Client는 재연결을 숨기므로 raw TCP를 쓴다.
func TestServiceServerClosesIdleConnectionAfterIdleTimeout(t *testing.T) {
	cfg := shortHTTPTimeouts()
	cfg.IdleTimeout = 100 * time.Millisecond
	addr := startTestServiceServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), cfg)

	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
	require.NoError(t, err)

	reader := bufio.NewReader(conn)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	resp, err := http.ReadResponse(reader, nil)
	require.NoError(t, err)
	_ = resp.Body.Close()

	time.Sleep(300 * time.Millisecond) // IdleTimeout(100ms)을 넘겨 그대로 둔다.

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, 1)
	_, rerr := conn.Read(buf)
	require.Error(t, rerr, "idle 상한을 넘긴 연결은 서버가 닫아야 한다(EOF)")
}

// H5: MaxHeaderBytes를 넘기면 431 또는 연결 닫힘이다.
func TestServiceServerRejectsOversizedHeaders(t *testing.T) {
	cfg := shortHTTPTimeouts()
	cfg.MaxHeaderBytes = 200
	addr := startTestServiceServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), cfg)

	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer conn.Close()

	// net/http는 MaxHeaderBytes에 내부적으로 약 4096바이트의 여유를 더 두므로
	// (요청 헤더 파서의 버퍼 크기), 그 여유를 확실히 넘기도록 충분히 크게 만든다.
	bigHeader := "X-Big: " + strings.Repeat("a", 64*1024) + "\r\n"
	req := "GET / HTTP/1.1\r\nHost: x\r\n" + bigHeader + "\r\n"
	_, err = conn.Write([]byte(req))
	require.NoError(t, err)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	reader := bufio.NewReader(conn)
	line, rerr := reader.ReadString('\n')
	if rerr == nil {
		assert.Contains(t, line, "431", "헤더가 너무 크면 431을 반환해야 한다: %q", line)
	}
	// rerr != nil(연결을 그냥 닫음)도 허용되는 결과다 — 431 응답 자체가 아니라
	// "과대 헤더를 받아주지 않는다"가 판정 대상이다.
}

// H6(회귀 방지): 서비스 서버 WriteTimeout을 아주 짧게(80ms) 잡아도, 업그레이드된
// WebSocket 연결은 그보다 오래 살아 메시지를 주고받는다 — 전역 WriteTimeout은
// 업그레이드 시점까지만 적용되고 hijack된 연결에는 적용되지 않는다(설계 §2.1).
func TestServiceServerWriteTimeoutDoesNotAffectWebSocketConnections(t *testing.T) {
	t.Setenv(ws.EnvGOExchangeWSAllowMissingOrigin, "true")
	gin.SetMode(gin.TestMode)

	hub := ws.NewHub()
	go hub.Run()

	router := gin.New()
	router.GET("/ws", func(c *gin.Context) { ws.ServeWs(hub, c) })

	srv := httptest.NewUnstartedServer(router)
	srv.Config.WriteTimeout = 80 * time.Millisecond
	srv.Start()
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	if resp != nil {
		_ = resp.Body.Close()
	}
	defer conn.Close()

	// WriteTimeout(80ms)보다 오래 살아남았음을 먼저 확인한다.
	time.Sleep(200 * time.Millisecond)

	hub.Broadcast <- ws.Message{CoinSymbol: "", Payload: []byte(`{"type":"ping-check"}`)}
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, payload, err := conn.ReadMessage()
	require.NoError(t, err, "WriteTimeout 경과 후에도 WS 메시지를 정상 수신해야 한다")
	assert.Contains(t, string(payload), "ping-check")
}

// H7: /metrics는 서비스 라우터에 없고(소스 검사 — main()의 라우트 등록은 함수로
// 분리돼 있지 않아 TestMainStartsOrderIdempotencyMonitor와 같은 방식을 쓴다),
// 관리 mux에는 있다. pprof는 env로 게이트된다.
func TestAdminMuxServesMetricsAndGatesPprofByEnv(t *testing.T) {
	t.Run("pprof off: /metrics 200, pprof 404", func(t *testing.T) {
		mux := newAdminMux(false)

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		assert.Equal(t, http.StatusOK, rec.Code)

		rec2 := httptest.NewRecorder()
		mux.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
		assert.Equal(t, http.StatusNotFound, rec2.Code)
	})

	t.Run("pprof on: 200", func(t *testing.T) {
		mux := newAdminMux(true)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

// 서비스 라우터는 main.go 안에 인라인으로 구성돼 별도 함수가 아니다 — 소스를
// 읽어 "/metrics" 라우트를 gin 서비스 라우터에 등록하지 않았음을 확인한다
// (TestMainStartsOrderIdempotencyMonitor·TestCORSAllowsTheHeadersOrderCreationRequires와 같은 방식).
func TestMainDoesNotRegisterMetricsOnServiceRouter(t *testing.T) {
	source, err := os.ReadFile("main.go")
	require.NoError(t, err)
	assert.NotContains(t, string(source), `r.GET("/metrics"`,
		"서비스 라우터(r)에 /metrics를 등록하면 안 된다 — 관리 서버로 옮겨야 한다")
}

// H8: 관리 서버 bind 실패는 부팅 실패로 이어진다(주입한 fatal 함수로 확인).
func TestAdminServerBindFailureTriggersFatal(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer taken.Close()

	var called bool
	var gotErr error
	fatalf := func(args ...interface{}) {
		called = true
		for _, a := range args {
			if e, ok := a.(error); ok {
				gotErr = e
			}
		}
	}

	ln := bindAdminServer(taken.Addr().String(), fatalf)
	assert.Nil(t, ln)
	assert.True(t, called, "bind 실패는 주입한 fatal 함수를 호출해야 한다")
	assert.Error(t, gotErr)
}

// H8: 관리 서버는 정상적으로 graceful shutdown된다.
func TestAdminServerGracefulShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := newAdminHTTPServer(ln.Addr().String(), newAdminMux(false), 2*time.Second)
	go func() { _ = srv.Serve(ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/metrics")
	require.NoError(t, err)
	_ = resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, srv.Shutdown(ctx))
}

// blockingHandler는 release가 닫힐 때까지 요청을 붙잡아 둔다 — Shutdown(ctx)이
// 그 요청이 끝나기 전에는 완료될 수 없게 만들어 실패를 결정적으로 재현한다.
func blockingHandler(started chan<- struct{}, release <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		w.WriteHeader(http.StatusOK)
	}
}

// H9: 서비스 Shutdown이 실패하면 파이프라인을 드레인하지 않고 exitFunc(1)을
// 호출한 뒤 곧바로 return한다.
func TestRunShutdownSequenceServiceFailureSkipsDrainAndExits(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)

	serviceLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serviceSrv := &http.Server{Handler: blockingHandler(started, release)}
	go func() { _ = serviceSrv.Serve(serviceLn) }()

	go func() {
		resp, err := http.Get("http://" + serviceLn.Addr().String() + "/")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started

	adminLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	adminSrv := &http.Server{Handler: http.NewServeMux()}
	go func() { _ = adminSrv.Serve(adminLn) }()
	defer adminSrv.Close()

	var drained, exited bool
	var exitCode int
	runShutdownSequence(
		serviceSrv, 50*time.Millisecond,
		adminSrv, 2*time.Second,
		func() { drained = true },
		func(code int) { exited = true; exitCode = code },
		t.Logf,
	)

	assert.False(t, drained, "서비스 Shutdown 실패 시 파이프라인을 드레인하면 안 된다")
	assert.True(t, exited, "exitFunc가 호출돼야 한다")
	assert.Equal(t, 1, exitCode)
}

// H9: 관리 서버만 실패하고 서비스가 성공하면 드레인은 정상 수행된다.
func TestRunShutdownSequenceAdminFailureStillDrainsWhenServiceSucceeds(t *testing.T) {
	serviceLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serviceSrv := &http.Server{Handler: http.NewServeMux()}
	go func() { _ = serviceSrv.Serve(serviceLn) }()

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	adminLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	adminSrv := &http.Server{Handler: blockingHandler(started, release)}
	go func() { _ = adminSrv.Serve(adminLn) }()

	go func() {
		resp, err := http.Get("http://" + adminLn.Addr().String() + "/")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started

	var drained, exited bool
	runShutdownSequence(
		serviceSrv, 2*time.Second,
		adminSrv, 50*time.Millisecond,
		func() { drained = true },
		func(code int) { exited = true },
		t.Logf,
	)

	assert.True(t, drained, "관리 서버만 실패하면 서비스가 성공했으니 드레인은 계속돼야 한다")
	assert.False(t, exited, "관리 서버 실패만으로는 exitFunc를 호출하면 안 된다")
}
