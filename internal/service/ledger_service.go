package service

import (
	"fmt"
	"sort"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// PostingInput은 계정 하나에 대한 변화량이다. 양수면 들어오고, 음수면 나간다.
type PostingInput struct {
	AccountType model.AccountType
	OwnerUserID *uint // 시스템 계정은 nil
	Asset       string
	Amount      decimal.Decimal
}

// JournalInput은 한 사건이다. Postings의 자산별 합은 정확히 0이어야 한다.
type JournalInput struct {
	EventType         model.JournalEventType
	IdempotencyKey    string
	ReferenceType     model.JournalReferenceType
	ReferenceID       uint
	ReversesJournalID *uint
	Postings          []PostingInput
}

// accountKey는 계정을 값으로 식별한다.
//
// repository.AccountSpec을 map 키로 쓰면 안 된다 — OwnerUserID가 포인터라
// 값이 같아도 주소가 다르면 다른 키가 되고, EnsureAccounts가 돌려준 행은
// 입력과 다른 포인터를 갖는다.
type accountKey struct {
	accountType model.AccountType
	ownerUserID uint // 시스템 계정은 0
	asset       string
}

func newAccountKey(accountType model.AccountType, ownerUserID *uint, asset string) accountKey {
	key := accountKey{accountType: accountType, asset: asset}
	if ownerUserID != nil {
		key.ownerUserID = *ownerUserID
	}
	return key
}

// LedgerService는 분개를 만드는 유일한 곳이다.
//
// postings·journal_entries·account_balances에 직접 쓰는 코드를 다른 곳에 두지
// 않는다. 검증을 우회하는 경로를 하나라도 만들면 그 순간부터 자산이 샌다.
type LedgerService struct {
	Accounts *repository.AccountRepository
	Journals *repository.JournalRepository
}

func NewLedgerService(db *gorm.DB) *LedgerService {
	return &LedgerService{
		Accounts: repository.NewAccountRepository(db),
		Journals: repository.NewJournalRepository(db),
	}
}

// Record는 호출자의 트랜잭션 안에서 실행된다. 스스로 트랜잭션을 열지 않는다 —
// HoldCoordinator는 한 트랜잭션에 분개 여러 개를 넣고, 정산은 주문 갱신과 같은
// 트랜잭션이어야 하기 때문이다.
//
// created가 false면 같은 idempotency_key의 기존 분개를 돌려주며 전기를 만들지
// 않는다. 그것은 오류가 아니라 "이미 기록된 사건"이라는 사실이다.
//
// 순서가 중요하다: 멱등 판정이 잔액 검사보다 **먼저**다. 반대로 두면 이미
// 처리된 요청이 현재 잔액 때문에 실패한다. 예를 들어 출금이 확정돼 잔액이 줄어든
// 뒤 같은 알림이 재전송되면, 기존 분개를 돌려주고 끝나야 할 재관측이 음수 검사에
// 걸려 오류가 된다. 그러면 외부가 알림을 계속 재전송하고 우리는 계속 실패를
// 돌려주는 고리에 들어간다.
//
// 신규 분개를 INSERT한 뒤 음수 검사에서 실패해도 찌꺼기는 남지 않는다. 오류를
// 반환하면 호출자가 트랜잭션 전체를 롤백하므로, 분개만 있고 전기가 없는 상태는
// 커밋되지 않는다.
func (s *LedgerService) Record(tx *gorm.DB, in JournalInput) (*model.JournalEntry, bool, error) {
	if tx == nil {
		return nil, false, fmt.Errorf("transaction is required")
	}
	if err := validateJournalInput(in); err != nil {
		return nil, false, err
	}

	journals := s.Journals.WithTx(tx)
	accounts := s.Accounts.WithTx(tx)

	entry := &model.JournalEntry{
		EventType:         in.EventType,
		IdempotencyKey:    in.IdempotencyKey,
		ReferenceType:     in.ReferenceType,
		ReferenceID:       in.ReferenceID,
		ReversesJournalID: in.ReversesJournalID,
	}
	created, err := journals.InsertOrGet(entry)
	if err != nil {
		return nil, false, err
	}
	if !created {
		return entry, false, nil
	}

	specs := make([]repository.AccountSpec, 0, len(in.Postings))
	for _, posting := range in.Postings {
		specs = append(specs, repository.AccountSpec{
			AccountType: posting.AccountType,
			OwnerUserID: posting.OwnerUserID,
			Asset:       posting.Asset,
		})
	}
	accountRows, err := accounts.EnsureAccounts(specs)
	if err != nil {
		return nil, false, err
	}
	accountByKey := make(map[accountKey]model.Account, len(accountRows))
	for _, account := range accountRows {
		accountByKey[newAccountKey(account.AccountType, account.OwnerUserID, account.Asset)] = account
	}

	// 계정별로 변화량을 합친다. 같은 계정에 두 줄이 오면(예: 잠금 해제와 체결이
	// 한 분개에 있는 경우) 잔액은 한 번만 갱신해야 한다.
	deltaByAccountID := map[uint]decimal.Decimal{}
	postings := make([]model.Posting, 0, len(in.Postings))
	for _, posting := range in.Postings {
		account, ok := accountByKey[newAccountKey(posting.AccountType, posting.OwnerUserID, posting.Asset)]
		if !ok {
			return nil, false, fmt.Errorf("account %s/%s was not provisioned", posting.AccountType, posting.Asset)
		}
		postings = append(postings, model.Posting{
			JournalID: entry.ID,
			AccountID: account.ID,
			Asset:     posting.Asset,
			Amount:    posting.Amount,
		})
		deltaByAccountID[account.ID] = deltaByAccountID[account.ID].Add(posting.Amount)
	}

	accountIDs := make([]uint, 0, len(deltaByAccountID))
	for id := range deltaByAccountID {
		accountIDs = append(accountIDs, id)
	}
	// 항상 오름차순으로 잠근다. 모든 트랜잭션이 같은 순서를 쓰므로 계정 간
	// AB-BA 데드락이 성립하지 않는다.
	sort.Slice(accountIDs, func(i, j int) bool { return accountIDs[i] < accountIDs[j] })

	balances, err := accounts.LockBalances(accountIDs)
	if err != nil {
		return nil, false, err
	}
	allowsNegative := map[uint]bool{}
	for _, account := range accountRows {
		allowsNegative[account.ID] = account.AllowsNegative
	}
	for _, balance := range balances {
		next := balance.Balance.Add(deltaByAccountID[balance.AccountID])
		if next.IsNegative() && !allowsNegative[balance.AccountID] {
			return nil, false, NewConflictErrorf(
				"account %d would go negative (%s)", balance.AccountID, next.String())
		}
	}

	if err := journals.CreatePostings(postings); err != nil {
		return nil, false, err
	}

	// 전기의 ID는 INSERT 후에 채워진다. 잔액 캐시가 "어디까지 반영했는지"를
	// 기록하려면 그 값이 필요하다.
	lastPostingByAccount := map[uint]uint64{}
	for _, posting := range postings {
		if posting.ID > lastPostingByAccount[posting.AccountID] {
			lastPostingByAccount[posting.AccountID] = posting.ID
		}
	}
	deltas := make([]repository.BalanceDelta, 0, len(accountIDs))
	for _, id := range accountIDs {
		deltas = append(deltas, repository.BalanceDelta{
			AccountID:     id,
			Delta:         deltaByAccountID[id],
			LastPostingID: lastPostingByAccount[id],
		})
	}
	if err := journals.ApplyBalanceDeltas(deltas); err != nil {
		return nil, false, err
	}

	return entry, true, nil
}

// Reverse는 원본 전기의 부호를 뒤집어 새 분개로 적는다. 원본은 그대로 남는다 —
// 잘못된 기록을 지우면 무엇이 어떻게 잘못됐는지 알 수 없게 된다.
//
// journal_entries의 UNIQUE(reverses_journal_id)가 한 분개를 두 번 되돌리는 것을
// 막는다.
func (s *LedgerService) Reverse(tx *gorm.DB, journalID uint) (*model.JournalEntry, error) {
	if tx == nil {
		return nil, fmt.Errorf("transaction is required")
	}

	var source model.JournalEntry
	if err := tx.Where("id = ?", journalID).First(&source).Error; err != nil {
		return nil, err
	}

	original, err := s.Journals.WithTx(tx).FindPostingsByJournalID(journalID)
	if err != nil {
		return nil, err
	}
	if len(original) == 0 {
		return nil, NewValidationErrorf("journal %d has no postings to reverse", journalID)
	}

	accountIDs := make([]uint, 0, len(original))
	seen := map[uint]bool{}
	for _, posting := range original {
		if !seen[posting.AccountID] {
			seen[posting.AccountID] = true
			accountIDs = append(accountIDs, posting.AccountID)
		}
	}
	var accounts []model.Account
	if err := tx.Where("id IN ?", accountIDs).Find(&accounts).Error; err != nil {
		return nil, err
	}
	accountByID := make(map[uint]model.Account, len(accounts))
	for _, account := range accounts {
		accountByID[account.ID] = account
	}

	postings := make([]PostingInput, 0, len(original))
	for _, posting := range original {
		account, ok := accountByID[posting.AccountID]
		if !ok {
			return nil, fmt.Errorf("account %d referenced by posting %d is missing", posting.AccountID, posting.ID)
		}
		postings = append(postings, PostingInput{
			AccountType: account.AccountType,
			OwnerUserID: account.OwnerUserID,
			Asset:       posting.Asset,
			Amount:      posting.Amount.Neg(),
		})
	}

	// 참조는 원본과 같은 것을 가리킨다. 역분개는 같은 사건을 되돌리는 것이지
	// 새로운 종류의 사건이 아니다.
	entry, _, err := s.Record(tx, JournalInput{
		EventType:         model.JournalEventReversal,
		IdempotencyKey:    fmt.Sprintf("reversal:%d", journalID),
		ReferenceType:     source.ReferenceType,
		ReferenceID:       source.ReferenceID,
		ReversesJournalID: &journalID,
		Postings:          postings,
	})
	return entry, err
}

func validateJournalInput(in JournalInput) error {
	if in.IdempotencyKey == "" {
		return NewValidationErrorf("idempotency key is required")
	}
	if in.EventType == "" {
		return NewValidationErrorf("event type is required")
	}
	if in.ReferenceType == "" {
		return NewValidationErrorf("reference type is required")
	}
	if len(in.Postings) < 2 {
		return NewValidationErrorf("a journal needs at least two postings")
	}
	if (in.EventType == model.JournalEventReversal) != (in.ReversesJournalID != nil) {
		return NewValidationErrorf("only a reversal may reference an original journal")
	}

	// 금액 0짜리 전기는 아무 사실도 기록하지 않으면서 분개만 늘린다. 출금
	// 수수료가 0일 때 FEE_INCOME 줄을 만들지 않는 것이 그 사례다.
	sums := map[string]decimal.Decimal{}
	for _, posting := range in.Postings {
		if posting.Asset == "" {
			return NewValidationErrorf("posting asset is required")
		}
		if posting.Amount.IsZero() {
			return NewValidationErrorf("posting amount must not be zero")
		}
		if model.IsSystemAccountType(posting.AccountType) != (posting.OwnerUserID == nil) {
			return NewValidationErrorf("account %s has the wrong owner", posting.AccountType)
		}
		sums[posting.Asset] = sums[posting.Asset].Add(posting.Amount)
	}
	for asset, sum := range sums {
		if !sum.IsZero() {
			return NewValidationErrorf("journal does not balance for %s: %s", asset, sum.String())
		}
	}
	return nil
}
