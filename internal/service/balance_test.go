package service

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyBuyOrderHoldMovesKRWAvailableToLocked(t *testing.T) {
	wallet := &model.Wallet{
		CoinSymbol:       model.KRWAssetSymbol,
		AvailableBalance: decimal.NewFromInt(1000),
		LockedBalance:    decimal.Zero,
	}

	update, err := applyBuyOrderHold(wallet, decimal.NewFromInt(250))

	require.NoError(t, err)
	assert.True(t, update.AvailableBalance.Equal(decimal.NewFromInt(750)))
	assert.True(t, update.LockedBalance.Equal(decimal.NewFromInt(250)))
	assert.True(t, update.KRW.Equal(decimal.NewFromInt(1000)))
}

func TestApplyBuyOrderHoldFallsBackToLegacyKRWWhenBalanceFieldsAreEmpty(t *testing.T) {
	wallet := &model.Wallet{
		CoinSymbol: model.KRWAssetSymbol,
		KRW:        decimal.NewFromInt(1000),
	}

	update, err := applyBuyOrderHold(wallet, decimal.NewFromInt(400))

	require.NoError(t, err)
	assert.True(t, update.AvailableBalance.Equal(decimal.NewFromInt(600)))
	assert.True(t, update.LockedBalance.Equal(decimal.NewFromInt(400)))
	assert.True(t, update.KRW.Equal(decimal.NewFromInt(1000)))
}

func TestApplyBuyOrderHoldRejectsInsufficientAvailableBalance(t *testing.T) {
	wallet := &model.Wallet{
		CoinSymbol:       model.KRWAssetSymbol,
		AvailableBalance: decimal.NewFromInt(100),
	}

	_, err := applyBuyOrderHold(wallet, decimal.NewFromInt(101))

	require.Error(t, err)
}

func TestApplySellOrderHoldMovesCoinAvailableToLocked(t *testing.T) {
	wallet := &model.Wallet{
		CoinSymbol:       "BTC",
		AvailableBalance: decimal.RequireFromString("2.5"),
		LockedBalance:    decimal.RequireFromString("0.5"),
	}

	update, err := applySellOrderHold(wallet, decimal.RequireFromString("1.25"))

	require.NoError(t, err)
	assert.True(t, update.AvailableBalance.Equal(decimal.RequireFromString("1.25")))
	assert.True(t, update.LockedBalance.Equal(decimal.RequireFromString("1.75")))
	assert.True(t, update.Quantity.Equal(decimal.RequireFromString("3.0")))
}

func TestApplySellOrderHoldRejectsInsufficientAvailableBalance(t *testing.T) {
	wallet := &model.Wallet{
		CoinSymbol:       "BTC",
		AvailableBalance: decimal.RequireFromString("0.5"),
	}

	_, err := applySellOrderHold(wallet, decimal.RequireFromString("0.75"))

	require.Error(t, err)
}

func TestSettleBuyerKRWConsumesLockedAndRefundsPriceImprovement(t *testing.T) {
	wallet := &model.Wallet{
		CoinSymbol:       model.KRWAssetSymbol,
		AvailableBalance: decimal.NewFromInt(100),
		LockedBalance:    decimal.NewFromInt(600),
	}

	update, err := settleBuyerKRW(wallet, decimal.NewFromInt(600), decimal.NewFromInt(500))

	require.NoError(t, err)
	assert.True(t, update.AvailableBalance.Equal(decimal.NewFromInt(200)))
	assert.True(t, update.LockedBalance.Equal(decimal.Zero))
	assert.True(t, update.KRW.Equal(decimal.NewFromInt(200)))
}

func TestSettleBuyerKRWLeavesRemainingLockedAfterPartialFill(t *testing.T) {
	wallet := &model.Wallet{
		CoinSymbol:       model.KRWAssetSymbol,
		AvailableBalance: decimal.Zero,
		LockedBalance:    decimal.NewFromInt(1000),
	}

	update, err := settleBuyerKRW(wallet, decimal.NewFromInt(400), decimal.NewFromInt(400))

	require.NoError(t, err)
	assert.True(t, update.AvailableBalance.Equal(decimal.Zero))
	assert.True(t, update.LockedBalance.Equal(decimal.NewFromInt(600)))
	assert.True(t, update.KRW.Equal(decimal.NewFromInt(600)))
}

func TestSettleBuyerKRWRejectsInsufficientLockedBalance(t *testing.T) {
	wallet := &model.Wallet{
		CoinSymbol:    model.KRWAssetSymbol,
		LockedBalance: decimal.NewFromInt(100),
	}

	_, err := settleBuyerKRW(wallet, decimal.NewFromInt(150), decimal.NewFromInt(150))

	require.Error(t, err)
}

func TestSettleSellerCoinConsumesLockedQuantity(t *testing.T) {
	wallet := &model.Wallet{
		CoinSymbol:       "BTC",
		AvailableBalance: decimal.RequireFromString("0.25"),
		LockedBalance:    decimal.RequireFromString("1.5"),
		AvgBuyPrice:      decimal.NewFromInt(100),
	}

	update, err := settleSellerCoin(wallet, decimal.RequireFromString("0.5"))

	require.NoError(t, err)
	assert.True(t, update.AvailableBalance.Equal(decimal.RequireFromString("0.25")))
	assert.True(t, update.LockedBalance.Equal(decimal.RequireFromString("1.0")))
	assert.True(t, update.Quantity.Equal(decimal.RequireFromString("1.25")))
	assert.True(t, update.AvgBuyPrice.Equal(decimal.NewFromInt(100)))
}

func TestSettleSellerCoinResetsAvgBuyPriceWhenFullySold(t *testing.T) {
	wallet := &model.Wallet{
		CoinSymbol:       "BTC",
		AvailableBalance: decimal.Zero,
		LockedBalance:    decimal.NewFromInt(1),
		AvgBuyPrice:      decimal.NewFromInt(100),
	}

	update, err := settleSellerCoin(wallet, decimal.NewFromInt(1))

	require.NoError(t, err)
	assert.True(t, update.Quantity.IsZero())
	assert.True(t, update.AvgBuyPrice.IsZero())
}

func TestCreditBuyerCoinWithAcquisitionCostSetsFirstAvgBuyPrice(t *testing.T) {
	wallet := &model.Wallet{
		CoinSymbol:       "BTC",
		AvailableBalance: decimal.Zero,
		LockedBalance:    decimal.Zero,
		AvgBuyPrice:      decimal.Zero,
	}

	update, err := creditBuyerCoinWithAcquisitionCost(wallet, decimal.NewFromInt(2), decimal.NewFromInt(210))

	require.NoError(t, err)
	assert.True(t, update.AvailableBalance.Equal(decimal.NewFromInt(2)))
	assert.True(t, update.Quantity.Equal(decimal.NewFromInt(2)))
	assert.True(t, update.AvgBuyPrice.Equal(decimal.NewFromInt(105)))
}

func TestCreditBuyerCoinWithAcquisitionCostUsesWeightedAverage(t *testing.T) {
	wallet := &model.Wallet{
		CoinSymbol:       "BTC",
		AvailableBalance: decimal.NewFromInt(1),
		LockedBalance:    decimal.NewFromInt(1),
		Quantity:         decimal.NewFromInt(2),
		AvgBuyPrice:      decimal.NewFromInt(100),
	}

	update, err := creditBuyerCoinWithAcquisitionCost(wallet, decimal.NewFromInt(1), decimal.NewFromInt(150))

	require.NoError(t, err)
	assert.True(t, update.AvailableBalance.Equal(decimal.NewFromInt(2)))
	assert.True(t, update.LockedBalance.Equal(decimal.NewFromInt(1)))
	assert.True(t, update.Quantity.Equal(decimal.NewFromInt(3)))
	assert.True(t, update.AvgBuyPrice.Equal(decimal.RequireFromString("116.6666666666666667")))
}

func TestCreditBuyerCoinWithAcquisitionCostTreatsZeroAverageInventoryAsZeroCost(t *testing.T) {
	wallet := &model.Wallet{
		CoinSymbol:       "BTC",
		AvailableBalance: decimal.NewFromInt(1),
		LockedBalance:    decimal.Zero,
		Quantity:         decimal.NewFromInt(1),
		AvgBuyPrice:      decimal.Zero,
	}

	update, err := creditBuyerCoinWithAcquisitionCost(wallet, decimal.NewFromInt(1), decimal.NewFromInt(100))

	require.NoError(t, err)
	assert.True(t, update.AvailableBalance.Equal(decimal.NewFromInt(2)))
	assert.True(t, update.Quantity.Equal(decimal.NewFromInt(2)))
	assert.True(t, update.AvgBuyPrice.Equal(decimal.NewFromInt(50)))
}

func TestCreditAvailableKeepsLockedBalance(t *testing.T) {
	wallet := &model.Wallet{
		CoinSymbol:       "BTC",
		AvailableBalance: decimal.RequireFromString("0.25"),
		LockedBalance:    decimal.RequireFromString("1.0"),
	}

	update, err := creditAvailable(wallet, decimal.RequireFromString("0.75"))

	require.NoError(t, err)
	assert.True(t, update.AvailableBalance.Equal(decimal.RequireFromString("1.0")))
	assert.True(t, update.LockedBalance.Equal(decimal.RequireFromString("1.0")))
	assert.True(t, update.Quantity.Equal(decimal.RequireFromString("2.0")))
}

func TestCreditAvailableKRWIncreasesAvailableAndLegacyKRW(t *testing.T) {
	wallet := &model.Wallet{
		CoinSymbol:       model.KRWAssetSymbol,
		AvailableBalance: decimal.NewFromInt(100),
		LockedBalance:    decimal.NewFromInt(50),
	}

	update, err := creditAvailable(wallet, decimal.NewFromInt(25))

	require.NoError(t, err)
	assert.True(t, update.AvailableBalance.Equal(decimal.NewFromInt(125)))
	assert.True(t, update.LockedBalance.Equal(decimal.NewFromInt(50)))
	assert.True(t, update.KRW.Equal(decimal.NewFromInt(175)))
}

func TestReleaseBuyOrderHoldMovesKRWLockedToAvailable(t *testing.T) {
	wallet := &model.Wallet{
		CoinSymbol:       model.KRWAssetSymbol,
		AvailableBalance: decimal.NewFromInt(100),
		LockedBalance:    decimal.NewFromInt(500),
	}

	update, err := releaseBuyOrderHold(wallet, decimal.NewFromInt(300))

	require.NoError(t, err)
	assert.True(t, update.AvailableBalance.Equal(decimal.NewFromInt(400)))
	assert.True(t, update.LockedBalance.Equal(decimal.NewFromInt(200)))
	assert.True(t, update.KRW.Equal(decimal.NewFromInt(600)))
}

func TestReleaseSellOrderHoldMovesCoinLockedToAvailable(t *testing.T) {
	wallet := &model.Wallet{
		CoinSymbol:       "BTC",
		AvailableBalance: decimal.RequireFromString("0.25"),
		LockedBalance:    decimal.RequireFromString("1.50"),
	}

	update, err := releaseSellOrderHold(wallet, decimal.RequireFromString("0.75"))

	require.NoError(t, err)
	assert.True(t, update.AvailableBalance.Equal(decimal.RequireFromString("1.00")))
	assert.True(t, update.LockedBalance.Equal(decimal.RequireFromString("0.75")))
	assert.True(t, update.Quantity.Equal(decimal.RequireFromString("1.75")))
}

func TestReleaseOrderHoldRejectsInsufficientLockedBalance(t *testing.T) {
	krwWallet := &model.Wallet{
		CoinSymbol:    model.KRWAssetSymbol,
		LockedBalance: decimal.NewFromInt(100),
	}
	coinWallet := &model.Wallet{
		CoinSymbol:    "BTC",
		LockedBalance: decimal.NewFromInt(1),
	}

	_, err := releaseBuyOrderHold(krwWallet, decimal.NewFromInt(101))
	require.Error(t, err)

	_, err = releaseSellOrderHold(coinWallet, decimal.NewFromInt(2))
	require.Error(t, err)
}
