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
// 검증·산술은 SettleTrade와 정확히 같은 헬퍼(applyTradeFill, tradePostings,
// s.Ledger.Record 등)를 재사용한다 — 배치 정산의 최종 상태는 같은 순서로
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

		// 4. 정적 검증(side·심볼 일치, settlementParticipants).
		participantsByTrade := make(map[int]SettlementParticipants, len(newIndexes))
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
		}

		// 5. 이 배치가 만질 계정을 전부 확보하고 account_id 오름차순으로 한 번에 잠근다.
		//
		// Record는 자기 호출 안에서만 계정을 정렬한다. 체결마다 Record를 부르면 각
		// 호출은 정렬돼 있어도 트랜잭션 전체의 획득 순서는 정렬되지 않는다 — 앞 체결이
		// 큰 ID를 쥔 채 다음 체결이 작은 ID를 요구할 수 있고, 오름차순으로 잠그는
		// HoldBatch와 만나면 순환 대기가 되어 교착상태가 된다.
		//
		// 잠글 집합은 전기에서 파생시킨다. refund·수수료 줄은 금액에 따라 있고 없고가
		// 갈리므로, 손으로 나열하면 Record가 실제로 잠그는 집합과 어긋난다.
		plans := make(map[int]tradeSettlementPlan, len(newIndexes))
		accountSpecs := make([]repository.AccountSpec, 0, len(newIndexes)*6)
		for _, i := range newIndexes {
			trade := items[i].Trade
			plan, err := planTradeSettlement(trade, orderByID[trade.BuyOrderID], participantsByTrade[i])
			if err != nil {
				return err
			}
			plans[i] = plan
			accountSpecs = append(accountSpecs, postingAccountSpecs(plan.Postings)...)
		}
		accountRepo := s.Ledger.Accounts.WithTx(tx)
		batchAccounts, err := accountRepo.EnsureAccounts(accountSpecs)
		if err != nil {
			return err
		}
		batchAccountIDs := make([]uint, 0, len(batchAccounts))
		for _, account := range batchAccounts {
			batchAccountIDs = append(batchAccountIDs, account.ID)
		}
		sort.Slice(batchAccountIDs, func(i, j int) bool { return batchAccountIDs[i] < batchAccountIDs[j] })
		if _, err := accountRepo.LockBalances(batchAccountIDs); err != nil {
			return err
		}

		// 6. 순차 정산: trade를 큐 순서대로 처리한다. LedgerService.Record가 매번
		// 계정을 잠그고 잔액 캐시를 갱신하므로, 같은 트랜잭션 안의 다음 trade는 앞선
		// trade가 이미 반영한 잔액을 그대로 본다 — 지갑 시절의 명시적 fold가
		// 여기서는 필요 없다. 위에서 이미 잠근 계정이라 Record의 재잠금은 대기가 없다.
		touchedOrderIDs := make(map[uint]bool, len(newIndexes)*2)

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
			plan := plans[i]

			if _, _, err := s.Ledger.Record(tx, JournalInput{
				EventType:      model.JournalEventTrade,
				IdempotencyKey: fmt.Sprintf("trade:%s", trade.IdempotencyKey),
				ReferenceType:  model.JournalReferenceTrade,
				ReferenceID:    trade.ID,
				Postings:       plan.Postings,
			}); err != nil {
				return err
			}
			if err := applyAvgBuyPrice(tx, participants.BuyerUserID, trade.CoinSymbol, trade.Quantity, plan.ExecutionDebit); err != nil {
				return err
			}
			if err := clearAvgBuyPriceIfEmpty(tx, participants.SellerUserID, trade.CoinSymbol); err != nil {
				return err
			}

			buyOrder.FilledAmount = buyFilled
			buyOrder.FilledQuoteAmount = buyFilledQuote
			buyOrder.Status = buyStatus
			sellOrder.FilledAmount = sellFilled
			sellOrder.FilledQuoteAmount = sellFilledQuote
			sellOrder.Status = sellStatus

			touchedOrderIDs[buyOrder.ID] = true
			touchedOrderIDs[sellOrder.ID] = true

			results[i] = SettlementResult{Applied: true, TradeID: trade.ID}
		}

		// 7. 배치 쓰기: 주문 체결 상태만 남았다 — 잔고·원장은 각 Record 호출이 이미 반영했다.
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

		return markSettledOutboxBatch(tx, collectOutboxIDs(items))
	})
	if err != nil {
		// 2단계 배치 INSERT는 RETURNING id로 items[i].Trade.ID를 호출자 소유 포인터에
		// 직접 채운다. 트랜잭션이 이후 단계에서 실패해 롤백되면 그 ID는 커밋된 적 없는
		// phantom 값인데 포인터에는 그대로 남는다 — 폴백 경로가 같은 포인터로
		// SettleTrade를 재호출하므로 여기서 원상복구해야 한다.
		for _, item := range items {
			item.Trade.ID = 0
		}
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
