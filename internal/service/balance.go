package service

import (
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
)

type WalletBalanceUpdate struct {
	AvailableBalance decimal.Decimal
	LockedBalance    decimal.Decimal
	KRW              decimal.Decimal
	Quantity         decimal.Decimal
	AvgBuyPrice      decimal.Decimal
}

func applyBuyOrderHold(wallet *model.Wallet, holdAmount decimal.Decimal) (WalletBalanceUpdate, error) {
	if wallet == nil {
		return WalletBalanceUpdate{}, NewValidationErrorf("wallet is required")
	}
	if wallet.CoinSymbol != model.KRWAssetSymbol {
		return WalletBalanceUpdate{}, NewValidationErrorf("buy hold requires KRW wallet")
	}
	if !holdAmount.GreaterThan(decimal.Zero) {
		return WalletBalanceUpdate{}, NewValidationErrorf("hold amount must be greater than zero")
	}

	available := walletAvailableBalance(wallet)
	locked := wallet.LockedBalance
	if available.LessThan(holdAmount) {
		return WalletBalanceUpdate{}, NewConflictErrorf("insufficient available KRW balance")
	}

	return walletBalanceUpdate(wallet, available.Sub(holdAmount), locked.Add(holdAmount)), nil
}

func applySellOrderHold(wallet *model.Wallet, holdQuantity decimal.Decimal) (WalletBalanceUpdate, error) {
	if wallet == nil {
		return WalletBalanceUpdate{}, NewValidationErrorf("wallet is required")
	}
	if wallet.CoinSymbol == model.KRWAssetSymbol {
		return WalletBalanceUpdate{}, NewValidationErrorf("sell hold requires coin wallet")
	}
	if !holdQuantity.GreaterThan(decimal.Zero) {
		return WalletBalanceUpdate{}, NewValidationErrorf("hold quantity must be greater than zero")
	}

	available := walletAvailableBalance(wallet)
	locked := wallet.LockedBalance
	if available.LessThan(holdQuantity) {
		return WalletBalanceUpdate{}, NewConflictErrorf("insufficient available coin balance")
	}

	return walletBalanceUpdate(wallet, available.Sub(holdQuantity), locked.Add(holdQuantity)), nil
}

func releaseBuyOrderHold(wallet *model.Wallet, releaseAmount decimal.Decimal) (WalletBalanceUpdate, error) {
	if wallet == nil {
		return WalletBalanceUpdate{}, NewValidationErrorf("wallet is required")
	}
	if wallet.CoinSymbol != model.KRWAssetSymbol {
		return WalletBalanceUpdate{}, NewValidationErrorf("buy release requires KRW wallet")
	}
	if !releaseAmount.GreaterThan(decimal.Zero) {
		return WalletBalanceUpdate{}, NewValidationErrorf("release amount must be greater than zero")
	}
	if wallet.LockedBalance.LessThan(releaseAmount) {
		return WalletBalanceUpdate{}, NewConflictErrorf("insufficient locked KRW balance")
	}

	available := walletAvailableBalance(wallet).Add(releaseAmount)
	locked := wallet.LockedBalance.Sub(releaseAmount)
	return walletBalanceUpdate(wallet, available, locked), nil
}

func releaseSellOrderHold(wallet *model.Wallet, releaseQuantity decimal.Decimal) (WalletBalanceUpdate, error) {
	if wallet == nil {
		return WalletBalanceUpdate{}, NewValidationErrorf("wallet is required")
	}
	if wallet.CoinSymbol == model.KRWAssetSymbol {
		return WalletBalanceUpdate{}, NewValidationErrorf("sell release requires coin wallet")
	}
	if !releaseQuantity.GreaterThan(decimal.Zero) {
		return WalletBalanceUpdate{}, NewValidationErrorf("release quantity must be greater than zero")
	}
	if wallet.LockedBalance.LessThan(releaseQuantity) {
		return WalletBalanceUpdate{}, NewConflictErrorf("insufficient locked coin balance")
	}

	available := walletAvailableBalance(wallet).Add(releaseQuantity)
	locked := wallet.LockedBalance.Sub(releaseQuantity)
	return walletBalanceUpdate(wallet, available, locked), nil
}

func settleBuyerKRW(wallet *model.Wallet, reservedAmount decimal.Decimal, executionAmount decimal.Decimal) (WalletBalanceUpdate, error) {
	if wallet == nil {
		return WalletBalanceUpdate{}, NewValidationErrorf("wallet is required")
	}
	if wallet.CoinSymbol != model.KRWAssetSymbol {
		return WalletBalanceUpdate{}, NewValidationErrorf("buyer quote settlement requires KRW wallet")
	}
	if !reservedAmount.GreaterThan(decimal.Zero) || !executionAmount.GreaterThan(decimal.Zero) {
		return WalletBalanceUpdate{}, NewValidationErrorf("settlement amounts must be greater than zero")
	}
	if reservedAmount.LessThan(executionAmount) {
		return WalletBalanceUpdate{}, NewValidationErrorf("reserved amount is less than execution amount")
	}
	if wallet.LockedBalance.LessThan(reservedAmount) {
		return WalletBalanceUpdate{}, NewConflictErrorf("buyer has insufficient locked KRW balance")
	}

	available := walletAvailableBalance(wallet).Add(reservedAmount.Sub(executionAmount))
	locked := wallet.LockedBalance.Sub(reservedAmount)
	return walletBalanceUpdate(wallet, available, locked), nil
}

func settleSellerCoin(wallet *model.Wallet, quantity decimal.Decimal) (WalletBalanceUpdate, error) {
	if wallet == nil {
		return WalletBalanceUpdate{}, NewValidationErrorf("wallet is required")
	}
	if wallet.CoinSymbol == model.KRWAssetSymbol {
		return WalletBalanceUpdate{}, NewValidationErrorf("seller base settlement requires coin wallet")
	}
	if !quantity.GreaterThan(decimal.Zero) {
		return WalletBalanceUpdate{}, NewValidationErrorf("settlement quantity must be greater than zero")
	}
	if wallet.LockedBalance.LessThan(quantity) {
		return WalletBalanceUpdate{}, NewConflictErrorf("seller has insufficient locked coin balance")
	}

	update := walletBalanceUpdate(wallet, walletAvailableBalance(wallet), wallet.LockedBalance.Sub(quantity))
	if update.Quantity.IsZero() {
		update.AvgBuyPrice = decimal.Zero
	}
	return update, nil
}

func creditAvailable(wallet *model.Wallet, amount decimal.Decimal) (WalletBalanceUpdate, error) {
	if wallet == nil {
		return WalletBalanceUpdate{}, NewValidationErrorf("wallet is required")
	}
	if !amount.GreaterThan(decimal.Zero) {
		return WalletBalanceUpdate{}, NewValidationErrorf("credit amount must be greater than zero")
	}

	return walletBalanceUpdate(wallet, walletAvailableBalance(wallet).Add(amount), wallet.LockedBalance), nil
}

func creditBuyerCoinWithAcquisitionCost(wallet *model.Wallet, quantity decimal.Decimal, acquisitionCost decimal.Decimal) (WalletBalanceUpdate, error) {
	if wallet == nil {
		return WalletBalanceUpdate{}, NewValidationErrorf("wallet is required")
	}
	if wallet.CoinSymbol == model.KRWAssetSymbol {
		return WalletBalanceUpdate{}, NewValidationErrorf("buyer base settlement requires coin wallet")
	}
	if !quantity.GreaterThan(decimal.Zero) {
		return WalletBalanceUpdate{}, NewValidationErrorf("settlement quantity must be greater than zero")
	}
	if !acquisitionCost.GreaterThan(decimal.Zero) {
		return WalletBalanceUpdate{}, NewValidationErrorf("acquisition cost must be greater than zero")
	}
	if wallet.AvgBuyPrice.IsNegative() {
		return WalletBalanceUpdate{}, NewValidationErrorf("avg buy price must be greater than or equal to zero")
	}

	existingQuantity := walletTotalBalance(wallet)
	newQuantity := existingQuantity.Add(quantity)
	if !newQuantity.GreaterThan(decimal.Zero) {
		return WalletBalanceUpdate{}, NewValidationErrorf("wallet quantity must be greater than zero")
	}

	update := walletBalanceUpdate(wallet, walletAvailableBalance(wallet).Add(quantity), wallet.LockedBalance)
	existingCost := wallet.AvgBuyPrice.Mul(existingQuantity)
	update.AvgBuyPrice = existingCost.Add(acquisitionCost).Div(newQuantity)
	return update, nil
}

func walletAvailableBalance(wallet *model.Wallet) decimal.Decimal {
	if wallet.AvailableBalance.IsZero() && wallet.LockedBalance.IsZero() {
		if wallet.CoinSymbol == model.KRWAssetSymbol && wallet.KRW.GreaterThan(decimal.Zero) {
			return wallet.KRW
		}
		if wallet.CoinSymbol != model.KRWAssetSymbol && wallet.Quantity.GreaterThan(decimal.Zero) {
			return wallet.Quantity
		}
	}
	return wallet.AvailableBalance
}

func walletTotalBalance(wallet *model.Wallet) decimal.Decimal {
	if wallet == nil {
		return decimal.Zero
	}
	return walletAvailableBalance(wallet).Add(wallet.LockedBalance)
}

func walletBalanceUpdate(wallet *model.Wallet, available decimal.Decimal, locked decimal.Decimal) WalletBalanceUpdate {
	update := WalletBalanceUpdate{
		AvailableBalance: available,
		LockedBalance:    locked,
		KRW:              wallet.KRW,
		Quantity:         wallet.Quantity,
		AvgBuyPrice:      wallet.AvgBuyPrice,
	}

	total := available.Add(locked)
	if wallet.CoinSymbol == model.KRWAssetSymbol {
		update.KRW = total
	} else {
		update.Quantity = total
	}

	return update
}
