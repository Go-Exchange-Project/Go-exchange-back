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

	fa, err := ComputeOrderFingerprint(a, CurrentOrderFingerprintVersion)
	require.NoError(t, err)
	fb, err := ComputeOrderFingerprint(b, CurrentOrderFingerprintVersion)
	require.NoError(t, err)

	assert.Equal(t, fa, fb)
}

// 자릿수를 잘라내는 것은 정규화가 아니라 정보 손실이다. 입력이 소수 18자리로 제한되지
// 않으므로, 19번째 자리만 다른 두 주문이 같은 지문을 받으면 서로를 재시도로 오인한다.
func TestComputeOrderFingerprintKeepsDigitsBeyondEighteenPlaces(t *testing.T) {
	a := fingerprintInput()
	a.Amount = decimal.RequireFromString("1.0000000000000000001")
	b := fingerprintInput()
	b.Amount = decimal.RequireFromString("1.0000000000000000002")

	fa, err := ComputeOrderFingerprint(a, CurrentOrderFingerprintVersion)
	require.NoError(t, err)
	fb, err := ComputeOrderFingerprint(b, CurrentOrderFingerprintVersion)
	require.NoError(t, err)

	assert.NotEqual(t, fa, fb)
}

// 단순 연결이면 ("BTC","SELL")과 ("BTCS","ELL")이 같은 입력 문자열이 된다.
func TestComputeOrderFingerprintIsUnambiguousAcrossFieldBoundaries(t *testing.T) {
	a := fingerprintInput()
	a.CoinSymbol, a.Side = "BTC", "SELL"
	b := fingerprintInput()
	b.CoinSymbol, b.Side = "BTCS", "ELL"

	fa, err := ComputeOrderFingerprint(a, CurrentOrderFingerprintVersion)
	require.NoError(t, err)
	fb, err := ComputeOrderFingerprint(b, CurrentOrderFingerprintVersion)
	require.NoError(t, err)

	assert.NotEqual(t, fa, fb, "필드 경계가 모호하면 서로 다른 요청이 같은 지문을 갖는다")
}

func TestComputeOrderFingerprintDiffersPerField(t *testing.T) {
	base, err := ComputeOrderFingerprint(fingerprintInput(), CurrentOrderFingerprintVersion)
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
		got, err := ComputeOrderFingerprint(in, CurrentOrderFingerprintVersion)
		require.NoError(t, err)
		assert.NotEqual(t, base, got, "%s가 지문에 반영되지 않았다", name)
	}
}

// 저장된 버전의 규칙으로 비교해야 배포만으로 기존 재시도가 409가 되지 않는다.
func TestComputeOrderFingerprintRejectsUnknownVersion(t *testing.T) {
	_, err := ComputeOrderFingerprint(fingerprintInput(), 99)
	require.Error(t, err)
}

// v1은 DB에 저장된 값이다. CurrentOrderFingerprintVersion을 2로 올려도 v1 계산은
// 그대로여야 한다 — 이 값이 바뀌면 배포만으로 기존 키의 재시도가 409가 된다.
// 버전을 올릴 때 이 테스트를 고치면 안 되고, 새 버전용 golden을 추가해야 한다.
func TestComputeOrderFingerprintV1IsFrozen(t *testing.T) {
	got, err := ComputeOrderFingerprint(fingerprintInput(), 1)
	require.NoError(t, err)

	assert.Equal(t, "95798288d827b6cccfc97d5bd57abb442f1f00047f1c9433b4f57463f699c398", got)
}
