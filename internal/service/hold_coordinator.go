package service

import (
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

const defaultHoldBatchSize = 64
const defaultHoldFlushInterval = 5 * time.Millisecond
const holdCoordinatorInputCap = 1024

type holdRole uint8

const (
	holdRoleOwner holdRole = iota
	holdRoleFollower
)

// idempotencyContext는 요청의 멱등성 키와 지문을 hold 경로까지 실어 나른다.
type idempotencyContext struct {
	Key         string
	Fingerprint string
	Version     int
	RecordID    uint64 // INSERT 후 채워진다
}

type holdRequest struct {
	order    *model.Order
	idem     *idempotencyContext
	resultCh chan holdResult
}

type holdResult struct {
	Order *model.Order // 성공 시 ID 채워짐
	Err   error        // nil=성공, ConflictError=잔고부족, 그 외=시스템
	// Role은 이 요청이 주문을 실제로 만들었는지(owner) 아니면 같은 키의 중복인지
	// (follower) 구분한다. follower는 엔진에 제출하지 않는다 — 제출하면 hold는
	// 한 번인데 엔진 제출이 두 번이 된다.
	Role     holdRole
	Existing *model.OrderIdempotencyKey // 이미 커밋된 키를 따라갈 때 저장된 레코드
}

// groupIdempotentRequests는 SQL 이전에 배치 안의 (user_id, key) 중복을 정리한다.
//
// 같은 키·같은 지문이면 앞선 것이 owner, 나머지는 follower다.
// 같은 키·다른 지문이면 앞선 것만 진행하고 나머지는 conflict(409)다.
// 도착 순서(인덱스)로 결정하므로 같은 입력은 항상 같은 결과를 낸다 — map 순회에
// 맡기면 실행마다 달라진다.
func groupIdempotentRequests(reqs []holdRequest) (owners []int, followers map[int]int, conflicts []int) {
	type groupKey struct {
		userID uint
		key    string
	}
	first := map[groupKey]int{}
	followers = map[int]int{}

	for i := range reqs {
		if reqs[i].idem == nil {
			owners = append(owners, i)
			continue
		}
		gk := groupKey{userID: reqs[i].order.UserID, key: reqs[i].idem.Key}
		leader, seen := first[gk]
		if !seen {
			first[gk] = i
			owners = append(owners, i)
			continue
		}
		if reqs[leader].idem.Fingerprint == reqs[i].idem.Fingerprint {
			followers[i] = leader
		} else {
			conflicts = append(conflicts, i)
		}
	}
	return owners, followers, conflicts
}

type HoldCoordinator struct {
	DB         *gorm.DB
	OrderRepo  *repository.OrderRepository
	WalletRepo *repository.WalletRepository
	LedgerRepo *repository.LedgerRepository
	IdemRepo   *repository.OrderIdempotencyRepository

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
// 리셋(phantom ID 방지). 성공 시 결과는 reqs 인덱스와 1:1.
//
// 멱등성 키가 있으면 지갑 hold보다 **먼저** 키를 INSERT해 owner/follower를 가른다.
// 순서가 뒤집히면 같은 키의 중복 요청이 지갑 hold를 두 번 소비한다.
func (c *HoldCoordinator) HoldBatch(reqs []holdRequest) ([]holdResult, error) {
	results := make([]holdResult, len(reqs))
	owners, followers, conflicts := groupIdempotentRequests(reqs)

	for _, i := range conflicts {
		results[i] = holdResult{Err: NewConflictErrorf("idempotency key reused with a different request")}
	}

	err := c.DB.Transaction(func(tx *gorm.DB) error {
		orderRepo := c.OrderRepo.WithTx(tx)
		walletRepo := c.WalletRepo.WithTx(tx)
		ledgerRepo := c.LedgerRepo.WithTx(tx)

		activeOwners, err := c.claimIdempotencyKeys(tx, reqs, owners, results)
		if err != nil {
			return err
		}

		// 1. 지갑 키 수집(dedup) → 2. FindByKeys로 ID 확보 → ID 오름차순 LockByIDs.
		keySet := map[repository.WalletKey]bool{}
		keys := make([]repository.WalletKey, 0, len(activeOwners))
		for _, i := range activeOwners {
			k := holdWalletKey(reqs[i].order)
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

		// hold 검증에 실패한 owner의 키는 이번 트랜잭션에서 지운다. HoldBatch는 실패분을
		// results에 격리하고 나머지와 함께 커밋하므로, 지우지 않으면 "검증 실패는 키를
		// 소비하지 않는다"가 깨진다.
		var failedRecordIDs []uint64
		failIdempotent := func(i int) {
			if reqs[i].idem != nil && reqs[i].idem.RecordID != 0 {
				failedRecordIDs = append(failedRecordIDs, reqs[i].idem.RecordID)
			}
		}

		for _, i := range activeOwners {
			order := reqs[i].order
			wallet := walletByKey[holdWalletKey(order)]
			if wallet == nil { // 지갑 없음 = 잔고 부족과 동일
				results[i] = holdResult{Err: NewConflictErrorf("insufficient available balance")}
				failIdempotent(i)
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
				failIdempotent(i)
				continue
			}
			// 원장 엔트리는 fold 전에 계산(delta = update - 현재 잔고).
			entry := ledgerEntryFromWalletUpdate(wallet, update, model.LedgerEntryTypeOrderHold, model.LedgerReferenceTypeOrder, 0, "")
			foldWalletBalanceUpdate(wallet, update) // 다음 주문이 차감된 잔고를 본다
			changedWallets[wallet.ID] = wallet
			passing = append(passing, passingHold{idx: i, order: order, entry: entry})
		}

		// 전원 실패 조기 반환 경로에도 같은 정리가 필요하다.
		if len(failedRecordIDs) > 0 {
			if err := c.IdemRepo.WithTx(tx).DeleteByIDs(failedRecordIDs); err != nil {
				return err
			}
		}
		for _, i := range activeOwners {
			if results[i].Err != nil && reqs[i].idem != nil {
				reqs[i].idem.RecordID = 0 // 지워진 레코드를 가리키지 않게 한다
			}
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

		// 성공한 owner의 레코드에 order_id와 PENDING을 기록한다. ACCEPTED로 앞당겨
		// 쓰지 않는다 — 엔진 제출은 이 트랜잭션 밖이다.
		idemRepo := c.IdemRepo.WithTx(tx)
		for j := range passing {
			idem := reqs[passing[j].idx].idem
			if idem == nil {
				continue
			}
			if err := idemRepo.SetOrderAndOutcome(
				idem.RecordID, passing[j].order.ID, model.OrderIdempotencyOutcomePending,
			); err != nil {
				return err
			}
		}

		// 통과분 결과 채움.
		for j := range passing {
			results[passing[j].idx] = holdResult{Order: passing[j].order}
		}
		return nil
	})

	if err != nil {
		for i := range reqs { // phantom ID 방지(B-4 8b3007f 교훈)
			reqs[i].order.ID = 0
			if reqs[i].idem != nil {
				// 롤백된 레코드를 가리키는 RecordID도 같은 종류의 유령이다.
				reqs[i].idem.RecordID = 0
			}
		}
		return nil, err
	}

	applyFollowerResults(results, followers)
	return results, nil
}

// applyFollowerResults는 같은 배치의 중복 요청에 leader의 결과를 복사한다.
// 엔진 제출은 owner만 하므로 follower는 결과만 공유한다.
//
// 이때 Existing은 nil로 남을 수 있다. leader가 이번 배치에서 주문을 만든 경우에는
// 저장된 레코드를 읽어 온 적이 없기 때문이다. 호출자는 Existing이 아니라 Role로
// follower를 판정해야 한다.
func applyFollowerResults(results []holdResult, followers map[int]int) {
	for i, leader := range followers {
		results[i] = holdResult{
			Order:    results[leader].Order,
			Err:      results[leader].Err,
			Role:     holdRoleFollower,
			Existing: results[leader].Existing,
		}
	}
}

// claimIdempotencyKeys는 owner 후보의 키를 먼저 INSERT하고, 실제로 삽입된 것만 돌려준다.
//
// 삽입되지 않은 키는 이미 커밋된 요청이다. 그 요청은 배치에서 빼고 저장된 레코드를
// results에 채워, 호출자가 저장된 결과로 응답하게 한다.
func (c *HoldCoordinator) claimIdempotencyKeys(tx *gorm.DB, reqs []holdRequest, owners []int, results []holdResult) ([]int, error) {
	newRecords := make([]*model.OrderIdempotencyKey, 0, len(owners))
	recordIdx := make([]int, 0, len(owners))
	active := make([]int, 0, len(owners))

	for _, i := range owners {
		if reqs[i].idem == nil {
			active = append(active, i)
			continue
		}
		newRecords = append(newRecords, &model.OrderIdempotencyKey{
			UserID:             reqs[i].order.UserID,
			IdempotencyKey:     reqs[i].idem.Key,
			Fingerprint:        reqs[i].idem.Fingerprint,
			FingerprintVersion: reqs[i].idem.Version,
			// outcome은 NOT NULL이다. 비워 두면 GORM이 빈 문자열을 넣어 CHECK에 걸린다.
			// 커밋 시점의 상태는 "durable하게 알지 못함" = PENDING으로 확정돼 있다.
			Outcome: model.OrderIdempotencyOutcomePending,
		})
		recordIdx = append(recordIdx, i)
	}
	if len(newRecords) == 0 {
		return active, nil
	}

	idemRepo := c.IdemRepo.WithTx(tx)
	if _, err := idemRepo.InsertNew(newRecords); err != nil {
		return nil, err
	}

	lookup := make([]repository.UserKeyPair, 0, len(newRecords))
	existingIdx := map[repository.UserKeyPair]int{}
	for j, i := range recordIdx {
		if newRecords[j].ID != 0 {
			reqs[i].idem.RecordID = newRecords[j].ID
			active = append(active, i)
			continue
		}
		pair := repository.UserKeyPair{UserID: reqs[i].order.UserID, Key: reqs[i].idem.Key}
		lookup = append(lookup, pair)
		existingIdx[pair] = i
	}

	if len(lookup) > 0 {
		found, err := idemRepo.FindByUserKeys(lookup)
		if err != nil {
			return nil, err
		}
		if len(found) != len(lookup) {
			// 방금 충돌한 키가 조회되지 않는다 = 다른 트랜잭션이 지웠다는 뜻이다.
			// 조용히 넘어가면 그 요청은 결과 없이 남는다.
			return nil, fmt.Errorf("idempotency lookup found %d of %d existing keys", len(found), len(lookup))
		}
		for k := range found {
			pair := repository.UserKeyPair{UserID: found[k].UserID, Key: found[k].IdempotencyKey}
			i, ok := existingIdx[pair]
			if !ok {
				continue
			}
			record := found[k]
			results[i] = holdResult{Role: holdRoleFollower, Existing: &record}
		}
	}

	// 도착 순서를 유지해야 결과가 결정적이다.
	sort.Ints(active)
	return active, nil
}

func NewHoldCoordinator(db *gorm.DB, orderRepo *repository.OrderRepository, walletRepo *repository.WalletRepository, ledgerRepo *repository.LedgerRepository, idemRepo *repository.OrderIdempotencyRepository, batchSize int) *HoldCoordinator {
	if batchSize <= 0 {
		batchSize = defaultHoldBatchSize
	}
	return &HoldCoordinator{
		DB: db, OrderRepo: orderRepo, WalletRepo: walletRepo, LedgerRepo: ledgerRepo, IdemRepo: idemRepo,
		BatchSize: batchSize, FlushInterval: defaultHoldFlushInterval,
		input: make(chan holdRequest, holdCoordinatorInputCap), done: make(chan struct{}),
	}
}

// Submit은 요청을 입력에 바운디드(논블로킹) 제출한다. 입력이 만석이면 즉시 503.
// 제출 성공 후 결과 대기엔 타임아웃이 없다(고아 방지 — 제출된 요청은 항상 유한 시간에 시그널).
func (c *HoldCoordinator) Submit(order *model.Order) (*model.Order, error) {
	res, err := c.SubmitWithIdempotency(order, nil)
	return res.Order, err
}

// SubmitWithIdempotency는 멱등성 키를 함께 실어 제출하고 owner/follower 판정까지 담은
// 결과를 돌려준다. 호출자는 Role을 보고 엔진 제출 여부를 정한다.
func (c *HoldCoordinator) SubmitWithIdempotency(order *model.Order, idem *idempotencyContext) (holdResult, error) {
	req := holdRequest{order: order, idem: idem, resultCh: make(chan holdResult, 1)}
	select {
	case c.input <- req:
	default:
		metrics.OrdersAdmissionRejectedTotal.WithLabelValues("coordinator").Inc()
		return holdResult{}, NewUnavailableErrorf("order intake is saturated, please retry shortly")
	}
	res := <-req.resultCh
	return res, res.Err
}

// Run은 input이 닫힐 때까지 배치를 수집·처리하고, 닫힌 뒤 잔여를 처리하고 done을 닫는다.
func (c *HoldCoordinator) Run() {
	defer close(c.done)
	for {
		first, ok := <-c.input
		if !ok {
			return
		}
		batch, open := c.collectBatch([]holdRequest{first})
		c.processBatch(batch)
		if !open {
			return
		}
	}
}

func (c *HoldCoordinator) collectBatch(batch []holdRequest) ([]holdRequest, bool) {
	timer := time.NewTimer(c.FlushInterval)
	defer timer.Stop()
	for len(batch) < c.BatchSize {
		select {
		case req, ok := <-c.input:
			if !ok {
				return batch, false
			}
			batch = append(batch, req)
		case <-timer.C:
			return batch, true
		}
	}
	return batch, true
}

func (c *HoldCoordinator) processBatch(reqs []holdRequest) {
	results, err := c.HoldBatch(reqs)
	if err != nil {
		metrics.HoldBatchFallbacksTotal.Inc()
		c.logf("hold batch of %d failed, falling back to per-order: %v", len(reqs), err)
		for i, res := range c.fallbackPerRequest(reqs) {
			reqs[i].resultCh <- res
		}
		return
	}
	metrics.HoldBatchSize.Observe(float64(len(reqs)))
	for i := range reqs {
		reqs[i].resultCh <- results[i]
	}
}

// fallbackPerRequest는 배치 트랜잭션이 실패했을 때 요청을 하나씩 처리한다.
//
// 여기서도 먼저 그룹화한다. 요청을 전부 독립 처리하면 배치가 내렸을 판정이 뒤집힌다 —
// 같은 키의 앞 요청이 검증 실패로 롤백되면 뒤의 다른 지문 요청이 409 대신 owner가 되어
// 주문을 만든다. owner만 단건 처리하고, follower는 결과를 복사하며, conflict는 409로 둔다.
func (c *HoldCoordinator) fallbackPerRequest(reqs []holdRequest) []holdResult {
	owners, followers, conflicts := groupIdempotentRequests(reqs)
	results := make([]holdResult, len(reqs))

	for _, i := range conflicts {
		results[i] = holdResult{Err: NewConflictErrorf("idempotency key reused with a different request")}
	}
	for _, i := range owners {
		results[i] = c.persistAndHoldOne(reqs[i])
	}
	applyFollowerResults(results, followers)
	return results
}

// persistAndHoldOne은 배치 트랜잭션이 실패했을 때의 단건 폴백이다.
//
// 배치와 같은 순서를 지킨다: 키를 먼저 INSERT해 owner인지 판정하고, owner일 때만
// 주문·hold를 만든다. 검증 실패는 이 트랜잭션 전체를 롤백하므로 키도 함께 사라진다 —
// 배치 경로의 DeleteByIDs와 같은 효과다.
func (c *HoldCoordinator) persistAndHoldOne(req holdRequest) holdResult {
	return persistAndHoldIdempotent(c.DB, c.OrderRepo, c.WalletRepo, c.LedgerRepo, c.IdemRepo, req)
}

// persistAndHoldIdempotent는 단건 경로의 본체다. 코디네이터 폴백과, 코디네이터가
// 배선되지 않은 서비스 경로가 같은 순서를 쓰도록 자유 함수로 둔다.
func persistAndHoldIdempotent(
	db *gorm.DB,
	orderRepo *repository.OrderRepository,
	walletRepo *repository.WalletRepository,
	ledgerRepo *repository.LedgerRepository,
	idemRepo *repository.OrderIdempotencyRepository,
	req holdRequest,
) holdResult {
	if req.idem == nil {
		err := persistAndHold(db, orderRepo, walletRepo, ledgerRepo, req.order)
		return holdResult{Order: req.order, Err: err}
	}

	var result holdResult
	err := db.Transaction(func(tx *gorm.DB) error {
		txIdemRepo := idemRepo.WithTx(tx)
		record := &model.OrderIdempotencyKey{
			UserID:             req.order.UserID,
			IdempotencyKey:     req.idem.Key,
			Fingerprint:        req.idem.Fingerprint,
			FingerprintVersion: req.idem.Version,
			Outcome:            model.OrderIdempotencyOutcomePending,
		}
		inserted, err := txIdemRepo.InsertNew([]*model.OrderIdempotencyKey{record})
		if err != nil {
			return err
		}
		if len(inserted) == 0 {
			found, err := txIdemRepo.FindByUserKeys(
				[]repository.UserKeyPair{{UserID: req.order.UserID, Key: req.idem.Key}})
			if err != nil {
				return err
			}
			if len(found) != 1 {
				return fmt.Errorf("idempotency lookup found %d rows for an existing key", len(found))
			}
			existing := found[0]
			result = holdResult{Role: holdRoleFollower, Existing: &existing}
			return nil
		}
		req.idem.RecordID = record.ID

		if err := orderRepo.WithTx(tx).CreateOrder(req.order); err != nil {
			return err
		}
		if err := holdOrderAssets(walletRepo.WithTx(tx), ledgerRepo.WithTx(tx), req.order); err != nil {
			return err
		}
		if err := txIdemRepo.SetOrderAndOutcome(
			record.ID, req.order.ID, model.OrderIdempotencyOutcomePending); err != nil {
			return err
		}
		result = holdResult{Order: req.order}
		return nil
	})
	if err != nil {
		req.order.ID = 0 // 롤백된 트랜잭션의 ID를 남기지 않는다
		req.idem.RecordID = 0
		return holdResult{Order: req.order, Err: err}
	}
	return result
}

// Shutdown은 입력을 닫아 drain을 트리거하고 Run 종료를 기다린다.
func (c *HoldCoordinator) Shutdown() {
	close(c.input)
	<-c.done
}

// InputLen은 입력 채널의 현재 적체를 반환한다 — 게이지 등록용.
func (c *HoldCoordinator) InputLen() int {
	return len(c.input)
}

func (c *HoldCoordinator) logf(format string, args ...interface{}) {
	logger := c.Logger
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf(format, args...)
}
