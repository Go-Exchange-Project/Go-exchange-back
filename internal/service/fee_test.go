package service

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyTradeFeePolicyChargesKRWFees(t *testing.T) {
	trade := &model.Trade{
		CoinSymbol: "BTC",
		Price:      decimal.NewFromInt(5000),
		Quantity:   decimal.NewFromInt(2),
	}

	err := applyTradeFeePolicy(trade)

	require.NoError(t, err)
	assert.True(t, trade.FeeRate.Equal(decimal.RequireFromString("0.0005")))
	assert.True(t, trade.BuyerFee.Equal(decimal.NewFromInt(5)))
	assert.Equal(t, model.KRWAssetSymbol, trade.BuyerFeeAsset)
	assert.True(t, trade.SellerFee.Equal(decimal.NewFromInt(5)))
	assert.Equal(t, model.KRWAssetSymbol, trade.SellerFeeAsset)
}

func TestQuoteAmountWithTradingFee(t *testing.T) {
	assert.True(t, quoteAmountWithTradingFee(decimal.NewFromInt(100)).Equal(decimal.RequireFromString("100.05")))
}

func TestMarketBuyExecutableQuoteAmountTreatsBudgetAsFeeInclusive(t *testing.T) {
	executableQuote := marketBuyExecutableQuoteAmount(decimal.RequireFromString("100.05"))

	assert.True(t, executableQuote.Equal(decimal.NewFromInt(100)), "executable_quote=%s", executableQuote.String())
}

func TestAmountAfterFeeReturnsNetAmount(t *testing.T) {
	net, err := amountAfterFee(
		decimal.NewFromInt(10000),
		decimal.NewFromInt(5),
		"seller",
	)

	require.NoError(t, err)
	assert.True(t, net.Equal(decimal.NewFromInt(9995)))
}

func TestAmountAfterFeeRejectsFeeGreaterThanOrEqualToGross(t *testing.T) {
	_, err := amountAfterFee(decimal.NewFromInt(1), decimal.NewFromInt(1), "buyer")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "fee must be less than gross amount")
}
