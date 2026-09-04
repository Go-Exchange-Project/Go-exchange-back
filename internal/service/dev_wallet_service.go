package service

import (
	"fmt"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type DevWalletService struct {
	DB       *gorm.DB
	Ledger   *LedgerService
	Accounts *repository.AccountRepository
}

type FundWalletInput struct {
	UserID     uint
	CoinSymbol string
	Amount     string
	// RequestKey는 같은 지급 요청의 재시도를 식별한다. 서버가 자동 생성하지
	// 않는다 — 자동 생성하면 재시도가 항상 새 키가 되어 멱등이 무의미해진다.
	RequestKey string
}

func NewDevWalletService(db *gorm.DB) *DevWalletService {
	return &DevWalletService{
		DB:       db,
		Ledger:   NewLedgerService(db),
		Accounts: repository.NewAccountRepository(db),
	}
}

// FundWallet은 개발용 공급 계정에서 사용자 계정으로 자산을 옮긴다.
//
// 잔액을 직접 늘리지 않는다. 돈이 무에서 생기는 것처럼 보이지 않도록 DEV_MINT가
// 상대편을 받아 주며, 그 계정을 따로 두어 "실제로 들어온 돈"과 "테스트로 만든
// 돈"을 원장에서 구분할 수 있다.
func (s *DevWalletService) FundWallet(input FundWalletInput) (*repository.UserAssetBalance, error) {
	if s == nil || s.DB == nil {
		return nil, fmt.Errorf("database is required")
	}

	userID, coinSymbol, amount, requestKey, err := normalizeFundWalletInput(input)
	if err != nil {
		return nil, err
	}

	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		_, _, recordErr := s.Ledger.Record(tx, JournalInput{
			EventType:      model.JournalEventDevFund,
			IdempotencyKey: fmt.Sprintf("devfund:%d:%s:%s", userID, coinSymbol, requestKey),
			ReferenceType:  model.JournalReferenceDevFund,
			ReferenceID:    userID,
			Postings: []PostingInput{
				{AccountType: model.AccountDevMint, Asset: coinSymbol, Amount: amount.Neg()},
				{AccountType: model.AccountUserAvailable, OwnerUserID: &userID, Asset: coinSymbol, Amount: amount},
			},
		})
		return recordErr
	}); err != nil {
		return nil, err
	}

	balances, err := s.Accounts.ListUserBalances(userID)
	if err != nil {
		return nil, err
	}
	for i := range balances {
		if balances[i].Asset == coinSymbol {
			return &balances[i], nil
		}
	}
	return nil, fmt.Errorf("funded balance for %s was not found", coinSymbol)
}

func normalizeFundWalletInput(input FundWalletInput) (uint, string, decimal.Decimal, string, error) {
	if input.UserID == 0 {
		return 0, "", decimal.Zero, "", NewValidationErrorf("user_id is required")
	}

	coinSymbol := normalizeCoinSymbol(input.CoinSymbol)
	if coinSymbol == "" {
		return 0, "", decimal.Zero, "", NewValidationErrorf("coin_symbol is required")
	}

	amount, err := parsePositiveDecimal(input.Amount, "amount")
	if err != nil {
		return 0, "", decimal.Zero, "", err
	}

	// 주문 멱등성 키와 같은 계약(공백 제외 1~128자)을 쓴다. 같은 성격의 값에
	// 다른 규칙을 두면 어느 쪽이 맞는지 나중에 알 수 없다.
	requestKey, err := normalizeIdempotencyKey(input.RequestKey)
	if err != nil {
		return 0, "", decimal.Zero, "", NewValidationErrorf("request_key is required and must be 1..%d characters", maxIdempotencyKeyLength)
	}

	return input.UserID, coinSymbol, amount, requestKey, nil
}
