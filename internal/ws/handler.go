package ws

import (
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const (
	EnvGOExchangeWSAllowedOrigins     = "GOEXCHANGE_WS_ALLOWED_ORIGINS"
	EnvWSAllowedOrigins               = "WS_ALLOWED_ORIGINS"
	EnvGOExchangeWSAllowMissingOrigin = "GOEXCHANGE_WS_ALLOW_MISSING_ORIGIN"
	EnvWSAllowMissingOrigin           = "WS_ALLOW_MISSING_ORIGIN"
	writeWait                         = 10 * time.Second
	pongWait                          = 60 * time.Second
	pingPeriod                        = (pongWait * 9) / 10
	maxMessageSize                    = 1024
)

var defaultWSAllowedOrigins = []string{
	"http://localhost:3000",
	"http://localhost:5173",
}

type OriginCheckerConfig struct {
	AllowedOrigins     []string
	AllowMissingOrigin bool
}

func ServeWs(hub *Hub, c *gin.Context) {
	upgrader := websocket.Upgrader{
		CheckOrigin: NewOriginCheckerFromEnv(),
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}

	client := &Client{
		Conn: conn,
		Send: make(chan []byte, 256),
	}
	hub.Register <- client

	go client.writePump(hub)
	client.readPump(hub)
}

func NewOriginCheckerFromEnv() func(r *http.Request) bool {
	return NewOriginChecker(OriginCheckerConfigFromEnv())
}

func OriginCheckerConfigFromEnv() OriginCheckerConfig {
	return OriginCheckerConfig{
		AllowedOrigins:     allowedOriginsFromEnv(),
		AllowMissingOrigin: allowMissingOriginFromEnv(),
	}
}

func NewOriginChecker(cfg OriginCheckerConfig) func(r *http.Request) bool {
	allowedOrigins := make(map[string]struct{}, len(cfg.AllowedOrigins))
	for _, origin := range cfg.AllowedOrigins {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		allowedOrigins[origin] = struct{}{}
	}

	return func(r *http.Request) bool {
		if r == nil {
			return false
		}

		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin == "" {
			return cfg.AllowMissingOrigin
		}

		_, ok := allowedOrigins[origin]
		return ok
	}
}

func (c *Client) readPump(hub *Hub) {
	defer func() {
		hub.Unregister <- c
		_ = c.Conn.Close()
	}()

	c.Conn.SetReadLimit(maxMessageSize)
	_ = c.Conn.SetReadDeadline(time.Now().Add(pongWait))
	c.Conn.SetPongHandler(func(string) error {
		return c.Conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		if _, _, err := c.Conn.ReadMessage(); err != nil {
			return
		}
	}
}

func (c *Client) writePump(hub *Hub) {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		hub.Unregister <- c
		_ = c.Conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.Send:
			_ = c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.Conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func allowedOriginsFromEnv() []string {
	if origins, ok := lookupTrimmedEnv(EnvGOExchangeWSAllowedOrigins); ok {
		return parseAllowedOrigins(origins)
	}
	if origins, ok := lookupTrimmedEnv(EnvWSAllowedOrigins); ok {
		return parseAllowedOrigins(origins)
	}

	return append([]string(nil), defaultWSAllowedOrigins...)
}

func allowMissingOriginFromEnv() bool {
	if value, ok := lookupTrimmedEnv(EnvGOExchangeWSAllowMissingOrigin); ok {
		return parseBool(value)
	}
	if value, ok := lookupTrimmedEnv(EnvWSAllowMissingOrigin); ok {
		return parseBool(value)
	}

	return false
}

func parseAllowedOrigins(raw string) []string {
	parts := strings.Split(raw, ",")
	origins := make([]string, 0, len(parts))
	for _, part := range parts {
		origin := strings.TrimSpace(part)
		if origin == "" {
			continue
		}
		origins = append(origins, origin)
	}
	return origins
}

func lookupTrimmedEnv(key string) (string, bool) {
	value, ok := os.LookupEnv(key)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(value), true
}

func parseBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	default:
		return false
	}
}
