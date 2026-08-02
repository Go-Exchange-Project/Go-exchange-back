package service

// 고정 부하 정산 비용 상승 진단 하니스 — 합성 시드와 TEMPLATE 스냅샷.
//
// 왜 필요한가: 32-B에서 부하가 일정한데 작업당 정산 비용이 9.19ms → 23.75ms로 올랐다.
// 원인 후보(데이터 크기 · 인덱스 · WAL · 행 락 경합 · job mix)가 실부하에서는 전부 같이
// 움직여 분리되지 않는다. 이 하니스는 **DB 크기 축과 동시성 축만 움직이고 나머지를 고정**해
// 후보를 좁힌다.
//
// 왜 합성 시드인가: 32-B의 DB는 남아 있지 않다(매 실행 TRUNCATE, VM 정지). 합성 시드가
// 가장 싸고 크기 축을 정확히 통제한다. 단, **행 수만 맞추면 안 된다** — 측정 대상 fixture는
// 세 크기에서 완전히 동일해야 하고, 크기 차이는 **종결된 과거 이력 행만**으로 만든다.
//
// 왜 TEMPLATE인가: 6셀 × 복원이라 pg_dump/pg_restore는 누적 비용이 크다.
// CREATE DATABASE ... TEMPLATE 은 파일 복사라 훨씬 빠르다.
//
// 게이트가 두 겹인 이유: CI의 `Postgres integration tests` job이 이미
// GOEXCHANGE_TEST_DATABASE_DSN을 설정한다. DSN만으로 게이팅하면 진단이 CI에서 돌아간다.

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/testdb"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	// 진단 opt-in 게이트. DSN 단독 게이팅이면 CI에서 돈다.
	diagRunEnv   = "GOEXCHANGE_RUN_SETTLEMENT_DIAGNOSTIC"
	diagScaleEnv = "GOEXCHANGE_SETTLEMENT_DIAGNOSTIC_SCALE" // "smoke" | "full"(기본)

	diagSeed = 20260802 // 고정 seed — 세 크기·6셀이 정확히 같은 fixture를 본다
	// 과거 이력 원장의 reference_id 오프셋. fixture 주문 ID와 겹치면 실작업 검증이
	// 이력 행을 이번 실행의 산출물로 오인한다(실제로 한 번 걸렸다).
	diagHistoryRefIDBase = 1_000_000_000
	diagUserCount        = 750 // 32-B Run 1과 동일한 사용자 수
	diagUserIDBase       = 900_000_000
	diagCoin             = "BTC"

	// 32-B Run 1 종료 규모. 출처: docs/benchmarks/32-2026-08-01-capacity-boundary-session-b.md
	// 무결성 교차검증 표(orders는 k6 주문 시도 − 셰딩과 정확히 일치한 값).
	diagFullOrders = 546_783
	diagFullTrades = 311_207
	diagFullLedger = 1_886_343
	diagFullOutbox = 459_346

	// 32-B hold 평균 배치 크기 2.549에 가장 가까운 정수. 평균이 아니라 정확한 정수로 고정한다.
	diagBatchSize = 3
	// 32-B 실측 terminal job 비율 53~56%의 중앙값.
	diagTerminalRatio = 0.55
)

// diagSize는 한 크기 축 지점의 과거 이력 행 수다. fixture는 여기 포함되지 않는다.
type diagSize struct {
	Name   string
	Orders int
	Trades int
	Ledger int
	Outbox int
}

func diagSmoke() bool { return os.Getenv(diagScaleEnv) == "smoke" }

// diagSizeSet은 세 크기를 돌려준다. "초기"는 빈 DB가 아니라 **fixture는 있고 과거 이력만
// 없는** DB다 — 빈 DB로 시작하면 크기 축이 아니라 "fixture 유무"를 재게 된다.
func diagSizeSet() []diagSize {
	full := diagSize{"full", diagFullOrders, diagFullTrades, diagFullLedger, diagFullOutbox}
	if diagSmoke() {
		// 하니스 동작 확인용. 비율은 full과 같게 유지한다(1/200).
		full = diagSize{"full", diagFullOrders / 200, diagFullTrades / 200, diagFullLedger / 200, diagFullOutbox / 200}
	}
	return []diagSize{
		{"initial", 0, 0, 0, 0},
		{"mid", full.Orders / 2, full.Trades / 2, full.Ledger / 2, full.Outbox / 2},
		full,
	}
}

// diagRequireOptIn은 두 겹 게이트를 강제한다. DSN이 있어도 opt-in이 없으면 skip한다.
func diagRequireOptIn(t testing.TB) {
	t.Helper()

	if os.Getenv(diagRunEnv) != "1" {
		t.Skipf("%s=1 이 아니므로 정산 비용 진단을 건너뛴다", diagRunEnv)
	}
}

// ---------------------------------------------------------------------------
// DSN 조작 — TEMPLATE 복제는 다른 데이터베이스에 붙어야 하므로 dbname만 바꾼다.
// ---------------------------------------------------------------------------

// diagBaseDSN은 CI·로컬이 쓰는 keyword/value DSN을 그대로 읽는다.
// (예: "host=localhost user=... password=... dbname=... port=5432 sslmode=disable")
func diagBaseDSN(t testing.TB) string {
	t.Helper()

	dsn := os.Getenv("GOEXCHANGE_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("GOEXCHANGE_TEST_DATABASE_DSN is not set; skipping Postgres integration test")
	}
	return dsn
}

func diagDSNWithDatabase(t testing.TB, dsn string, dbName string) string {
	t.Helper()

	fields := strings.Fields(dsn)
	out := make([]string, 0, len(fields)+1)
	replaced := false
	for _, f := range fields {
		if strings.HasPrefix(f, "dbname=") {
			out = append(out, "dbname="+dbName)
			replaced = true
			continue
		}
		out = append(out, f)
	}
	if !replaced {
		out = append(out, "dbname="+dbName)
	}
	return strings.Join(out, " ")
}

// diagOpenAdmin은 maintenance 데이터베이스(postgres)에 붙는다. CREATE/DROP DATABASE는
// 대상 DB에 붙은 채로는 실행할 수 없다.
func diagOpenAdmin(t testing.TB) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(postgres.Open(diagDSNWithDatabase(t, diagBaseDSN(t), "postgres")),
		&gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// diagDropDatabase는 남은 접속을 먼저 끊고 DROP한다. 접속이 남아 있으면
// DROP도 CREATE ... TEMPLATE도 실패한다 — 6셀 연속 복제에서 이것이 유일한 반복 실패 원인이다.
func diagDropDatabase(t testing.TB, admin *gorm.DB, name string) {
	t.Helper()

	require.NoError(t, admin.Exec(
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = ? AND pid <> pg_backend_pid()`,
		name).Error)
	require.NoError(t, admin.Exec(fmt.Sprintf(`DROP DATABASE IF EXISTS %q`, name)).Error)
}

func diagCreateDatabase(t testing.TB, admin *gorm.DB, name string, template string) {
	t.Helper()

	diagDropDatabase(t, admin, name)
	stmt := fmt.Sprintf(`CREATE DATABASE %q`, name)
	if template != "" {
		// template DB에 접속이 남아 있으면 실패한다. 먼저 끊는다.
		require.NoError(t, admin.Exec(
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = ? AND pid <> pg_backend_pid()`,
			template).Error)
		stmt = fmt.Sprintf(`CREATE DATABASE %q TEMPLATE %q`, name, template)
	}
	require.NoError(t, admin.Exec(stmt).Error)
}

// diagOpenMigrated는 기존 testdb.OpenIntegrationDB를 그대로 재사용한다(AutoMigrate + goose).
// 대상 DB만 바꾸기 위해 env를 임시로 덮어쓴다 — t.Setenv은 테스트 종료 시 자동 복구된다.
//
// ⚠ OpenIntegrationDB는 t.Cleanup으로 접속을 닫는다. TEMPLATE로 쓸 DB는 **접속이 없어야**
// 하므로, template 시드는 반드시 subtest 안에서 호출해 subtest 종료 시 닫히게 한다.
func diagOpenMigrated(t testing.TB, dbName string) *gorm.DB {
	t.Helper()

	t.Setenv("GOEXCHANGE_TEST_DATABASE_DSN", diagDSNWithDatabase(t, diagBaseDSN(t), dbName))
	db := testdb.OpenIntegrationDB(t)
	// pg_stat_statements는 DB마다 등록해야 뷰가 보인다. template에 만들어 두면 복제본이
	// 그대로 물려받지만, 셀 DB를 직접 열 때도 안전하도록 여기서 보장한다.
	require.NoError(t, db.Exec(`CREATE EXTENSION IF NOT EXISTS pg_stat_statements`).Error,
		"pg_stat_statements를 만들 수 없다 — shared_preload_libraries에 추가하고 Postgres를 재기동해야 한다")
	return db
}

func diagTemplateName(size string) string { return "goex_diag_tmpl_" + size }

// ---------------------------------------------------------------------------
// fixture — 세 크기에서 완전히 동일해야 한다
// ---------------------------------------------------------------------------

type diagJobKind string

const (
	diagJobTradeBatch     diagJobKind = "trade_batch"
	diagJobCancelTerminal diagJobKind = "cancel_terminal"
	diagJobMarketTerminal diagJobKind = "market_terminal"
)

// diagTradeSpec은 SettleTradeBatch에 넘길 체결 1건의 재료다. model.Trade는 실행 시점에
// 새 idempotency key로 만든다(재사용하면 멱등 중복 경로를 재게 된다).
type diagTradeSpec struct {
	BuyOrderID  uint
	SellOrderID uint
	Price       decimal.Decimal
	Quantity    decimal.Decimal
}

type diagJob struct {
	Index  int
	Kind   diagJobKind
	Trades []diagTradeSpec // trade_batch 전용, len == diagBatchSize

	// terminal 전용
	OrderID uint
	UserID  uint
	// market_terminal 전용 — CompleteMarketOrderInput 재료
	FilledAmount         decimal.Decimal
	FilledQuoteAmount    decimal.Decimal
	RemainingQuoteAmount decimal.Decimal
}

type diagFixture struct {
	UserIDs  []uint
	Warmup   []diagJob
	Measured []diagJob
}

type diagWorkloadSpec struct {
	WarmupJobs   int
	MeasuredJobs int
}

func diagWorkload() diagWorkloadSpec {
	if diagSmoke() {
		return diagWorkloadSpec{WarmupJobs: 10, MeasuredJobs: 40}
	}
	return diagWorkloadSpec{WarmupJobs: 100, MeasuredJobs: 1000}
}

// diagComposeJobKinds는 전체 job 수를 32-B 실측 비율로 쪼갠다.
// terminal 안에서 취소/시장가 완료를 반반으로 나누는 것은 32-B에서 분해되지 않은 값이라
// **가정**이다(결과 파일에 그대로 기록한다).
func diagComposeJobKinds(total int) (batch int, cancel int, market int) {
	terminal := int(math.Round(float64(total) * diagTerminalRatio))
	cancel = terminal / 2
	market = terminal - cancel
	return total - terminal, cancel, market
}

func diagUserID(i int) uint { return uint(diagUserIDBase + 1 + i) }

// diagShuffleJobs는 고정 seed로 job 순서를 섞고 Index를 부여한다.
// 종류가 뭉쳐 있으면 N=8에서 같은 종류끼리만 경합해 실부하의 혼합 경합을 재지 못한다.
//
// 시드(seedDiagnosticFixture)와 재구성(diagRebuildJobs)이 **반드시 같은 함수**를 써야 한다 —
// 순서가 어긋나면 job이 엉뚱한 주문을 가리키고, 그러면 시작 상태 게이트가 즉시 실패한다.
func diagShuffleJobs(jobs []diagJob) {
	rng := rand.New(rand.NewSource(diagSeed))
	rng.Shuffle(len(jobs), func(a, b int) { jobs[a], jobs[b] = jobs[b], jobs[a] })
	for i := range jobs {
		jobs[i].Index = i
	}
}

// diagPrice/diagQuantity는 결정론적이되 모든 행이 같지 않게 흩뜨린다 — 값이 전부 같으면
// 계획 캐시·버퍼 적중이 비현실적으로 좋아진다.
func diagPrice(i int) decimal.Decimal {
	return decimal.NewFromInt(int64(50_000_000 + (i%997)*13))
}

func diagQuantity(i int) decimal.Decimal {
	return decimal.NewFromInt(int64(10 + i%7)).Div(decimal.NewFromInt(1000)) // 0.010 ~ 0.016
}

// seedDiagnosticFixture는 사용자·지갑·측정 대상 active 주문을 만든다.
// **반드시 과거 이력보다 먼저** 호출한다 — 시퀀스가 낮은 ID부터 시작해야 세 크기의
// fixture 행이 같은 ID를 갖고, 그래야 fixture 해시가 일치한다.
func seedDiagnosticFixture(t testing.TB, db *gorm.DB) diagFixture {
	t.Helper()

	users := make([]model.User, 0, diagUserCount)
	userIDs := make([]uint, 0, diagUserCount)
	for i := 0; i < diagUserCount; i++ {
		id := diagUserID(i)
		userIDs = append(userIDs, id)
		users = append(users, model.User{
			ID:    id,
			Name:  fmt.Sprintf("diag-user-%d", i),
			Email: fmt.Sprintf("diag-user-%d@diag.local", i),
		})
	}
	require.NoError(t, db.CreateInBatches(&users, 500).Error)

	spec := diagWorkload()
	total := spec.WarmupJobs + spec.MeasuredJobs
	batchJobs, cancelJobs, marketJobs := diagComposeJobKinds(total)

	// 사용자별로 필요한 locked 잔고를 누적한 뒤 한 번에 지갑을 만든다.
	lockedKRW := make(map[uint]decimal.Decimal, diagUserCount)
	lockedCoin := make(map[uint]decimal.Decimal, diagUserCount)
	addKRW := func(id uint, v decimal.Decimal) {
		lockedKRW[id] = lockedKRW[id].Add(v)
	}
	addCoin := func(id uint, v decimal.Decimal) {
		lockedCoin[id] = lockedCoin[id].Add(v)
	}

	orders := make([]model.Order, 0, batchJobs*diagBatchSize*2+cancelJobs+marketJobs)
	// 주문을 만들 때 인덱스를 기억해 두었다가, Create 이후 부여된 ID를 job에 채운다.
	type pending struct {
		kind  diagJobKind
		slot  int // job 내 위치(trade_batch의 0..2), terminal은 0
		jobIx int
		buy   bool
	}
	pendings := make([]pending, 0, cap(orders))

	jobs := make([]diagJob, 0, total)
	seq := 0

	for j := 0; j < batchJobs; j++ {
		job := diagJob{Kind: diagJobTradeBatch, Trades: make([]diagTradeSpec, diagBatchSize)}
		for s := 0; s < diagBatchSize; s++ {
			buyer := userIDs[seq%diagUserCount]
			seller := userIDs[(seq+diagUserCount/2)%diagUserCount]
			price, qty := diagPrice(seq), diagQuantity(seq)

			orders = append(orders, model.Order{
				UserID: buyer, CoinSymbol: diagCoin, Side: model.OrderSideBuy,
				OrderType: model.OrderTypeLimit, Price: price, Amount: qty,
				Status: model.OrderStatusPending, FilledAmount: decimal.Zero,
			})
			pendings = append(pendings, pending{diagJobTradeBatch, s, len(jobs), true})
			addKRW(buyer, quoteAmountWithTradingFee(price.Mul(qty)))

			orders = append(orders, model.Order{
				UserID: seller, CoinSymbol: diagCoin, Side: model.OrderSideSell,
				OrderType: model.OrderTypeLimit, Price: price, Amount: qty,
				Status: model.OrderStatusPending, FilledAmount: decimal.Zero,
			})
			pendings = append(pendings, pending{diagJobTradeBatch, s, len(jobs), false})
			addCoin(seller, qty)

			job.Trades[s] = diagTradeSpec{Price: price, Quantity: qty}
			seq++
		}
		jobs = append(jobs, job)
	}

	// 취소 terminal: 부분 체결된 지정가 매수. 잔여분에 대한 hold가 실제로 남아 있어야
	// ProcessOrderCancellation이 no-op이 아니게 된다.
	for j := 0; j < cancelJobs; j++ {
		owner := userIDs[seq%diagUserCount]
		price := diagPrice(seq)
		amount := diagQuantity(seq).Mul(decimal.NewFromInt(4))
		filled := diagQuantity(seq)
		remaining := amount.Sub(filled)

		orders = append(orders, model.Order{
			UserID: owner, CoinSymbol: diagCoin, Side: model.OrderSideBuy,
			OrderType: model.OrderTypeLimit, Price: price, Amount: amount,
			Status: model.OrderStatusPartial, FilledAmount: filled,
			FilledQuoteAmount: price.Mul(filled),
		})
		pendings = append(pendings, pending{diagJobCancelTerminal, 0, len(jobs), true})
		addKRW(owner, quoteAmountWithTradingFee(price.Mul(remaining)))

		jobs = append(jobs, diagJob{Kind: diagJobCancelTerminal, UserID: owner})
		seq++
	}

	// 시장가 완료 terminal: 예산이 남은 시장가 매수. remainingQuote > 0 이어야
	// CompleteMarketOrder가 hold 해제 + 원장 기록이라는 실제 일을 한다.
	for j := 0; j < marketJobs; j++ {
		owner := userIDs[seq%diagUserCount]
		quote := decimal.NewFromInt(int64(1_000_000 + (seq%503)*17))
		filledQuote := quote.Div(decimal.NewFromInt(2)).Round(0)
		filledAmount := diagQuantity(seq)
		remainingQuote := quote.Sub(filledQuote)

		orders = append(orders, model.Order{
			UserID: owner, CoinSymbol: diagCoin, Side: model.OrderSideBuy,
			OrderType: model.OrderTypeMarket, Price: decimal.Zero, Amount: decimal.Zero,
			QuoteAmount: quote, Status: model.OrderStatusPartial,
			FilledAmount: filledAmount, FilledQuoteAmount: filledQuote,
		})
		pendings = append(pendings, pending{diagJobMarketTerminal, 0, len(jobs), true})
		addKRW(owner, remainingQuote)

		jobs = append(jobs, diagJob{
			Kind: diagJobMarketTerminal, UserID: owner,
			FilledAmount: filledAmount, FilledQuoteAmount: filledQuote,
			RemainingQuoteAmount: decimal.Zero, // 0이면 FILLED로 종결된다
		})
		seq++
	}

	// 지갑은 원장이 설명할 수 있어야 한다. ReconciliationWorker의 ledger_wallet 검사는
	// 지갑 잔고와 원장 델타 합계를 tolerance 0으로 비교하고, asset_conservation은
	// Σ(available+locked) + 수수료 == Σ(DEV_FUND delta)를 요구한다. 잔고만 심어 두면
	// 지갑 수만큼 위반이 뜨고, 그러면 성능 수치를 읽을 수 없다.
	wallets := make([]model.Wallet, 0, diagUserCount*2)
	entries := make([]model.LedgerEntry, 0, diagUserCount*4)
	fund := func(userID uint, coin string, available decimal.Decimal, locked decimal.Decimal) {
		total := available.Add(locked)
		entries = append(entries, model.LedgerEntry{
			UserID: userID, CoinSymbol: coin, EntryType: model.LedgerEntryTypeDevFund,
			AvailableDelta: total, LockedDelta: decimal.Zero,
			AvailableBalanceAfter: total, LockedBalanceAfter: decimal.Zero,
			ReferenceType: model.LedgerReferenceTypeDevFund,
			ReferenceKey:  fmt.Sprintf("diag-fixture-fund-%d-%s", userID, coin),
		})
		if locked.GreaterThan(decimal.Zero) {
			entries = append(entries, model.LedgerEntry{
				UserID: userID, CoinSymbol: coin, EntryType: model.LedgerEntryTypeOrderHold,
				AvailableDelta: locked.Neg(), LockedDelta: locked,
				AvailableBalanceAfter: available, LockedBalanceAfter: locked,
				ReferenceType: model.LedgerReferenceTypeOrder,
				ReferenceKey:  fmt.Sprintf("diag-fixture-hold-%d-%s", userID, coin),
			})
		}
	}

	krwAvailable := decimal.NewFromInt(1_000_000)
	coinAvailable := decimal.NewFromInt(1)
	for _, id := range userIDs {
		krwLocked := lockedKRW[id]
		coinLocked := lockedCoin[id]
		wallets = append(wallets, model.Wallet{
			UserID: id, CoinSymbol: model.KRWAssetSymbol,
			KRW:              krwLocked.Add(krwAvailable),
			AvailableBalance: krwAvailable,
			LockedBalance:    krwLocked,
		})
		fund(id, model.KRWAssetSymbol, krwAvailable, krwLocked)

		wallets = append(wallets, model.Wallet{
			UserID: id, CoinSymbol: diagCoin,
			Quantity:         coinLocked.Add(coinAvailable),
			AvailableBalance: coinAvailable,
			LockedBalance:    coinLocked,
		})
		fund(id, diagCoin, coinAvailable, coinLocked)
	}
	require.NoError(t, db.CreateInBatches(&wallets, 500).Error)
	// 원장은 과거 이력보다 **먼저** 넣는다 — ledger_wallet 검사의 legacy_mismatch 분기가
	// (user, coin)별 최초 원장 행으로 초기 잔액을 추정하므로, DEV_FUND가 가장 낮은 ID여야 한다.
	require.NoError(t, db.CreateInBatches(&entries, 500).Error)
	require.NoError(t, db.CreateInBatches(&orders, 500).Error)

	for i, p := range pendings {
		id := orders[i].ID
		require.NotZero(t, id, "주문 ID가 부여되지 않았다")
		switch p.kind {
		case diagJobTradeBatch:
			if p.buy {
				jobs[p.jobIx].Trades[p.slot].BuyOrderID = id
			} else {
				jobs[p.jobIx].Trades[p.slot].SellOrderID = id
			}
		default:
			jobs[p.jobIx].OrderID = id
		}
	}

	diagShuffleJobs(jobs)

	return diagFixture{
		UserIDs:  userIDs,
		Warmup:   jobs[:spec.WarmupJobs],
		Measured: jobs[spec.WarmupJobs:],
	}
}

// ---------------------------------------------------------------------------
// 과거 이력 — 크기 차이는 여기서만 만든다
// ---------------------------------------------------------------------------

// diagUserExpr는 generate_series 값 g를 사용자 ID로 옮기는 결정론적 식이다.
// LCG로 흩뜨린 뒤 power(x, 1.5)로 기울여 **균등이 아닌** 분포를 만든다 — 실부하에서
// 사용자별 주문 수는 균등하지 않고, 균등으로 뭉개면 지갑 행 락 경합이 과소평가된다.
//
// g::bigint 캐스팅은 필수다 — generate_series(1, n)은 int4를 돌려주므로 LCG 곱셈이
// int32 범위를 넘어 "integer out of range"로 실패한다.
func diagUserExpr() string { return diagUserExprOf("g::bigint") }

// diagUserExprOf는 임의의 정수 식을 사용자 ID로 옮긴다. 원장 이력은 4행 그룹이 같은
// 사용자·같은 자산이어야 상계되므로 그룹 번호를 넣어 쓴다.
func diagUserExprOf(expr string) string {
	return fmt.Sprintf(
		`(%d + 1 + floor(%d * power(((((%s) * 1103515245 + 12345) %% 2147483648)::numeric / 2147483648.0), 1.5))::bigint)`,
		diagUserIDBase, diagUserCount, expr)
}

// seedDiagnosticHistory는 **종결된 과거 행만** 넣는다. active fixture는 건드리지 않는다 —
// 그래서 세 크기에서 status IN ('PENDING','PARTIAL') 행 수가 동일하다.
func seedDiagnosticHistory(t testing.TB, db *gorm.DB, size diagSize) {
	t.Helper()

	if size.Orders > 0 {
		require.NoError(t, db.Exec(fmt.Sprintf(`
INSERT INTO orders (user_id, amount, quote_amount, coin_symbol, side, status, filled_amount, filled_quote_amount, created_at, order_type, price)
SELECT %s,
       0.01, 0,
       '%s',
       CASE WHEN g %% 2 = 0 THEN 'BUY' ELSE 'SELL' END,
       CASE WHEN g %% 4 = 0 THEN 'CANCELLED' ELSE 'FILLED' END,
       CASE WHEN g %% 4 = 0 THEN 0 ELSE 0.01 END,
       CASE WHEN g %% 4 = 0 THEN 0 ELSE 500000 END,
       now() - (g %% 600) * interval '1 second',
       'LIMIT',
       50000000 + (g %% 997) * 13
FROM generate_series(1, %d) g`, diagUserExpr(), diagCoin, size.Orders)).Error)
	}

	if size.Trades > 0 {
		// 수수료는 0으로 둔다. asset_conservation 검사가 Σ(지갑) + Σ(trades 수수료) ==
		// Σ(DEV_FUND delta)를 요구하므로, 실제로 지갑에서 빠져나간 적 없는 합성 수수료를
		// 넣으면 그 금액만큼 위반이 된다. 행 수·인덱스 크기가 진단 대상이지 수수료 값이 아니다.
		require.NoError(t, db.Exec(fmt.Sprintf(`
INSERT INTO trades (idempotency_key, engine_sequence, engine_event_id, coin_symbol, price, quantity, fee_rate, buyer_fee, buyer_fee_asset, seller_fee, seller_fee_asset, traded_at, buy_order_id, sell_order_id)
SELECT 'diag-hist-' || g, g, 'diag-hist-evt-' || g,
       '%s',
       50000000 + (g %% 997) * 13,
       0.01, 0, 0, 'KRW', 0, 'KRW',
       now() - (g %% 600) * interval '1 second',
       g, g + 1
FROM generate_series(1, %d) g`, diagCoin, size.Trades)).Error)
	}

	if size.Ledger > 0 {
		// 4행이 한 그룹이고, 그룹 안에서 available/locked 델타 합이 **정확히 0**이다.
		// 그래야 ledger_wallet 검사(지갑 잔고 == 원장 델타 합, tolerance 0)가 유지된다.
		// DEV_FUND는 쓰지 않는다 — asset_conservation의 우변이 DEV_FUND 합이라, 여기에
		// 이력을 섞으면 자산이 허공에서 생긴 것으로 계산된다.
		//
		//   1: ORDER_HOLD       (-1000, +1000)
		//   2: ORDER_RELEASE    (+1000, -1000)
		//   3: TRADE_SETTLEMENT (+1000,     0)
		//   4: TRADE_SETTLEMENT (-1000,     0)
		rows := size.Ledger - size.Ledger%4
		require.NoError(t, db.Exec(fmt.Sprintf(`
INSERT INTO ledger_entries (user_id, coin_symbol, entry_type, available_delta, locked_delta, available_balance_after, locked_balance_after, reference_type, reference_id, reference_key, created_at)
SELECT %s,
       CASE WHEN ((g - 1) / 4) %% 2 = 0 THEN 'KRW' ELSE '%s' END,
       CASE (g - 1) %% 4 WHEN 0 THEN 'ORDER_HOLD' WHEN 1 THEN 'ORDER_RELEASE' ELSE 'TRADE_SETTLEMENT' END,
       CASE (g - 1) %% 4 WHEN 0 THEN -1000 WHEN 1 THEN 1000 WHEN 2 THEN 1000 ELSE -1000 END,
       CASE (g - 1) %% 4 WHEN 0 THEN 1000 WHEN 1 THEN -1000 ELSE 0 END,
       1000000,
       CASE (g - 1) %% 4 WHEN 0 THEN 1000 ELSE 0 END,
       CASE WHEN (g - 1) %% 4 >= 2 THEN 'TRADE' ELSE 'ORDER' END,
       -- reference_id는 fixture 주문 ID와 겹치면 안 된다. 겹치면 실작업 검증이
       -- 이력 행을 "이번 실행이 만든 ORDER_RELEASE"로 오인한다.
       g + %d, 'diag-hist-' || g,
       now() - (g %% 600) * interval '1 second'
FROM generate_series(1, %d) g`, diagUserExprOf("((g::bigint - 1) / 4)"), diagCoin, diagHistoryRefIDBase, rows)).Error)
	}

	if size.Outbox > 0 {
		require.NoError(t, db.Exec(fmt.Sprintf(`
INSERT INTO trade_outbox_events (event_type, coin_symbol, engine_event_id, payload, status, created_at, processed_at)
SELECT CASE WHEN g %% 10 = 0 THEN 'ORDER_CANCELLED' WHEN g %% 10 = 1 THEN 'MARKET_ORDER_DONE' ELSE 'TRADE' END,
       '%s', 'diag-hist-evt-' || g,
       jsonb_build_object('diag', true, 'g', g),
       'PROCESSED',
       now() - (g %% 600) * interval '1 second',
       now() - (g %% 600) * interval '1 second'
FROM generate_series(1, %d) g`, diagCoin, size.Outbox)).Error)
	}

	require.NoError(t, db.Exec(`ANALYZE`).Error)
}

// ---------------------------------------------------------------------------
// 검증 도구
// ---------------------------------------------------------------------------

// diagFixtureHash는 측정 대상 fixture 행만 해시한다. 과거 이력이 섞이면 세 크기에서
// 값이 달라지므로, **active 주문(PENDING/PARTIAL)과 진단 사용자·지갑만** 대상으로 한다.
func diagFixtureHash(t testing.TB, db *gorm.DB) string {
	t.Helper()

	var hash string
	require.NoError(t, db.Raw(`
SELECT md5(string_agg(line, '|' ORDER BY line)) FROM (
  SELECT 'o:' || id || ':' || user_id || ':' || side || ':' || order_type || ':' || status || ':' ||
         amount || ':' || quote_amount || ':' || filled_amount || ':' || filled_quote_amount || ':' || price AS line
  FROM orders WHERE status IN ('PENDING','PARTIAL')
  UNION ALL
  SELECT 'w:' || user_id || ':' || coin_symbol || ':' || krw || ':' || quantity || ':' ||
         available_balance || ':' || locked_balance
  FROM wallets WHERE user_id > ?
  UNION ALL
  SELECT 'u:' || id || ':' || name FROM users WHERE id > ?
  UNION ALL
  SELECT 'l:' || user_id || ':' || coin_symbol || ':' || entry_type || ':' ||
         available_delta || ':' || locked_delta
  FROM ledger_entries WHERE reference_key LIKE 'diag-fixture-%%'
) s`, diagUserIDBase, diagUserIDBase).Scan(&hash).Error)
	require.NotEmpty(t, hash, "fixture 해시가 비어 있다 — fixture가 시드되지 않았다")
	return hash
}

// diagRowCounts는 크기 축 검증에 쓴다. **fixture 분과 과거 이력 분을 분리해서** 센다 —
// 합쳐 세면 "이력 행 수가 목표치인가"를 확인할 수 없다.
type diagRowCounts struct {
	Orders        int64 `json:"orders"`
	Trades        int64 `json:"trades"`
	Ledger        int64 `json:"ledger_entries"`
	Outbox        int64 `json:"trade_outbox_events"`
	ActiveOrders  int64 `json:"active_orders"`
	FixtureLedger int64 `json:"fixture_ledger_entries"`
	Users         int64 `json:"users"`
	Wallets       int64 `json:"wallets"`
}

func diagCountRows(t testing.TB, db *gorm.DB) diagRowCounts {
	t.Helper()

	var c diagRowCounts
	count := func(sql string, dst *int64) {
		require.NoError(t, db.Raw(sql).Scan(dst).Error)
	}
	count(`SELECT count(*) FROM orders`, &c.Orders)
	count(`SELECT count(*) FROM trades`, &c.Trades)
	count(`SELECT count(*) FROM ledger_entries`, &c.Ledger)
	count(`SELECT count(*) FROM trade_outbox_events`, &c.Outbox)
	count(`SELECT count(*) FROM orders WHERE status IN ('PENDING','PARTIAL')`, &c.ActiveOrders)
	count(`SELECT count(*) FROM ledger_entries WHERE reference_key LIKE 'diag-fixture-%'`, &c.FixtureLedger)
	count(`SELECT count(*) FROM users`, &c.Users)
	count(`SELECT count(*) FROM wallets`, &c.Wallets)
	return c
}

// diagBuildTemplates는 크기별 template DB를 1회 만든다. 반환 후에는 template에 접속이
// 남아 있지 않다(subtest 스코프에서 열고 닫는다).
func diagBuildTemplates(t *testing.T, admin *gorm.DB) map[string]diagRowCounts {
	t.Helper()

	counts := make(map[string]diagRowCounts, 3)
	for _, size := range diagSizeSet() {
		name := diagTemplateName(size.Name)
		diagCreateDatabase(t, admin, name, "")
		t.Run("seed-"+size.Name, func(t *testing.T) {
			db := diagOpenMigrated(t, name)
			started := time.Now()
			seedDiagnosticFixture(t, db)
			seedDiagnosticHistory(t, db, size)
			counts[size.Name] = diagCountRows(t, db)
			t.Logf("template %s 시드 완료: %+v (%.1fs)", name, counts[size.Name], time.Since(started).Seconds())
		})
	}
	return counts
}

// ---------------------------------------------------------------------------
// Phase 1·2 검증 테스트
// ---------------------------------------------------------------------------

// 크기 축이 정말 크기만 움직이는지 검증한다. 여기가 틀리면 6셀 결과 전체가 무의미하다.
func TestSettlementDiagnosticSeedIsolatesSizeAxis(t *testing.T) {
	diagRequireOptIn(t)
	admin := diagOpenAdmin(t)

	counts := diagBuildTemplates(t, admin)
	hashes := make(map[string]string, 3)
	active := make(map[string]int64, 3)

	for _, size := range diagSizeSet() {
		t.Run("verify-"+size.Name, func(t *testing.T) {
			db := diagOpenMigrated(t, diagTemplateName(size.Name))
			hashes[size.Name] = diagFixtureHash(t, db)
			active[size.Name] = counts[size.Name].ActiveOrders

			// 과거 이력 행 수가 목표의 ±1% 이내인가.
			got := counts[size.Name]
			assertWithinOnePercent(t, size.Trades, got.Trades, "trades")
			assertWithinOnePercent(t, size.Outbox, got.Outbox, "trade_outbox_events")
			// orders·ledger_entries는 fixture 분이 더해지므로 그만큼 빼고 비교한다.
			assertWithinOnePercent(t, size.Orders, got.Orders-got.ActiveOrders, "orders(history)")
			assertWithinOnePercent(t, size.Ledger, got.Ledger-got.FixtureLedger, "ledger_entries(history)")

			require.EqualValues(t, diagUserCount, got.Users)
			require.EqualValues(t, diagUserCount*2, got.Wallets)

			// 시드 직후에 이미 정합성이 깨져 있으면 측정은 무의미하다. 합성 시드는
			// 지갑 잔고를 원장으로 설명할 수 있어야 하고, 과거 이력은 그 균형을 흔들면 안 된다.
			(&ReconciliationWorker{Repository: repository.NewReconciliationRepository(db)}).RunOnce()
			var violations int64
			require.NoError(t, db.Raw(`SELECT count(*) FROM reconciliation_violations`).Scan(&violations).Error)
			require.Zero(t, violations, "시드 직후 정합성 위반 %d건 — 합성 시드가 원장 불변식을 깼다", violations)
			require.NoError(t, db.Exec(`DELETE FROM reconciliation_violations`).Error)
		})
	}

	sizes := diagSizeSet()
	base := sizes[0].Name
	for _, size := range sizes[1:] {
		require.Equal(t, hashes[base], hashes[size.Name],
			"fixture 해시가 크기 %s에서 달라졌다 — 크기 축이 fixture를 오염시켰다", size.Name)
		require.Equal(t, active[base], active[size.Name],
			"active 주문 수가 크기 %s에서 달라졌다", size.Name)
	}
	t.Logf("fixture 해시(세 크기 동일): %s / active 주문 %d행", hashes[base], active[base])
}

// TEMPLATE 복제가 원본과 정확히 같은지, 그리고 6번 연속 복제해도 실패하지 않는지 본다.
// 접속이 남아 있으면 CREATE DATABASE ... TEMPLATE 은 실패한다 — 그것이 유일한 반복 실패 원인이다.
func TestSettlementDiagnosticTemplateCloneMatchesTemplate(t *testing.T) {
	diagRequireOptIn(t)
	admin := diagOpenAdmin(t)

	size := diagSizeSet()[0] // 복제 등가성 검증에 크기는 무관하다 — 가장 싼 것을 쓴다
	template := diagTemplateName(size.Name)
	diagCreateDatabase(t, admin, template, "")
	var want diagRowCounts
	var wantHash string
	t.Run("seed-template", func(t *testing.T) {
		db := diagOpenMigrated(t, template)
		seedDiagnosticFixture(t, db)
		seedDiagnosticHistory(t, db, size)
		want = diagCountRows(t, db)
		wantHash = diagFixtureHash(t, db)
	})

	for i := 0; i < 6; i++ {
		cell := fmt.Sprintf("goex_diag_clone_%d", i)
		diagCreateDatabase(t, admin, cell, template)
		t.Run(fmt.Sprintf("clone-%d", i), func(t *testing.T) {
			db := diagOpenMigrated(t, cell)
			require.Equal(t, want, diagCountRows(t, db), "복제 %d의 행 수가 template과 다르다", i)
			require.Equal(t, wantHash, diagFixtureHash(t, db), "복제 %d의 fixture 해시가 template과 다르다", i)

			// 복제 직후 ANALYZE가 실제로 수행되는지 — 통계가 없으면 실행 계획이 달라진다.
			require.NoError(t, db.Exec(`ANALYZE`).Error)
			var analyzed int64
			require.NoError(t, db.Raw(`SELECT count(*) FROM pg_stat_user_tables
                WHERE relname IN ('orders','trades','ledger_entries','wallets') AND last_analyze IS NOT NULL`).
				Scan(&analyzed).Error)
			require.EqualValues(t, 4, analyzed, "복제 후 ANALYZE가 반영되지 않았다")
		})
		diagDropDatabase(t, admin, cell)
	}
}

func assertWithinOnePercent(t testing.TB, want int, got int64, label string) {
	t.Helper()

	if want == 0 {
		require.EqualValues(t, 0, got, "%s: 0이어야 한다", label)
		return
	}
	diff := math.Abs(float64(got)-float64(want)) / float64(want)
	require.LessOrEqual(t, diff, 0.01, "%s: 목표 %d 대비 실제 %d (오차 %.3f%%)", label, want, got, diff*100)
}
