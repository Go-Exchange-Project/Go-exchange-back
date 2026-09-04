package service

import (
	"errors"
	"fmt"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// holdOrderAssets는 주문 1건의 자산을 잠근다. 호출자의 트랜잭션 안에서 돈다.
//
// 잔고 부족은 LedgerService의 음수 검사가 ConflictError로 돌려주므로, 여기서
// 미리 읽어 비교하지 않는다. 읽고 나서 잠그는 사이에 잔액이 바뀌면 그 검사는
// 거짓말이 된다 — 잠근 상태에서 한 번만 판정하는 것이 유일하게 옳다.
func holdOrderAssets(ledger *LedgerService, tx *gorm.DB, order *model.Order) error {
	if order.Side != model.OrderSideBuy && order.Side != model.OrderSideSell {
		return NewValidationErrorf("invalid order side")
	}
	amount := holdAmountFor(order)
	if !amount.IsPositive() {
		return NewValidationErrorf("hold amount must be greater than zero")
	}

	_, _, err := ledger.Record(tx, JournalInput{
		EventType:      model.JournalEventOrderHold,
		IdempotencyKey: fmt.Sprintf("order-hold:%d", order.ID),
		ReferenceType:  model.JournalReferenceOrder,
		ReferenceID:    order.ID,
		Postings:       orderHoldPostings(order, amount),
	})
	return asInsufficientBalance(err, order.Side)
}

// releaseOrderAssets는 잠긴 자산을 사용 가능 잔액으로 되돌린다.
//
// reason이 멱등성 키에 들어간다. 사용자 취소와 시장가 잔액 반환이 같은 키를
// 쓰면 둘 중 하나가 조용히 무시된다 — 그러면 돈이 잠긴 채 남는다.
func releaseOrderAssets(ledger *LedgerService, tx *gorm.DB, order *model.Order, amount decimal.Decimal, reason string) error {
	if order.Side != model.OrderSideBuy && order.Side != model.OrderSideSell {
		return NewValidationErrorf("invalid order side")
	}
	if !amount.IsPositive() {
		return NewValidationErrorf("release amount must be greater than zero")
	}
	if reason == "" {
		return NewValidationErrorf("release reason is required")
	}

	_, _, err := ledger.Record(tx, JournalInput{
		EventType:      model.JournalEventOrderRelease,
		IdempotencyKey: fmt.Sprintf("order-release:%d:%s", order.ID, reason),
		ReferenceType:  model.JournalReferenceOrder,
		ReferenceID:    order.ID,
		Postings:       orderReleasePostings(order, amount),
	})
	return err
}

// 주문 해제 사유. 멱등성 키의 일부이므로 값이 바뀌면 과거 해제가 재실행된다.
const (
	releaseReasonCancel       = "cancel"
	releaseReasonMarketRemain = "market-remain"
)

// asInsufficientBalance는 원장의 음수 거부를 기존 주문 경로가 쓰던 메시지로
// 바꾼다. HTTP 계약(409 + 문구)이 바뀌지 않게 하기 위해서다.
func asInsufficientBalance(err error, side model.OrderSide) error {
	if err == nil {
		return nil
	}
	var domain *DomainError
	if !errors.As(err, &domain) || domain.Kind != ErrorKindConflict {
		return err
	}
	return insufficientBalanceError(side)
}

// insufficientBalanceError는 잔고 부족의 표준 문구다. 배치 경로의 사전 검증과
// 원장의 음수 거부가 같은 메시지를 내야 HTTP 계약이 갈리지 않는다.
func insufficientBalanceError(side model.OrderSide) error {
	if side == model.OrderSideBuy {
		return NewConflictErrorf("insufficient available KRW balance")
	}
	return NewConflictErrorf("insufficient available coin balance")
}

// orderHoldAsset은 주문이 잠그는 자산을 돌려준다. 매수는 KRW, 매도는 코인이다.
func orderHoldAsset(order *model.Order) string {
	if order.Side == model.OrderSideBuy {
		return model.KRWAssetSymbol
	}
	return order.CoinSymbol
}

// orderHoldPostings는 available에서 locked로 옮기는 전기 2줄이다.
func orderHoldPostings(order *model.Order, amount decimal.Decimal) []PostingInput {
	asset := orderHoldAsset(order)
	userID := order.UserID
	return []PostingInput{
		{AccountType: model.AccountUserAvailable, OwnerUserID: &userID, Asset: asset, Amount: amount.Neg()},
		{AccountType: model.AccountUserLocked, OwnerUserID: &userID, Asset: asset, Amount: amount},
	}
}

// orderReleasePostings는 잠금 전기의 반대 방향이다.
func orderReleasePostings(order *model.Order, amount decimal.Decimal) []PostingInput {
	asset := orderHoldAsset(order)
	userID := order.UserID
	return []PostingInput{
		{AccountType: model.AccountUserLocked, OwnerUserID: &userID, Asset: asset, Amount: amount.Neg()},
		{AccountType: model.AccountUserAvailable, OwnerUserID: &userID, Asset: asset, Amount: amount},
	}
}

// tradePostings는 체결 하나의 전기를 만든다. 단건 정산과 배치 정산이 이 함수
// 하나를 공유한다 — 각자 만들면 배치 결과가 단건 N회와 달라지는 것을 아무도
// 눈치채지 못한다(settlement_batch.go의 등가성 불변식).
//
//	USER_LOCKED(매수자)    KRW  −reservedDebit
//	USER_AVAILABLE(매수자) KRW  +(reservedDebit − executionDebit)   ← 0이면 줄을 만들지 않는다
//	USER_AVAILABLE(매도자) KRW  +sellerQuoteNet
//	FEE_INCOME             KRW  +(buyerFee + sellerFee)
//	USER_LOCKED(매도자)    코인 −quantity
//	USER_AVAILABLE(매수자) 코인 +quantity
//
// KRW 합이 0인 것은 산술로 보장된다:
//
//	−reservedDebit + (reservedDebit − executionDebit) + sellerQuoteNet + fees
//	= −executionDebit + sellerQuoteNet + buyerFee + sellerFee
//	= −(executionQuote + buyerFee) + (executionQuote − sellerFee) + buyerFee + sellerFee
//	= 0
func tradePostings(
	trade *model.Trade,
	buyerUserID uint,
	sellerUserID uint,
	reservedDebit decimal.Decimal,
	executionDebit decimal.Decimal,
	sellerQuoteNet decimal.Decimal,
) []PostingInput {
	buyer := buyerUserID
	seller := sellerUserID
	fees := trade.BuyerFee.Add(trade.SellerFee)

	postings := []PostingInput{
		{AccountType: model.AccountUserLocked, OwnerUserID: &buyer, Asset: model.KRWAssetSymbol, Amount: reservedDebit.Neg()},
		{AccountType: model.AccountUserAvailable, OwnerUserID: &seller, Asset: model.KRWAssetSymbol, Amount: sellerQuoteNet},
		{AccountType: model.AccountUserLocked, OwnerUserID: &seller, Asset: trade.CoinSymbol, Amount: trade.Quantity.Neg()},
		{AccountType: model.AccountUserAvailable, OwnerUserID: &buyer, Asset: trade.CoinSymbol, Amount: trade.Quantity},
	}

	// 지정가가 유리하게 체결되면 잠금액이 체결액보다 크다. 그 차액은 같은
	// 분개에서 available로 되돌린다 — 별도 분개로 나누면 두 기록 사이에
	// 잔액이 어긋난 순간이 생긴다.
	if refund := reservedDebit.Sub(executionDebit); refund.IsPositive() {
		postings = append(postings, PostingInput{
			AccountType: model.AccountUserAvailable, OwnerUserID: &buyer,
			Asset: model.KRWAssetSymbol, Amount: refund,
		})
	}

	// 첫 구현의 거래 수수료는 항상 0보다 크지만, 0원 전기를 만들지 않는다는
	// 규칙은 여기서도 지킨다.
	if fees.IsPositive() {
		postings = append(postings, PostingInput{
			AccountType: model.AccountFeeIncome,
			Asset:       model.KRWAssetSymbol, Amount: fees,
		})
	}
	return postings
}

// applyAvgBuyPrice는 매수 정산 뒤 평균 매수가를 갱신한다.
//
// 평균 매수가는 자산이 아니라 통계라 원장에 넣지 않는다. 단건·배치 정산이 이
// 함수를 공유해야 등가성 불변식이 avg_buy_price에서도 성립한다.
//
// 산술은 기존 creditBuyerCoinWithAcquisitionCost와 같다: 기존 평단가 × 기존
// 수량에 이번 취득원가를 더하고 새 수량으로 나눈다. "기존 수량"은 원장의
// available + locked이며, 이번 체결분은 이미 반영돼 있으므로 빼고 센다.
func applyAvgBuyPrice(
	tx *gorm.DB,
	userID uint,
	asset string,
	quantity decimal.Decimal,
	acquisitionCost decimal.Decimal,
) error {
	if !quantity.IsPositive() {
		return NewValidationErrorf("quantity must be greater than zero")
	}

	var total decimal.Decimal
	if err := tx.Raw(`
		SELECT COALESCE(SUM(b.balance), 0)
		FROM accounts a
		JOIN account_balances b ON b.account_id = a.id
		WHERE a.owner_user_id = ? AND a.asset = ?
		  AND a.account_type IN ('USER_AVAILABLE','USER_LOCKED')`,
		userID, asset).Scan(&total).Error; err != nil {
		return err
	}
	if !total.IsPositive() {
		return fmt.Errorf("avg buy price needs a positive quantity, got %s", total)
	}

	previousQuantity := total.Sub(quantity)
	var previousAvg decimal.Decimal
	if err := tx.Raw(`
		SELECT COALESCE(MAX(avg_buy_price), 0) FROM user_asset_stats
		WHERE user_id = ? AND asset = ?`, userID, asset).Scan(&previousAvg).Error; err != nil {
		return err
	}

	newAvg := previousAvg.Mul(previousQuantity).Add(acquisitionCost).Div(total)
	return tx.Exec(`
		INSERT INTO user_asset_stats (user_id, asset, avg_buy_price, updated_at)
		VALUES (?, ?, ?, now())
		ON CONFLICT (user_id, asset)
		DO UPDATE SET avg_buy_price = EXCLUDED.avg_buy_price, updated_at = EXCLUDED.updated_at`,
		userID, asset, newAvg).Error
}

// clearAvgBuyPriceIfEmpty는 자산을 전부 판 뒤 평단가를 0으로 되돌린다.
// 남은 수량이 0인데 평단가가 남아 있으면 다음 매수의 평균이 어긋난다.
func clearAvgBuyPriceIfEmpty(tx *gorm.DB, userID uint, asset string) error {
	return tx.Exec(`
		UPDATE user_asset_stats s
		SET avg_buy_price = 0, updated_at = now()
		WHERE s.user_id = ? AND s.asset = ?
		  AND NOT EXISTS (
			SELECT 1 FROM accounts a
			JOIN account_balances b ON b.account_id = a.id
			WHERE a.owner_user_id = s.user_id AND a.asset = s.asset
			  AND a.account_type IN ('USER_AVAILABLE','USER_LOCKED')
			GROUP BY a.owner_user_id
			HAVING SUM(b.balance) > 0
		  )`, userID, asset).Error
}
