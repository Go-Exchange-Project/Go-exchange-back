package service

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fingerprintInput() OrderFingerprintInput {
	return OrderFingerprintInput{
		UserID:      7,
		CoinSymbol:  "BTC",
		Side:        "SELL",
		OrderType:   "LIMIT",
		Price:       decimal.RequireFromString("100.50"),
		Amount:      decimal.RequireFromString("1.5"),
		QuoteAmount: decimal.Zero,
	}
}

// 1.50과 1.5는 같은 주문이다. 표현 차이가 다른 지문이 되면 재시도가 409가 된다.
func TestComputeOrderFingerprintNormalizesDecimals(t *testing.T) {
	a := fingerprintInput()
	a.Price = decimal.RequireFromString("100.50")
	b := fingerprintInput()
	b.Price = decimal.RequireFromString("100.5")

	fa, err := ComputeOrderFingerprint(a, OrderFingerprintVersion)
	require.NoError(t, err)
	fb, err := ComputeOrderFingerprint(b, OrderFingerprintVersion)
	require.NoError(t, err)

	assert.Equal(t, fa, fb)
}

// 단순 연결이면 ("BTC","SELL")과 ("BTCS","ELL")이 같은 입력 문자열이 된다.
func TestComputeOrderFingerprintIsUnambiguousAcrossFieldBoundaries(t *testing.T) {
	a := fingerprintInput()
	a.CoinSymbol, a.Side = "BTC", "SELL"
	b := fingerprintInput()
	b.CoinSymbol, b.Side = "BTCS", "ELL"

	fa, err := ComputeOrderFingerprint(a, OrderFingerprintVersion)
	require.NoError(t, err)
	fb, err := ComputeOrderFingerprint(b, OrderFingerprintVersion)
	require.NoError(t, err)

	assert.NotEqual(t, fa, fb, "필드 경계가 모호하면 서로 다른 요청이 같은 지문을 갖는다")
}

func TestComputeOrderFingerprintDiffersPerField(t *testing.T) {
	base, err := ComputeOrderFingerprint(fingerprintInput(), OrderFingerprintVersion)
	require.NoError(t, err)

	mutations := map[string]func(*OrderFingerprintInput){
		"user":  func(in *OrderFingerprintInput) { in.UserID = 8 },
		"coin":  func(in *OrderFingerprintInput) { in.CoinSymbol = "ETH" },
		"side":  func(in *OrderFingerprintInput) { in.Side = "BUY" },
		"type":  func(in *OrderFingerprintInput) { in.OrderType = "MARKET" },
		"price": func(in *OrderFingerprintInput) { in.Price = decimal.RequireFromString("101") },
		"amt":   func(in *OrderFingerprintInput) { in.Amount = decimal.RequireFromString("2") },
		"quote": func(in *OrderFingerprintInput) { in.QuoteAmount = decimal.RequireFromString("5") },
	}
	for name, mutate := range mutations {
		in := fingerprintInput()
		mutate(&in)
		got, err := ComputeOrderFingerprint(in, OrderFingerprintVersion)
		require.NoError(t, err)
		assert.NotEqual(t, base, got, "%s가 지문에 반영되지 않았다", name)
	}
}

// 저장된 버전의 규칙으로 비교해야 배포만으로 기존 재시도가 409가 되지 않는다.
func TestComputeOrderFingerprintRejectsUnknownVersion(t *testing.T) {
	_, err := ComputeOrderFingerprint(fingerprintInput(), 99)
	require.Error(t, err)
}
