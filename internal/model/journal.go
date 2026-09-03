package model

import (
	"time"

	"github.com/shopspring/decimal"
)

// JournalEventType은 한 사건의 종류다.
type JournalEventType string

const (
	JournalEventDeposit      JournalEventType = "DEPOSIT"
	JournalEventWithdrawal   JournalEventType = "WITHDRAWAL"
	JournalEventOrderHold    JournalEventType = "ORDER_HOLD"
	JournalEventOrderRelease JournalEventType = "ORDER_RELEASE"
	JournalEventTrade        JournalEventType = "TRADE"
	JournalEventDevFund      JournalEventType = "DEV_FUND"
	JournalEventReversal     JournalEventType = "REVERSAL"
)

// JournalReferenceType은 분개가 가리키는 원본 자료의 종류다.
type JournalReferenceType string

const (
	JournalReferenceOrder    JournalReferenceType = "ORDER"
	JournalReferenceTrade    JournalReferenceType = "TRADE"
	JournalReferenceTransfer JournalReferenceType = "TRANSFER"
	JournalReferenceDevFund  JournalReferenceType = "DEV_FUND"
)

// JournalEntry는 사건 하나다. 이 묶음에 속한 전기들의 자산별 합은 정확히 0이다.
//
// IdempotencyKey UNIQUE가 같은 사건이 두 번 기록되는 것을 DB에서 막는다.
// 중복은 INSERT ... ON CONFLICT DO NOTHING RETURNING의 반환 없음으로 판정하며,
// 유니크 위반을 일으켜 잡지 않는다 — Postgres에서 그 위반은 트랜잭션 전체를
// abort시켜 이후 문장을 전부 막는다.
//
// UPDATE·DELETE를 하지 않는다. 정정은 역분개(ReversesJournalID)로만 한다.
type JournalEntry struct {
	ID             uint                 `gorm:"primaryKey"`
	EventType      JournalEventType     `gorm:"size:32;not null"`
	IdempotencyKey string               `gorm:"size:160;not null;uniqueIndex:idx_journal_entries_idempotency_key"`
	ReferenceType  JournalReferenceType `gorm:"size:32;not null;index:idx_journal_entries_reference,priority:1"`
	ReferenceID    uint                 `gorm:"not null;index:idx_journal_entries_reference,priority:2"`
	// ReversesJournalID는 역분개일 때만 채워진다. UNIQUE라 한 분개는 최대 한 번만
	// 되돌릴 수 있다.
	ReversesJournalID *uint     `gorm:"uniqueIndex:idx_journal_entries_reverses"`
	CreatedAt         time.Time `gorm:"not null"`
}

// Posting은 계정 하나에 대한 한 줄 기록이다. 들어오면 양수, 나가면 음수다.
//
// 금액 0짜리 전기는 아무 사실도 기록하지 않으면서 분개만 늘리므로 막는다
// (migration 009의 CHECK). 첫 구현의 출금 수수료가 0인 것이 그 사례다.
//
// 인덱스는 전부 migration 009가 만든다 — 계정별 합계용 (account_id, id) 복합
// 인덱스를 GORM 태그로 표현하면 id가 primaryKey라 단일 인덱스가 하나 더 생긴다.
type Posting struct {
	ID        uint64          `gorm:"primaryKey"`
	JournalID uint            `gorm:"not null"`
	AccountID uint            `gorm:"not null"`
	Asset     string          `gorm:"size:16;not null"`
	Amount    decimal.Decimal `gorm:"type:numeric;not null"`
	CreatedAt time.Time       `gorm:"not null"`
}
