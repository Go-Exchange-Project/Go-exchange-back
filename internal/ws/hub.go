// hub란 연결된 브라우저들을 관리하는 중앙 관리자
// hub가 하는 일
// 브라우저 연결/해제 관리
// 업비트 시세를 연결된 모든 브라우저에 전송

package ws

import "github.com/gorilla/websocket"

type Client struct {
	Conn *websocket.Conn
	Send chan []byte
}

type Hub struct {
	Clients	map[*Client]bool
	Broadcast	chan []byte
	Register	chan *Client
	Unregister	chan *Client
}

func NewHub() *Hub {
    return &Hub{
        Clients:    make(map[*Client]bool),
        Broadcast:  make(chan []byte, 256),
        Register:   make(chan *Client),
        Unregister: make(chan *Client),
    }
}

func (h *Hub) Run() {
    for {
        select {
        case client := <-h.Register:
            h.Clients[client] = true
        case client := <-h.Unregister:
            delete(h.Clients, client)
        case msg := <-h.Broadcast:
            for client := range h.Clients {
                client.Send <- msg
            }
        }
    }
}