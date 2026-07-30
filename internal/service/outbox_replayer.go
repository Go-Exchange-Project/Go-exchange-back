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
	// 반환합니다. false면 PENDING으로 남기고 이번 부팅의 replay를 즉시 중단합니다 —
	// 뒤 이벤트를 계속 처리하면 미정산 trade 위에서 terminal이 실행될 수 있습니다.
	// sourceOutboxID는 원본 행 ID로, 실패 기록의 provenance에 쓰입니다.
	Process  func(sourceOutboxID uint64, event matching.ExecutionEvent) bool
	PageSize int
	Logger   *log.Logger
}

type OutboxReplayResult struct {
	Replayed  int // 처리 후 PROCESSED 마킹까지 끝난 이벤트
	Deferred  int // 도메인 처리는 확정됐으나 MarkProcessed만 실패(다음 부팅이 멱등 재처리)
	Undurable int // 내구 확정 실패 — PENDING 유지, replay 중단
	Corrupted int // 역직렬화 불가 — PENDING 유지, replay 중단(마킹하지 않는다)
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
				// 처리할 수 없는 금융 이벤트를 PROCESSED로 선언하지 않는다 —
				// 마킹하면 이벤트가 영구 소실되고 뒤 terminal이 그 위에서 실행된다.
				// 부팅을 막고 운영자가 복구하게 한다(runbook 참조).
				r.logf("outbox replay: CORRUPTED event %d, replay aborted: %v", row.ID, err)
				result.Corrupted++
				return result, fmt.Errorf("corrupted outbox event %d: %w", row.ID, err)
			}

			if !r.Process(row.ID, event) {
				// 내구 확정 실패(정산 실패의 기록조차 실패). 뒤 이벤트를 계속 처리하면
				// 미정산 trade 위에서 terminal이 실행될 수 있으므로 즉시 중단한다.
				r.logf("outbox replay: event %d not durably handled, replay aborted", row.ID)
				result.Undurable++
				return result, fmt.Errorf("outbox event %d not durably handled", row.ID)
			}
			if err := r.Repo.MarkProcessed(row.ID); err != nil {
				// 정산은 커밋됐다 — dependency는 충족이므로 계속 진행한다.
				// 다음 리플레이가 멱등 재처리한다.
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
