package service

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
)

// 여러 체결 순열에 대해 러닝 가중평균의 최대 오차를 측정한다(balance.go의 산술과 동일).
// "항상 1e-16"이라 단정하지 않고, 관측된 최대 오차가 tolerance 이하임을 단언한다.
func TestAvgBuyPriceOrderDependenceStaysWithinTolerance(t *testing.T) {
	tolerance := decimal.RequireFromString("0.000001") // 표시 단위보다 충분히 작음
	fills := []struct{ qty, cost string }{
		{"0.11", "5500001.11"}, {"0.13", "6500003.33"},
		{"0.17", "8500007.77"}, {"0.19", "9500011.19"},
	}
	apply := func(order []int) decimal.Decimal {
		qty := decimal.RequireFromString("0.7")
		avg := decimal.RequireFromString("49999999.9999999999999999")
		for _, i := range order {
			q := decimal.RequireFromString(fills[i].qty)
			c := decimal.RequireFromString(fills[i].cost)
			newQty := qty.Add(q)
			avg = avg.Mul(qty).Add(c).Div(newQty)
			qty = newQty
		}
		return avg
	}
	base := apply([]int{0, 1, 2, 3})
	maxDiff := decimal.Zero
	for _, perm := range [][]int{{3, 2, 1, 0}, {1, 0, 3, 2}, {2, 3, 0, 1}, {0, 2, 1, 3}, {3, 0, 2, 1}} {
		diff := base.Sub(apply(perm)).Abs()
		if diff.GreaterThan(maxDiff) {
			maxDiff = diff
		}
	}
	t.Logf("관측된 최대 AvgBuyPrice 순서 오차 = %s", maxDiff) // 완료 문서에 이 값을 기록
	assert.True(t, maxDiff.LessThanOrEqual(tolerance),
		"순열 간 평균매입가 오차 %s가 허용치 %s를 초과", maxDiff, tolerance)
}
