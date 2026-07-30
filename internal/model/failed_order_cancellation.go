package model

import "time"

// FailedOrderCancellation은 취소 terminal을 실행하지 못했을 때 남기는 내구 기록입니다.
// 진실의 원본은 outbox이며, 이 테이블은 온라인 복구를 위한 retry index입니다 —
// 기록이 커밋되면 원본 outbox는 PROCESSED로 닫히고(durable handoff) 이후 복구는
// SettlementRetryWorker가 담당합니다.
// OrderCancelled 이벤트는 엔진 메모리에만 존재하므로 여기 저장된 필드만으로
// 취소를 재시도할 수 있어야 합니다.
type FailedOrderCancellation struct {
	ID uint `gorm:"primaryKey"`
	// 주문당 record 1건으로 수렴시킨다 — replay·재시도가 같은 행을 멱등 재사용한다.
	OrderID uint `gorm:"not null;uniqueIndex:idx_failed_order_cancellations_order_id"`
	// 원본 추적·감사·1:1 연결용 provenance. 복구 시 마킹 키가 아니다.
	OutboxEventID uint64                 `gorm:"not null"`
	CoinSymbol    string                 `gorm:"not null"`
	Side          OrderSide              `gorm:"not null"`
	EngineEventID string                 `gorm:"type:text"`
	ErrorMessage  string                 `gorm:"type:text;not null;check:ck_failed_order_cancellations_error_message_not_empty,length(btrim(error_message)) > 0"`
	Status        FailedSettlementStatus `gorm:"not null;default:OPEN;check:ck_failed_order_cancellations_status_valid,status IN ('OPEN', 'RESOLVED')"`
	// dependency 차단으로 생성되면 0(실행 시도 없음), 실제 실패면 1부터.
	RetryCount uint      `gorm:"not null;default:0;check:ck_failed_order_cancellations_retry_count_non_negative,retry_count >= 0"`
	OccurredAt time.Time `gorm:"not null"`
	Resolution string    `gorm:"type:text"`
	ResolvedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}
