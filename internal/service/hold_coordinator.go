package service

import (
	"log"
	"sort"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type holdRequest struct {
	order    *model.Order
	resultCh chan holdResult
}

type holdResult struct {
	Order *model.Order // 성공 시 ID 채워짐
	Err   error        // nil=성공, ConflictError=잔고부족, 그 외=시스템
}

type HoldCoordinator struct {
	DB         *gorm.DB
	OrderRepo  *repository.OrderRepository
	WalletRepo *repository.WalletRepository
	LedgerRepo *repository.LedgerRepository

	BatchSize     int           // 기본 64
	FlushInterval time.Duration // 기본 5ms
	Logger        *log.Logger

	input chan holdRequest
	done  chan struct{}
}

// holdWalletKey: 매수=유저 KRW, 매도=유저 코인.
func holdWalletKey(order *model.Order) repository.WalletKey {
	if order.Side == model.OrderSideBuy {
		return repository.WalletKey{UserID: order.UserID, CoinSymbol: model.KRWAssetSymbol}
	}
	return repository.WalletKey{UserID: order.UserID, CoinSymbol: order.CoinSymbol}
}

// holdAmountFor: holdOrderAssets와 동일 산술. 매수 지정가=quoteAmountWithTradingFee(Price*Amount),
// 매수 시장가=QuoteAmount, 매도=Amount.
func holdAmountFor(order *model.Order) decimal.Decimal {
	if order.Side == model.OrderSideBuy {
		if order.OrderType == model.OrderTypeMarket {
			return order.QuoteAmount
		}
		return quoteAmountWithTradingFee(order.Price.Mul(order.Amount))
	}
	return order.Amount
}

// HoldBatch는 배치를 한 트랜잭션에 persist+hold한다. 통과분만 INSERT/홀드하고 실패분은
// holdResult.Err로 격리한다. txn-레벨 실패면 (nil, err) 반환 + 모든 orders.ID를 0으로
// 리셋(phantom ID 방지). 성공 시 결과는 orders 인덱스와 1:1.
func (c *HoldCoordinator) HoldBatch(orders []*model.Order) ([]holdResult, error) {
	results := make([]holdResult, len(orders))

	err := c.DB.Transaction(func(tx *gorm.DB) error {
		orderRepo := c.OrderRepo.WithTx(tx)
		walletRepo := c.WalletRepo.WithTx(tx)
		ledgerRepo := c.LedgerRepo.WithTx(tx)

		// 1. 지갑 키 수집(dedup) → 2. FindByKeys로 ID 확보 → ID 오름차순 LockByIDs.
		keySet := map[repository.WalletKey]bool{}
		keys := make([]repository.WalletKey, 0, len(orders))
		for _, o := range orders {
			k := holdWalletKey(o)
			if !keySet[k] {
				keySet[k] = true
				keys = append(keys, k)
			}
		}
		found, err := walletRepo.FindByKeys(keys)
		if err != nil {
			return err
		}
		ids := make([]uint, 0, len(found))
		for i := range found {
			ids = append(ids, found[i].ID)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		locked, err := walletRepo.LockByIDs(ids)
		if err != nil {
			return err
		}
		walletByKey := map[repository.WalletKey]*model.Wallet{}
		for i := range locked {
			w := &locked[i]
			walletByKey[repository.WalletKey{UserID: w.UserID, CoinSymbol: w.CoinSymbol}] = w
		}

		// 3. 순차 fold-검증. 통과분만 수집.
		type passingHold struct {
			idx   int
			order *model.Order
			entry model.LedgerEntry // ReferenceID는 INSERT 후 채움
		}
		var passing []passingHold
		changedWallets := map[uint]*model.Wallet{}

		for i, order := range orders {
			wallet := walletByKey[holdWalletKey(order)]
			if wallet == nil { // 지갑 없음 = 잔고 부족과 동일
				results[i] = holdResult{Err: NewConflictErrorf("insufficient available balance")}
				continue
			}
			amount := holdAmountFor(order)
			var update WalletBalanceUpdate
			var herr error
			if order.Side == model.OrderSideBuy {
				update, herr = applyBuyOrderHold(wallet, amount)
			} else {
				update, herr = applySellOrderHold(wallet, amount)
			}
			if herr != nil { // ConflictError(잔고 부족) 격리
				results[i] = holdResult{Err: herr}
				continue
			}
			// 원장 엔트리는 fold 전에 계산(delta = update - 현재 잔고).
			entry := ledgerEntryFromWalletUpdate(wallet, update, model.LedgerEntryTypeOrderHold, model.LedgerReferenceTypeOrder, 0, "")
			foldWalletBalanceUpdate(wallet, update) // 다음 주문이 차감된 잔고를 본다
			changedWallets[wallet.ID] = wallet
			passing = append(passing, passingHold{idx: i, order: order, entry: entry})
		}

		if len(passing) == 0 {
			return nil // 전원 실패 — 쓸 것 없음, results엔 개별 에러
		}

		// 4. 통과 주문 배치 INSERT (ID 채워짐).
		passingOrders := make([]*model.Order, len(passing))
		for j := range passing {
			passingOrders[j] = passing[j].order
		}
		if err := orderRepo.CreateOrders(passingOrders); err != nil {
			return err
		}

		// 5. 변경 지갑 일괄 UPDATE.
		updates := make([]repository.WalletBatchUpdate, 0, len(changedWallets))
		for _, w := range changedWallets {
			updates = append(updates, repository.WalletBatchUpdate{
				WalletID: w.ID, AvailableBalance: w.AvailableBalance, LockedBalance: w.LockedBalance,
				KRW: w.KRW, Quantity: w.Quantity, AvgBuyPrice: w.AvgBuyPrice,
			})
		}
		if err := walletRepo.BatchUpdateBalances(updates); err != nil {
			return err
		}

		// 6. OrderHold 원장 일괄 INSERT(새 order.ID 참조).
		entries := make([]model.LedgerEntry, len(passing))
		for j := range passing {
			e := passing[j].entry
			e.ReferenceID = passing[j].order.ID
			entries[j] = e
		}
		if err := ledgerRepo.CreateMany(entries); err != nil {
			return err
		}

		// 통과분 결과 채움.
		for j := range passing {
			results[passing[j].idx] = holdResult{Order: passing[j].order}
		}
		return nil
	})

	if err != nil {
		for _, o := range orders { // phantom ID 방지(B-4 8b3007f 교훈)
			o.ID = 0
		}
		return nil, err
	}
	return results, nil
}
