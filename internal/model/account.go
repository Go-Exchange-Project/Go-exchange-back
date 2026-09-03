package model

import (
	"time"

	"github.com/shopspring/decimal"
)

// AccountType은 돈이 담기는 칸의 종류다. 계정은 (종류, 소유자, 자산)으로 유일하다.
type AccountType string

const (
	// AccountUserAvailable은 사용자가 지금 쓸 수 있는 양이다.
	AccountUserAvailable AccountType = "USER_AVAILABLE"
	// AccountUserLocked는 주문이나 출금에 묶인 양이다.
	AccountUserLocked AccountType = "USER_LOCKED"
	// AccountFeeIncome은 거래소가 받은 수수료다. 이 계정이 없으면 수수료가 사라진다.
	AccountFeeIncome AccountType = "FEE_INCOME"
	// AccountExternalBank는 가짜 은행 바깥 세상이다.
	AccountExternalBank AccountType = "EXTERNAL_BANK"
	// AccountExternalChain은 가짜 블록체인 바깥 세상이다.
	AccountExternalChain AccountType = "EXTERNAL_CHAIN"
	// AccountDevMint는 개발용 지급의 출처다. 진짜 입금과 섞이지 않게 따로 둔다.
	AccountDevMint AccountType = "DEV_MINT"
)

// SystemAccountTypes는 소유자가 없는 계정 종류다.
var SystemAccountTypes = map[AccountType]bool{
	AccountFeeIncome:     true,
	AccountExternalBank:  true,
	AccountExternalChain: true,
	AccountDevMint:       true,
}

// negativeAllowedAccountTypes는 잔액이 음수여도 되는 계정이다.
//
// 사용자가 1,000원을 입금하면 은행 쪽은 −1,000이 된다. 이것은 "바깥에서 안으로
// 1,000이 들어왔다"는 뜻이지 빚이 아니다. 시스템 전체 합이 항상 0이 되는 것은
// 이 계정들이 반대편을 받아 주기 때문이다.
var negativeAllowedAccountTypes = map[AccountType]bool{
	AccountExternalBank:  true,
	AccountExternalChain: true,
	AccountDevMint:       true,
}

// AllowsNegativeBalance는 해당 종류의 계정이 음수 잔액을 가질 수 있는지 알려준다.
func AllowsNegativeBalance(accountType AccountType) bool {
	return negativeAllowedAccountTypes[accountType]
}

// IsSystemAccountType은 소유자 없는 계정 종류인지 알려준다.
func IsSystemAccountType(accountType AccountType) bool {
	return SystemAccountTypes[accountType]
}

// Account는 돈이 담기는 칸이다.
//
// UNIQUE는 표현식 인덱스라 GORM 태그로 걸 수 없다 — 시스템 계정의
// owner_user_id가 NULL이고 Postgres에서 NULL은 서로 다르게 취급되므로,
// COALESCE(owner_user_id, 0)을 쓴 인덱스를 migration 009에서 만든다.
type Account struct {
	ID             uint        `gorm:"primaryKey"`
	AccountType    AccountType `gorm:"size:32;not null;index:idx_accounts_owner_asset,priority:1"`
	OwnerUserID    *uint       `gorm:"index:idx_accounts_owner_asset,priority:2"`
	Asset          string      `gorm:"size:16;not null;index:idx_accounts_owner_asset,priority:3"`
	AllowsNegative bool        `gorm:"not null;default:false"`
	CreatedAt      time.Time   `gorm:"not null"`
}

// AccountBalance는 전기(posting) 합의 캐시다. 진실은 postings이고 이 표는
// 매번 합계를 내지 않기 위해 존재한다. 검산 2가 "캐시 == 전기 합"을 확인한다.
//
// LastPostingID는 어디까지 반영했는지를 기록해, 캐시가 어긋났을 때 어느
// 시점부터 다시 계산해야 하는지 알려준다.
type AccountBalance struct {
	AccountID     uint            `gorm:"primaryKey"`
	Balance       decimal.Decimal `gorm:"type:numeric;not null;default:0"`
	LastPostingID uint64          `gorm:"not null;default:0"`
	UpdatedAt     time.Time       `gorm:"not null"`
}
