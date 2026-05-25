package service

import (
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
)

func ledgerEntryFromWalletUpdate(
	wallet *model.Wallet,
	update WalletBalanceUpdate,
	entryType model.LedgerEntryType,
	referenceType model.LedgerReferenceType,
	referenceID uint,
	referenceKey string,
) model.LedgerEntry {
	availableBefore := walletAvailableBalance(wallet)
	lockedBefore := wallet.LockedBalance
	return model.LedgerEntry{
		UserID:                wallet.UserID,
		CoinSymbol:            wallet.CoinSymbol,
		EntryType:             entryType,
		AvailableDelta:        update.AvailableBalance.Sub(availableBefore),
		LockedDelta:           update.LockedBalance.Sub(lockedBefore),
		AvailableBalanceAfter: update.AvailableBalance,
		LockedBalanceAfter:    update.LockedBalance,
		ReferenceType:         referenceType,
		ReferenceID:           referenceID,
		ReferenceKey:          referenceKey,
	}
}

func devFundLedgerEntry(wallet *model.Wallet, amount decimal.Decimal) model.LedgerEntry {
	return model.LedgerEntry{
		UserID:                wallet.UserID,
		CoinSymbol:            wallet.CoinSymbol,
		EntryType:             model.LedgerEntryTypeDevFund,
		AvailableDelta:        amount,
		LockedDelta:           decimal.Zero,
		AvailableBalanceAfter: wallet.AvailableBalance,
		LockedBalanceAfter:    wallet.LockedBalance,
		ReferenceType:         model.LedgerReferenceTypeDevFund,
		ReferenceID:           0,
	}
}
