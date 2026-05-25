package model

import (
	"time"

	"github.com/shopspring/decimal"
)

type LedgerEntryType string
type LedgerReferenceType string

const (
	LedgerEntryTypeDevFund         LedgerEntryType = "DEV_FUND"
	LedgerEntryTypeOrderHold       LedgerEntryType = "ORDER_HOLD"
	LedgerEntryTypeOrderRelease    LedgerEntryType = "ORDER_RELEASE"
	LedgerEntryTypeTradeSettlement LedgerEntryType = "TRADE_SETTLEMENT"
)

const (
	LedgerReferenceTypeDevFund LedgerReferenceType = "DEV_FUND"
	LedgerReferenceTypeOrder   LedgerReferenceType = "ORDER"
	LedgerReferenceTypeTrade   LedgerReferenceType = "TRADE"
)

type LedgerEntry struct {
	ID                    uint                `gorm:"primaryKey"`
	UserID                uint                `gorm:"not null;index:idx_ledger_entries_user_asset_created_at,priority:1"`
	CoinSymbol            string              `gorm:"not null;index:idx_ledger_entries_user_asset_created_at,priority:2"`
	EntryType             LedgerEntryType     `gorm:"size:64;not null;index:idx_ledger_entries_type_reference,priority:1"`
	AvailableDelta        decimal.Decimal     `gorm:"type:numeric;not null;default:0"`
	LockedDelta           decimal.Decimal     `gorm:"type:numeric;not null;default:0"`
	AvailableBalanceAfter decimal.Decimal     `gorm:"type:numeric;not null;default:0;check:ck_ledger_entries_available_after_non_negative,available_balance_after >= 0"`
	LockedBalanceAfter    decimal.Decimal     `gorm:"type:numeric;not null;default:0;check:ck_ledger_entries_locked_after_non_negative,locked_balance_after >= 0"`
	ReferenceType         LedgerReferenceType `gorm:"size:32;not null;index:idx_ledger_entries_type_reference,priority:2"`
	ReferenceID           uint                `gorm:"not null;index:idx_ledger_entries_type_reference,priority:3"`
	ReferenceKey          string              `gorm:"size:128;index:idx_ledger_entries_reference_key"`
	CreatedAt             time.Time           `gorm:"not null;index:idx_ledger_entries_user_asset_created_at,priority:3"`
}
