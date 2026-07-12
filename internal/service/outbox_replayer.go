package service

import (
	"fmt"
	"log"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
)

const defaultOutboxReplayPageSize = 500

type OutboxReplaySource interface {
	FindPendingAfter(afterID uint64, limit int) ([]model.TradeOutboxEvent, error)
	MarkProcessed(id uint64) error
}

// OutboxReplayer는 부팅 시 PENDING 이벤트를 ID 순으로 순차 재처리합니다.
// 순차라서 심볼별 FIFO(trade들 → MarketOrderDone)가 자명하게 보존되고,
// PENDING 잔량은 크래시 시점의 in-flight뿐이라 순차로 충분합니다.
// 라이브 파이프라인(부트스트랩·HTTP) 개시 전에 완료돼야 합니다.
type OutboxReplayer struct {
	Repo OutboxReplaySource
	// Process는 이벤트를 정산 파이프라인과 동일한 로직으로 처리하고,
	// 처리 결과가 내구적으로 확정됐는지(정산 성공/멱등 no-op/실패의 내구 기록)를
	// 반환합니다. false면 PENDING으로 남겨 다음 부팅이 다시 시도합니다.
	Process  func(event matching.ExecutionEvent) bool
	PageSize int
	Logger   *log.Logger
}

type OutboxReplayResult struct {
	Replayed  int // 처리 후 PROCESSED 마킹까지 끝난 이벤트
	Deferred  int // 내구 확정 실패로 PENDING 유지(다음 부팅 재시도)
	Corrupted int // 역직렬화 불가 — PROCESSED 마킹으로 격리(행은 포렌식용으로 남음)
}

func (r *OutboxReplayer) Replay() (OutboxReplayResult, error) {
	var result OutboxReplayResult
	if r.Repo == nil || r.Process == nil {
		return result, fmt.Errorf("outbox replayer requires repo and process func")
	}

	var afterID uint64
	for {
		rows, err := r.Repo.FindPendingAfter(afterID, r.pageSize())
		if err != nil {
			return result, fmt.Errorf("load pending outbox events: %w", err)
		}
		if len(rows) == 0 {
			return result, nil
		}
		for _, row := range rows {
			afterID = row.ID

			event, err := ExecutionEventFromOutbox(row)
			if err != nil {
				// 처리 불가능한 행을 PENDING으로 두면 매 부팅이 같은 곳에서 막힌다.
				// 마킹으로 격리하되 크게 남긴다 — 이 로그는 조사 대상이다.
				r.logf("outbox replay: CORRUPTED event %d isolated: %v", row.ID, err)
				result.Corrupted++
				if markErr := r.Repo.MarkProcessed(row.ID); markErr != nil {
					return result, fmt.Errorf("mark corrupted outbox event %d: %w", row.ID, markErr)
				}
				continue
			}

			if !r.Process(event) {
				// 정산 실패의 내구 기록조차 실패한 경우(DB 이상 등). PENDING으로 남겨
				// 다음 부팅이 재시도한다. 정산은 멱등·가환이라 뒤 이벤트를 계속
				// 처리해도 안전하다(A-1/A-2에서 확립된 재시도 순서 논거와 동일).
				r.logf("outbox replay: event %d not durably handled, left PENDING", row.ID)
				result.Deferred++
				continue
			}
			if err := r.Repo.MarkProcessed(row.ID); err != nil {
				// 마킹 실패는 유실이 아니라 다음 리플레이의 중복 처리(멱등 no-op)일 뿐.
				r.logf("outbox replay: mark event %d processed failed: %v", row.ID, err)
				result.Deferred++
				continue
			}
			result.Replayed++
		}
		if len(rows) < r.pageSize() {
			return result, nil
		}
	}
}

func (r *OutboxReplayer) pageSize() int {
	if r.PageSize > 0 {
		return r.PageSize
	}
	return defaultOutboxReplayPageSize
}

func (r *OutboxReplayer) logf(format string, args ...interface{}) {
	logger := r.Logger
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf(format, args...)
}
