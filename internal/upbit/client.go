package upbit

import (
	"github.com/gorilla/websocket"
	"encoding/json"
)

type UpbitClient struct {
	Conn *websocket.Conn
}

type TickerResponse struct {
    Code         string  `json:"code"`
    TradePrice   float64 `json:"trade_price"`
}

func NewUpbitClient() (*UpbitClient, error) {
	conn, _, err := websocket.DefaultDialer.Dial("wss://api.upbit.com/websocket/v1", nil)
	if err != nil {
		return nil, err
	}
	return &UpbitClient{Conn: conn}, nil
}

func (c *UpbitClient) Subscribe(codes []string) error {
    msg := []interface{}{
        map[string]string{"ticket": "test"},
        map[string]interface{}{
            "type":  "ticker",
            "codes": codes,
        },
    }
    return c.Conn.WriteJSON(msg)
}

func (c *UpbitClient) Listen(onTicker func(price float64)) {
    for {
        _, msg, err := c.Conn.ReadMessage()
        if err != nil {
            break
        }
        var ticker TickerResponse
        if err := json.Unmarshal(msg, &ticker); err != nil {
            continue
        }
        onTicker(ticker.TradePrice)
    }
}