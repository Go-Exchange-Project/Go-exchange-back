package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
)

const (
	// 21번 벤치마크: 상한 64가 포화(평균 54.4건/flush)돼 write-ahead 관문이
	// 파이프라인을 캡했다. 512는 왕복·fsync 횟수를 1/8로 줄인다(파라미터
	// ~3.6k « Postgres 한도 65,535). 운영 조정은 GOEXCHANGE_OUTBOX_BATCH_SIZE.
	defaultOutboxBatchSize      = 512
	defaultOutboxFlushInterval  = 5 * time.Millisecond
	defaultOutboxRetryBaseDelay = 50 * time.Millisecond
	maxOutboxRetryDelay         = time.Second
)

// OutboxEvent는 outbox에 커밋된 실행 이벤트입니다. OutboxID는 정산 완료 후
// PROCESSED 마킹에 쓰입니다.
type OutboxEvent struct {
	OutboxID uint64
	Event    matching.ExecutionEvent
}

type outboxBatchInserter interface {
	InsertBatch(events []*model.TradeOutboxEvent) error
}

// OutboxWriter는 엔진 ExecutionCh의 유일한 소비자로, 이벤트를 배치로 모아
// 한 트랜잭션에 커밋(group commit)한 뒤에만 정산 파이프라인에 전달합니다.
// "정산은 outbox에 커밋된 이벤트만 처리한다"는 write-ahead 불변식의 관문입니다.
type OutboxWriter struct {
	Repo    outboxBatchInserter
	Source  <-chan matching.ExecutionEvent
	Forward func(OutboxEvent)

	BatchSize      int           // 기본 64
	FlushInterval  time.Duration // 기본 5ms
	RetryBaseDelay time.Duration // 기본 50ms (테스트 단축용)
	Logger         *log.Logger
}

// Run은 Source가 닫힐 때까지 블로킹하며, 닫힌 뒤 잔여 배치까지 flush하고 반환합니다.
// 반환 시점 이후에는 어떤 이벤트도 미커밋 상태로 남지 않습니다(graceful shutdown 보장).
func (w *OutboxWriter) Run() {
	for {
		event, ok := <-w.Source
		if !ok {
			return
		}
		batch, open := w.collectBatch([]matching.ExecutionEvent{event})
		w.flushAndForward(batch)
		if !open {
			return
		}
	}
}

func (w *OutboxWriter) collectBatch(batch []matching.ExecutionEvent) ([]matching.ExecutionEvent, bool) {
	timer := time.NewTimer(w.flushInterval())
	defer timer.Stop()

	for len(batch) < w.batchSize() {
		select {
		case event, ok := <-w.Source:
			if !ok {
				return batch, false
			}
			batch = append(batch, event)
		case <-timer.C:
			return batch, true
		}
	}
	return batch, true
}

// flushAndForward는 배치를 커밋될 때까지 무한 재시도합니다. 이 동안 ExecutionCh가
// 차면 엔진 매칭이 블록되는데, 이는 의도된 백프레셔입니다 — DB가 죽었는데 매칭만
// 계속되면 유실 대기 이벤트가 메모리에 무한 적체됩니다.
func (w *OutboxWriter) flushAndForward(events []matching.ExecutionEvent) {
	rows := make([]*model.TradeOutboxEvent, 0, len(events))
	forwarded := make([]matching.ExecutionEvent, 0, len(events))
	for _, event := range events {
		row, err := NewTradeOutboxEvent(event)
		if err != nil {
			w.logf("outbox: drop unencodable execution event: %v", err)
			continue
		}
		rows = append(rows, row)
		forwarded = append(forwarded, event)
	}
	if len(rows) == 0 {
		return
	}

	delay := w.retryBaseDelay()
	for {
		start := time.Now()
		err := w.Repo.InsertBatch(rows)
		if err == nil {
			metrics.TradeOutboxFlushDuration.Observe(time.Since(start).Seconds())
			metrics.TradeOutboxFlushBatchSize.Observe(float64(len(rows)))
			break
		}
		metrics.TradeOutboxWriteErrorsTotal.Inc()
		w.logf("outbox: batch insert of %d events failed (retrying in %s): %v", len(rows), delay, err)
		time.Sleep(delay)
		delay *= 2
		if delay > maxOutboxRetryDelay {
			delay = maxOutboxRetryDelay
		}
	}

	if w.Forward == nil {
		return
	}
	for i, row := range rows {
		w.Forward(OutboxEvent{OutboxID: row.ID, Event: forwarded[i]})
	}
}

// NewTradeOutboxEvent는 실행 이벤트를 outbox 행으로 직렬화합니다.
func NewTradeOutboxEvent(event matching.ExecutionEvent) (*model.TradeOutboxEvent, error) {
	switch {
	case event.Trade != nil:
		payload, err := json.Marshal(event.Trade)
		if err != nil {
			return nil, fmt.Errorf("marshal trade outbox payload: %w", err)
		}
		return &model.TradeOutboxEvent{
			EventType:     model.TradeOutboxEventTypeTrade,
			CoinSymbol:    event.Trade.CoinSymbol,
			EngineEventID: event.Trade.EngineEventID,
			Payload:       payload,
			Status:        model.TradeOutboxStatusPending,
		}, nil
	case event.MarketOrderDone != nil:
		payload, err := json.Marshal(event.MarketOrderDone)
		if err != nil {
			return nil, fmt.Errorf("marshal market order done outbox payload: %w", err)
		}
		return &model.TradeOutboxEvent{
			EventType:  model.TradeOutboxEventTypeMarketOrderDone,
			CoinSymbol: event.MarketOrderDone.CoinSymbol,
			Payload:    payload,
			Status:     model.TradeOutboxStatusPending,
		}, nil
	case event.OrderCancelled != nil:
		payload, err := json.Marshal(event.OrderCancelled)
		if err != nil {
			return nil, fmt.Errorf("marshal order cancelled outbox payload: %w", err)
		}
		return &model.TradeOutboxEvent{
			EventType:     model.TradeOutboxEventTypeOrderCancelled,
			CoinSymbol:    event.OrderCancelled.CoinSymbol,
			EngineEventID: event.OrderCancelled.EngineEventID,
			Payload:       payload,
			Status:        model.TradeOutboxStatusPending,
		}, nil
	default:
		return nil, errors.New("execution event has neither trade nor market order done")
	}
}

// ExecutionEventFromOutbox는 outbox 행을 실행 이벤트로 복원합니다(리플레이용).
func ExecutionEventFromOutbox(row model.TradeOutboxEvent) (matching.ExecutionEvent, error) {
	switch row.EventType {
	case model.TradeOutboxEventTypeTrade:
		var trade model.Trade
		if err := json.Unmarshal(row.Payload, &trade); err != nil {
			return matching.ExecutionEvent{}, fmt.Errorf("unmarshal trade outbox payload %d: %w", row.ID, err)
		}
		return matching.ExecutionEvent{Trade: &trade}, nil
	case model.TradeOutboxEventTypeMarketOrderDone:
		var done matching.MarketOrderDone
		if err := json.Unmarshal(row.Payload, &done); err != nil {
			return matching.ExecutionEvent{}, fmt.Errorf("unmarshal market order done outbox payload %d: %w", row.ID, err)
		}
		return matching.ExecutionEvent{MarketOrderDone: &done}, nil
	case model.TradeOutboxEventTypeOrderCancelled:
		var cancelled matching.OrderCancelled
		if err := json.Unmarshal(row.Payload, &cancelled); err != nil {
			return matching.ExecutionEvent{}, fmt.Errorf("unmarshal order cancelled outbox payload %d: %w", row.ID, err)
		}
		return matching.ExecutionEvent{OrderCancelled: &cancelled}, nil
	default:
		return matching.ExecutionEvent{}, fmt.Errorf("unknown trade outbox event type %q (id %d)", row.EventType, row.ID)
	}
}

func (w *OutboxWriter) batchSize() int {
	if w.BatchSize > 0 {
		return w.BatchSize
	}
	return defaultOutboxBatchSize
}

func (w *OutboxWriter) flushInterval() time.Duration {
	if w.FlushInterval > 0 {
		return w.FlushInterval
	}
	return defaultOutboxFlushInterval
}

func (w *OutboxWriter) retryBaseDelay() time.Duration {
	if w.RetryBaseDelay > 0 {
		return w.RetryBaseDelay
	}
	return defaultOutboxRetryBaseDelay
}

func (w *OutboxWriter) logf(format string, args ...interface{}) {
	logger := w.Logger
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf(format, args...)
}
