package ws

import "github.com/gorilla/websocket"

// Message는 hub로 발행되는 브로드캐스트 단위입니다. CoinSymbol이 비어 있으면
// 전역 메시지로 모든 클라이언트에게 전달되고, 심볼이 있으면 그 심볼 구독자
// (그리고 구독을 한 번도 안 한 legacy full-feed 클라이언트)에게만 전달됩니다.
type Message struct {
	CoinSymbol string
	Payload    []byte
}

type Client struct {
	Conn *websocket.Conn
	Send chan []byte

	// subscriptions와 subscribed는 hub goroutine에서만 접근한다(락 불필요).
	// subscribed=false면 legacy full-feed — 구독 메시지를 보낸 적 없는 기존
	// 클라이언트는 종전처럼 모든 메시지를 받는다(하위 호환).
	subscriptions map[string]bool
	subscribed    bool
}

// SubscriptionUpdate는 readPump가 파싱한 구독 변경을 hub 루프로 전달합니다.
type SubscriptionUpdate struct {
	Client      *Client
	CoinSymbols []string
	Unsubscribe bool
}

type Hub struct {
	Clients    map[*Client]bool
	Broadcast  chan Message
	Register   chan *Client
	Unregister chan *Client
	Subscribe  chan SubscriptionUpdate
}

func NewHub() *Hub {
	return &Hub{
		Clients:    make(map[*Client]bool),
		Broadcast:  make(chan Message, 256),
		Register:   make(chan *Client),
		Unregister: make(chan *Client),
		Subscribe:  make(chan SubscriptionUpdate, 256),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case client := <-h.Register:
			h.registerClient(client)
		case client := <-h.Unregister:
			h.unregisterClient(client)
		case update := <-h.Subscribe:
			h.applySubscription(update)
		case msg := <-h.Broadcast:
			h.broadcast(msg)
		}
	}
}

func (h *Hub) registerClient(client *Client) {
	h.Clients[client] = true
}

func (h *Hub) unregisterClient(client *Client) {
	if _, ok := h.Clients[client]; !ok {
		return
	}

	delete(h.Clients, client)
	close(client.Send)
}

func (h *Hub) applySubscription(update SubscriptionUpdate) {
	client := update.Client
	if client == nil || !h.Clients[client] {
		return
	}
	if client.subscriptions == nil {
		client.subscriptions = make(map[string]bool)
	}
	// 첫 구독 변경부터 필터 모드 — 이후로는 구독한 심볼만 받는다.
	client.subscribed = true

	for _, coinSymbol := range update.CoinSymbols {
		if coinSymbol == "" {
			continue
		}
		if update.Unsubscribe {
			delete(client.subscriptions, coinSymbol)
		} else {
			client.subscriptions[coinSymbol] = true
		}
	}
}

func (h *Hub) broadcast(msg Message) {
	for client := range h.Clients {
		if !h.shouldReceive(client, msg) {
			continue
		}
		select {
		case client.Send <- msg.Payload:
		default:
			h.unregisterClient(client)
		}
	}
}

func (h *Hub) shouldReceive(client *Client, msg Message) bool {
	if msg.CoinSymbol == "" {
		return true // 전역 메시지
	}
	if !client.subscribed {
		return true // legacy full-feed (하위 호환)
	}
	return client.subscriptions[msg.CoinSymbol]
}
