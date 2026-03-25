// 브라우저가 WebSocket으로 연결했을 때 처리하는 핸들러 파일
package ws

import (
	"net/http"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // ** 모든 도메인 연결 허용 중이라 배포할 때 확인 **
	},
}

func ServeWs(hub *Hub, c *gin.Context) {
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}

	client := &Client{
		Conn: conn,
		Send: make(chan []byte, 256),
	}
	hub.Register <- client

	go client.writePump()
}

func (c *Client) writePump() {
    defer c.Conn.Close()
    for msg := range c.Send {
        c.Conn.WriteMessage(websocket.TextMessage, msg)
    }
}