package service

import (
	"fmt"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
)

type WalletBalanceUpdate struct {
	AvailableBalance decimal.Decimal
	LockedBalance    decimal.Decimal
	KRW              decimal.Decimal
	Quantity         decimal.Decimal
}

func applyBuyOrderHold(wallet *model.Wallet, holdAmount decimal.Decimal) (WalletBalanceUpdate, error) {
	if wallet == nil {
		return WalletBalanceUpdate{}, fmt.Errorf("wallet is required")
	}
	if wallet.CoinSymbol != model.KRWAssetSymbol {
		return WalletBalanceUpdate{}, fmt.Errorf("buy hold requires KRW wallet")
	}
	if !holdAmount.GreaterThan(decimal.Zero) {
		return WalletBalanceUpdate{}, fmt.Errorf("hold amount must be greater than zero")
	}

	available := walletAvailableBalance(wallet)
	locked := wallet.LockedBalance
	if available.LessThan(holdAmount) {
		return WalletBalanceUpdate{}, fmt.Errorf("insufficient available KRW balance")
	}

	return walletBalanceUpdate(wallet, available.Sub(holdAmount), locked.Add(holdAmount)), nil
}

func applySellOrderHold(wallet *model.Wallet, holdQuantity decimal.Decimal) (WalletBalanceUpdate, error) {
	if wallet == nil {
		return WalletBalanceUpdate{}, fmt.Errorf("wallet is required")
	}
	if wallet.CoinSymbol == model.KRWAssetSymbol {
		return WalletBalanceUpdate{}, fmt.Errorf("sell hold requires coin wallet")
	}
	if !holdQuantity.GreaterThan(decimal.Zero) {
		return WalletBalanceUpdate{}, fmt.Errorf("hold quantity must be greater than zero")
	}

	available := walletAvailableBalance(wallet)
	locked := wallet.LockedBalance
	if available.LessThan(holdQuantity) {
		return WalletBalanceUpdate{}, fmt.Errorf("insufficient available coin balance")
	}

	return walletBalanceUpdate(wallet, available.Sub(holdQuantity), locked.Add(holdQuantity)), nil
}

func releaseBuyOrderHold(wallet *model.Wallet, releaseAmount decimal.Decimal) (WalletBalanceUpdate, error) {
	if wallet == nil {
		return WalletBalanceUpdate{}, fmt.Errorf("wallet is required")
	}
	if wallet.CoinSymbol != model.KRWAssetSymbol {
		return WalletBalanceUpdate{}, fmt.Errorf("buy release requires KRW wallet")
	}
	if !releaseAmount.GreaterThan(decimal.Zero) {
		return WalletBalanceUpdate{}, fmt.Errorf("release amount must be greater than zero")
	}
	if wallet.LockedBalance.LessThan(releaseAmount) {
		return WalletBalanceUpdate{}, fmt.Errorf("insufficient locked KRW balance")
	}

	available := walletAvailableBalance(wallet).Add(releaseAmount)
	locked := wallet.LockedBalance.Sub(releaseAmount)
	return walletBalanceUpdate(wallet, available, locked), nil
}

func releaseSellOrderHold(wallet *model.Wallet, releaseQuantity decimal.Decimal) (WalletBalanceUpdate, error) {
	if wallet == nil {
		return WalletBalanceUpdate{}, fmt.Errorf("wallet is required")
	}
	if wallet.CoinSymbol == model.KRWAssetSymbol {
		return WalletBalanceUpdate{}, fmt.Errorf("sell release requires coin wallet")
	}
	if !releaseQuantity.GreaterThan(decimal.Zero) {
		return WalletBalanceUpdate{}, fmt.Errorf("release quantity must be greater than zero")
	}
	if wallet.LockedBalance.LessThan(releaseQuantity) {
		return WalletBalanceUpdate{}, fmt.Errorf("insufficient locked coin balance")
	}

	available := walletAvailableBalance(wallet).Add(releaseQuantity)
	locked := wallet.LockedBalance.Sub(releaseQuantity)
	return walletBalanceUpdate(wallet, available, locked), nil
}

func settleBuyerKRW(wallet *model.Wallet, reservedAmount decimal.Decimal, executionAmount decimal.Decimal) (WalletBalanceUpdate, error) {
	if wallet == nil {
		return WalletBalanceUpdate{}, fmt.Errorf("wallet is required")
	}
	if wallet.CoinSymbol != model.KRWAssetSymbol {
		return WalletBalanceUpdate{}, fmt.Errorf("buyer quote settlement requires KRW wallet")
	}
	if !reservedAmount.GreaterThan(decimal.Zero) || !executionAmount.GreaterThan(decimal.Zero) {
		return WalletBalanceUpdate{}, fmt.Errorf("settlement amounts must be greater than zero")
	}
	if reservedAmount.LessThan(executionAmount) {
		return WalletBalanceUpdate{}, fmt.Errorf("reserved amount is less than execution amount")
	}
	if wallet.LockedBalance.LessThan(reservedAmount) {
		return WalletBalanceUpdate{}, fmt.Errorf("buyer has insufficient locked KRW balance")
	}

	available := walletAvailableBalance(wallet).Add(reservedAmount.Sub(executionAmount))
	locked := wallet.LockedBalance.Sub(reservedAmount)
	return walletBalanceUpdate(wallet, available, locked), nil
}

func settleSellerCoin(wallet *model.Wallet, quantity decimal.Decimal) (WalletBalanceUpdate, error) {
	if wallet == nil {
		return WalletBalanceUpdate{}, fmt.Errorf("wallet is required")
	}
	if wallet.CoinSymbol == model.KRWAssetSymbol {
		return WalletBalanceUpdate{}, fmt.Errorf("seller base settlement requires coin wallet")
	}
	if !quantity.GreaterThan(decimal.Zero) {
		return WalletBalanceUpdate{}, fmt.Errorf("settlement quantity must be greater than zero")
	}
	if wallet.LockedBalance.LessThan(quantity) {
		return WalletBalanceUpdate{}, fmt.Errorf("seller has insufficient locked coin balance")
	}

	return walletBalanceUpdate(wallet, walletAvailableBalance(wallet), wallet.LockedBalance.Sub(quantity)), nil
}

func creditAvailable(wallet *model.Wallet, amount decimal.Decimal) (WalletBalanceUpdate, error) {
	if wallet == nil {
		return WalletBalanceUpdate{}, fmt.Errorf("wallet is required")
	}
	if !amount.GreaterThan(decimal.Zero) {
		return WalletBalanceUpdate{}, fmt.Errorf("credit amount must be greater than zero")
	}

	return walletBalanceUpdate(wallet, walletAvailableBalance(wallet).Add(amount), wallet.LockedBalance), nil
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

func walletBalanceUpdate(wallet *model.Wallet, available decimal.Decimal, locked decimal.Decimal) WalletBalanceUpdate {
	update := WalletBalanceUpdate{
		AvailableBalance: available,
		LockedBalance:    locked,
		KRW:              wallet.KRW,
		Quantity:         wallet.Quantity,
	}

	total := available.Add(locked)
	if wallet.CoinSymbol == model.KRWAssetSymbol {
		update.KRW = total
	} else {
		update.Quantity = total
	}

	return update
}
