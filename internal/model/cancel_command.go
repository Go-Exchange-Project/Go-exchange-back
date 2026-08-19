package model

import (
	"time"

	"github.com/shopspring/decimal"
)

type CancelCommandStatus string

// PROCESSED는 엔진이 주문을 제거했고 그 실행 이벤트가 execution outbox와 같은
// 트랜잭션에서 커밋됐다는 뜻이다. NOOP은 이미 체결·취소돼 할 일이 없었다는 뜻이며
// 실패가 아니다.
const (
	CancelCommandStatusPending   CancelCommandStatus = "PENDING"
	CancelCommandStatusProcessed CancelCommandStatus = "PROCESSED"
	CancelCommandStatusNoop      CancelCommandStatus = "NOOP"
)

// CancelCommand는 취소 "의도"를 엔진에 넣기 전에 내구 기록한다. 사용자에게 접수를
// 알린 뒤 프로세스가 죽어도 재기동한 worker가 이 행을 보고 취소를 다시 실행한다.
//
// Price가 필요한 이유: 엔진의 handleCancel은 가격 레벨로 주문을 찾으므로 이 값이
// 없으면 worker가 matching.CancelOrderCommand를 복원할 수 없다.
//
// AttemptCount는 관측용이다. 재시도 예산으로 쓰지 않는다 — 취소는 포기하면 안 된다.
//
// 이 테이블은 AutoMigrate 대상이 아니다. 스키마는 migration 007이 전부 소유한다.
// AutoMigrate에 넣으면 007이 만든 order_id UNIQUE를 GORM이 자기 명명규칙
// (uni_cancel_commands_order_id)으로 DROP하려 해서 두 번째 부팅부터 실패한다.
type CancelCommand struct {
	ID           uint64              `gorm:"primaryKey"`
	OrderID      uint                `gorm:"not null"`
	UserID       uint                `gorm:"not null"`
	CoinSymbol   string              `gorm:"not null"`
	Side         OrderSide           `gorm:"not null"`
	Price        decimal.Decimal     `gorm:"type:numeric;not null"`
	Status       CancelCommandStatus `gorm:"not null;default:PENDING"`
	AttemptCount int                 `gorm:"not null;default:0"`
	LastError    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
