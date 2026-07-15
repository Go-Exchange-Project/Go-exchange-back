package service

import (
	"fmt"
	"sort"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TradeBatchItem은 SettleTradeBatch에 넘길 trade 1건과, 정산과 같은 트랜잭션에서
// PROCESSED로 마킹할 outbox 이벤트 ID(0이면 마킹 생략, SettleTrade의 outboxEventID와
// 같은 의미)를 묶는다.
type TradeBatchItem struct {
	Trade         *model.Trade
	OutboxEventID uint64
}

// SettleTradeBatch는 여러 체결을 한 트랜잭션으로 정산한다. 반환하는 []SettlementResult는
// items와 같은 인덱스를 갖는다(Applied면 브로드캐스트 대상). 에러를 반환하면 트랜잭션
// 전체가 롤백되어 아무것도 커밋되지 않는다 — 이 경우 결과는 nil이며, 호출자는 같은
// 배치를 SettleTrade로 건별 폴백 처리해야 한다.
//
// 검증·산술은 SettleTrade와 정확히 같은 헬퍼(applyTradeFill, settleBuyerKRW,
// creditBuyerCoinWithAcquisitionCost, settleSellerCoin, creditAvailable,
// ledgerEntryFromWalletUpdate 등)를 재사용한다 — 배치 정산의 최종 상태는 같은 순서로
// SettleTrade를 N회 실행한 결과와 정확히 같아야 한다(등가성 불변식).
func (s *SettlementService) SettleTradeBatch(items []TradeBatchItem) ([]SettlementResult, error) {
	if len(items) == 0 {
		return nil, nil
	}
	for _, item := range items {
		if item.Trade == nil {
			return nil, fmt.Errorf("trade is required")
		}
		if err := prepareTradeForSettlement(item.Trade); err != nil {
			return nil, err
		}
		if err := applyTradeFeePolicy(item.Trade); err != nil {
			return nil, err
		}
	}

	results := make([]SettlementResult, len(items))
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		orderRepo := s.OrderRepository.WithTx(tx)
		walletRepo := s.WalletRepository.WithTx(tx)
		ledgerRepo := s.LedgerRepository.WithTx(tx)

		// 1. 중복 분리: idempotency_key IN (전체 키) 1왕복.
		keys := make([]string, 0, len(items))
		seenKeys := make(map[string]bool, len(items))
		for _, item := range items {
			if !seenKeys[item.Trade.IdempotencyKey] {
				seenKeys[item.Trade.IdempotencyKey] = true
				keys = append(keys, item.Trade.IdempotencyKey)
			}
		}
		existingByKey, err := findTradesByIdempotencyKeys(tx, keys)
		if err != nil {
			return err
		}

		newIndexes := make([]int, 0, len(items))
		for i, item := range items {
			existing, ok := existingByKey[item.Trade.IdempotencyKey]
			if !ok {
				newIndexes = append(newIndexes, i)
				continue
			}
			if err := validateIdempotentTradePayload(&existing, item.Trade); err != nil {
				return err
			}
			results[i] = duplicateSettlementResult(existing)
		}

		// 2. 신규 trade 배치 INSERT (ON CONFLICT DO NOTHING). 개수 불일치는 1과 이
		// INSERT 사이에 경쟁자가 끼어들었다는 뜻이므로 배치를 포기한다(단건 폴백이
		// 멱등 경로로 건별 정리한다).
		if len(newIndexes) > 0 {
			newTrades := make([]*model.Trade, len(newIndexes))
			for j, i := range newIndexes {
				newTrades[j] = items[i].Trade
			}
			createResult := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "idempotency_key"}},
				DoNothing: true,
			}).Create(&newTrades)
			if createResult.Error != nil {
				return createResult.Error
			}
			if int(createResult.RowsAffected) != len(newIndexes) {
				return fmt.Errorf("trade batch insert expected %d rows, inserted %d", len(newIndexes), createResult.RowsAffected)
			}
		}

		// 3. 신규 trade들의 고유 주문 ID를 오름차순으로 일괄 락.
		orderIDSet := make(map[uint]bool, len(newIndexes)*2)
		for _, i := range newIndexes {
			trade := items[i].Trade
			orderIDSet[trade.BuyOrderID] = true
			orderIDSet[trade.SellOrderID] = true
		}
		lockedOrders, err := orderRepo.LockByIDs(sortedUintKeys(orderIDSet))
		if err != nil {
			return err
		}
		orderByID := make(map[uint]*model.Order, len(lockedOrders))
		for i := range lockedOrders {
			orderByID[lockedOrders[i].ID] = &lockedOrders[i]
		}

		// 4. 정적 검증(side·심볼 일치, settlementParticipants) + 지갑 키 수집.
		participantsByTrade := make(map[int]SettlementParticipants, len(newIndexes))
		walletKeySet := make(map[repository.WalletKey]bool, len(newIndexes)*4)
		creatableKeySet := make(map[repository.WalletKey]bool, len(newIndexes)*2)
		for _, i := range newIndexes {
			trade := items[i].Trade
			buyOrder := orderByID[trade.BuyOrderID]
			sellOrder := orderByID[trade.SellOrderID]
			if buyOrder.Side != model.OrderSideBuy {
				return fmt.Errorf("buy order %d has invalid side", buyOrder.ID)
			}
			if sellOrder.Side != model.OrderSideSell {
				return fmt.Errorf("sell order %d has invalid side", sellOrder.ID)
			}
			if buyOrder.CoinSymbol != trade.CoinSymbol || sellOrder.CoinSymbol != trade.CoinSymbol {
				return fmt.Errorf("trade coin symbol does not match both orders")
			}
			participants, err := settlementParticipants(buyOrder, sellOrder)
			if err != nil {
				return err
			}
			participantsByTrade[i] = participants

			buyerKRWKey := repository.WalletKey{UserID: participants.BuyerUserID, CoinSymbol: model.KRWAssetSymbol}
			buyerCoinKey := repository.WalletKey{UserID: participants.BuyerUserID, CoinSymbol: trade.CoinSymbol}
			sellerCoinKey := repository.WalletKey{UserID: participants.SellerUserID, CoinSymbol: trade.CoinSymbol}
			sellerKRWKey := repository.WalletKey{UserID: participants.SellerUserID, CoinSymbol: model.KRWAssetSymbol}

			walletKeySet[buyerKRWKey] = true
			walletKeySet[buyerCoinKey] = true
			walletKeySet[sellerCoinKey] = true
			walletKeySet[sellerKRWKey] = true
			creatableKeySet[buyerCoinKey] = true
			creatableKeySet[sellerKRWKey] = true
		}

		allKeys := make([]repository.WalletKey, 0, len(walletKeySet))
		for k := range walletKeySet {
			allKeys = append(allKeys, k)
		}

		// 5. 지갑 확보: FindByKeys, 없는 키 중 생성 허용 역할만 CreateZeroBalanceWallets.
		walletByKey, err := findWalletsByKeys(walletRepo, allKeys)
		if err != nil {
			return err
		}
		missingCreatable := make([]repository.WalletKey, 0)
		for k := range walletKeySet {
			if _, ok := walletByKey[k]; !ok && creatableKeySet[k] {
				missingCreatable = append(missingCreatable, k)
			}
		}
		if len(missingCreatable) > 0 {
			if err := walletRepo.CreateZeroBalanceWallets(missingCreatable); err != nil {
				return err
			}
			walletByKey, err = findWalletsByKeys(walletRepo, allKeys)
			if err != nil {
				return err
			}
		}
		for k := range walletKeySet {
			if _, ok := walletByKey[k]; !ok {
				return fmt.Errorf("wallet not found for user %d coin %s", k.UserID, k.CoinSymbol)
			}
		}

		// 6. 지갑 일괄 락: 고유 지갑 ID 오름차순.
		walletIDSet := make(map[uint]bool, len(walletByKey))
		for _, w := range walletByKey {
			walletIDSet[w.ID] = true
		}
		lockedWallets, err := walletRepo.LockByIDs(sortedUintKeys(walletIDSet))
		if err != nil {
			return err
		}
		walletPtrByKey := make(map[repository.WalletKey]*model.Wallet, len(lockedWallets))
		walletPtrByID := make(map[uint]*model.Wallet, len(lockedWallets))
		for i := range lockedWallets {
			w := &lockedWallets[i]
			walletPtrByKey[repository.WalletKey{UserID: w.UserID, CoinSymbol: w.CoinSymbol}] = w
			walletPtrByID[w.ID] = w
		}

		// 7. 순차 산술: trade를 큐 순서대로, 전부 메모리에서 처리한다. 선행 trade의
		// fold 결과가 후행 trade에 그대로 보인다(단건 순차 실행과 동일한 관점).
		touchedOrderIDs := make(map[uint]bool, len(newIndexes)*2)
		touchedWalletIDs := make(map[uint]bool, len(newIndexes)*4)
		var ledgerEntries []model.LedgerEntry

		for _, i := range newIndexes {
			trade := items[i].Trade
			buyOrder := orderByID[trade.BuyOrderID]
			sellOrder := orderByID[trade.SellOrderID]

			if err := validateOrderStatusForSettlement(buyOrder, "buy"); err != nil {
				return err
			}
			if err := validateOrderStatusForSettlement(sellOrder, "sell"); err != nil {
				return err
			}

			executionQuote := tradeQuoteAmount(trade)
			buyFilled, buyFilledQuote, buyStatus, err := applyTradeFill(buyOrder, trade.Quantity, executionQuote)
			if err != nil {
				return fmt.Errorf("buy order fill: %w", err)
			}
			sellFilled, sellFilledQuote, sellStatus, err := applyTradeFill(sellOrder, trade.Quantity, executionQuote)
			if err != nil {
				return fmt.Errorf("sell order fill: %w", err)
			}

			participants := participantsByTrade[i]
			buyerKRW := walletPtrByKey[repository.WalletKey{UserID: participants.BuyerUserID, CoinSymbol: model.KRWAssetSymbol}]
			buyerCoin := walletPtrByKey[repository.WalletKey{UserID: participants.BuyerUserID, CoinSymbol: trade.CoinSymbol}]
			sellerCoin := walletPtrByKey[repository.WalletKey{UserID: participants.SellerUserID, CoinSymbol: trade.CoinSymbol}]
			sellerKRW := walletPtrByKey[repository.WalletKey{UserID: participants.SellerUserID, CoinSymbol: model.KRWAssetSymbol}]

			reservedDebit := reservedBuyDebitAmount(buyOrder, trade)
			executionDebit := executionQuote.Add(trade.BuyerFee)
			sellerQuoteNet, err := amountAfterFee(executionQuote, trade.SellerFee, "seller")
			if err != nil {
				return err
			}
			buyerKRWUpdate, err := settleBuyerKRW(buyerKRW, reservedDebit, executionDebit)
			if err != nil {
				return err
			}
			buyerCoinUpdate, err := creditBuyerCoinWithAcquisitionCost(buyerCoin, trade.Quantity, executionDebit)
			if err != nil {
				return err
			}
			sellerCoinUpdate, err := settleSellerCoin(sellerCoin, trade.Quantity)
			if err != nil {
				return err
			}
			sellerKRWUpdate, err := creditAvailable(sellerKRW, sellerQuoteNet)
			if err != nil {
				return err
			}

			// 원장 엔트리는 반드시 fold 전에 생성한다 — delta = update - 현재 잔고이므로
			// 순서가 정합성 그 자체다.
			ledgerEntries = append(ledgerEntries,
				ledgerEntryFromWalletUpdate(buyerKRW, buyerKRWUpdate, model.LedgerEntryTypeTradeSettlement, model.LedgerReferenceTypeTrade, trade.ID, trade.IdempotencyKey),
				ledgerEntryFromWalletUpdate(buyerCoin, buyerCoinUpdate, model.LedgerEntryTypeTradeSettlement, model.LedgerReferenceTypeTrade, trade.ID, trade.IdempotencyKey),
				ledgerEntryFromWalletUpdate(sellerCoin, sellerCoinUpdate, model.LedgerEntryTypeTradeSettlement, model.LedgerReferenceTypeTrade, trade.ID, trade.IdempotencyKey),
				ledgerEntryFromWalletUpdate(sellerKRW, sellerKRWUpdate, model.LedgerEntryTypeTradeSettlement, model.LedgerReferenceTypeTrade, trade.ID, trade.IdempotencyKey),
			)

			foldWalletBalanceUpdate(buyerKRW, buyerKRWUpdate)
			foldWalletBalanceUpdate(buyerCoin, buyerCoinUpdate)
			foldWalletBalanceUpdate(sellerCoin, sellerCoinUpdate)
			foldWalletBalanceUpdate(sellerKRW, sellerKRWUpdate)

			buyOrder.FilledAmount = buyFilled
			buyOrder.FilledQuoteAmount = buyFilledQuote
			buyOrder.Status = buyStatus
			sellOrder.FilledAmount = sellFilled
			sellOrder.FilledQuoteAmount = sellFilledQuote
			sellOrder.Status = sellStatus

			touchedOrderIDs[buyOrder.ID] = true
			touchedOrderIDs[sellOrder.ID] = true
			touchedWalletIDs[buyerKRW.ID] = true
			touchedWalletIDs[buyerCoin.ID] = true
			touchedWalletIDs[sellerCoin.ID] = true
			touchedWalletIDs[sellerKRW.ID] = true

			results[i] = SettlementResult{Applied: true, TradeID: trade.ID}
		}

		// 8. 배치 쓰기.
		orderUpdates := make([]repository.OrderExecutionBatchUpdate, 0, len(touchedOrderIDs))
		for id := range touchedOrderIDs {
			o := orderByID[id]
			orderUpdates = append(orderUpdates, repository.OrderExecutionBatchUpdate{
				OrderID:           o.ID,
				FilledAmount:      o.FilledAmount,
				FilledQuoteAmount: o.FilledQuoteAmount,
				Status:            o.Status,
			})
		}
		if err := orderRepo.BatchUpdateExecutions(orderUpdates); err != nil {
			return err
		}

		walletUpdates := make([]repository.WalletBatchUpdate, 0, len(touchedWalletIDs))
		for id := range touchedWalletIDs {
			w := walletPtrByID[id]
			walletUpdates = append(walletUpdates, repository.WalletBatchUpdate{
				WalletID:         w.ID,
				AvailableBalance: w.AvailableBalance,
				LockedBalance:    w.LockedBalance,
				KRW:              w.KRW,
				Quantity:         w.Quantity,
				AvgBuyPrice:      w.AvgBuyPrice,
			})
		}
		if err := walletRepo.BatchUpdateBalances(walletUpdates); err != nil {
			return err
		}

		if err := ledgerRepo.CreateMany(ledgerEntries); err != nil {
			return err
		}

		return markSettledOutboxBatch(tx, collectOutboxIDs(items))
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// markSettledOutboxBatch는 markSettledOutbox의 배치 버전이다. 단건과 달리 개수 불일치를
// 에러로 취급한다 — 배치는 전체 롤백 후 폴백이 건별로 정리하므로 엄격한 쪽이 안전하다.
func markSettledOutboxBatch(tx *gorm.DB, ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}
	result := tx.Model(&model.TradeOutboxEvent{}).
		Where("id IN ?", ids).
		Updates(map[string]interface{}{
			"status":       model.TradeOutboxStatusProcessed,
			"processed_at": time.Now().UTC(),
		})
	if result.Error != nil {
		return result.Error
	}
	if int(result.RowsAffected) != len(ids) {
		return fmt.Errorf("outbox batch mark expected %d rows, affected %d", len(ids), result.RowsAffected)
	}
	return nil
}

// foldWalletBalanceUpdate는 계산된 업데이트를 메모리 지갑에 반영한다 — 단건 경로가
// WalletBatchUpdate로 DB에 쓰는 5개 컬럼과 정확히 같은 필드다. 배치 내 다음 trade는
// 커밋됐을 행과 동일한 상태를 본다.
func foldWalletBalanceUpdate(wallet *model.Wallet, update WalletBalanceUpdate) {
	wallet.AvailableBalance = update.AvailableBalance
	wallet.LockedBalance = update.LockedBalance
	wallet.KRW = update.KRW
	wallet.Quantity = update.Quantity
	wallet.AvgBuyPrice = update.AvgBuyPrice
}

func findTradesByIdempotencyKeys(tx *gorm.DB, keys []string) (map[string]model.Trade, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	var trades []model.Trade
	if err := tx.Where("idempotency_key IN ?", keys).Find(&trades).Error; err != nil {
		return nil, err
	}
	result := make(map[string]model.Trade, len(trades))
	for _, trade := range trades {
		result[trade.IdempotencyKey] = trade
	}
	return result, nil
}

func findWalletsByKeys(walletRepo *repository.WalletRepository, keys []repository.WalletKey) (map[repository.WalletKey]model.Wallet, error) {
	wallets, err := walletRepo.FindByKeys(keys)
	if err != nil {
		return nil, err
	}
	result := make(map[repository.WalletKey]model.Wallet, len(wallets))
	for _, w := range wallets {
		result[repository.WalletKey{UserID: w.UserID, CoinSymbol: w.CoinSymbol}] = w
	}
	return result, nil
}

func collectOutboxIDs(items []TradeBatchItem) []uint64 {
	ids := make([]uint64, 0, len(items))
	seen := make(map[uint64]bool, len(items))
	for _, item := range items {
		if item.OutboxEventID == 0 || seen[item.OutboxEventID] {
			continue
		}
		seen[item.OutboxEventID] = true
		ids = append(ids, item.OutboxEventID)
	}
	return ids
}

func sortedUintKeys(set map[uint]bool) []uint {
	ids := make([]uint, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
