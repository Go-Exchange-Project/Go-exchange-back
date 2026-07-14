package ws

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSubscriptionCommandSubscribe(t *testing.T) {
	client := newTestClient(1)
	update, ok := parseSubscriptionCommand(client, []byte(`{"action":"subscribe","coin_symbols":[" btc ","ETH"]}`))
	require.True(t, ok)
	assert.Equal(t, client, update.Client)
	assert.False(t, update.Unsubscribe)
	assert.Equal(t, []string{"BTC", "ETH"}, update.CoinSymbols, "심볼은 트림+대문자 정규화돼야 한다")
}

func TestParseSubscriptionCommandUnsubscribe(t *testing.T) {
	update, ok := parseSubscriptionCommand(newTestClient(1), []byte(`{"action":"UNSUBSCRIBE","coin_symbols":["BTC"]}`))
	require.True(t, ok)
	assert.True(t, update.Unsubscribe)
}

func TestParseSubscriptionCommandIgnoresGarbage(t *testing.T) {
	client := newTestClient(1)
	for name, payload := range map[string]string{
		"non-json":       `hello`,
		"unknown action": `{"action":"ping"}`,
		"empty action":   `{"coin_symbols":["BTC"]}`,
	} {
		_, ok := parseSubscriptionCommand(client, []byte(payload))
		assert.False(t, ok, "%s 메시지는 무시돼야 한다", name)
	}
}

func TestParseSubscriptionCommandDropsEmptySymbols(t *testing.T) {
	update, ok := parseSubscriptionCommand(newTestClient(1), []byte(`{"action":"subscribe","coin_symbols":["", "  ", "BTC"]}`))
	require.True(t, ok)
	assert.Equal(t, []string{"BTC"}, update.CoinSymbols)
}

func readTextMessage(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, payload, err := conn.ReadMessage()
	require.NoError(t, err)
	return string(payload)
}

// 엔드투엔드: 실제 WS 연결 2개 — 구독 클라이언트는 자기 심볼만, legacy
// 클라이언트는 전부 받는다. readPump→hub.Subscribe→라우팅의 전체 체인 검증.
//
// 구독 반영은 비동기(readPump→Subscribe 채널→hub 루프)라서, hub 내부 상태를
// 폴링하는 대신(레이스) "미구독 심볼 프로브 + 구독 심볼 sync" 쌍을 반복 발행해
// 프로브가 걸러지기 시작하는 시점을 관찰하는 방식으로 동기화한다 — sleep 없이
// 결정적이다(hub 브로드캐스트와 클라이언트별 전달은 FIFO).
func TestServeWsSymbolSubscriptionEndToEnd(t *testing.T) {
	t.Setenv(EnvGOExchangeWSAllowMissingOrigin, "true")
	gin.SetMode(gin.TestMode)

	hub := NewHub()
	go hub.Run()

	router := gin.New()
	router.GET("/ws", func(c *gin.Context) {
		ServeWs(hub, c)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	dial := func() *websocket.Conn {
		conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
		require.NoError(t, err)
		if resp != nil {
			_ = resp.Body.Close()
		}
		return conn
	}
	subscriberConn := dial()
	defer subscriberConn.Close()
	legacyConn := dial()
	defer legacyConn.Close()

	require.NoError(t, subscriberConn.WriteJSON(map[string]interface{}{
		"action":       "subscribe",
		"coin_symbols": []string{"BTC"},
	}))

	// 구독이 반영될 때까지: ETH 프로브가 걸러지고 BTC sync만 도착하면 반영된 것.
	subscribed := false
	for i := 0; i < 50 && !subscribed; i++ {
		probe := fmt.Sprintf("probe-%d", i)
		sync := fmt.Sprintf("sync-%d", i)
		hub.Broadcast <- Message{CoinSymbol: "ETH", Payload: []byte(probe)}
		hub.Broadcast <- Message{CoinSymbol: "BTC", Payload: []byte(sync)}

		sawProbe := false
		for {
			payload := readTextMessage(t, subscriberConn)
			if payload == probe {
				sawProbe = true
				continue
			}
			if payload == sync {
				break
			}
			// 이전 iteration의 잔여 메시지 — 무시
		}
		subscribed = !sawProbe
	}
	require.True(t, subscribed, "구독이 hub에 반영돼야 한다")

	// 구독 활성 확정 후: ETH는 걸러지고 BTC만 도착해야 한다.
	hub.Broadcast <- Message{CoinSymbol: "ETH", Payload: []byte("final-eth")}
	hub.Broadcast <- Message{CoinSymbol: "BTC", Payload: []byte("final-btc")}
	assert.Equal(t, "final-btc", readTextMessage(t, subscriberConn),
		"구독 클라이언트의 다음 수신은 final-btc여야 한다(final-eth는 필터링)")

	// legacy 클라이언트: 아무것도 구독하지 않았으므로 전부 순서대로 받는다 —
	// final-eth를 만날 때까지 읽고, 바로 다음이 final-btc여야 한다.
	for {
		if readTextMessage(t, legacyConn) == "final-eth" {
			break
		}
	}
	assert.Equal(t, "final-btc", readTextMessage(t, legacyConn), "legacy 클라이언트는 두 심볼 모두 받아야 한다")
}
