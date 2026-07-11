package model

import "time"

// ReconciliationViolation은 ReconciliationWorker가 발견한 정합성 위반을 기록합니다.
// 위반이 없으면 아무 행도 만들지 않으므로, 존재 자체가 "그 시점에 위반이 있었다"는
// 증거입니다. 같은 위반이 해소될 때까지 매 실행마다 새 행이 쌓입니다(dedup 안 함 —
// 위반 지속 이력 자체가 유용한 데이터).
type ReconciliationViolation struct {
	ID         uint      `gorm:"primaryKey"`
	CheckName  string    `gorm:"not null"`       // ledger_wallet | asset_conservation | stale_market_order | legacy_mismatch
	SubjectKey string    `gorm:"not null;index"` // 예: "wallet:123", "coin:BTC", "order:42"
	Detail     string    `gorm:"type:text;not null"`
	DetectedAt time.Time `gorm:"not null"`
}
