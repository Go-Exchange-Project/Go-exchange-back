package service

// 고정 부하 정산 비용 상승 진단 하니스 — 실행 러너와 수집.
//
// 세 종류(trade batch · 취소 terminal · 시장가 완료 terminal)를 **각각 별도로** 잰다.
// 합산 평균은 32-B가 이미 갖고 있고, 그것이 부족해서 하는 진단이다.
//
// ⚠ ProcessOrderCancellation · CompleteMarketOrder는 이미 종결된 주문이면 멱등 no-op이다.
// no-op을 재면 아주 빠른 숫자가 나오지만 아무것도 재지 않은 것이다. diagVerifyJobs가
// 그것을 잡는다 — 게이트가 실제로 실패하는지는 음성 테스트로 고정한다.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const diagResultDir = "../../_workspace/settlement-cost-diagnostic"

// ---------------------------------------------------------------------------
// 결과 스키마
// ---------------------------------------------------------------------------

type diagOpStats struct {
	Count      int            `json:"count"`
	OpsPerSec  float64        `json:"ops_per_sec"`
	P50Ms      float64        `json:"p50_ms"`
	P95Ms      float64        `json:"p95_ms"`
	P99Ms      float64        `json:"p99_ms"`
	MaxMs      float64        `json:"max_ms"`
	BatchSizes map[string]int `json:"batch_sizes,omitempty"` // trade_batch 전용: 전부 "3"이어야 한다
}

type diagWorkloadMeta struct {
	WarmupJobs        int     `json:"warmup_jobs"`
	MeasuredJobs      int     `json:"measured_jobs"`
	BatchSize         int     `json:"batch_size"`
	TerminalRatio     float64 `json:"terminal_ratio"`
	TerminalSplitNote string  `json:"terminal_split_note"`
	Seed              int     `json:"seed"`
	Users             int     `json:"users"`
	MaxInFlight       int64   `json:"max_in_flight"`
}

type diagStatement struct {
	Query       string  `json:"query"`
	Calls       int64   `json:"calls"`
	TotalMs     float64 `json:"total_exec_ms"`
	MeanMs      float64 `json:"mean_exec_ms"`
	Rows        int64   `json:"rows"`
	SharedHit   int64   `json:"shared_blks_hit"`
	SharedRead  int64   `json:"shared_blks_read"`
	WALBytes    int64   `json:"wal_bytes"`
	WALRecords  int64   `json:"wal_records"`
	BlkReadMs   float64 `json:"blk_read_ms"`
	BlkWriteMs  float64 `json:"blk_write_ms"`
	TempWritten int64   `json:"temp_blks_written"`
}

type diagTableStat struct {
	Table       string `json:"table"`
	SeqScan     int64  `json:"seq_scan_delta"`
	SeqTupRead  int64  `json:"seq_tup_read_delta"`
	IdxScan     int64  `json:"idx_scan_delta"`
	NLiveTup    int64  `json:"n_live_tup"`
	NDeadTup    int64  `json:"n_dead_tup"`
	AutoVacuums int64  `json:"autovacuum_count"`
	TotalBytes  int64  `json:"total_bytes"`
	IndexBytes  int64  `json:"index_bytes"`
}

type diagIndexStat struct {
	Table   string `json:"table"`
	Index   string `json:"index"`
	IdxScan int64  `json:"idx_scan_delta"`
	Bytes   int64  `json:"bytes"`
	Def     string `json:"definition"`
}

type diagDBStats struct {
	WALBytes       int64            `json:"wal_bytes"`
	BlksHit        int64            `json:"blks_hit_delta"`
	BlksRead       int64            `json:"blks_read_delta"`
	Deadlocks      int64            `json:"deadlocks_delta"`
	XactCommit     int64            `json:"xact_commit_delta"`
	XactRollback   int64            `json:"xact_rollback_delta"`
	TempBytes      int64            `json:"temp_bytes_delta"`
	LockWaitMax    int              `json:"lock_wait_max"`
	LockWaitMean   float64          `json:"lock_wait_mean"`
	LockWaitSample int              `json:"lock_wait_samples"`
	DBExecMs       float64          `json:"db_exec_ms_total"`
	CPUPercent     *float64         `json:"db_cpu_percent"` // 로컬 선별 실험에서는 수집하지 않는다(아래 note)
	CPUNote        string           `json:"db_cpu_note"`
	TopStatements  []diagStatement  `json:"top_statements"`
	Tables         []diagTableStat  `json:"tables"`
	Indexes        []diagIndexStat  `json:"indexes"`
	Checkpoints    map[string]int64 `json:"checkpointer"`
	Explain        []diagExplainRow `json:"explain"`
	SeqScanQueries []string         `json:"seq_scan_queries"` // 비어 있으면 "없음"
	SeqScanNote    string           `json:"seq_scan_note"`
}

// diagSeqScanNote — 작은 테이블에서는 플래너가 인덱스보다 seq scan을 고르는 것이 정상이다.
// "초기" 크기의 seq_scan=true를 missing index로 읽으면 안 된다. **32-B 종료 규모 셀의
// 목록만 판정에 쓴다.**
const diagSeqScanNote = "작은 테이블에서는 플래너가 seq scan을 고르는 것이 정상이다. " +
	"이 목록은 32-B 종료 규모(full) 셀에서만 판정에 쓴다 — 초기·중간 크기의 seq scan은 missing index 근거가 아니다."

// diagExplainRow는 정산 경로 읽기 쿼리 1건의 실행 계획이다.
// Phase 6의 산출물은 "seq scan이 뜨는 정산 쿼리 목록"이고, SeqScan이 그 판정값이다.
type diagExplainRow struct {
	Name    string `json:"name"`
	SQL     string `json:"sql"`
	SeqScan bool   `json:"seq_scan"`
	Plan    string `json:"plan"`
}

type diagIntegrity struct {
	Violations               []string `json:"violations"`
	FailedSettlements        int64    `json:"failed_settlements"`
	FailedOrderCancellations int64    `json:"failed_order_cancellations"`
	FailedMarketCompletions  int64    `json:"failed_market_completions"`
	ReconciliationViolations int64    `json:"reconciliation_violations"`
}

type diagCellResult struct {
	Schema      string                 `json:"schema"`
	Cell        string                 `json:"cell"`
	Scale       string                 `json:"scale"`
	Size        diagSize               `json:"size"`
	RowCounts   diagRowCounts          `json:"row_counts"`
	Concurrency int                    `json:"concurrency"`
	StartedAt   string                 `json:"started_at_utc"`
	ElapsedSec  float64                `json:"measured_elapsed_sec"`
	Workload    diagWorkloadMeta       `json:"workload"`
	Ops         map[string]diagOpStats `json:"ops"`
	DB          diagDBStats            `json:"db"`
	Integrity   diagIntegrity          `json:"integrity"`
	Env         map[string]string      `json:"env"`
}

// ---------------------------------------------------------------------------
// 실행
// ---------------------------------------------------------------------------

type diagSample struct {
	kind      diagJobKind
	seconds   float64
	batchSize int
}

// diagRunJobs는 job들을 concurrency개 워커로 실행하고 종류별 소요시간을 모은다.
// **측정 구간에는 production 함수 호출만 넣는다** — 검증 쿼리를 사이에 끼우면 그 부하가
// 측정 대상 DB 경합에 섞인다. 검증은 실행 전후 일괄로 수행한다(각 반복을 개별 검증한다).
func diagRunJobs(t testing.TB, settlement *SettlementService, orderSvc *OrderService,
	jobs []diagJob, concurrency int, record bool) ([]diagSample, int64) {
	t.Helper()

	queue := make(chan diagJob)
	samples := make([][]diagSample, concurrency)
	errs := make([]error, concurrency)
	var inFlight, maxInFlight int64

	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for job := range queue {
				cur := atomic.AddInt64(&inFlight, 1)
				for {
					old := atomic.LoadInt64(&maxInFlight)
					if cur <= old || atomic.CompareAndSwapInt64(&maxInFlight, old, cur) {
						break
					}
				}
				started := time.Now()
				err := diagExecuteJob(settlement, orderSvc, job)
				elapsed := time.Since(started).Seconds()
				atomic.AddInt64(&inFlight, -1)
				if err != nil {
					if errs[w] == nil {
						errs[w] = fmt.Errorf("job %d(%s): %w", job.Index, job.Kind, err)
					}
					continue
				}
				if record {
					samples[w] = append(samples[w], diagSample{
						kind: job.Kind, seconds: elapsed, batchSize: len(job.Trades),
					})
				}
			}
		}(w)
	}
	for _, job := range jobs {
		queue <- job
	}
	close(queue)
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err, "production 함수가 실패했다 — 성능 수치를 읽지 않는다")
	}
	out := make([]diagSample, 0, len(jobs))
	for _, s := range samples {
		out = append(out, s...)
	}
	return out, atomic.LoadInt64(&maxInFlight)
}

// diagExecuteJob은 production 함수를 그대로 호출한다. mock·래퍼를 두지 않는다.
func diagExecuteJob(settlement *SettlementService, orderSvc *OrderService, job diagJob) error {
	switch job.Kind {
	case diagJobTradeBatch:
		items := make([]TradeBatchItem, len(job.Trades))
		for i, spec := range job.Trades {
			items[i] = TradeBatchItem{Trade: &model.Trade{
				// EngineEventID가 있으면 idempotency key는 "engine:<id>"가 된다.
				// fixture 체결은 각각 1회만 쓰이므로 매번 새 키다.
				EngineEventID:  fmt.Sprintf("diag-%d-%d", job.Index, i),
				EngineSequence: int64(job.Index*diagBatchSize + i),
				CoinSymbol:     diagCoin,
				Price:          spec.Price,
				Quantity:       spec.Quantity,
				TradedAt:       time.Now().UTC(),
				BuyOrderID:     spec.BuyOrderID,
				SellOrderID:    spec.SellOrderID,
			}}
		}
		results, err := settlement.SettleTradeBatch(items)
		if err != nil {
			return err
		}
		for i, r := range results {
			if !r.Applied || r.Duplicate {
				return fmt.Errorf("체결 %d가 실제로 적용되지 않았다(applied=%v duplicate=%v)", i, r.Applied, r.Duplicate)
			}
		}
		return nil

	case diagJobCancelTerminal:
		return orderSvc.ProcessOrderCancellation(matching.OrderCancelled{
			OrderID: job.OrderID, CoinSymbol: diagCoin, Side: model.OrderSideBuy,
		})

	case diagJobMarketTerminal:
		return orderSvc.CompleteMarketOrder(CompleteMarketOrderInput{
			OrderID:              job.OrderID,
			FilledAmount:         job.FilledAmount,
			FilledQuoteAmount:    job.FilledQuoteAmount,
			RemainingQuoteAmount: job.RemainingQuoteAmount,
		})
	}
	return fmt.Errorf("알 수 없는 job 종류 %q", job.Kind)
}

// ---------------------------------------------------------------------------
// Phase 3 — terminal 실작업 검증 게이트
// ---------------------------------------------------------------------------

type diagOrderRow struct {
	ID                uint
	UserID            uint
	Status            string
	OrderType         string
	Amount            decimal.Decimal
	FilledAmount      decimal.Decimal
	QuoteAmount       decimal.Decimal
	FilledQuoteAmount decimal.Decimal
	Price             decimal.Decimal
}

func diagLoadOrders(t testing.TB, db *gorm.DB, ids []uint) map[uint]diagOrderRow {
	t.Helper()

	var rows []diagOrderRow
	require.NoError(t, db.Raw(`SELECT id, user_id, status, order_type, amount, filled_amount,
        quote_amount, filled_quote_amount, price FROM orders WHERE id IN ?`, ids).Scan(&rows).Error)
	out := make(map[uint]diagOrderRow, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

func diagJobOrderIDs(jobs []diagJob) []uint {
	ids := make([]uint, 0, len(jobs)*2*diagBatchSize)
	for _, job := range jobs {
		if job.Kind == diagJobTradeBatch {
			for _, s := range job.Trades {
				ids = append(ids, s.BuyOrderID, s.SellOrderID)
			}
			continue
		}
		ids = append(ids, job.OrderID)
	}
	return ids
}

// diagVerifyPreconditions는 실행 **전에** 모든 반복의 시작 상태를 확인한다.
// 여기가 통과하지 못하면 뒤이은 측정은 no-op을 재는 것이므로 실행 자체를 하지 않는다.
func diagVerifyPreconditions(t testing.TB, db *gorm.DB, jobs []diagJob) []string {
	t.Helper()

	orders := diagLoadOrders(t, db, diagJobOrderIDs(jobs))
	locked := diagLockedKRW(t, db)
	var v []string

	for _, job := range jobs {
		switch job.Kind {
		case diagJobTradeBatch:
			for _, s := range job.Trades {
				for _, id := range []uint{s.BuyOrderID, s.SellOrderID} {
					o, ok := orders[id]
					if !ok {
						v = append(v, fmt.Sprintf("job %d: 주문 %d가 없다", job.Index, id))
						continue
					}
					if o.Status != string(model.OrderStatusPending) && o.Status != string(model.OrderStatusPartial) {
						v = append(v, fmt.Sprintf("job %d: 주문 %d 시작 상태가 %s (PENDING/PARTIAL이어야 한다)", job.Index, id, o.Status))
					}
				}
			}

		case diagJobCancelTerminal:
			o, ok := orders[job.OrderID]
			if !ok {
				v = append(v, fmt.Sprintf("job %d: 주문 %d가 없다", job.Index, job.OrderID))
				continue
			}
			if o.Status != string(model.OrderStatusPending) && o.Status != string(model.OrderStatusPartial) {
				v = append(v, fmt.Sprintf("job %d: 취소 대상 주문 %d 상태가 %s — no-op이 된다", job.Index, job.OrderID, o.Status))
			}
			remaining := o.Amount.Sub(o.FilledAmount)
			if !remaining.GreaterThan(decimal.Zero) {
				v = append(v, fmt.Sprintf("job %d: 취소 대상 주문 %d의 잔여가 %s — no-op이 된다", job.Index, job.OrderID, remaining))
			}
			need := quoteAmountWithTradingFee(o.Price.Mul(remaining))
			if !locked[o.UserID].GreaterThanOrEqual(need) {
				v = append(v, fmt.Sprintf("job %d: 사용자 %d의 locked KRW %s < 해제 필요 %s", job.Index, o.UserID, locked[o.UserID], need))
			}

		case diagJobMarketTerminal:
			o, ok := orders[job.OrderID]
			if !ok {
				v = append(v, fmt.Sprintf("job %d: 주문 %d가 없다", job.Index, job.OrderID))
				continue
			}
			if o.OrderType != string(model.OrderTypeMarket) {
				v = append(v, fmt.Sprintf("job %d: 주문 %d가 시장가가 아니다(%s)", job.Index, job.OrderID, o.OrderType))
			}
			if o.Status != string(model.OrderStatusPending) && o.Status != string(model.OrderStatusPartial) {
				v = append(v, fmt.Sprintf("job %d: 완료 대상 주문 %d 상태가 %s — no-op이 된다", job.Index, job.OrderID, o.Status))
			}
			remainingQuote := o.QuoteAmount.Sub(o.FilledQuoteAmount)
			if !remainingQuote.GreaterThan(decimal.Zero) {
				v = append(v, fmt.Sprintf("job %d: 주문 %d의 잔여 예산이 %s — 원장 기록 없는 no-op이 된다", job.Index, job.OrderID, remainingQuote))
			}
			if !locked[o.UserID].GreaterThanOrEqual(remainingQuote) {
				v = append(v, fmt.Sprintf("job %d: 사용자 %d의 locked KRW %s < 해제 필요 %s", job.Index, o.UserID, locked[o.UserID], remainingQuote))
			}
		}
	}
	return v
}

// diagVerifyEffects는 실행 **후에** 모든 반복이 실제로 일을 했는지 확인한다.
// terminal의 no-op 판별 기준은 **ORDER_RELEASE 원장 행이 정확히 1건 생겼는가**다 —
// 상태만 보면 "원래 CANCELLED였던 주문"과 구분되지 않는다.
func diagVerifyEffects(t testing.TB, db *gorm.DB, jobs []diagJob) []string {
	t.Helper()

	orders := diagLoadOrders(t, db, diagJobOrderIDs(jobs))
	releases := diagReleaseLedgerCounts(t, db)
	trades := diagTradeKeysByOrder(t, db)
	var v []string

	for _, job := range jobs {
		switch job.Kind {
		case diagJobTradeBatch:
			for i, s := range job.Trades {
				key := fmt.Sprintf("engine:diag-%d-%d", job.Index, i)
				if trades[key] == 0 {
					v = append(v, fmt.Sprintf("job %d: 체결 %s가 커밋되지 않았다", job.Index, key))
				}
				if o, ok := orders[s.BuyOrderID]; ok && !o.FilledAmount.GreaterThan(decimal.Zero) {
					v = append(v, fmt.Sprintf("job %d: 매수 주문 %d의 체결 수량이 그대로 0이다", job.Index, s.BuyOrderID))
				}
			}

		case diagJobCancelTerminal:
			o := orders[job.OrderID]
			if o.Status != string(model.OrderStatusCancelled) {
				v = append(v, fmt.Sprintf("job %d: 취소 후 주문 %d 상태가 %s", job.Index, job.OrderID, o.Status))
			}
			if n := releases[job.OrderID]; n != 1 {
				v = append(v, fmt.Sprintf("job %d: 주문 %d의 ORDER_RELEASE 원장이 %d건 (%s)",
					job.Index, job.OrderID, n, diagReleaseCountReason(n)))
			}

		case diagJobMarketTerminal:
			o := orders[job.OrderID]
			if o.Status != string(model.OrderStatusFilled) {
				v = append(v, fmt.Sprintf("job %d: 완료 후 주문 %d 상태가 %s", job.Index, job.OrderID, o.Status))
			}
			if n := releases[job.OrderID]; n != 1 {
				v = append(v, fmt.Sprintf("job %d: 주문 %d의 ORDER_RELEASE 원장이 %d건 (%s)",
					job.Index, job.OrderID, n, diagReleaseCountReason(n)))
			}
		}
	}
	return v
}

// diagReleaseCountReason은 0건과 2건 이상을 구분한다. 둘 다 게이트 실패지만 원인이 다르다 —
// 0건은 멱등 no-op(측정이 무의미), 2건 이상은 중복 해제(정합성 문제)다.
func diagReleaseCountReason(n int) string {
	if n == 0 {
		return "no-op이었다 — 아무것도 재지 않았다"
	}
	return "중복 해제이거나 시드 행이 섞였다"
}

func diagLockedKRW(t testing.TB, db *gorm.DB) map[uint]decimal.Decimal {
	t.Helper()

	var rows []struct {
		UserID        uint
		LockedBalance decimal.Decimal
	}
	require.NoError(t, db.Raw(`SELECT user_id, locked_balance FROM wallets
        WHERE coin_symbol = 'KRW' AND user_id > ?`, diagUserIDBase).Scan(&rows).Error)
	out := make(map[uint]decimal.Decimal, len(rows))
	for _, r := range rows {
		out[r.UserID] = r.LockedBalance
	}
	return out
}

func diagReleaseLedgerCounts(t testing.TB, db *gorm.DB) map[uint]int {
	t.Helper()

	var rows []struct {
		ReferenceID uint
		N           int
	}
	// reference_key가 비어 있는 행만 센다 — 시드가 만든 행(diag-fixture-* / diag-hist-*)은
	// 이번 실행의 산출물이 아니다. production 경로는 ReferenceKey를 빈 문자열로 남긴다.
	require.NoError(t, db.Raw(`SELECT reference_id, count(*) AS n FROM ledger_entries
        WHERE entry_type = 'ORDER_RELEASE' AND reference_type = 'ORDER' AND user_id > ?
          AND coalesce(reference_key, '') = ''
        GROUP BY reference_id`, diagUserIDBase).Scan(&rows).Error)
	out := make(map[uint]int, len(rows))
	for _, r := range rows {
		out[r.ReferenceID] = r.N
	}
	return out
}

func diagTradeKeysByOrder(t testing.TB, db *gorm.DB) map[string]int {
	t.Helper()

	var keys []string
	require.NoError(t, db.Raw(`SELECT idempotency_key FROM trades WHERE idempotency_key LIKE 'engine:diag-%'`).
		Scan(&keys).Error)
	out := make(map[string]int, len(keys))
	for _, k := range keys {
		out[k]++
	}
	return out
}

// ---------------------------------------------------------------------------
// Phase 5 — 수집
// ---------------------------------------------------------------------------

type diagCounters struct {
	lsn        string
	blksHit    int64
	blksRead   int64
	deadlocks  int64
	commits    int64
	rollbacks  int64
	tempBytes  int64
	seqScan    map[string]int64
	seqTupRead map[string]int64
	idxScan    map[string]int64
	idxScanIdx map[string]int64
}

func diagSnapshotCounters(t testing.TB, db *gorm.DB) diagCounters {
	t.Helper()

	var c diagCounters
	require.NoError(t, db.Raw(`SELECT pg_current_wal_lsn()::text`).Scan(&c.lsn).Error)
	var row struct {
		BlksHit, BlksRead, Deadlocks, XactCommit, XactRollback, TempBytes int64
	}
	require.NoError(t, db.Raw(`SELECT blks_hit, blks_read, deadlocks, xact_commit, xact_rollback, temp_bytes
        FROM pg_stat_database WHERE datname = current_database()`).Scan(&row).Error)
	c.blksHit, c.blksRead, c.deadlocks = row.BlksHit, row.BlksRead, row.Deadlocks
	c.commits, c.rollbacks, c.tempBytes = row.XactCommit, row.XactRollback, row.TempBytes

	c.seqScan, c.seqTupRead, c.idxScan = map[string]int64{}, map[string]int64{}, map[string]int64{}
	var tables []struct {
		Relname                      string
		SeqScan, SeqTupRead, IdxScan int64
	}
	require.NoError(t, db.Raw(`SELECT relname, seq_scan, seq_tup_read, coalesce(idx_scan,0) AS idx_scan
        FROM pg_stat_user_tables`).Scan(&tables).Error)
	for _, r := range tables {
		c.seqScan[r.Relname], c.seqTupRead[r.Relname], c.idxScan[r.Relname] = r.SeqScan, r.SeqTupRead, r.IdxScan
	}

	c.idxScanIdx = map[string]int64{}
	var idx []struct {
		Indexrelname string
		IdxScan      int64
	}
	require.NoError(t, db.Raw(`SELECT indexrelname, coalesce(idx_scan,0) AS idx_scan FROM pg_stat_user_indexes`).
		Scan(&idx).Error)
	for _, r := range idx {
		c.idxScanIdx[r.Indexrelname] = r.IdxScan
	}
	return c
}

// diagLockSampler는 실행 중 대기 중인 락 수를 표본한다. pg_locks의 NOT granted 행이
// 곧 "누군가 행 락을 기다리는 중"이다 — 동시성 축 판정의 직접 증거다.
type diagLockSampler struct {
	stop    chan struct{}
	done    chan struct{}
	max     int
	sum     int
	samples int
}

func diagStartLockSampler(db *gorm.DB) *diagLockSampler {
	s := &diagLockSampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				var n int
				if err := db.Raw(`SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&n).Error; err != nil {
					continue
				}
				s.samples++
				s.sum += n
				if n > s.max {
					s.max = n
				}
			}
		}
	}()
	return s
}

func (s *diagLockSampler) finish() (max int, mean float64, samples int) {
	close(s.stop)
	<-s.done
	if s.samples == 0 {
		return 0, 0, 0
	}
	return s.max, float64(s.sum) / float64(s.samples), s.samples
}

func diagCollectDBStats(t testing.TB, db *gorm.DB, before diagCounters) diagDBStats {
	t.Helper()

	after := diagSnapshotCounters(t, db)
	var stats diagDBStats
	require.NoError(t, db.Raw(`SELECT pg_wal_lsn_diff(?::pg_lsn, ?::pg_lsn)::bigint`, after.lsn, before.lsn).
		Scan(&stats.WALBytes).Error)
	stats.BlksHit = after.blksHit - before.blksHit
	stats.BlksRead = after.blksRead - before.blksRead
	stats.Deadlocks = after.deadlocks - before.deadlocks
	stats.XactCommit = after.commits - before.commits
	stats.XactRollback = after.rollbacks - before.rollbacks
	stats.TempBytes = after.tempBytes - before.tempBytes
	stats.CPUNote = "로컬 Docker 선별 실험에서는 DB 호스트 CPU%를 수집하지 않는다. " +
		"DB 측 작업량은 db_exec_ms_total(pg_stat_statements)로 대신 읽는다. " +
		"환경 의존 원인(WAL·스토리지·autovacuum)의 최종 기각은 GCP에서만 가능하다."

	require.NoError(t, db.Raw(`
SELECT query, calls, total_exec_time AS total_ms, mean_exec_time AS mean_ms, rows,
       shared_blks_hit, shared_blks_read, wal_bytes, wal_records,
       blk_read_time AS blk_read_ms, blk_write_time AS blk_write_ms, temp_blks_written
FROM pg_stat_statements
WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
ORDER BY total_exec_time DESC LIMIT 25`).Scan(&stats.TopStatements).Error)
	for _, s := range stats.TopStatements {
		stats.DBExecMs += s.TotalMs
	}

	var tables []struct {
		Relname                string
		NLiveTup, NDeadTup     int64
		AutovacuumCount        int64
		TotalBytes, IndexBytes int64
	}
	require.NoError(t, db.Raw(`
SELECT relname, n_live_tup, n_dead_tup, autovacuum_count,
       pg_total_relation_size(relid) AS total_bytes, pg_indexes_size(relid) AS index_bytes
FROM pg_stat_user_tables ORDER BY relname`).Scan(&tables).Error)
	for _, r := range tables {
		stats.Tables = append(stats.Tables, diagTableStat{
			Table:      r.Relname,
			SeqScan:    after.seqScan[r.Relname] - before.seqScan[r.Relname],
			SeqTupRead: after.seqTupRead[r.Relname] - before.seqTupRead[r.Relname],
			IdxScan:    after.idxScan[r.Relname] - before.idxScan[r.Relname],
			NLiveTup:   r.NLiveTup, NDeadTup: r.NDeadTup, AutoVacuums: r.AutovacuumCount,
			TotalBytes: r.TotalBytes, IndexBytes: r.IndexBytes,
		})
	}

	var idx []struct {
		Relname, Indexrelname, Def string
		Bytes                      int64
	}
	require.NoError(t, db.Raw(`
SELECT s.relname, s.indexrelname, pg_relation_size(s.indexrelid) AS bytes, i.indexdef AS def
FROM pg_stat_user_indexes s JOIN pg_indexes i ON i.indexname = s.indexrelname
ORDER BY s.relname, s.indexrelname`).Scan(&idx).Error)
	for _, r := range idx {
		stats.Indexes = append(stats.Indexes, diagIndexStat{
			Table: r.Relname, Index: r.Indexrelname, Def: r.Def, Bytes: r.Bytes,
			IdxScan: after.idxScanIdx[r.Indexrelname] - before.idxScanIdx[r.Indexrelname],
		})
	}

	stats.Checkpoints = diagCheckpointStats(t, db)
	stats.Explain = diagExplainSurvey(t, db)
	stats.SeqScanNote = diagSeqScanNote
	for _, e := range stats.Explain {
		if e.SeqScan {
			stats.SeqScanQueries = append(stats.SeqScanQueries, e.Name)
		}
	}
	return stats
}

// diagExplainSurvey는 정산 경로의 **읽기** 쿼리를 EXPLAIN (ANALYZE, BUFFERS, WAL)한다.
//
// ⚠ EXPLAIN만으로는 진단이 닫히지 않는다 — 정산 경로의 중심은 INSERT·UPDATE·행 락·
// COMMIT·WAL이고 EXPLAIN은 읽기 계획만 잘 보여준다. 5.1의 크기·동시성 스윕이 본체이고,
// 이 조사는 명백한 missing index를 먼저 걸러내는 용도다.
//
// ANALYZE는 실제로 실행하고 FOR UPDATE는 실제로 락을 잡으므로 반드시 롤백하는
// 트랜잭션 안에서 돌린다.
func diagExplainSurvey(t testing.TB, db *gorm.DB) []diagExplainRow {
	t.Helper()

	var orderIDs []uint
	require.NoError(t, db.Raw(`SELECT id FROM orders ORDER BY id LIMIT 6`).Scan(&orderIDs).Error)
	var userID uint
	require.NoError(t, db.Raw(`SELECT user_id FROM wallets WHERE coin_symbol = 'KRW' ORDER BY id LIMIT 1`).
		Scan(&userID).Error)
	if len(orderIDs) == 0 || userID == 0 {
		return nil
	}

	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{"orders_find_by_id_for_update",
			`SELECT * FROM orders WHERE id = ? ORDER BY id LIMIT 1 FOR UPDATE`,
			[]any{orderIDs[0]}},
		{"orders_lock_by_ids",
			`SELECT * FROM orders WHERE id IN ? ORDER BY id ASC FOR UPDATE`,
			[]any{orderIDs}},
		{"trades_by_idempotency_keys",
			`SELECT * FROM trades WHERE idempotency_key IN ?`,
			[]any{[]string{"engine:diag-0-0", "engine:diag-0-1", "engine:diag-0-2"}}},
		{"trades_sum_buyer_fee_by_buy_order",
			`SELECT COALESCE(SUM(buyer_fee), 0) FROM trades WHERE buy_order_id = ?`,
			[]any{orderIDs[0]}},
		{"wallets_by_user_coin_for_update",
			`SELECT * FROM wallets WHERE user_id = ? AND coin_symbol = ? ORDER BY id LIMIT 1 FOR UPDATE`,
			[]any{userID, model.KRWAssetSymbol}},
	}

	rows := make([]diagExplainRow, 0, len(cases))
	for _, c := range cases {
		var plan []string
		err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Raw(`EXPLAIN (ANALYZE, BUFFERS, WAL) `+c.sql, c.args...).Scan(&plan).Error; err != nil {
				return err
			}
			return fmt.Errorf("rollback: EXPLAIN ANALYZE는 실제로 실행·락하므로 되돌린다")
		})
		if err != nil && len(plan) == 0 {
			rows = append(rows, diagExplainRow{Name: c.name, SQL: c.sql, Plan: "EXPLAIN 실패: " + err.Error()})
			continue
		}
		text := strings.Join(plan, "\n")
		rows = append(rows, diagExplainRow{
			Name: c.name, SQL: c.sql, SeqScan: strings.Contains(text, "Seq Scan"), Plan: text,
		})
	}
	return rows
}

// diagCheckpointStats는 서버 버전에 따라 뷰가 다르다 — pg_stat_checkpointer는 PG17부터고,
// 그 이전에는 pg_stat_bgwriter가 같은 카운터를 다른 이름으로 갖고 있다.
func diagCheckpointStats(t testing.TB, db *gorm.DB) map[string]int64 {
	t.Helper()

	var versionNum int
	require.NoError(t, db.Raw(`SELECT current_setting('server_version_num')::int`).Scan(&versionNum).Error)

	query := `
SELECT 'num_timed' AS key, checkpoints_timed AS val FROM pg_stat_bgwriter
UNION ALL SELECT 'num_requested', checkpoints_req FROM pg_stat_bgwriter
UNION ALL SELECT 'buffers_written', buffers_checkpoint FROM pg_stat_bgwriter`
	if versionNum >= 170000 {
		query = `
SELECT 'num_timed' AS key, num_timed AS val FROM pg_stat_checkpointer
UNION ALL SELECT 'num_requested', num_requested FROM pg_stat_checkpointer
UNION ALL SELECT 'buffers_written', buffers_written FROM pg_stat_checkpointer`
	}

	var rows []struct {
		Key string
		Val int64
	}
	require.NoError(t, db.Raw(query).Scan(&rows).Error)
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.Key] = r.Val
	}
	return out
}

func diagPercentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank] * 1000
}

func diagAggregate(samples []diagSample, elapsed float64) map[string]diagOpStats {
	byKind := map[diagJobKind][]diagSample{}
	for _, s := range samples {
		byKind[s.kind] = append(byKind[s.kind], s)
	}
	out := make(map[string]diagOpStats, len(byKind))
	for kind, list := range byKind {
		secs := make([]float64, 0, len(list))
		batches := map[string]int{}
		for _, s := range list {
			secs = append(secs, s.seconds)
			if kind == diagJobTradeBatch {
				batches[fmt.Sprintf("%d", s.batchSize)]++
			}
		}
		sort.Float64s(secs)
		st := diagOpStats{
			Count: len(list),
			P50Ms: diagPercentile(secs, 0.50),
			P95Ms: diagPercentile(secs, 0.95),
			P99Ms: diagPercentile(secs, 0.99),
			MaxMs: secs[len(secs)-1] * 1000,
		}
		if elapsed > 0 {
			st.OpsPerSec = float64(len(list)) / elapsed
		}
		if len(batches) > 0 {
			st.BatchSizes = batches
		}
		out[string(kind)] = st
	}
	return out
}

// ---------------------------------------------------------------------------
// 셀 1개 실행
// ---------------------------------------------------------------------------

func diagRunCell(t *testing.T, admin *gorm.DB, size diagSize, concurrency int) diagCellResult {
	t.Helper()

	cellDB := fmt.Sprintf("goex_diag_cell_%s_n%d", size.Name, concurrency)
	diagCreateDatabase(t, admin, cellDB, diagTemplateName(size.Name))

	result := diagCellResult{
		Schema:      "goexchange.settlement-cost-diagnostic/v1",
		Cell:        fmt.Sprintf("%s/N%d", size.Name, concurrency),
		Scale:       map[bool]string{true: "smoke", false: "full"}[diagSmoke()],
		Size:        size,
		Concurrency: concurrency,
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
		Env:         map[string]string{},
	}

	t.Run(result.Cell, func(t *testing.T) {
		db := diagOpenMigrated(t, cellDB)
		require.NoError(t, db.Exec(`ANALYZE`).Error)

		var pgVersion string
		require.NoError(t, db.Raw(`SELECT version()`).Scan(&pgVersion).Error)
		result.Env["postgres_version"] = pgVersion
		// GOOS/GOARCH 환경변수는 런타임에 보통 비어 있다 — runtime 패키지가 실제 값이다.
		// NumCPU는 N=8 셀의 해석에 직접 걸린다(코어보다 동시성이 크면 경합이 섞인다).
		result.Env["go_os_arch"] = fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
		result.Env["go_num_cpu"] = fmt.Sprint(runtime.NumCPU())
		var settings []struct{ Name, Setting string }
		require.NoError(t, db.Raw(`SELECT name, setting FROM pg_settings WHERE name IN
            ('shared_buffers','work_mem','max_wal_size','checkpoint_timeout','synchronous_commit',
             'autovacuum','wal_level','max_connections','effective_cache_size')`).Scan(&settings).Error)
		for _, s := range settings {
			result.Env["pg."+s.Name] = s.Setting
		}
		result.RowCounts = diagCountRows(t, db)

		fixture := seedDiagnosticFixtureView(t, db)
		spec := diagWorkload()

		// 게이트 1: 실행 전에 모든 반복의 시작 상태를 확인한다.
		pre := diagVerifyPreconditions(t, db, append(append([]diagJob{}, fixture.Warmup...), fixture.Measured...))
		require.Empty(t, pre, "시작 상태 게이트 실패 — no-op을 재게 되므로 실행하지 않는다")

		orderRepo := repository.NewOrderRepository(db)
		walletRepo := repository.NewWalletRepository(db)
		settlement := NewSettlementService(db, orderRepo, walletRepo)
		orderSvc := NewOrderService(orderRepo, walletRepo, matching.NewMatchingEngine())

		// warm-up은 결과에서 제외한다(record=false).
		diagRunJobs(t, settlement, orderSvc, fixture.Warmup, concurrency, false)

		require.NoError(t, db.Exec(`SELECT pg_stat_statements_reset()`).Error)
		before := diagSnapshotCounters(t, db)
		sampler := diagStartLockSampler(db)

		started := time.Now()
		samples, maxInFlight := diagRunJobs(t, settlement, orderSvc, fixture.Measured, concurrency, true)
		result.ElapsedSec = time.Since(started).Seconds()

		result.DB = diagCollectDBStats(t, db, before)
		result.DB.LockWaitMax, result.DB.LockWaitMean, result.DB.LockWaitSample = sampler.finish()
		result.Ops = diagAggregate(samples, result.ElapsedSec)

		batchJobs, cancelJobs, marketJobs := diagComposeJobKinds(spec.WarmupJobs + spec.MeasuredJobs)
		result.Workload = diagWorkloadMeta{
			WarmupJobs: spec.WarmupJobs, MeasuredJobs: spec.MeasuredJobs,
			BatchSize: diagBatchSize, TerminalRatio: diagTerminalRatio,
			TerminalSplitNote: fmt.Sprintf("terminal %d건을 취소 %d / 시장가 완료 %d로 반씩 나눈 것은 "+
				"32-B에서 분해되지 않은 값이라 가정이다(batch job %d건)", cancelJobs+marketJobs, cancelJobs, marketJobs, batchJobs),
			Seed: diagSeed, Users: diagUserCount, MaxInFlight: maxInFlight,
		}

		// 게이트 2: 모든 반복이 실제로 일을 했는가.
		result.Integrity.Violations = diagVerifyEffects(t, db, fixture.Measured)
		result.Integrity.Violations = append(result.Integrity.Violations,
			diagVerifyEffects(t, db, fixture.Warmup)...)
		result.Integrity.FailedSettlements = diagCount(t, db, `SELECT count(*) FROM failed_settlements`)
		result.Integrity.FailedOrderCancellations = diagCount(t, db, `SELECT count(*) FROM failed_order_cancellations`)
		result.Integrity.FailedMarketCompletions = diagCount(t, db, `SELECT count(*) FROM failed_market_completions`)

		// production 정합성 검사를 그대로 돌린다 — 위반이 있으면 성능 수치를 읽지 않는다.
		(&ReconciliationWorker{Repository: repository.NewReconciliationRepository(db)}).RunOnce()
		result.Integrity.ReconciliationViolations = diagCount(t, db, `SELECT count(*) FROM reconciliation_violations`)

		require.EqualValues(t, int64(concurrency), maxInFlight,
			"동시 실행 수가 %d에 도달하지 않았다(최댓값 %d)", concurrency, maxInFlight)
		require.Empty(t, result.Integrity.Violations, "실작업 검증 게이트 실패 — 이 셀의 결과를 버린다")
		require.Zero(t, result.Integrity.FailedSettlements+result.Integrity.FailedOrderCancellations+
			result.Integrity.FailedMarketCompletions+result.Integrity.ReconciliationViolations,
			"정합성 위반이 있다 — 성능 수치를 읽지 않는다")
	})

	diagDropDatabase(t, admin, cellDB)
	return result
}

func diagCount(t testing.TB, db *gorm.DB, sql string) int64 {
	t.Helper()

	var n int64
	require.NoError(t, db.Raw(sql).Scan(&n).Error)
	return n
}

// seedDiagnosticFixtureView는 이미 시드된 셀 DB에서 fixture를 **다시 만들지 않고**
// 같은 결정론적 절차로 job 목록만 재구성한다. 시드와 같은 seed·같은 순서를 쓰므로
// template에 들어 있는 주문 ID와 정확히 대응한다.
func seedDiagnosticFixtureView(t testing.TB, db *gorm.DB) diagFixture {
	t.Helper()

	// fixture 주문은 시드 시점에 가장 먼저 만들어졌으므로 ID 오름차순이 생성 순서다.
	var ids []uint
	require.NoError(t, db.Raw(`SELECT id FROM orders WHERE status IN ('PENDING','PARTIAL') ORDER BY id`).
		Scan(&ids).Error)
	return diagRebuildJobs(t, ids)
}

// diagRebuildJobs는 seedDiagnosticFixture와 **정확히 같은 순서**로 job을 재구성한다.
// 두 곳의 순서가 어긋나면 job이 엉뚱한 주문을 가리키므로, 시작 상태 게이트가 즉시 실패한다.
func diagRebuildJobs(t testing.TB, orderIDs []uint) diagFixture {
	t.Helper()

	spec := diagWorkload()
	total := spec.WarmupJobs + spec.MeasuredJobs
	batchJobs, cancelJobs, marketJobs := diagComposeJobKinds(total)
	require.Equal(t, batchJobs*diagBatchSize*2+cancelJobs+marketJobs, len(orderIDs),
		"active 주문 수가 job 구성과 맞지 않는다")

	jobs := make([]diagJob, 0, total)
	next := 0
	take := func() uint { id := orderIDs[next]; next++; return id }
	seq := 0

	for j := 0; j < batchJobs; j++ {
		job := diagJob{Kind: diagJobTradeBatch, Trades: make([]diagTradeSpec, diagBatchSize)}
		for s := 0; s < diagBatchSize; s++ {
			job.Trades[s] = diagTradeSpec{
				BuyOrderID: take(), SellOrderID: take(),
				Price: diagPrice(seq), Quantity: diagQuantity(seq),
			}
			seq++
		}
		jobs = append(jobs, job)
	}
	for j := 0; j < cancelJobs; j++ {
		jobs = append(jobs, diagJob{Kind: diagJobCancelTerminal, OrderID: take()})
		seq++
	}
	for j := 0; j < marketJobs; j++ {
		quote := decimal.NewFromInt(int64(1_000_000 + (seq%503)*17))
		jobs = append(jobs, diagJob{
			Kind: diagJobMarketTerminal, OrderID: take(),
			FilledAmount: diagQuantity(seq), FilledQuoteAmount: quote.Div(decimal.NewFromInt(2)).Round(0),
			RemainingQuoteAmount: decimal.Zero,
		})
		seq++
	}

	diagShuffleJobs(jobs)
	return diagFixture{Warmup: jobs[:spec.WarmupJobs], Measured: jobs[spec.WarmupJobs:]}
}

func diagWriteResult(t testing.TB, result diagCellResult) string {
	t.Helper()

	require.NoError(t, os.MkdirAll(diagResultDir, 0o755))
	name := fmt.Sprintf("cell-%s-n%d.json", result.Size.Name, result.Concurrency)
	path := filepath.Join(diagResultDir, name)
	data, err := json.MarshalIndent(result, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(data, '\n'), 0o644))
	return path
}

// ---------------------------------------------------------------------------
// 테스트
// ---------------------------------------------------------------------------

// 하니스가 실제로 도는지 확인하고 결과 스키마 예시 1개를 만든다.
// **6셀 실측은 다음 세션이다** — 여기서는 한 셀만 돌린다.
//
// 가장 큰 크기(32-B 종료 규모) × N=8을 고른다. 여섯 셀 중 가장 무거운 조합이므로,
// 이것이 통과하면 나머지 다섯도 자원·시간 면에서 통과한다.
func TestSettlementDiagnosticHarnessProducesCellResult(t *testing.T) {
	diagRequireOptIn(t)
	admin := diagOpenAdmin(t)
	diagBuildTemplates(t, admin)

	sizes := diagSizeSet()
	largest := sizes[len(sizes)-1]

	// 동시성 축의 **양 끝**을 모두 돌린다. N=1과 N=8은 같은 러너를 쓰지만, 동시 실행 수
	// 단언과 워커 분배는 두 값에서 각각 확인해야 다음 세션이 6셀을 믿고 돌릴 수 있다.
	for _, concurrency := range []int{1, 8} {
		result := diagRunCell(t, admin, largest, concurrency)
		path := diagWriteResult(t, result)
		t.Logf("셀 결과 기록: %s", path)
		t.Logf("N=%d 종류별: %+v", concurrency, result.Ops)

		require.NotEmpty(t, result.Ops, "N=%d 종류별 결과가 비어 있다", concurrency)
		for kind, st := range result.Ops {
			require.Positive(t, st.Count, "N=%d %s 표본이 없다", concurrency, kind)
			if kind == string(diagJobTradeBatch) {
				require.Equal(t, map[string]int{"3": st.Count}, st.BatchSizes,
					"N=%d batch 크기가 전부 3이 아니다", concurrency)
			}
		}
		require.EqualValues(t, concurrency, result.Workload.MaxInFlight)
		require.Positive(t, result.DB.WALBytes, "N=%d WAL 증가가 0이다 — 아무것도 쓰지 않았다", concurrency)
	}
}

// ⚠ 게이트가 통과만 하는 것은 게이트가 아니다.
// 일부러 이미 종결된 주문을 넣으면 시작 상태 게이트와 실작업 게이트가 **둘 다** 실패해야 한다.
func TestSettlementDiagnosticGateRejectsAlreadyTerminalOrder(t *testing.T) {
	diagRequireOptIn(t)
	admin := diagOpenAdmin(t)

	size := diagSizeSet()[0]
	template := diagTemplateName(size.Name)
	diagCreateDatabase(t, admin, template, "")
	t.Run("seed", func(t *testing.T) {
		db := diagOpenMigrated(t, template)
		seedDiagnosticFixture(t, db)
		seedDiagnosticHistory(t, db, size)
	})

	cell := "goex_diag_negative"
	diagCreateDatabase(t, admin, cell, template)
	defer func() { diagDropDatabase(t, admin, cell) }()

	t.Run("poisoned", func(t *testing.T) {
		db := diagOpenMigrated(t, cell)
		fixture := seedDiagnosticFixtureView(t, db)
		all := append(append([]diagJob{}, fixture.Warmup...), fixture.Measured...)

		// 건강한 fixture는 게이트를 통과해야 한다 — 이 단언이 없으면 아래 실패가
		// "게이트가 항상 실패한다"와 구분되지 않는다.
		require.Empty(t, diagVerifyPreconditions(t, db, all), "정상 fixture가 시작 상태 게이트를 통과하지 못했다")

		var victim diagJob
		for _, job := range all {
			if job.Kind == diagJobCancelTerminal {
				victim = job
				break
			}
		}
		require.NotZero(t, victim.OrderID, "취소 terminal job을 찾지 못했다")

		// 이미 종결된 주문으로 만든다 — ProcessOrderCancellation은 no-op이 된다.
		require.NoError(t, db.Exec(`UPDATE orders SET status = 'CANCELLED' WHERE id = ?`, victim.OrderID).Error)

		pre := diagVerifyPreconditions(t, db, all)
		require.NotEmpty(t, pre, "이미 종결된 주문인데 시작 상태 게이트가 통과했다")
		require.Contains(t, fmt.Sprint(pre), fmt.Sprintf("주문 %d", victim.OrderID))

		// 실제로 호출해 보면 no-op이라 원장이 생기지 않고, 실작업 게이트가 잡아야 한다.
		orderRepo := repository.NewOrderRepository(db)
		walletRepo := repository.NewWalletRepository(db)
		orderSvc := NewOrderService(orderRepo, walletRepo, matching.NewMatchingEngine())
		require.NoError(t, orderSvc.ProcessOrderCancellation(matching.OrderCancelled{
			OrderID: victim.OrderID, CoinSymbol: diagCoin, Side: model.OrderSideBuy,
		}), "멱등 no-op은 에러 없이 성공한다 — 그래서 지연만 보면 알 수 없다")

		effects := diagVerifyEffects(t, db, []diagJob{victim})
		require.NotEmpty(t, effects, "no-op이었는데 실작업 게이트가 통과했다")
		require.Contains(t, fmt.Sprint(effects), "no-op이었다")
	})
}
