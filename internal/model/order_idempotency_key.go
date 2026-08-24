package model

import "time"

type OrderIdempotencyOutcome string

// PENDING은 "아직 진행 중"만 뜻하지 않는다. 이후 UPDATE가 실패해도 여기 머물므로,
// "이 시점 이후를 서버가 durable하게 알지 못한다"는 뜻이다.
const (
	OrderIdempotencyOutcomePending  OrderIdempotencyOutcome = "PENDING"
	OrderIdempotencyOutcomeAccepted OrderIdempotencyOutcome = "ACCEPTED"
	OrderIdempotencyOutcomeRejected OrderIdempotencyOutcome = "REJECTED"
	OrderIdempotencyOutcomeUnknown  OrderIdempotencyOutcome = "UNKNOWN"
)

// OrderIdempotencyKey는 주문 생성 요청의 재시도를 식별한다.
//
// 이 테이블은 AutoMigrate 대상이 아니다. 스키마는 migration 008이 전부 소유한다 —
// AutoMigrate에 넣으면 008이 만든 UNIQUE를 GORM이 자기 명명규칙
// (uni_order_idempotency_keys_...)으로 DROP하려 해 두 번째 부팅부터 실패한다.
type OrderIdempotencyKey struct {
	ID                 uint64                  `gorm:"primaryKey"`
	UserID             uint                    `gorm:"not null"`
	IdempotencyKey     string                  `gorm:"not null"`
	Fingerprint        string                  `gorm:"not null"`
	FingerprintVersion int                     `gorm:"not null"`
	OrderID            *uint                   // 1단계 INSERT 시점에는 모른다
	Outcome            OrderIdempotencyOutcome // 〃
	CreatedAt          time.Time
	UpdatedAt          time.Time
}
