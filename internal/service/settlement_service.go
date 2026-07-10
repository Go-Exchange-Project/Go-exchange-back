package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type SettlementService struct {
	DB               *gorm.DB
	OrderRepository  *repository.OrderRepository
	WalletRepository *repository.WalletRepository
	LedgerRepository *repository.LedgerRepository
}

type SettlementParticipants struct {
	BuyerUserID  uint
	SellerUserID uint
}

type SettlementResult struct {
	Applied   bool
	Duplicate bool
	TradeID   uint
}

func NewSettlementService(db *gorm.DB, orderRepo *repository.OrderRepository, walletRepo *repository.WalletRepository) *SettlementService {
	return &SettlementService{
		DB:               db,
		OrderRepository:  orderRepo,
		WalletRepository: walletRepo,
		LedgerRepository: repository.NewLedgerRepository(db),
	}
}

func (s *SettlementService) SettleTrade(trade *model.Trade) (SettlementResult, error) {
	if trade == nil {
		return SettlementResult{}, fmt.Errorf("trade is required")
	}
	if err := prepareTradeForSettlement(trade); err != nil {
		return SettlementResult{}, err
	}
	if err := applyTradeFeePolicy(trade); err != nil {
		return SettlementResult{}, err
	}

	var result SettlementResult
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		existingTrade, err := findTradeByIdempotencyKey(tx, trade.IdempotencyKey)
		if err == nil {
			if err := validateIdempotentTradePayload(&existingTrade, trade); err != nil {
				return err
			}
			result = duplicateSettlementResult(existingTrade)
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		createResult := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "idempotency_key"}},
			DoNothing: true,
		}).Create(trade)
		if createResult.Error != nil {
			return createResult.Error
		}
		if createResult.RowsAffected == 0 {
			existingTrade, err := findTradeByIdempotencyKey(tx, trade.IdempotencyKey)
			if err != nil {
				return err
			}
			if err := validateIdempotentTradePayload(&existingTrade, trade); err != nil {
				return err
			}
			result = duplicateSettlementResult(existingTrade)
			return nil
		}

		orderRepo := s.OrderRepository.WithTx(tx)
		walletRepo := s.WalletRepository.WithTx(tx)
		ledgerRepo := s.LedgerRepository.WithTx(tx)

		buyOrder, err := orderRepo.FindByIDForUpdate(trade.BuyOrderID)
		if err != nil {
			return err
		}
		sellOrder, err := orderRepo.FindByIDForUpdate(trade.SellOrderID)
		if err != nil {
			return err
		}

		if buyOrder.Side != model.OrderSideBuy {
			return fmt.Errorf("buy order %d has invalid side", buyOrder.ID)
		}
		if sellOrder.Side != model.OrderSideSell {
			return fmt.Errorf("sell order %d has invalid side", sellOrder.ID)
		}
		if buyOrder.CoinSymbol != trade.CoinSymbol || sellOrder.CoinSymbol != trade.CoinSymbol {
			return fmt.Errorf("trade coin symbol does not match both orders")
		}
		if err := validateOrderStatusForSettlement(buyOrder, "buy"); err != nil {
			return err
		}
		if err := validateOrderStatusForSettlement(sellOrder, "sell"); err != nil {
			return err
		}
		participants, err := settlementParticipants(buyOrder, sellOrder)
		if err != nil {
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

		wallets, err := lockSettlementWallets(walletRepo, participants, trade.CoinSymbol)
		if err != nil {
			return err
		}
		buyerKRW := wallets.BuyerKRW
		buyerCoin := wallets.BuyerCoin
		sellerKRW := wallets.SellerKRW
		sellerCoin := wallets.SellerCoin

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

		if err := orderRepo.UpdateOrderExecution(buyOrder.ID, buyFilled, buyFilledQuote, buyStatus); err != nil {
			return err
		}
		if err := orderRepo.UpdateOrderExecution(sellOrder.ID, sellFilled, sellFilledQuote, sellStatus); err != nil {
			return err
		}

		if err := walletRepo.BatchUpdateBalances([]repository.WalletBatchUpdate{
			{WalletID: buyerKRW.ID, AvailableBalance: buyerKRWUpdate.AvailableBalance, LockedBalance: buyerKRWUpdate.LockedBalance, KRW: buyerKRWUpdate.KRW, Quantity: buyerKRWUpdate.Quantity, AvgBuyPrice: buyerKRWUpdate.AvgBuyPrice},
			{WalletID: buyerCoin.ID, AvailableBalance: buyerCoinUpdate.AvailableBalance, LockedBalance: buyerCoinUpdate.LockedBalance, KRW: buyerCoinUpdate.KRW, Quantity: buyerCoinUpdate.Quantity, AvgBuyPrice: buyerCoinUpdate.AvgBuyPrice},
			{WalletID: sellerCoin.ID, AvailableBalance: sellerCoinUpdate.AvailableBalance, LockedBalance: sellerCoinUpdate.LockedBalance, KRW: sellerCoinUpdate.KRW, Quantity: sellerCoinUpdate.Quantity, AvgBuyPrice: sellerCoinUpdate.AvgBuyPrice},
			{WalletID: sellerKRW.ID, AvailableBalance: sellerKRWUpdate.AvailableBalance, LockedBalance: sellerKRWUpdate.LockedBalance, KRW: sellerKRWUpdate.KRW, Quantity: sellerKRWUpdate.Quantity, AvgBuyPrice: sellerKRWUpdate.AvgBuyPrice},
		}); err != nil {
			return err
		}

		entries := []model.LedgerEntry{
			ledgerEntryFromWalletUpdate(buyerKRW, buyerKRWUpdate, model.LedgerEntryTypeTradeSettlement, model.LedgerReferenceTypeTrade, trade.ID, trade.IdempotencyKey),
			ledgerEntryFromWalletUpdate(buyerCoin, buyerCoinUpdate, model.LedgerEntryTypeTradeSettlement, model.LedgerReferenceTypeTrade, trade.ID, trade.IdempotencyKey),
			ledgerEntryFromWalletUpdate(sellerCoin, sellerCoinUpdate, model.LedgerEntryTypeTradeSettlement, model.LedgerReferenceTypeTrade, trade.ID, trade.IdempotencyKey),
			ledgerEntryFromWalletUpdate(sellerKRW, sellerKRWUpdate, model.LedgerEntryTypeTradeSettlement, model.LedgerReferenceTypeTrade, trade.ID, trade.IdempotencyKey),
		}
		if err := ledgerRepo.CreateMany(entries); err != nil {
			return err
		}

		result = SettlementResult{Applied: true, TradeID: trade.ID}
		return nil
	})
	return result, err
}

type settlementWallets struct {
	BuyerKRW   *model.Wallet
	BuyerCoin  *model.Wallet
	SellerKRW  *model.Wallet
	SellerCoin *model.Wallet
}

// lockSettlementWallets는 데드락을 막기 위해 지갑을 2단계로 잠급니다.
// 1단계: 락 없이 4개 지갑의 ID만 확보(없는 지갑은 생성).
// 2단계: ID 오름차순으로 한 번에 FOR UPDATE.
// 모든 정산이 같은 순서로 잠그므로 지갑 간 AB-BA 데드락이 성립하지 않습니다.
// 잔고 산술은 반드시 2단계에서 잠근 행으로만 해야 합니다(1단계 값은 stale).
func lockSettlementWallets(walletRepo *repository.WalletRepository, participants SettlementParticipants, coinSymbol string) (settlementWallets, error) {
	buyerKRWRef, err := walletRepo.FindKRWWalletByUserID(participants.BuyerUserID)
	if err != nil {
		return settlementWallets{}, err
	}
	buyerCoinRef, err := walletRepo.FindOrCreateByUserIDAndCoinSymbol(participants.BuyerUserID, coinSymbol)
	if err != nil {
		return settlementWallets{}, err
	}
	sellerKRWRef, err := walletRepo.FindOrCreateByUserIDAndCoinSymbol(participants.SellerUserID, model.KRWAssetSymbol)
	if err != nil {
		return settlementWallets{}, err
	}
	sellerCoinRef, err := walletRepo.FindByUserIDAndCoinSymbol(participants.SellerUserID, coinSymbol)
	if err != nil {
		return settlementWallets{}, err
	}

	ids := []uint{buyerKRWRef.ID, buyerCoinRef.ID, sellerKRWRef.ID, sellerCoinRef.ID}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	locked, err := walletRepo.LockByIDs(ids)
	if err != nil {
		return settlementWallets{}, err
	}

	wallets := settlementWallets{}
	for i := range locked {
		wallet := &locked[i]
		switch wallet.ID {
		case buyerKRWRef.ID:
			wallets.BuyerKRW = wallet
		case buyerCoinRef.ID:
			wallets.BuyerCoin = wallet
		case sellerKRWRef.ID:
			wallets.SellerKRW = wallet
		case sellerCoinRef.ID:
			wallets.SellerCoin = wallet
		}
	}
	if wallets.BuyerKRW == nil || wallets.BuyerCoin == nil || wallets.SellerKRW == nil || wallets.SellerCoin == nil {
		return settlementWallets{}, fmt.Errorf("settlement wallet lock did not resolve all four wallets")
	}
	return wallets, nil
}

func applyTradeFill(order *model.Order, tradeQuantity decimal.Decimal, tradeQuoteAmount decimal.Decimal) (decimal.Decimal, decimal.Decimal, model.OrderStatus, error) {
	if order == nil {
		return decimal.Zero, decimal.Zero, "", fmt.Errorf("order is required")
	}
	if !tradeQuantity.GreaterThan(decimal.Zero) {
		return decimal.Zero, decimal.Zero, "", fmt.Errorf("trade quantity must be greater than zero")
	}
	if !tradeQuoteAmount.GreaterThan(decimal.Zero) {
		return decimal.Zero, decimal.Zero, "", fmt.Errorf("trade quote amount must be greater than zero")
	}

	filledAmount := order.FilledAmount.Add(tradeQuantity)
	filledQuoteAmount := order.FilledQuoteAmount.Add(tradeQuoteAmount)
	if order.OrderType == model.OrderTypeMarket && order.Side == model.OrderSideBuy {
		if filledQuoteAmount.GreaterThan(order.QuoteAmount) {
			return decimal.Zero, decimal.Zero, "", fmt.Errorf("order %d filled quote amount %s exceeds quote amount %s", order.ID, filledQuoteAmount.String(), order.QuoteAmount.String())
		}
		if filledQuoteAmount.Equal(order.QuoteAmount) {
			return filledAmount, filledQuoteAmount, model.OrderStatusFilled, nil
		}
		return filledAmount, filledQuoteAmount, model.OrderStatusPartial, nil
	}
	if filledAmount.GreaterThan(order.Amount) {
		return decimal.Zero, decimal.Zero, "", fmt.Errorf("order %d filled amount %s exceeds order amount %s", order.ID, filledAmount.String(), order.Amount.String())
	}
	if filledAmount.Equal(order.Amount) {
		return filledAmount, filledQuoteAmount, model.OrderStatusFilled, nil
	}
	return filledAmount, filledQuoteAmount, model.OrderStatusPartial, nil
}

func validateOrderStatusForSettlement(order *model.Order, role string) error {
	if order == nil {
		return fmt.Errorf("%s order is required", role)
	}

	switch order.Status {
	case model.OrderStatusPending, model.OrderStatusPartial:
		return nil
	case model.OrderStatusCancelled:
		return fmt.Errorf("%s order %d status %s cannot be settled", role, order.ID, order.Status)
	case model.OrderStatusFilled:
		return fmt.Errorf("%s order %d status %s cannot receive additional settlement", role, order.ID, order.Status)
	default:
		return fmt.Errorf("%s order %d has unknown status %s", role, order.ID, order.Status)
	}
}

func settlementParticipants(buyOrder *model.Order, sellOrder *model.Order) (SettlementParticipants, error) {
	if buyOrder == nil || sellOrder == nil {
		return SettlementParticipants{}, fmt.Errorf("both orders are required")
	}
	if buyOrder.Side != model.OrderSideBuy {
		return SettlementParticipants{}, fmt.Errorf("buy order has invalid side")
	}
	if sellOrder.Side != model.OrderSideSell {
		return SettlementParticipants{}, fmt.Errorf("sell order has invalid side")
	}
	return SettlementParticipants{
		BuyerUserID:  buyOrder.UserID,
		SellerUserID: sellOrder.UserID,
	}, validateDistinctSettlementParticipants(buyOrder.UserID, sellOrder.UserID)
}

func validateDistinctSettlementParticipants(buyerUserID uint, sellerUserID uint) error {
	if buyerUserID == 0 || sellerUserID == 0 {
		return fmt.Errorf("settlement participants must have user IDs")
	}
	if buyerUserID == sellerUserID {
		return fmt.Errorf("self-trade settlement is not allowed for user %d", buyerUserID)
	}
	return nil
}

func tradeQuoteAmount(trade *model.Trade) decimal.Decimal {
	return trade.Price.Mul(trade.Quantity)
}

func reservedBuyDebitAmount(buyOrder *model.Order, trade *model.Trade) decimal.Decimal {
	if buyOrder.OrderType == model.OrderTypeMarket {
		return quoteAmountWithTradingFee(tradeQuoteAmount(trade))
	}
	return quoteAmountWithTradingFee(buyOrder.Price.Mul(trade.Quantity))
}

func prepareTradeForSettlement(trade *model.Trade) error {
	trade.CoinSymbol = normalizeTradeCoinSymbol(trade.CoinSymbol)
	if trade.CoinSymbol == "" {
		return fmt.Errorf("trade coin symbol is required")
	}
	if trade.BuyOrderID == 0 || trade.SellOrderID == 0 {
		return fmt.Errorf("trade buy_order_id and sell_order_id are required")
	}
	if !trade.Price.GreaterThan(decimal.Zero) || !trade.Quantity.GreaterThan(decimal.Zero) {
		return fmt.Errorf("trade price and quantity must be greater than zero")
	}
	if trade.EngineSequence < 0 {
		return fmt.Errorf("trade engine_sequence must be greater than or equal to zero")
	}
	trade.EngineEventID = strings.TrimSpace(trade.EngineEventID)

	trade.IdempotencyKey = strings.TrimSpace(trade.IdempotencyKey)
	if trade.IdempotencyKey == "" {
		trade.IdempotencyKey = tradeIdempotencyKey(trade)
	}
	return nil
}

func tradeIdempotencyKey(trade *model.Trade) string {
	if engineEventID := strings.TrimSpace(trade.EngineEventID); engineEventID != "" {
		return fmt.Sprintf("engine:%s", engineEventID)
	}
	return deterministicTradeIdempotencyKey(trade)
}

func deterministicTradeIdempotencyKey(trade *model.Trade) string {
	payload := fmt.Sprintf(
		"%s|%d|%d|%s|%s",
		normalizeTradeCoinSymbol(trade.CoinSymbol),
		trade.BuyOrderID,
		trade.SellOrderID,
		trade.Price.String(),
		trade.Quantity.String(),
	)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func normalizeTradeCoinSymbol(coinSymbol string) string {
	return strings.ToUpper(strings.TrimSpace(coinSymbol))
}

func findTradeByIdempotencyKey(tx *gorm.DB, idempotencyKey string) (model.Trade, error) {
	var trade model.Trade
	err := tx.Where("idempotency_key = ?", idempotencyKey).First(&trade).Error
	return trade, err
}

func validateIdempotentTradePayload(existing *model.Trade, incoming *model.Trade) error {
	if existing == nil || incoming == nil {
		return fmt.Errorf("both trades are required")
	}
	if normalizeTradeCoinSymbol(existing.CoinSymbol) != normalizeTradeCoinSymbol(incoming.CoinSymbol) ||
		existing.EngineSequence != incoming.EngineSequence ||
		strings.TrimSpace(existing.EngineEventID) != strings.TrimSpace(incoming.EngineEventID) ||
		existing.BuyOrderID != incoming.BuyOrderID ||
		existing.SellOrderID != incoming.SellOrderID ||
		!existing.Price.Equal(incoming.Price) ||
		!existing.Quantity.Equal(incoming.Quantity) ||
		!existing.FeeRate.Equal(incoming.FeeRate) ||
		!existing.BuyerFee.Equal(incoming.BuyerFee) ||
		strings.TrimSpace(existing.BuyerFeeAsset) != strings.TrimSpace(incoming.BuyerFeeAsset) ||
		!existing.SellerFee.Equal(incoming.SellerFee) ||
		strings.TrimSpace(existing.SellerFeeAsset) != strings.TrimSpace(incoming.SellerFeeAsset) {
		return fmt.Errorf("idempotency key conflict for %q: existing trade payload differs", incoming.IdempotencyKey)
	}
	return nil
}

func duplicateSettlementResult(trade model.Trade) SettlementResult {
	return SettlementResult{
		Applied:   false,
		Duplicate: true,
		TradeID:   trade.ID,
	}
}
