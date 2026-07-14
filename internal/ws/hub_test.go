package ws

import "testing"

func newTestClient(buffer int) *Client {
	return &Client{Send: make(chan []byte, buffer)}
}

func mustReceive(t *testing.T, client *Client, want string) {
	t.Helper()
	select {
	case got := <-client.Send:
		if string(got) != want {
			t.Fatalf("received payload = %q, want %q", got, want)
		}
	default:
		t.Fatalf("expected client to receive %q but Send is empty", want)
	}
}

func mustNotReceive(t *testing.T, client *Client) {
	t.Helper()
	select {
	case got := <-client.Send:
		t.Fatalf("expected no message, but received %q", got)
	default:
	}
}

func TestHubBroadcastSendsToReadyClient(t *testing.T) {
	hub := NewHub()
	client := newTestClient(1)
	hub.registerClient(client)

	hub.broadcast(Message{CoinSymbol: "BTC", Payload: []byte("tick")})

	if !hub.Clients[client] {
		t.Fatal("expected ready client to remain registered")
	}
	mustReceive(t, client, "tick")
}

func TestHubBroadcastDropsSlowClient(t *testing.T) {
	hub := NewHub()
	client := newTestClient(1)
	client.Send <- []byte("queued")
	hub.registerClient(client)

	hub.broadcast(Message{CoinSymbol: "BTC", Payload: []byte("latest")})

	if hub.Clients[client] {
		t.Fatal("expected slow client to be removed")
	}

	got, ok := <-client.Send
	if !ok {
		t.Fatal("expected queued message to remain readable before channel close is observed")
	}
	if string(got) != "queued" {
		t.Fatalf("queued payload = %q, want %q", got, "queued")
	}

	_, ok = <-client.Send
	if ok {
		t.Fatal("expected slow client send channel to be closed")
	}
}

func TestHubUnregisterRemovesClientAndClosesSend(t *testing.T) {
	hub := NewHub()
	client := newTestClient(0)
	hub.registerClient(client)

	hub.unregisterClient(client)

	if hub.Clients[client] {
		t.Fatal("expected unregistered client to be removed")
	}

	_, ok := <-client.Send
	if ok {
		t.Fatal("expected unregistered client send channel to be closed")
	}

	hub.unregisterClient(client)
}

// 구독 라우팅: 구독한 심볼만 받고, 다른 심볼은 차단된다.
func TestHubRoutesSymbolMessagesToSubscribersOnly(t *testing.T) {
	hub := NewHub()
	subscriber := newTestClient(4)
	hub.registerClient(subscriber)
	hub.applySubscription(SubscriptionUpdate{Client: subscriber, CoinSymbols: []string{"BTC"}})

	hub.broadcast(Message{CoinSymbol: "BTC", Payload: []byte("btc-book")})
	mustReceive(t, subscriber, "btc-book")

	hub.broadcast(Message{CoinSymbol: "ETH", Payload: []byte("eth-book")})
	mustNotReceive(t, subscriber)
}

// 하위 호환: 구독 메시지를 보낸 적 없는 클라이언트는 기존처럼 전부 받는다.
func TestHubLegacyClientWithoutSubscriptionReceivesEverything(t *testing.T) {
	hub := NewHub()
	legacy := newTestClient(4)
	hub.registerClient(legacy)

	hub.broadcast(Message{CoinSymbol: "BTC", Payload: []byte("btc-book")})
	mustReceive(t, legacy, "btc-book")
	hub.broadcast(Message{CoinSymbol: "ETH", Payload: []byte("eth-book")})
	mustReceive(t, legacy, "eth-book")
}

// 전역 메시지(CoinSymbol="")는 구독 여부와 무관하게 모두에게 간다 (upbit ticker).
func TestHubGlobalMessageReachesAllClients(t *testing.T) {
	hub := NewHub()
	subscriber := newTestClient(4)
	legacy := newTestClient(4)
	hub.registerClient(subscriber)
	hub.registerClient(legacy)
	hub.applySubscription(SubscriptionUpdate{Client: subscriber, CoinSymbols: []string{"BTC"}})

	hub.broadcast(Message{Payload: []byte("ticker")})

	mustReceive(t, subscriber, "ticker")
	mustReceive(t, legacy, "ticker")
}

func TestHubUnsubscribeStopsSymbolDelivery(t *testing.T) {
	hub := NewHub()
	client := newTestClient(4)
	hub.registerClient(client)
	hub.applySubscription(SubscriptionUpdate{Client: client, CoinSymbols: []string{"BTC", "ETH"}})
	hub.applySubscription(SubscriptionUpdate{Client: client, CoinSymbols: []string{"BTC"}, Unsubscribe: true})

	hub.broadcast(Message{CoinSymbol: "BTC", Payload: []byte("btc-book")})
	mustNotReceive(t, client)

	hub.broadcast(Message{CoinSymbol: "ETH", Payload: []byte("eth-book")})
	mustReceive(t, client, "eth-book")
}

// 모든 심볼을 unsubscribe해도 필터 모드는 유지된다 — legacy full-feed로
// 돌아가지 않는다(의도적으로 아무것도 안 받는 상태).
func TestHubUnsubscribeAllDoesNotRevertToLegacyFeed(t *testing.T) {
	hub := NewHub()
	client := newTestClient(4)
	hub.registerClient(client)
	hub.applySubscription(SubscriptionUpdate{Client: client, CoinSymbols: []string{"BTC"}})
	hub.applySubscription(SubscriptionUpdate{Client: client, CoinSymbols: []string{"BTC"}, Unsubscribe: true})

	hub.broadcast(Message{CoinSymbol: "BTC", Payload: []byte("btc-book")})
	mustNotReceive(t, client)
}

func TestHubSubscriptionForUnknownClientIsIgnored(t *testing.T) {
	hub := NewHub()
	stranger := newTestClient(1)

	hub.applySubscription(SubscriptionUpdate{Client: stranger, CoinSymbols: []string{"BTC"}})
	hub.applySubscription(SubscriptionUpdate{Client: nil, CoinSymbols: []string{"BTC"}})

	if len(hub.Clients) != 0 {
		t.Fatal("subscription must not register unknown clients")
	}
}
