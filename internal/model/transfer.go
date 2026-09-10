package model

import (
	"time"

	"github.com/shopspring/decimal"
)

// TransferDirection은 돈이 들어오는지 나가는지다.
type TransferDirection string

const (
	TransferDirectionDeposit    TransferDirection = "DEPOSIT"
	TransferDirectionWithdrawal TransferDirection = "WITHDRAWAL"
)

// TransferRail은 어느 바깥 세상을 쓰는지다.
type TransferRail string

const (
	TransferRailBank  TransferRail = "BANK"  // KRW
	TransferRailChain TransferRail = "CHAIN" // BTC·ETH
)

// TransferStatus는 돈의 처리 상태다. 운영자 확인 표시는 여기에 섞지 않는다
// (ReviewRequiredAt·ReviewReason 참고).
type TransferStatus string

const (
	TransferStatusReceived   TransferStatus = "RECEIVED"
	TransferStatusProcessing TransferStatus = "PROCESSING"
	TransferStatusCompleted  TransferStatus = "COMPLETED"
	TransferStatusFailed     TransferStatus = "FAILED"
)

// IsTerminal은 더 이상 돈이 움직이지 않는 상태인지 알려준다.
func (s TransferStatus) IsTerminal() bool {
	return s == TransferStatusCompleted || s == TransferStatusFailed
}

// TransferOutcome은 외부에서 알게 된 결과다.
type TransferOutcome string

const (
	TransferOutcomeSuccess TransferOutcome = "SUCCESS"
	TransferOutcomeFailure TransferOutcome = "FAILURE"
	TransferOutcomePending TransferOutcome = "PENDING"
	TransferOutcomeUnknown TransferOutcome = "UNKNOWN"
)

// IsTerminal은 돈을 움직여도 되는 확정 결과인지 알려준다.
//
// ResolveTransfer는 확정 결과만 받고, RecordObservation은 미확정만 받는다.
// 한 함수가 넷을 다 받으면 "미확정인데 확정 분기로 떨어지는" 실수가 가능해지고,
// 그 실수는 돈을 움직인다. 분기로 막지 않고 함수를 나눠서 막는다.
func (o TransferOutcome) IsTerminal() bool {
	return o == TransferOutcomeSuccess || o == TransferOutcomeFailure
}

// TransferEventSource는 그 사실을 어떻게 알게 됐는지다.
type TransferEventSource string

const (
	TransferEventSourceCallback TransferEventSource = "CALLBACK" // 외부가 알려줌
	TransferEventSourcePoll     TransferEventSource = "POLL"     // 우리가 물어봄
)

// 운영자 확인이 필요해진 사유.
const (
	// ReviewReasonExternalUnknown은 외부 조회가 UNKNOWN을 돌려준 경우다.
	ReviewReasonExternalUnknown = "EXTERNAL_UNKNOWN"
	// ReviewReasonExternalUnreachable은 외부가 응답하지 않는 경우다.
	ReviewReasonExternalUnreachable = "EXTERNAL_UNREACHABLE"
	// ReviewReasonPendingTooLong은 PENDING이 임계 시간을 넘긴 경우다.
	ReviewReasonPendingTooLong = "PENDING_TOO_LONG"
	// ReviewReasonConflictingTerminalOutcome은 확정 뒤 반대 결과가 온 경우다.
	// 돈은 그대로 두고 사람을 부른다 — 어느 쪽이 참인지 우리는 모른다.
	ReviewReasonConflictingTerminalOutcome = "CONFLICTING_TERMINAL_OUTCOME"
	// ReviewReasonTerminalBeforeDispatch는 보낸 적 없는데 결과가 온 경우다.
	// 우리 인식과 외부 현실이 어긋났다는 신호다.
	ReviewReasonTerminalBeforeDispatch = "TERMINAL_BEFORE_DISPATCH"
)

// TransferRequest는 가짜 입출금 요청 하나다.
//
// 돈의 처리 상태(Status)와 운영자 확인 표시(ReviewRequiredAt·ReviewReason)를
// 나눠 둔다. 한 열에 섞으면 "확인 필요"가 처리 종료 상태가 되고, 담당자가 놓친
// 출금은 영원히 잠긴 채 남는다. 나눠 두면 확인 표시가 켜져 있어도 자동 조회는
// 계속 돌고, 외부 시스템이 복구되는 순간 정상 경로로 마무리된다.
//
// HoldJournalID는 출금 접수 트랜잭션 안에서 NULL로 INSERT된 뒤 UPDATE로 채워진다.
// 요청 id가 있어야 잠금 분개를 만들 수 있고, 잠금 분개가 있어야 이 열을 채울 수
// 있기 때문이다. 즉시 CHECK로는 이 순서를 표현할 수 없으므로 migration 009가
// DEFERRABLE constraint trigger로 커밋 시점의 최종 행만 검사한다.
type TransferRequest struct {
	ID        uint              `gorm:"primaryKey"`
	UserID    uint              `gorm:"not null;uniqueIndex:idx_transfer_requests_user_request_key,priority:1"`
	Direction TransferDirection `gorm:"size:16;not null"`
	Rail      TransferRail      `gorm:"size:16;not null"`
	Asset     string            `gorm:"size:16;not null"`
	Amount    decimal.Decimal   `gorm:"type:numeric;not null"`
	// FeeAmount는 접수 시점에 확정해 저장한다. 처리 도중 다시 계산하면 그사이
	// 정책이 바뀌었을 때 잠근 금액과 최종 차감액이 달라지고, 그 차액이 곧
	// 사라지거나 남아도는 돈이다. 첫 구현에서는 항상 0이다.
	FeeAmount decimal.Decimal `gorm:"type:numeric;not null;default:0"`
	FeeAsset  string          `gorm:"size:16;not null"`
	Status    TransferStatus  `gorm:"size:16;not null"`
	// ClientRequestKey는 같은 버튼을 두 번 누르는 것을 막는다. 접수는 항상
	// 이 키를 먼저 선점한다 — 키보다 잠금이 먼저면 같은 요청이 두 번 잠근다.
	ClientRequestKey string `gorm:"size:128;not null;uniqueIndex:idx_transfer_requests_user_request_key,priority:2"`
	// ExternalRef는 외부 제출 전(RECEIVED)에는 NULL이다. 아직 외부 거래번호가
	// 없기 때문이다. UNIQUE는 NULL이 아닌 값에만 걸리는 부분 인덱스로 009가 만든다.
	ExternalRef *string `gorm:"size:128"`
	// ResolutionJournalID는 확정을 만든 분개다. 완료 분개와 출금 실패 반환
	// 분개를 둘 다 이 열로 연결한다.
	ResolutionJournalID *uint
	HoldJournalID       *uint
	LastCheckedAt       *time.Time
	NextCheckAt         *time.Time
	CheckAttempts       int `gorm:"not null;default:0"`
	// ReviewRequiredAt은 사람을 부르는 깃발이지 처리를 멈추는 스위치가 아니다.
	// 이 값이 켜져 있어도 상태 조회는 계속된다.
	//
	// ReviewReason이 포인터인 이유: 깃발과 사유는 항상 함께 움직여야 하는데,
	// 값 타입이면 "깃발은 없는데 사유는 빈 문자열"과 "깃발도 사유도 없음"을
	// DB에서 구분할 수 없다. 둘 다 NULL이거나 둘 다 값이어야 한다.
	ReviewRequiredAt *time.Time
	ReviewReason     *string `gorm:"size:64"`
	FailureReason    string
	CreatedAt        time.Time `gorm:"not null"`
	UpdatedAt        time.Time `gorm:"not null"`
}

// TransferStatusEvent는 외부 상태를 한 번 알게 된 사건이다.
//
// 알림으로 알았든 우리가 조회해서 알았든 같은 종류의 사건이므로 표 하나에 담고
// Source로 구분한다. 표를 나누면 운영자가 두 곳을 번갈아 보며 시간순으로
// 맞춰야 한다.
//
// 요청과의 연결은 TransferRequestID FK 하나뿐이다. external_ref를 여기에도
// 적으면 언젠가 두 값이 달라진다 — Wallet.KRW가 정확히 그 문제였다.
type TransferStatusEvent struct {
	ID                uint64              `gorm:"primaryKey"`
	TransferRequestID uint                `gorm:"not null"`
	Source            TransferEventSource `gorm:"size:16;not null"`
	// EventKey는 CALLBACK이면 "callback:{rail}:{외부 event id}",
	// POLL이면 "poll:{transfer_request_id}:{check_attempts}"다.
	// rail을 붙이는 것은 가짜 은행과 가짜 체인이 각자 발급한 id가 겹칠 수 있어서다.
	EventKey string          `gorm:"size:160;not null;uniqueIndex:idx_transfer_status_events_event_key"`
	Outcome  TransferOutcome `gorm:"size:16;not null"`
	// Payload는 허용 목록에 있는 필드만 담는다. 외부가 보낸 것을 통째로 저장하면
	// 예상하지 못한 내용이 원장 옆 표에 그대로 쌓인다.
	Payload    []byte    `gorm:"type:jsonb"`
	ReceivedAt time.Time `gorm:"not null"`
}
