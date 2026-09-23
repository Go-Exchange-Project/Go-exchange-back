package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// httpTimeoutConfig는 서비스 http.Server의 4종 상한 + MaxHeaderBytes다(설계 §2.1).
// env 파싱과 분리해 테스트가 짧은 값을 직접 주입할 수 있게 한다.
type httpTimeoutConfig struct {
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
}

// newServiceHTTPServer는 설계 §2.1의 서비스 서버를 만든다.
func newServiceHTTPServer(addr string, handler http.Handler, cfg httpTimeoutConfig) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
	}
}

// newAdminMux는 관리 서버의 라우트를 만든다. /metrics는 항상 등록하고, pprof는
// pprofEnabled일 때만 등록한다(설계 §7). net/http/pprof를 blank import하지 않는
// 이유는 그 관용구가 무조건 http.DefaultServeMux에 등록해 조건부 게이트를 걸
// 수 없기 때문이다 — 여기서는 명시적으로 이 mux에만, 켜졌을 때만 붙인다.
func newAdminMux(pprofEnabled bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	if pprofEnabled {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	return mux
}

// newAdminHTTPServer는 설계 §2.2의 관리 서버를 만든다. WriteTimeout이 서비스보다
// 훨씬 길다 — pprof CPU 프로파일 기본 길이(30초)가 그 안에 들어와야 한다.
func newAdminHTTPServer(addr string, handler http.Handler, writeTimeout time.Duration) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// bindAdminServer는 관리 서버 포트를 bind한다. bind 실패는 부팅 실패다(설계
// §7) — 관리 포트가 조용히 없으면 지표·프로파일이 사라진다. fatalf를 주입받는
// 이유는 테스트가 "bind 실패 = 부팅 실패"를 os.Exit 없이 확인할 수 있게
// 하기 위해서다(H8, log.Fatal과 같은 시그니처). 실패 시 nil을 돌려준다 —
// 프로덕션에서는 fatalf가 log.Fatal이라 이 반환값에 도달하기 전에 종료된다.
func bindAdminServer(addr string, fatalf func(args ...interface{})) net.Listener {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fatalf("admin server bind failed: ", err)
		return nil
	}
	return ln
}

// serveAdminServer는 이미 bind된 리스너로 관리 서버를 백그라운드에서 서빙한다.
func serveAdminServer(srv *http.Server, ln net.Listener) {
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatal("admin server failed: ", err)
		}
	}()
}

// runShutdownSequence는 설계 §5의 종료 분기다. 두 서버가 timeout 예산을 공유하지
// 않는다 — 각자 독립된 context.WithTimeout을 쓴다. 서비스 Shutdown이 실패하면
// 파이프라인을 드레인하지 않고 exitFunc(1)을 호출한 뒤 곧바로 return한다(그
// 뒤의 defer는 실행되지 않는다 — 닫지 않고 죽는 것이 의도다, 설계 §5 근거 참고).
// 관리 서버 실패는 Close()만 하고, 서비스가 성공했으면 드레인을 계속한다.
func runShutdownSequence(
	serviceSrv *http.Server, serviceShutdownTimeout time.Duration,
	adminSrv *http.Server, adminShutdownTimeout time.Duration,
	drainPipeline func(),
	exitFunc func(int),
	logf func(format string, args ...any),
) {
	serviceCtx, cancelService := context.WithTimeout(context.Background(), serviceShutdownTimeout)
	defer cancelService()
	serviceErr := serviceSrv.Shutdown(serviceCtx)

	adminCtx, cancelAdmin := context.WithTimeout(context.Background(), adminShutdownTimeout)
	defer cancelAdmin()
	if err := adminSrv.Shutdown(adminCtx); err != nil {
		logf("shutdown: admin server shutdown failed: %v", err)
		_ = adminSrv.Close()
	}

	if serviceErr != nil {
		logf("shutdown: service server shutdown failed: %v — pipeline will not be drained", serviceErr)
		exitFunc(1)
		return
	}

	drainPipeline()
}
