# B. 시장가 완료 복구 dependency guard 구현 계획 (정합성 수정)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:executing-plans로 태스크별 실행.
> Steps use checkbox (`- [ ]`). superpowers:test-driven-development로 RED→GREEN.

**Goal:** 미정산 trade 위에서 시장가 완료가 실행돼 **잔여 홀드를 잘못 반환**하는 현재의 정합성
구멍을 닫는다. 같은 주문을 참조하는 `OPEN` failed settlement가 있으면 terminal을 **durable defer**
한다.

**Architecture:** `HasOpenFailureForOrder(orderID)`를 repository→service 인터페이스→service→워커
store **네 계층에 추가**하고, `retryFailedCompletions`가 completion 실행 전에 호출한다. 판정은
**DB `EXISTS`**(메모리 검색 금지). 오류·nil store는 **phase 전체 중단**(fail-closed).

**Tech Stack:** Go, GORM, prometheus/client_golang.

**스펙 문서:** `docs/superpowers/specs/2026-07-29-failed-completion-dependency-guard-design.md`

## Global Constraints

- **정산·매칭·dispatcher 동작 변경 금지.** 이번 변경은 **복구 경로(`SettlementRetryWorker`)와 그
  조회 계층**에 한정한다. 4차 축 1의 A(런타임 fence)·C(취소 durable defer)는 **범위 밖**.
- **스키마 변경 금지** — `NextRetryAt`·`EngineSequence` 추가 없음. 기존 컬럼만 사용.
- **`ListOpenFailures` 결과를 메모리에서 검색하지 않는다** — batch limit(50) 밖을 놓쳐 fail-open이 된다.
- **fail-closed는 phase 단위**: store nil 또는 조회 오류 → 오류 로그 **1회** 후 이번 `RunOnce()`의
  **completion phase 전체 중단**.
- **차단은 오류가 아니다** — 로그 대신 counter. `settlement_completion_blocked_total`은 **차단된
  polling 횟수**이며 unique order 수가 아니다. gauge는 만들지 않는다.
- 커밋 전 `commit-message` 스킬(author→reviewer, 한글). 고장 시 `git diff --cached`로 직접 작성하고 보고.
- 통합 테스트 DSN(포트 55432). 최종 검증에 `-race` 포함.

---

### Task 1: repository — `HasOpenFailureForOrder` (EXISTS)

**Files:**
- Modify: `internal/repository/failed_settlement_repository.go`
- Test: `internal/repository/failed_settlement_repository_integration_test.go`

**Interfaces:**
- Produces: `func (r *FailedSettlementRepository) HasOpenFailureForOrder(orderID uint) (bool, error)`

- [x] **Step 1: 실패 테스트** — 기존 통합 테스트 파일에 추가(기존 시드 헬퍼 재사용):

```go
func TestIntegrationHasOpenFailureForOrderMatchesBuyAndSellSide(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewFailedSettlementRepository(db)
	// buy_order_id = 1001, sell_order_id = 2001인 OPEN 실패 1건 시드
	seedOpenFailedSettlement(t, db, 1001, 2001)

	has, err := repo.HasOpenFailureForOrder(1001) // maker 측
	require.NoError(t, err)
	assert.True(t, has, "buy_order_id 매칭")

	has, err = repo.HasOpenFailureForOrder(2001) // taker 측
	require.NoError(t, err)
	assert.True(t, has, "sell_order_id 매칭")

	has, err = repo.HasOpenFailureForOrder(9999) // unrelated
	require.NoError(t, err)
	assert.False(t, has)
}

// batch limit 회귀 고정: 앞쪽에 unrelated OPEN 50건 이상을 깔아도 찾아야 한다.
func TestIntegrationHasOpenFailureForOrderIgnoresListLimit(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewFailedSettlementRepository(db)
	for i := 0; i < 60; i++ { // unrelated 60건 먼저
		seedOpenFailedSettlement(t, db, uint(3000+i), uint(4000+i))
	}
	seedOpenFailedSettlement(t, db, 7001, 7002) // 그 뒤에 대상

	has, err := repo.HasOpenFailureForOrder(7001)
	require.NoError(t, err)
	assert.True(t, has, "ListOpenFailures(50) 메모리 검색이면 여기서 false가 된다")
}

// RESOLVED는 dependency가 아니다.
func TestIntegrationHasOpenFailureForOrderIgnoresResolved(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewFailedSettlementRepository(db)
	f := seedOpenFailedSettlement(t, db, 5001, 5002)
	require.NoError(t, repo.MarkResolved(f.ID, "manual", "test", ""))

	has, err := repo.HasOpenFailureForOrder(5001)
	require.NoError(t, err)
	assert.False(t, has)
}
```

Run: `go test ./internal/repository/... -run HasOpenFailureForOrder -v` → FAIL(undefined).

- [x] **Step 2: 구현** — `failed_settlement_repository.go`(기존 `FindOpen` 스타일 그대로):

```go
// HasOpenFailureForOrder는 해당 주문을 maker 또는 taker로 참조하는 OPEN 실패가 있는지
// DB에서 EXISTS로 판정한다. ListOpenFailures(limit) 결과를 메모리에서 검색하면 batch
// limit 밖의 dependency를 놓쳐 fail-open이 되므로 반드시 이 경로를 쓴다.
func (r *FailedSettlementRepository) HasOpenFailureForOrder(orderID uint) (bool, error) {
	if r == nil || r.DB == nil {
		return false, fmt.Errorf("failed settlement repository DB is required")
	}
	if orderID == 0 {
		return false, fmt.Errorf("order id is required")
	}

	var exists bool
	err := r.DB.Raw(
		`SELECT EXISTS (
			SELECT 1 FROM failed_settlements
			WHERE status = ? AND (buy_order_id = ? OR sell_order_id = ?)
		)`,
		model.FailedSettlementStatusOpen, orderID, orderID,
	).Scan(&exists).Error
	return exists, err
}
```

Run: 위 3종 → PASS.

- [x] **Step 3: Commit** — 초안: `feat(repository): 주문별 OPEN 정산 실패 존재 조회 추가 (B)` (커밋 `3ec09cd`)

---

### Task 2: service 계층 통과 + 메트릭

**Files:**
- Modify: `internal/service/failed_settlement_service.go`(인터페이스 + 메서드)
- Modify: `internal/metrics/metrics.go`
- Test: `internal/service/failed_settlement_service_test.go`, `internal/metrics/...`

**Interfaces:**
- Produces: `failedSettlementRepository`에 `HasOpenFailureForOrder(orderID uint) (bool, error)` 추가,
  `func (s *FailedSettlementService) HasOpenFailureForOrder(orderID uint) (bool, error)`,
  `metrics.SettlementCompletionBlockedTotal prometheus.Counter`

- [x] **Step 1: 실패 테스트** — service가 repository로 위임하고, nil repo는 오류:
  (`fakeFailedSettlementRepo`가 아닌 기존 `fakeFailedSettlementRepository`를 확장해 재사용 — 같은 패키지에
  구조적으로 동일한 fake가 중복 생기는 것을 피함)

```go
func TestHasOpenFailureForOrderDelegatesToRepository(t *testing.T) {
	repo := &fakeFailedSettlementRepo{hasOpen: true}
	svc := &FailedSettlementService{Repository: repo}

	has, err := svc.HasOpenFailureForOrder(1001)
	require.NoError(t, err)
	assert.True(t, has)
	assert.Equal(t, uint(1001), repo.lastQueriedOrderID)
}

func TestHasOpenFailureForOrderRequiresRepository(t *testing.T) {
	svc := &FailedSettlementService{}
	_, err := svc.HasOpenFailureForOrder(1001)
	assert.Error(t, err, "repository 없으면 오류 — 호출자가 fail-closed로 처리한다")
}
```

Run: `go test ./internal/service/... -run HasOpenFailureForOrder -v` → FAIL.

- [x] **Step 2: 구현** — 인터페이스에 메서드 추가 + 서비스 위임(기존 `ListOpenFailures` 패턴):

```go
type failedSettlementRepository interface {
	RecordFailure(failure *model.FailedSettlement) (*model.FailedSettlement, error)
	FindOpen(limit int) ([]model.FailedSettlement, error)
	FindByID(id uint) (*model.FailedSettlement, error)
	MarkResolved(id uint, resolution string, resolvedBy string, notes string) error
	HasOpenFailureForOrder(orderID uint) (bool, error)
}

func (s *FailedSettlementService) HasOpenFailureForOrder(orderID uint) (bool, error) {
	if s == nil || s.Repository == nil {
		return false, fmt.Errorf("failed settlement repository is required")
	}
	return s.Repository.HasOpenFailureForOrder(orderID)
}
```

`metrics.go`의 `var (...)`에 추가:

```go
	// 차단된 polling "횟수"다 — 현재 차단된 주문 수가 아니다(rate/increase로 지속 차단 탐지).
	SettlementCompletionBlockedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "settlement_completion_blocked_total",
		Help: "Times a market-completion retry was skipped due to an OPEN failed settlement dependency.",
	})
```

**기존 fake/mock에 메서드 추가**가 필요하다(컴파일 실패로 드러난다) — 전부 채운다.

Run: `go test ./internal/service/... ./internal/metrics/... -count=1` → PASS.

- [x] **Step 3: Commit** — 초안: `feat(settlement): dependency 조회 서비스 계층과 차단 카운터 추가 (B)`

---

### Task 3: 워커 guard 배선 (핵심, TDD)

**Files:**
- Modify: `internal/service/settlement_retry_worker.go`
- Test: `internal/service/settlement_retry_worker_test.go`

**Interfaces:**
- Consumes: `retryFailedSettlementStore`에 `HasOpenFailureForOrder(orderID uint) (bool, error)` 추가.

- [x] **Step 1: 실패 테스트(테이블 테스트 1개로 묶어도 됨)** — 스펙의 경계 전부:
  (테이블 테스트의 `assert.False(t, completions.resolved, ...)` 단언은 원안 그대로 쓰면 "dependency
  없으면 실행" 케이스에서 실제로는 정상 완료·resolve돼야 하는데 항상 false를 기대해 모순이었다 —
  `assert.Equal(t, tc.wantCompleted, completions.resolved)`로 고쳐 케이스별로 옳게 검증하게 했다.)

```go
func TestRetryFailedCompletionsRespectsDependencyGuard(t *testing.T) {
	cases := []struct {
		name            string
		hasOpen         bool
		hasOpenErr      error
		nilStore        bool
		wantCompleted   bool   // CompleteMarketOrder 호출됐나
		wantBlockedInc  bool   // blocked counter 증가했나
	}{
		{name: "OPEN dependency면 차단", hasOpen: true, wantCompleted: false, wantBlockedInc: true},
		{name: "dependency 없으면 실행", hasOpen: false, wantCompleted: true, wantBlockedInc: false},
		{name: "조회 오류면 fail-closed", hasOpenErr: errors.New("db down"), wantCompleted: false, wantBlockedInc: false},
		{name: "store가 nil이면 phase 중단", nilStore: true, wantCompleted: false, wantBlockedInc: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			completer := &fakeMarketCompleter{}
			completions := &fakeCompletionStore{failures: []model.FailedMarketCompletion{
				{ID: 1, OrderID: 1001, RetryCount: 1, Status: model.FailedSettlementStatusOpen},
			}}
			w := &SettlementRetryWorker{MarketCompleter: completer, FailedCompletions: completions}
			if !tc.nilStore {
				w.FailedSettlements = &fakeSettlementStore{hasOpen: tc.hasOpen, hasOpenErr: tc.hasOpenErr}
			}
			before := testutil.ToFloat64(metrics.SettlementCompletionBlockedTotal)

			w.RunOnce()

			assert.Equal(t, tc.wantCompleted, completer.called)
			assert.Equal(t, uint(1), completions.failures[0].RetryCount, "차단 시 retry count 미소비")
			assert.False(t, completions.resolved, "차단 시 성공 처리 금지")
			assert.False(t, completions.recordedFailure, "차단 시 실패 처리 금지")
			after := testutil.ToFloat64(metrics.SettlementCompletionBlockedTotal)
			if tc.wantBlockedInc {
				assert.Equal(t, before+1, after)
			} else {
				assert.Equal(t, before, after)
			}
		})
	}
}

// phase 중단: 첫 조회가 실패하면 뒤 completion도 이번 사이클엔 실행되지 않는다.
func TestRetryFailedCompletionsAbortsPhaseOnQueryError(t *testing.T) {
	completer := &fakeMarketCompleter{}
	completions := &fakeCompletionStore{failures: []model.FailedMarketCompletion{
		{ID: 1, OrderID: 1001, RetryCount: 1, Status: model.FailedSettlementStatusOpen},
		{ID: 2, OrderID: 1002, RetryCount: 1, Status: model.FailedSettlementStatusOpen},
	}}
	w := &SettlementRetryWorker{
		MarketCompleter:   completer,
		FailedCompletions: completions,
		FailedSettlements: &fakeSettlementStore{hasOpenErr: errors.New("db down")},
	}

	w.RunOnce()

	assert.False(t, completer.called, "첫 조회 실패 시 뒤 completion도 미실행")
	assert.Equal(t, 1, w.FailedSettlements.(*fakeSettlementStore).hasOpenCalls,
		"phase 중단이므로 조회도 1회만")
}

// 해결 후 다음 RunOnce에서 실행된다(핵심 흐름).
func TestRetryFailedCompletionsRunsAfterDependencyResolved(t *testing.T) {
	completer := &fakeMarketCompleter{}
	settlements := &fakeSettlementStore{hasOpen: true}
	completions := &fakeCompletionStore{failures: []model.FailedMarketCompletion{
		{ID: 1, OrderID: 1001, RetryCount: 1, Status: model.FailedSettlementStatusOpen},
	}}
	w := &SettlementRetryWorker{MarketCompleter: completer, FailedCompletions: completions,
		FailedSettlements: settlements}

	w.RunOnce()
	require.False(t, completer.called, "dependency OPEN이면 미실행")

	settlements.hasOpen = false // 복구됨
	w.RunOnce()
	assert.True(t, completer.called, "해결 후 다음 RunOnce에서 실행")
}
```

Run: `go test ./internal/service/... -run RetryFailedCompletions -v` → FAIL.

- [x] **Step 2: 구현** — 인터페이스 확장 + `retryFailedCompletions` guard:
  (기존 completion 전용 테스트 3종은 `FailedSettlements`를 아예 설정하지 않았는데, 새 fail-closed
  규칙상 nil이면 phase가 중단돼 completer가 0회 호출된다 — 각 테스트에
  `FailedSettlements: &fakeFailedSettlementStore{}`(기본값 `HasOpenFailureForOrder`→false,nil)를
  추가해 기존 단언은 그대로 두고 guard가 있어도 정상 흐름이 통과함을 확인했다.)

```go
type retryFailedSettlementStore interface {
	ListOpenFailures(limit int) ([]model.FailedSettlement, error)
	ResolveFailure(input ResolveFailureInput) (*model.FailedSettlement, error)
	RecordFailure(trade *model.Trade, settlementErr error) (*model.FailedSettlement, error)
	HasOpenFailureForOrder(orderID uint) (bool, error)
}
```

```go
func (w *SettlementRetryWorker) retryFailedCompletions() {
	if w.MarketCompleter == nil || w.FailedCompletions == nil {
		return
	}
	// fail-closed: dependency를 확인할 수단이 없으면 terminal을 실행하지 않는다.
	if w.FailedSettlements == nil {
		w.logf("retry worker: dependency store unavailable, skipping completion phase")
		return
	}

	failures, err := w.FailedCompletions.ListOpenFailures(settlementRetryBatchLimit)
	if err != nil {
		w.logf("retry worker: list open failed market completions failed: %v", err)
		return
	}

	for i := range failures {
		failure := &failures[i]
		if failure.RetryCount >= w.maxRetryCount() {
			continue
		}

		// 같은 주문을 참조하는 OPEN 정산 실패가 있으면 terminal을 durable defer 한다.
		// 조회 오류는 fail-closed — 오류 로그 1회 후 이번 사이클의 completion phase 전체 중단.
		hasOpen, depErr := w.FailedSettlements.HasOpenFailureForOrder(failure.OrderID)
		if depErr != nil {
			w.logf("retry worker: dependency check failed for order %d: %v", failure.OrderID, depErr)
			return
		}
		if hasOpen {
			metrics.SettlementCompletionBlockedTotal.Inc()
			continue // 차단은 정상 동작이라 로그를 남기지 않는다
		}

		// ... 기존 completion 복구 로직 그대로 ...
	}
}
```

Run: 위 테스트 전부 → PASS.

- [x] **Step 3: Commit** — 초안: `fix(settlement): 미정산 trade 위 시장가 완료 복구를 차단 (B)`

---

### Task 4: 전체 검증 + 문서

- [x] **Step 1: 전체 검증** — `go build ./...` + `go vet` + `go test ./... -count=1`(통합 SKIP 0,
  DSN 55432) + `go test ./internal/service/... ./internal/repository/... ./cmd/... -race -count=1`.
  **기존 워커·정산·복구 테스트가 무수정 그린**이어야 한다(동작 축소가 아니라 가드 추가임의 증거).
  (completion 전용 3종은 새 fail-closed 규칙상 `FailedSettlements` 필드 추가가 필요해 그 한 줄만
  보강 — 기존 단언은 무수정.)
- [x] **Step 2: 완료 문서** — `docs/refactor/21_시장가완료_dependency_guard_완료.md`:
  왜(현재 fail-open 구멍 — 비-transient·retry 소진이 `OPEN`으로 남는데 completion은 실행됨) /
  어떻게(EXISTS 조회, 네 계층 통과, phase 단위 fail-closed, counter) / 결과(테스트·회귀 그린) /
  **범위 밖**(A 런타임 fence·C 취소 durable defer) 명기.
- [x] **Step 3: README** — 4차 축 1 현재 단계에 "B 완료 → 다음 A+C" 반영.
- [ ] **Step 4: Commit + 푸시 + CI** — `gh run watch` 그린.

---

## 다음 (범위 밖)

**A. per-order runtime fence**(dispatcher의 파티션 전체 배리어를 주문별 dependency fence로 축소) +
**C. cancel terminal의 durable defer 계약** — B가 "복구 경로는 순서를 지킨다"는 전제를 확보한 뒤
하나의 스펙으로 설계한다.
