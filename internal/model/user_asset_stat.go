package model

import (
	"time"

	"github.com/shopspring/decimal"
)

// UserAssetStat은 사용자별·자산별 통계다.
//
// 평균 매수가는 자산이 아니라 통계이므로 원장에 넣지 않는다. 원장의 전기는
// 자산별 합이 0이어야 하는데 평단가에는 그런 성질이 없고, 섞어 두면 검산이
// 의미를 잃는다.
//
// 표는 다른 원장 표와 함께 만들지만, 값을 채우는 곳은 매수 정산 한 곳뿐이다.
type UserAssetStat struct {
	ID          uint            `gorm:"primaryKey"`
	UserID      uint            `gorm:"not null;uniqueIndex:idx_user_asset_stats_user_asset,priority:1"`
	Asset       string          `gorm:"size:16;not null;uniqueIndex:idx_user_asset_stats_user_asset,priority:2"`
	AvgBuyPrice decimal.Decimal `gorm:"type:numeric;not null;default:0"`
	UpdatedAt   time.Time       `gorm:"not null"`
}
