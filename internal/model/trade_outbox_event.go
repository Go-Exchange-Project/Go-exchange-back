package model

import "time"

type TradeOutboxEventType string

const (
	TradeOutboxEventTypeTrade           TradeOutboxEventType = "TRADE"
	TradeOutboxEventTypeMarketOrderDone TradeOutboxEventType = "MARKET_ORDER_DONE"
)

type TradeOutboxStatus string

const (
	TradeOutboxStatusPending   TradeOutboxStatus = "PENDING"
	TradeOutboxStatusProcessed TradeOutboxStatus = "PROCESSED"
)

// TradeOutboxEvent는 엔진이 방출한 체결/시장가 완료 이벤트의 write-ahead 기록입니다.
// 정산은 이 테이블에 커밋된 이벤트만 처리합니다 — 커밋 이전 크래시는 매칭 자체가
// 롤백되고(자금 무변동), 이후 크래시는 부팅 리플레이가 PENDING을 재처리합니다.
// 단일 writer가 순서대로 삽입하므로 ID 순서 = 엔진 방출 순서입니다.
type TradeOutboxEvent struct {
	ID            uint64               `gorm:"primaryKey"`
	EventType     TradeOutboxEventType `gorm:"size:32;not null;check:ck_trade_outbox_event_type,event_type IN ('TRADE','MARKET_ORDER_DONE')"`
	CoinSymbol    string               `gorm:"size:32;not null"`
	EngineEventID string               `gorm:"size:128;not null;default:''"`
	Payload       []byte               `gorm:"type:jsonb;not null"`
	Status        TradeOutboxStatus    `gorm:"size:16;not null;default:'PENDING';index:idx_trade_outbox_events_pending,where:status = 'PENDING';check:ck_trade_outbox_status,status IN ('PENDING','PROCESSED')"`
	CreatedAt     time.Time
	ProcessedAt   *time.Time
}
