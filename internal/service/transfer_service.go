package service

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// ExternalTransferProcessor는 가짜 은행·가짜 체인을 흉내 낸다. Submit은 안정적인
// dispatchKey("transfer:{requestID}")를 받아 외부 송금을 멱등하게 만든다 — 제출
// 후 응답 전에 죽어도 재시도가 같은 external_ref를 돌려받는다.
type ExternalTransferProcessor interface {
	Submit(dispatchKey string, req model.TransferRequest) (externalRef string, err error)
	GetTransferStatus(externalRef string) (model.TransferOutcome, map[string]any, error)
}

// TransferService는 가짜 입출금 접수·확정·미확정 관측을 담당한다.
//
// ResolveTransfer(확정 경로)와 RecordObservation(미확정 경로)을 함수로 나눈 것이
// 핵심이다 — 한 함수가 네 outcome을 모두 받으면 "미확정인데 확정 분기로 떨어지는"
// 실수가 가능해지고, 그 실수는 돈을 움직인다.
type TransferService struct {
	DB        *gorm.DB
	Ledger    *LedgerService
	Transfers *repository.TransferRepository
	Processor ExternalTransferProcessor
	Now       func() time.Time

	// afterLock은 테스트 전용이다. FOR UPDATE 반환 직후, 사건 INSERT 이전에 A
	// 경로에서만 한 번 불린다. 프로덕션은 항상 nil이다.
	afterLock func()
}

func NewTransferService(db *gorm.DB, processor ExternalTransferProcessor) *TransferService {
	return &TransferService{
		DB:        db,
		Ledger:    NewLedgerService(db),
		Transfers: repository.NewTransferRepository(db),
		Processor: processor,
	}
}

func (s *TransferService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

type DepositInput struct {
	UserID           uint
	Rail             model.TransferRail
	Asset            string
	Amount           string
	ClientRequestKey string
}

type WithdrawalInput struct {
	UserID           uint
	Rail             model.TransferRail
	Asset            string
	Amount           string
	ClientRequestKey string
}

type ResolveInput struct {
	TransferRequestID uint
	Source            model.TransferEventSource
	EventKey          string
	// Outcome은 SUCCESS 또는 FAILURE만 받는다.
	Outcome model.TransferOutcome
	Payload map[string]any
}

type ObservationInput struct {
	TransferRequestID uint
	EventKey          string
	// Outcome은 PENDING 또는 UNKNOWN만 받는다.
	Outcome model.TransferOutcome
	Payload map[string]any
}

// RequestDeposit은 입금 요청을 접수한다. 잠글 것이 없으므로 RECEIVED로 끝난다.
func (s *TransferService) RequestDeposit(in DepositInput) (*model.TransferRequest, error) {
	userID, rail, asset, amount, clientKey, err := normalizeTransferInput(
		in.UserID, in.Rail, in.Asset, in.Amount, in.ClientRequestKey)
	if err != nil {
		return nil, err
	}

	candidate := &model.TransferRequest{
		UserID: userID, Direction: model.TransferDirectionDeposit, Rail: rail,
		Asset: asset, Amount: amount, FeeAmount: decimal.Zero, FeeAsset: asset,
		Status: model.TransferStatusReceived, ClientRequestKey: clientKey,
	}
	created, result, err := s.Transfers.InsertOrGetByUserRequestKey(candidate)
	if err != nil {
		return nil, err
	}
	if !created && !sameTransferBody(result, model.TransferDirectionDeposit, rail, asset, amount) {
		return nil, NewConflictErrorf("client_request_key %q was already used with a different request", clientKey)
	}

	return s.dispatchAndReload(*result)
}

// RequestWithdrawal은 출금 요청을 접수한다. 잠금 분개는 접수와 같은 트랜잭션 —
// 키 선점보다 잠금이 먼저면 같은 요청이 두 번 잠근다.
func (s *TransferService) RequestWithdrawal(in WithdrawalInput) (*model.TransferRequest, error) {
	userID, rail, asset, amount, clientKey, err := normalizeTransferInput(
		in.UserID, in.Rail, in.Asset, in.Amount, in.ClientRequestKey)
	if err != nil {
		return nil, err
	}

	var result *model.TransferRequest
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		candidate := &model.TransferRequest{
			UserID: userID, Direction: model.TransferDirectionWithdrawal, Rail: rail,
			Asset: asset, Amount: amount, FeeAmount: decimal.Zero, FeeAsset: asset,
			Status: model.TransferStatusReceived, ClientRequestKey: clientKey,
		}
		created, existing, insertErr := s.Transfers.WithTx(tx).InsertOrGetByUserRequestKey(candidate)
		if insertErr != nil {
			return insertErr
		}
		if !created {
			if !sameTransferBody(existing, model.TransferDirectionWithdrawal, rail, asset, amount) {
				return NewConflictErrorf("client_request_key %q was already used with a different request", clientKey)
			}
			result = existing
			return nil
		}

		// 출금액 + 수수료를 함께 잠근다. 첫 구현의 수수료는 0이라 잠금액이
		// 출금액과 같지만, 산술은 처음부터 amount + fee로 쓴다.
		locked := amount.Add(candidate.FeeAmount)
		owner := userID
		entry, _, recordErr := s.Ledger.Record(tx, JournalInput{
			EventType:      model.JournalEventWithdrawal,
			IdempotencyKey: fmt.Sprintf("withdraw-hold:%d", candidate.ID),
			ReferenceType:  model.JournalReferenceTransfer,
			ReferenceID:    candidate.ID,
			Postings: []PostingInput{
				{AccountType: model.AccountUserAvailable, OwnerUserID: &owner, Asset: asset, Amount: locked.Neg()},
				{AccountType: model.AccountUserLocked, OwnerUserID: &owner, Asset: asset, Amount: locked},
			},
		})
		if recordErr != nil {
			return recordErr
		}
		if setErr := s.Transfers.WithTx(tx).SetHoldJournal(candidate.ID, entry.ID); setErr != nil {
			return setErr
		}
		result = candidate
		return nil
	})
	if err != nil {
		return nil, err
	}

	return s.dispatchAndReload(*result)
}

// Dispatch는 RECEIVED 요청을 외부로 제출한다. RECEIVED가 아니면 아무 일도 하지
// 않는다 — 이미 제출됐거나 확정된 요청을 다시 제출하면 안 된다.
//
// 성공하면 next_check_at을 제출 시각 + pollBaseInterval로 설정하고
// check_attempts를 0으로 초기화한다 — 조회 백오프가 제출 실패 횟수를 이어받지
// 않게 한다. 실패하면 HandleDispatchFailure가 제출 재시도 일정을 뒤로 미룬다
// — 그러지 않으면 poller가 매 틱마다 같은 요청을 재제출해 뒤의 다른 요청들이
// 굶는다.
func (s *TransferService) Dispatch(request model.TransferRequest) error {
	if request.Status != model.TransferStatusReceived {
		return nil
	}
	dispatchKey := fmt.Sprintf("transfer:%d", request.ID)
	externalRef, err := s.Processor.Submit(dispatchKey, request)
	if err != nil {
		if scheduleErr := s.HandleDispatchFailure(request.ID); scheduleErr != nil {
			return scheduleErr
		}
		return err
	}
	nextCheckAt := s.now().Add(pollBaseInterval)
	return s.Transfers.SetDispatched(request.ID, externalRef, nextCheckAt)
}

// HandleDispatchFailure는 Submit(외부 제출) 시도가 실패했을 때 쓴다.
// RecordObservation·HandleStatusCheckFailure와 같은 잠금 규율을 따른다: 잠근
// 뒤 다시 읽은 상태가 RECEIVED가 아니면 그사이 다른 경로로 진행됐다는 뜻이므로
// 아무것도 하지 않는다. 분개는 만들지 않는다 — 제출 실패가 돈을 움직이면
// 안 된다.
func (s *TransferService) HandleDispatchFailure(transferRequestID uint) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		transfers := s.Transfers.WithTx(tx)
		request, err := transfers.LockByID(transferRequestID)
		if err != nil {
			return err
		}
		if request.Status != model.TransferStatusReceived {
			return nil
		}
		now := s.now()
		attempts := request.CheckAttempts + 1
		nextCheckAt := now.Add(backoffInterval(attempts))
		return transfers.AdvanceDispatchSchedule(request.ID, nextCheckAt, attempts)
	})
}

// dispatchAndReload는 접수 직후(또는 재시도) 외부 제출을 시도한다. 실패해도
// 오류를 올리지 않는다 — 접수는 이미 끝났고, RECEIVED로 남은 요청은 poller의
// 제출 재시도가 마무리한다.
func (s *TransferService) dispatchAndReload(request model.TransferRequest) (*model.TransferRequest, error) {
	_ = s.Dispatch(request)

	var reloaded model.TransferRequest
	if err := s.DB.Where("id = ?", request.ID).First(&reloaded).Error; err != nil {
		return nil, err
	}
	return &reloaded, nil
}

// ResolveTransfer는 확정 결과(SUCCESS/FAILURE)만 받는다. PENDING·UNKNOWN을 받으면
// 프로그래밍 오류로 즉시 거부한다 — 분기가 아니라 함수로 막는다.
//
// 1번 FOR UPDATE가 이 함수 전체의 근거다. 잠근 뒤 다시 읽은 status는 우리가
// 커밋할 때까지 바뀌지 않는다.
func (s *TransferService) ResolveTransfer(in ResolveInput) error {
	if !in.Outcome.IsTerminal() {
		return fmt.Errorf("ResolveTransfer requires a terminal outcome, got %s", in.Outcome)
	}
	if in.TransferRequestID == 0 || in.EventKey == "" {
		return fmt.Errorf("transfer_request_id and event_key are required")
	}

	return s.DB.Transaction(func(tx *gorm.DB) error {
		transfers := s.Transfers.WithTx(tx)
		request, err := transfers.LockByID(in.TransferRequestID)
		if err != nil {
			return err
		}

		if s.afterLock != nil {
			hook := s.afterLock
			s.afterLock = nil // 일회성 — 스스로 해제되어 이후 호출에는 걸리지 않는다.
			hook()
		}

		payload, err := encodeTransferPayload(in.Payload)
		if err != nil {
			return err
		}
		created, err := transfers.InsertEventIfAbsent(&model.TransferStatusEvent{
			TransferRequestID: request.ID,
			Source:            in.Source,
			EventKey:          in.EventKey,
			Outcome:           in.Outcome,
			Payload:           payload,
			ReceivedAt:        s.now(),
		})
		if err != nil {
			return err
		}
		if !created {
			return nil // 이미 본 사건이다. 커밋하고 종료.
		}

		switch {
		case request.Status == model.TransferStatusReceived:
			// 아직 외부로 보낸 적이 없는데 결과가 왔다 — 우리 인식과 외부 현실이
			// 어긋났다는 신호다. 돈을 움직이지 않는다.
			return transfers.SetReviewRequired(request.ID, model.ReviewReasonTerminalBeforeDispatch, s.now())

		case request.Status == model.TransferStatusProcessing:
			return s.confirmTerminal(tx, transfers, request, in.Outcome, in.Payload)

		case request.Status.IsTerminal() && matchesTerminalOutcome(request.Status, in.Outcome):
			return nil // 같은 결과의 재관측. 사건만 남기고 확인 표시는 건드리지 않는다.

		case request.Status.IsTerminal():
			// 반대 결과. 어느 쪽이 참인지 우리는 모른다 — 돈과 상태를 그대로 두고
			// 사람을 부른다.
			return transfers.SetReviewRequired(request.ID, model.ReviewReasonConflictingTerminalOutcome, s.now())

		default:
			return fmt.Errorf("transfer request %d has unexpected status %s", request.ID, request.Status)
		}
	})
}

// RecordObservation은 미확정 결과(PENDING/UNKNOWN)만 받는다. 분개를 만드는
// 코드가 없다 — ResolveTransfer와 함수를 나눈 이유 그 자체다.
//
// PROCESSING·terminal 둘 다 ResolveTransfer와 같은 행을 잠그므로 서로를
// 직렬화한다. 잠근 뒤 다시 읽지 않으면, 확정 직후의 조회가 끝난 요청의 조회
// 일정을 되살린다.
func (s *TransferService) RecordObservation(in ObservationInput) error {
	if in.Outcome.IsTerminal() {
		return fmt.Errorf("RecordObservation requires a non-terminal outcome, got %s", in.Outcome)
	}
	if in.TransferRequestID == 0 || in.EventKey == "" {
		return fmt.Errorf("transfer_request_id and event_key are required")
	}

	return s.DB.Transaction(func(tx *gorm.DB) error {
		transfers := s.Transfers.WithTx(tx)
		request, err := transfers.LockByID(in.TransferRequestID)
		if err != nil {
			return err
		}

		payload, err := encodeTransferPayload(in.Payload)
		if err != nil {
			return err
		}
		created, err := transfers.InsertEventIfAbsent(&model.TransferStatusEvent{
			TransferRequestID: request.ID,
			Source:            model.TransferEventSourcePoll,
			EventKey:          in.EventKey,
			Outcome:           in.Outcome,
			Payload:           payload,
			ReceivedAt:        s.now(),
		})
		if err != nil {
			return err
		}
		if !created {
			return nil
		}

		switch {
		case request.Status == model.TransferStatusProcessing:
			return s.updatePollSchedule(transfers, request, in.Outcome)

		case request.Status.IsTerminal():
			return nil // 사건만 남긴다. status·next_check_at·review 두 열 모두 불변.

		case request.Status == model.TransferStatusReceived:
			// 아직 조회 대상이 아니다 — 외부가 아니라 우리 쪽 호출 순서 오류다.
			return fmt.Errorf("RecordObservation called for transfer request %d that has not been dispatched yet", request.ID)

		default:
			return fmt.Errorf("transfer request %d has unexpected status %s", request.ID, request.Status)
		}
	})
}

func matchesTerminalOutcome(status model.TransferStatus, outcome model.TransferOutcome) bool {
	switch status {
	case model.TransferStatusCompleted:
		return outcome == model.TransferOutcomeSuccess
	case model.TransferStatusFailed:
		return outcome == model.TransferOutcomeFailure
	default:
		return false
	}
}

// confirmTerminal은 분개를 만들고 §4.5의 단일 UPDATE로 확정한다.
//
// failure_reason은 FAILURE에서만 채운다 — SUCCESS 확정에는 실패 사유가 있을 수
// 없으므로 항상 빈 문자열을 넘긴다.
func (s *TransferService) confirmTerminal(tx *gorm.DB, transfers *repository.TransferRepository, request *model.TransferRequest, outcome model.TransferOutcome, payload map[string]any) error {
	journalID, err := s.recordConfirmationJournal(tx, request, outcome)
	if err != nil {
		return err
	}

	status := model.TransferStatusFailed
	failureReason := ""
	if outcome == model.TransferOutcomeSuccess {
		status = model.TransferStatusCompleted
	} else {
		failureReason = failureReasonFromPayload(payload)
	}
	return transfers.ConfirmTerminal(request.ID, status, journalID, failureReason)
}

// failureReasonFromPayload는 콜백·조회가 보낸 payload["reason"]에서 실패 사유를
// 고른다. 문자열이 아니거나 없으면 빈 값이다 — 외부가 보낸 임의 값을 그대로
// 신뢰하지 않고, 저장할 형태(문자열)가 아니면 버린다.
func failureReasonFromPayload(payload map[string]any) string {
	reason, ok := payload["reason"].(string)
	if !ok {
		return ""
	}
	return reason
}

// recordConfirmationJournal은 방향×결과 조합별로 §5.2~5.4의 전기를 만든다.
// 입금 실패만 분개가 없다 — 잠글 것이 없었으므로 되돌릴 것도 없다.
func (s *TransferService) recordConfirmationJournal(tx *gorm.DB, request *model.TransferRequest, outcome model.TransferOutcome) (*uint, error) {
	if request.ExternalRef == nil {
		return nil, fmt.Errorf("transfer request %d is PROCESSING without an external_ref", request.ID)
	}
	ref := *request.ExternalRef
	owner := request.UserID
	externalAccountType := model.AccountExternalBank
	if request.Rail == model.TransferRailChain {
		externalAccountType = model.AccountExternalChain
	}

	switch {
	case request.Direction == model.TransferDirectionDeposit && outcome == model.TransferOutcomeSuccess:
		entry, _, err := s.Ledger.Record(tx, JournalInput{
			EventType:      model.JournalEventDeposit,
			IdempotencyKey: fmt.Sprintf("deposit-settle:%s", ref),
			ReferenceType:  model.JournalReferenceTransfer,
			ReferenceID:    request.ID,
			Postings: []PostingInput{
				{AccountType: externalAccountType, Asset: request.Asset, Amount: request.Amount.Neg()},
				{AccountType: model.AccountUserAvailable, OwnerUserID: &owner, Asset: request.Asset, Amount: request.Amount},
			},
		})
		if err != nil {
			return nil, err
		}
		return &entry.ID, nil

	case request.Direction == model.TransferDirectionDeposit && outcome == model.TransferOutcomeFailure:
		return nil, nil

	case request.Direction == model.TransferDirectionWithdrawal && outcome == model.TransferOutcomeSuccess:
		locked := request.Amount.Add(request.FeeAmount)
		postings := []PostingInput{
			{AccountType: model.AccountUserLocked, OwnerUserID: &owner, Asset: request.Asset, Amount: locked.Neg()},
			{AccountType: externalAccountType, Asset: request.Asset, Amount: request.Amount},
		}
		// 수수료 0짜리 FEE_INCOME 전기는 아무 사실도 기록하지 않으면서 분개만
		// 늘린다 — 수수료가 있을 때만 줄을 더한다.
		if request.FeeAmount.IsPositive() {
			postings = append(postings, PostingInput{
				AccountType: model.AccountFeeIncome, Asset: request.FeeAsset, Amount: request.FeeAmount,
			})
		}
		entry, _, err := s.Ledger.Record(tx, JournalInput{
			EventType:      model.JournalEventWithdrawal,
			IdempotencyKey: fmt.Sprintf("withdraw-settle:%s", ref),
			ReferenceType:  model.JournalReferenceTransfer,
			ReferenceID:    request.ID,
			Postings:       postings,
		})
		if err != nil {
			return nil, err
		}
		return &entry.ID, nil

	case request.Direction == model.TransferDirectionWithdrawal && outcome == model.TransferOutcomeFailure:
		locked := request.Amount.Add(request.FeeAmount)
		entry, _, err := s.Ledger.Record(tx, JournalInput{
			EventType:      model.JournalEventWithdrawal,
			IdempotencyKey: fmt.Sprintf("withdraw-refund:%s", ref),
			ReferenceType:  model.JournalReferenceTransfer,
			ReferenceID:    request.ID,
			Postings: []PostingInput{
				{AccountType: model.AccountUserLocked, OwnerUserID: &owner, Asset: request.Asset, Amount: locked.Neg()},
				{AccountType: model.AccountUserAvailable, OwnerUserID: &owner, Asset: request.Asset, Amount: locked},
			},
		})
		if err != nil {
			return nil, err
		}
		return &entry.ID, nil

	default:
		return nil, fmt.Errorf("unhandled direction/outcome combination: %s/%s", request.Direction, outcome)
	}
}

const (
	pollBaseInterval    = 10 * time.Second
	pollMaxInterval     = time.Hour
	pollReviewThreshold = 30 * time.Minute
)

// backoffInterval은 §8.6의 조회 간격이다: 10초 → 직전 간격×2 → 상한 1시간.
// attempts는 이번에 기록하는 시도 번호(1부터)다.
func backoffInterval(attempts int) time.Duration {
	if attempts <= 1 {
		return pollBaseInterval
	}
	interval := pollBaseInterval
	for i := 1; i < attempts; i++ {
		interval *= 2
		if interval >= pollMaxInterval {
			return pollMaxInterval
		}
	}
	return interval
}

// pollFailureKind는 조회가 확정에 이르지 못한 이유다. RecordObservation의 두
// outcome과 GetTransferStatus 자체의 실패를 같은 조회 일정 갱신 로직으로 다루기
// 위한 공통 분류다 — 같은 규칙을 두 곳에서 따로 구현하면 한쪽만 바뀐다.
type pollFailureKind int

const (
	pollFailureUnknown     pollFailureKind = iota // 외부가 UNKNOWN을 돌려줌 — 즉시 확인 필요
	pollFailurePending                            // 외부가 PENDING을 돌려줌 — 임계 넘으면 확인 필요
	pollFailureUnreachable                        // GetTransferStatus 자체가 실패 — 임계 넘으면 확인 필요
)

func reviewReasonForPollFailure(kind pollFailureKind, now time.Time, createdAt time.Time) *string {
	switch kind {
	case pollFailureUnknown:
		reason := model.ReviewReasonExternalUnknown
		return &reason
	case pollFailurePending:
		if now.Sub(createdAt) >= pollReviewThreshold {
			reason := model.ReviewReasonPendingTooLong
			return &reason
		}
	case pollFailureUnreachable:
		if now.Sub(createdAt) >= pollReviewThreshold {
			reason := model.ReviewReasonExternalUnreachable
			return &reason
		}
	}
	return nil
}

// advancePollSchedule은 §8.6의 조회 일정 갱신이다. 확인 표시를 지우지는 않는다 —
// 그것은 ConfirmTerminal만 하는 일이다.
func (s *TransferService) advancePollSchedule(transfers *repository.TransferRepository, request *model.TransferRequest, kind pollFailureKind) error {
	now := s.now()
	attempts := request.CheckAttempts + 1
	nextCheckAt := now.Add(backoffInterval(attempts))
	reviewReason := reviewReasonForPollFailure(kind, now, request.CreatedAt)
	return transfers.UpdatePollSchedule(request.ID, now, nextCheckAt, attempts, reviewReason)
}

// updatePollSchedule은 RecordObservation의 PROCESSING 분기가 쓴다.
func (s *TransferService) updatePollSchedule(transfers *repository.TransferRepository, request *model.TransferRequest, outcome model.TransferOutcome) error {
	kind := pollFailurePending
	if outcome == model.TransferOutcomeUnknown {
		kind = pollFailureUnknown
	}
	return s.advancePollSchedule(transfers, request, kind)
}

// HandleStatusCheckFailure는 GetTransferStatus 자체가 실패했을 때 쓴다(실제 외부
// 처리기는 진짜로 응답하지 않을 수 있다). 외부로부터 알게 된 사실이 없으므로
// transfer_status_events에는 아무것도 남기지 않는다 — §8.6 백오프대로 조회
// 일정만 전진시키고, 임계를 넘겼으면 확인 표시를 켠다. 분개는 절대 만들지
// 않는다 — 시간(이나 조회 실패)이 돈을 움직이지 않는다.
//
// RecordObservation과 같은 잠금 규율을 따른다(§8.9): 잠근 뒤 다시 읽은 상태가
// PROCESSING이 아니면 그사이 확정됐다는 뜻이므로 아무것도 하지 않는다.
func (s *TransferService) HandleStatusCheckFailure(transferRequestID uint) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		transfers := s.Transfers.WithTx(tx)
		request, err := transfers.LockByID(transferRequestID)
		if err != nil {
			return err
		}
		if request.Status != model.TransferStatusProcessing {
			return nil
		}
		return s.advancePollSchedule(transfers, request, pollFailureUnreachable)
	})
}

func normalizeTransferInput(userID uint, rail model.TransferRail, asset string, rawAmount string, rawClientKey string) (uint, model.TransferRail, string, decimal.Decimal, string, error) {
	if userID == 0 {
		return 0, "", "", decimal.Zero, "", NewValidationErrorf("user_id is required")
	}
	switch rail {
	case model.TransferRailBank, model.TransferRailChain:
	default:
		return 0, "", "", decimal.Zero, "", NewValidationErrorf("invalid rail")
	}
	normalizedAsset := normalizeCoinSymbol(asset)
	if normalizedAsset == "" {
		return 0, "", "", decimal.Zero, "", NewValidationErrorf("asset is required")
	}
	if rail == model.TransferRailBank && normalizedAsset != model.KRWAssetSymbol {
		return 0, "", "", decimal.Zero, "", NewValidationErrorf("bank rail requires KRW")
	}
	amount, err := parsePositiveDecimal(rawAmount, "amount")
	if err != nil {
		return 0, "", "", decimal.Zero, "", err
	}
	clientKey, err := normalizeIdempotencyKey(rawClientKey)
	if err != nil {
		return 0, "", "", decimal.Zero, "", NewValidationErrorf(
			"client_request_key is required and must be 1..%d characters", maxIdempotencyKeyLength)
	}
	return userID, rail, normalizedAsset, amount, clientKey, nil
}

func sameTransferBody(existing *model.TransferRequest, direction model.TransferDirection, rail model.TransferRail, asset string, amount decimal.Decimal) bool {
	return existing.Direction == direction &&
		existing.Rail == rail &&
		existing.Asset == asset &&
		existing.Amount.Equal(amount)
}

// transferPayloadAllowedFields는 저장을 허용하는 필드 목록이다. 목록 밖 필드는
// 버리고 이름과 개수만 로그에 남긴다 — 값은 남기지 않는다.
var transferPayloadAllowedFields = map[string]bool{
	"external_id":        true, // 외부 거래 식별자
	"status_code":        true, // 외부 상태 코드
	"reason":             true, // 실패 사유 문자열
	"amount":             true, // 외부가 보고한 금액
	"asset":              true, // 외부가 보고한 자산
	"external_timestamp": true, // 외부 타임스탬프
}

func encodeTransferPayload(payload map[string]any) ([]byte, error) {
	if len(payload) == 0 {
		return nil, nil
	}

	allowed := make(map[string]any, len(payload))
	var dropped []string
	for key, value := range payload {
		if transferPayloadAllowedFields[key] {
			allowed[key] = value
		} else {
			dropped = append(dropped, key)
		}
	}
	if len(dropped) > 0 {
		log.Printf("unknown transfer payload fields: count=%d names=[%s]", len(dropped), strings.Join(dropped, ", "))
	}
	if len(allowed) == 0 {
		return nil, nil
	}
	return json.Marshal(allowed)
}
