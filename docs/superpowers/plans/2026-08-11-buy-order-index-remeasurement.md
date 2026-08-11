# `trades.buy_order_id` 인덱스 단독 수정·재측정 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 운영 변경을 `trades(buy_order_id)` 단일 인덱스로 제한하고, 로컬 6셀과 GCP 500/750 VU 사전 등록 게이트로 비용 기울기 제거와 용량 경계 이동을 분리 판정한다.

**Architecture:** goose의 non-transactional concurrent migration으로 인덱스를 추가한다. 기존 settlement diagnostic은 workload를 바꾸지 않고 출력 위치만 격리해 2회 반복한다. 로컬 PASS 후에만 32-B와 같은 GCP capacity 프로파일을 500 VU 1회, 750 VU 최소 1회·최대 2회 실행한다.

**Tech Stack:** Go 1.25.7, goose v3.27.1, GORM, PostgreSQL, testify, Python 3, k6, Docker Compose, GCE, PowerShell.

**Design:** [2026-08-11-buy-order-index-remeasurement-design.md](../specs/2026-08-11-buy-order-index-remeasurement-design.md)

## 고정 제약과 판정 순서

```text
index migration
  → local 6 cells × 2 runs
      FAIL: stop
      PASS → GCP 500 × 1
                 FAIL: stop
                 PASS → GCP 750 run 1
                            FAIL: stop, ceiling=500
                            PASS → independent 750 confirmation
                                       FAIL: ceiling=500
                                       PASS: ceiling≥750
```

- 운영 변경은 non-unique single-column B-tree `idx_trades_buy_order_id` 하나뿐이다.
- GORM tag, production Go code, k6 workload/API 계약, settlement metric label은 바꾸지 않는다.
- migration은 `-- +goose NO TRANSACTION`, `CREATE INDEX CONCURRENTLY IF NOT EXISTS`, 같은 Up 안의 catalog 검증·`RAISE EXCEPTION`을 사용한다.
- 인덱스가 같은 이름의 invalid/wrong definition이면 goose version을 기록하지 않는다. 복구는 정확한 이름을 확인한 뒤 concurrent drop→Up 재실행이다.
- 로컬 주 판정은 `median(full p50 run1/run2) / median(initial p50 run1/run2) ≤ 1.40`, N=1·N=8 모두다.
- 중앙값은 PASS지만 raw 회차 하나가 1.40을 넘으면 PASS를 유지하고 한계로 기록한다. threshold 변경이나 추가 회차 선택을 금지한다.
- GCP 500은 `job_growth≤1.40`과 모든 100%/0건 계약을 동시에 만족해야 한다.
- 750 첫 FAIL은 즉시 탈락, 첫 PASS만 독립 확증 실행, 두 실행 모두 PASS해야 최고 검증 용량을 최소 750으로 올린다.
- 모든 GCP snapshot에 trade/cancel/market_done/total job count를 보존한다. job mix는 진단값이지 PASS threshold가 아니다.
- 성능 수치는 정합성·fallback·quarantine·duplicate·reconciliation 위반이 모두 0일 때만 읽는다.
- 기존 `_workspace`는 삭제·덮어쓰기하지 않는다. 새 산출물은 `_workspace/buy-order-index-remeasurement/` 아래만 쓴다.
- 각 코드 커밋 전 `commit-message` skill의 author→reviewer PASS를 받는다.
- token, password, secret hash/fingerprint를 출력하거나 산출물에 저장하지 않는다.

## 파일 지도

Tracked:

- Create: `migrations/006_trades_buy_order_id_index.sql`
- Modify: `internal/dbmigration/runner_test.go`
- Create: `internal/dbmigration/trades_buy_order_index_integration_test.go`
- Modify: `.github/workflows/backend-ci.yml`
- Modify: `internal/service/settlement_diagnostic_test.go`
- Create after measurement: `docs/benchmarks/34-2026-08-11-buy-order-index-remeasurement.md`
- Modify after measurement: `docs/refactor/README.md`, `docs/ENGINEERING-SUMMARY.md`

Untracked:

- `_workspace/buy-order-index-remeasurement/local/run1/`, `run2/`
- `_workspace/buy-order-index-remeasurement/scripts/`
- `_workspace/buy-order-index-remeasurement/gcp/index500r1/`
- `_workspace/buy-order-index-remeasurement/gcp/index750r1/`
- 조건부 `_workspace/buy-order-index-remeasurement/gcp/index750r2/`

---

### Task 1: Concurrent index migration을 TDD로 추가

**Files:**

- Create: `migrations/006_trades_buy_order_id_index.sql`
- Modify: `internal/dbmigration/runner_test.go`
- Create: `internal/dbmigration/trades_buy_order_index_integration_test.go`
- Modify: `.github/workflows/backend-ci.yml`

- [ ] **Step 1: migration 정적 계약 RED를 작성한다**

`internal/dbmigration/runner_test.go`에 `TestTradesBuyOrderIDIndexMigrationIsConcurrentAndValidated`를 추가해 다음 문자열을 모두 요구한다.

```go
assert.True(t, strings.HasPrefix(sql, "-- +goose NO TRANSACTION\n"))
assert.Contains(t, sql, "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_trades_buy_order_id")
assert.Contains(t, sql, "ON trades (buy_order_id)")
assert.Contains(t, sql, "indisready")
assert.Contains(t, sql, "indisvalid")
assert.Contains(t, sql, "RAISE EXCEPTION")
assert.Contains(t, sql, "DROP INDEX CONCURRENTLY IF EXISTS idx_trades_buy_order_id")
```

```powershell
go test ./internal/dbmigration -run TestTradesBuyOrderIDIndexMigrationIsConcurrentAndValidated -v -count=1
```

Expected: migration 파일 부재로 FAIL.

- [ ] **Step 2: 실제 catalog integration RED를 작성한다**

`testdb.OpenIntegrationDB(t)`로 migration runner를 거친 DB에서 `pg_class`, `pg_index`, `pg_attribute`, `pg_am`을 조회한다. 다음을 모두 assert한다.

```text
schema/table/index = public/trades/idx_trades_buy_order_id
indisready=true, indisvalid=true, indisunique=false
indnkeyatts=1, indnatts=1
access method=btree, first column=buy_order_id
indexprs IS NULL, indpred IS NULL
pg_get_indexdef contains ON public.trades USING btree (buy_order_id)
```

```powershell
docker compose -f docker-compose.test.yml up -d
$env:GOEXCHANGE_TEST_DATABASE_DSN='host=localhost user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=55432 sslmode=disable'
go test ./internal/dbmigration -run TestTradesBuyOrderIDIndexIntegration -v -count=1
```

Expected: index 부재로 FAIL. 공유 DB에 수동 index가 이미 있어 RED가 재현되지 않으면 임의 drop하지 말고 작업을 중단한다.

- [ ] **Step 3: 최소 migration을 작성한다**

`migrations/006_trades_buy_order_id_index.sql`:

```sql
-- +goose NO TRANSACTION

-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_trades_buy_order_id
    ON trades (buy_order_id);

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_class index_rel
        JOIN pg_namespace index_ns ON index_ns.oid = index_rel.relnamespace
        JOIN pg_index index_meta ON index_meta.indexrelid = index_rel.oid
        JOIN pg_class table_rel ON table_rel.oid = index_meta.indrelid
        JOIN pg_namespace table_ns ON table_ns.oid = table_rel.relnamespace
        JOIN pg_am access_method ON access_method.oid = index_rel.relam
        JOIN pg_attribute column_meta
          ON column_meta.attrelid = table_rel.oid
         AND column_meta.attnum = index_meta.indkey[0]
        WHERE index_ns.nspname = current_schema()
          AND table_ns.nspname = current_schema()
          AND table_rel.relname = 'trades'
          AND index_rel.relname = 'idx_trades_buy_order_id'
          AND access_method.amname = 'btree'
          AND column_meta.attname = 'buy_order_id'
          AND index_meta.indnkeyatts = 1
          AND index_meta.indnatts = 1
          AND index_meta.indisready
          AND index_meta.indisvalid
          AND NOT index_meta.indisunique
          AND index_meta.indexprs IS NULL
          AND index_meta.indpred IS NULL
    ) THEN
        RAISE EXCEPTION 'idx_trades_buy_order_id is missing, invalid, or has the wrong definition';
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_trades_buy_order_id;
```

- [ ] **Step 4: GREEN과 CI wiring을 검증한다**

```powershell
go test ./internal/dbmigration -run TestTradesBuyOrderIDIndexMigrationIsConcurrentAndValidated -v -count=1
go test ./internal/dbmigration -run TestTradesBuyOrderIDIndexIntegration -v -count=1
go test ./internal/dbmigration -run TestTradesBuyOrderIDIndexIntegration -v -count=1
```

두 번째 integration 실행은 적용 완료 DB의 재검증 안전성을 확인한다. `IF NOT EXISTS` 복구 분기는 정적 계약과 아래 절차로 검증한다.

```sql
SELECT c.relname, i.indisready, i.indisvalid, pg_get_indexdef(c.oid)
FROM pg_class c JOIN pg_index i ON i.indexrelid = c.oid
WHERE c.relname = 'idx_trades_buy_order_id';

DROP INDEX CONCURRENTLY IF EXISTS idx_trades_buy_order_id;
-- 정의를 확인한 운영자가 그 뒤 goose Up을 재실행한다.
```

`.github/workflows/backend-ci.yml`의 Postgres job은 다음으로 바꾼다.

```yaml
run: go test -run Integration -v -p 1 ./internal/dbmigration ./internal/repository ./internal/service
```

- [ ] **Step 5: Task 1만 commit-message 검토 후 커밋한다**

```powershell
git add migrations/006_trades_buy_order_id_index.sql internal/dbmigration/runner_test.go internal/dbmigration/trades_buy_order_index_integration_test.go .github/workflows/backend-ci.yml
git commit -F _workspace/commit-draft.md
```

권장 subject: `perf(db): 매수 주문 체결 조회 인덱스 추가`

---

### Task 2: 로컬 진단 출력 경로만 격리

**Files:**

- Modify: `internal/service/settlement_diagnostic_test.go`

- [ ] **Step 1: env override RED를 작성한다**

```go
func TestSettlementDiagnosticResultDirUsesOverride(t *testing.T) {
    want := t.TempDir()
    t.Setenv(diagResultDirEnv, want)
    require.Equal(t, want, diagResultDir())
}
```

```powershell
go test ./internal/service -run TestSettlementDiagnosticResultDirUsesOverride -v -count=1
```

Expected: helper 부재로 compile FAIL.

- [ ] **Step 2: 경로 선택만 구현하고 기본값을 회귀 테스트한다**

```go
const (
    diagResultDirEnv     = "GOEXCHANGE_SETTLEMENT_DIAGNOSTIC_RESULT_DIR"
    defaultDiagResultDir = "../../_workspace/settlement-cost-diagnostic"
)

func diagResultDir() string {
    if dir := strings.TrimSpace(os.Getenv(diagResultDirEnv)); dir != "" {
        return dir
    }
    return defaultDiagResultDir
}
```

`diagWriteResult`의 `MkdirAll`과 `filepath.Join`만 `diagResultDir()`을 사용하게 한다. fixture, 반복 수, warm-up, job 구성, 측정 구간은 바꾸지 않는다.

```go
func TestSettlementDiagnosticResultDirDefaultsToExistingLocation(t *testing.T) {
    t.Setenv(diagResultDirEnv, "")
    require.Equal(t, defaultDiagResultDir, diagResultDir())
}
```

```powershell
gofmt -w internal/service/settlement_diagnostic_test.go
go test ./internal/service -run 'TestSettlementDiagnosticResultDir' -v -count=1
git diff 82b4d7f..HEAD -- '*.go' ':(exclude)*_test.go'
go build -trimpath -ldflags='-s -w' ./cmd
```

Expected: tests/build PASS, production Go diff 비어 있음.

- [ ] **Step 3: Task 2만 commit-message 검토 후 커밋한다**

```powershell
git add internal/service/settlement_diagnostic_test.go
git commit -F _workspace/commit-draft.md
```

권장 subject: `test(settlement): 진단 결과 경로 격리`

---

### Task 3: 로컬·GCP 판정기를 측정 전에 고정

**Files:**

- Create: `_workspace/buy-order-index-remeasurement/scripts/local_gate.py`
- Create: `_workspace/buy-order-index-remeasurement/scripts/gcp_gate.py`

- [ ] **Step 1: `local_gate.py`의 입력·출력 계약 테스트를 먼저 작성한다**

표준 라이브러리 `unittest`를 사용해 `_workspace/buy-order-index-remeasurement/scripts/test_gates.py`에 다음 fixture를 만든다.

```text
run1, run2 × initial, mid, full × N=1,8
ops.<kind>.p50_ms: trade_batch, cancel_terminal, market_terminal
integrity: violations=null, 나머지 count=0
db.explain: name=trades_sum_buyer_fee_by_buy_order,
              seq_scan=false,
              plan contains idx_trades_buy_order_id
```

테스트 사례:

1. market median slope N=1·N=8가 각각 1.40이면 PASS.
2. 둘 중 하나가 1.40001이면 FAIL.
3. 중앙값 PASS + raw 한 회차 1.40 초과면 PASS와 `raw_run_disagreement_limitation=true`.
4. integrity 하나가 non-zero면 FAIL.
5. mid/full 한 셀에서 index plan이 없으면 FAIL.
6. `--skip-index-check`는 기준선 검산에서만 index 검사를 생략.

```powershell
python -m unittest _workspace/buy-order-index-remeasurement/scripts/test_gates.py -v
```

Expected: 구현 부재로 FAIL.

- [ ] **Step 2: `local_gate.py`를 최소 구현한다**

CLI:

```text
python local_gate.py MEASUREMENT_ROOT [--max-slope 1.40] [--skip-index-check]
exit 0 = PASS, exit 1 = gate FAIL, exit 2 = malformed/missing input
```

반드시 다음 식을 그대로 쓴다.

```python
initial = statistics.median(run1_initial_p50, run2_initial_p50)
full = statistics.median(run1_full_p50, run2_full_p50)
slope = full / initial
```

stdout JSON 필드:

```text
pass, max_slope, slopes, raw_market_ratios,
raw_run_disagreement_limitation, integrity_pass, index_plan_pass
```

각 p50은 `cell["ops"][kind]["p50_ms"]`에서 읽는다. integrity는 `violations`가 null/empty이고 나머지 count가 모두 0일 때만 PASS한다. index plan 검사는 `cell["db"]["explain"]`에서 양 run의 mid/full, N=1·N=8 모두 `seq_scan=false`와 `idx_trades_buy_order_id` 포함을 요구한다.

- [ ] **Step 3: 33번 원본으로 local 계산을 검산한다**

```powershell
python _workspace/buy-order-index-remeasurement/scripts/local_gate.py _workspace/settlement-cost-diagnostic --skip-index-check
```

Expected: exit 1, 반올림 결과가 아래와 일치.

| 종류 | N=1 | N=8 |
|---|---:|---:|
| trade_batch | 1.258 | 1.284 |
| cancel_terminal | 1.281 | 1.334 |
| market_terminal | 4.885 | 5.724 |

다르면 threshold를 바꾸지 말고 필드 매핑과 중앙값 방향을 수정한다.

- [ ] **Step 4: `gcp_gate.py`의 counter-delta 테스트를 작성한다**

15초 snapshot fixture로 다음을 검증한다.

1. hold 첫 완전 1분과 마지막 완전 1분의 `sum delta / count delta` 계산.
2. 45초 미만 표본이면 exit 2.
3. counter reset이면 exit 2.
4. `job_growth=last_ms/first_ms`.
5. full hold에서 아래 count와 share 출력.

```text
total_success = delta(settlement_job_execution_seconds_count{result="success"})
trade = delta(settlement_batch_size_count)
cancel = delta(settlement_terminal_wait_seconds_count{kind="cancel"})
market_done = delta(settlement_terminal_wait_seconds_count{kind="market_done"})
worker_terminal = total_success - trade
dispatch_terminal = cancel + market_done
market_share_of_all = market_done / total_success
market_share_of_terminal = market_done / dispatch_terminal
terminal_boundary_difference = dispatch_terminal - worker_terminal
```

`terminal_boundary_difference`는 dispatch와 worker 완료의 1분 경계상 in-flight 차이로 보고하며 PASS gate로 쓰지 않는다.

`--max-growth`가 있을 때만 growth를 gate로 사용한다. 없으면 report-only exit 0이다.

- [ ] **Step 5: 32-B 750 원본으로 GCP 계산을 검산한다**

```powershell
python _workspace/buy-order-index-remeasurement/scripts/gcp_gate.py _workspace/capacity-32b/cap750r1/snapshots --hold-start 1785615403 --hold-end 1785616003
```

Expected: 약 `9.19ms → 23.75ms`, growth `2.58`. market_done의 전체 약 `40.2%`, terminal 약 `73.5%`는 final 누적 기준이며, hold delta는 ramp 제외로 조금 다를 수 있다. 시간 구간 차이를 감안해도 설명되지 않는 편차가 있으면 GCP를 시작하지 않는다.

```powershell
python -m unittest _workspace/buy-order-index-remeasurement/scripts/test_gates.py -v
```

Expected: 모든 gate unit test PASS.

- [ ] **Step 6: 공통 100%/0건 판정기를 고정한다**

`_workspace/buy-order-index-remeasurement/scripts/hard_gate.py`를 추가하고 `test_gates.py`에 PASS/FAIL fixture를 넣는다.

CLI:

Example invocation for the mandatory 500 phase:

```powershell
python hard_gate.py _workspace/buy-order-index-remeasurement/gcp/index500r1 index500r1 --restart-epoch $restartEpoch
```

아래를 모두 만족할 때만 exit 0과 `HARD_GATE_PASS` JSON을 출력한다.

- A/B summary 각각 `sli_order_response_availability`, `sli_order_business_success`, `sli_cancel_success`: `fails=0`, `passes>0`
- A/B `sli_order_response_over_slo_total.count=0`
- final metrics에서 admission rejection, hold/settlement fallback, completion blocked, dependency-record failure, duplicate terminal, failed/fallback job count 전부 0; metric 부재 counter는 0으로 해석
- 모든 `settlement_outstanding_jobs{partition}`와 `settlement_quarantined_orders{partition}`가 0
- integrity text의 failed settlement/cancel/market-completion/reconciliation table count가 0
- postrestart timestamp가 전달된 restart epoch 이상이고 reconciliation check가 정확히 4개, 모두 0

malformed/missing evidence는 PASS로 보지 않고 exit 2로 실패한다.

---

### Task 4: 로컬 6셀을 독립 2회 실행하고 1차 판정

**Files:**

- Create: `_workspace/buy-order-index-remeasurement/local/run1/*.json`
- Create: `_workspace/buy-order-index-remeasurement/local/run2/*.json`
- Create: `_workspace/buy-order-index-remeasurement/local-gate.json`

- [ ] **Step 1: 후보 상태와 전체 테스트를 확인한다**

```powershell
$candidateSha = git rev-parse HEAD
$baselineSha = '82b4d7f'
git status --short
git diff "$baselineSha..$candidateSha" -- '*.go' ':(exclude)*_test.go'
docker compose -f docker-compose.test.yml up -d
$env:GOEXCHANGE_TEST_DATABASE_DSN='host=localhost user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=55432 sslmode=disable'
go test ./...
go vet ./...
go build -trimpath -ldflags='-s -w' ./cmd
```

Expected: test/vet/build PASS, production Go diff 비어 있음. 기존 untracked `_workspace`는 허용하되 새 측정 경로와 겹치면 중단한다.

- [ ] **Step 2: 첫 독립 6셀을 실행한다**

```powershell
$env:GOEXCHANGE_RUN_SETTLEMENT_DIAGNOSTIC='1'
$env:GOEXCHANGE_SETTLEMENT_DIAGNOSTIC_RESULT_DIR=(Resolve-Path '.').Path + '\_workspace\buy-order-index-remeasurement\local\run1'
go test ./internal/service -run '^TestSettlementDiagnosticHarnessProducesCellResult$' -timeout 60m -count=1 -v
```

Expected: `initial|mid|full × N=1|8` 여섯 JSON 생성.

- [ ] **Step 3: DB 프로세스를 초기화하고 두 번째 6셀을 실행한다**

```powershell
docker compose -f docker-compose.test.yml restart postgres-test
docker compose -f docker-compose.test.yml ps
$env:GOEXCHANGE_SETTLEMENT_DIAGNOSTIC_RESULT_DIR=(Resolve-Path '.').Path + '\_workspace\buy-order-index-remeasurement\local\run2'
go test ./internal/service -run '^TestSettlementDiagnosticHarnessProducesCellResult$' -timeout 60m -count=1 -v
```

Expected: 별도 디렉터리에 여섯 JSON 생성.

- [ ] **Step 4: 로컬 gate를 판정한다**

```powershell
python _workspace/buy-order-index-remeasurement/scripts/local_gate.py _workspace/buy-order-index-remeasurement/local 2>&1 | Tee-Object -FilePath _workspace/buy-order-index-remeasurement/local-gate.json
if ($LASTEXITCODE -ne 0) { throw 'local gate failed; do not push or start GCP' }
```

PASS 조건:

- market terminal slope N=1·N=8 모두 `≤1.40`
- 12셀 integrity의 `violations`가 null/empty이고 나머지 count 전부 0
- 두 run의 mid/full N=1·N=8에서 대상 buyer-fee SUM이 새 index 사용

raw 한 회차만 1.40을 넘으면 limitation을 기록하고 PASS를 유지한다. 위 조건 하나라도 실패하면 GCP를 실행하지 않는다. 다만 Task 1의 migration·catalog test·CI wiring 커밋은 전수 스캔 제거라는 독립 정확성 수정으로 채택·push하고, Task 2의 측정 전용 변경은 push하지 않는다.

- [ ] **Step 5: 로컬 PASS일 때만 구현 커밋을 push하고 해당 SHA의 CI를 확인한다**

```powershell
git push origin HEAD:main
$candidateSha = git rev-parse HEAD
$runId = gh run list --workflow 'Backend CI' --commit $candidateSha --limit 1 --json databaseId --jq '.[0].databaseId'
if (-not $runId) { throw 'Backend CI run not found for candidate SHA' }
gh run watch $runId --exit-status
```

Expected: Backend CI PASS. 실패하면 GCP를 시작하지 않는다.

---

### Task 5: GCP 도구·환경과 공통 phase protocol 고정

**Files:**

- Copy into `_workspace/buy-order-index-remeasurement/scripts/`: `cap_loadgen.sh`, `sli.py`, `p1_integrity.sql`, `truncate10.sql`, `order-spike-availability.js`
- Create: `_workspace/buy-order-index-remeasurement/scripts/collect_vm.sh`

- [ ] **Step 1: 32-B 도구를 수정 없이 복사하고 checksum을 남긴다**

```powershell
$scriptRoot = '_workspace/buy-order-index-remeasurement/scripts'
Copy-Item '_workspace/capacity-32b/cap_loadgen.sh' $scriptRoot
Copy-Item '_workspace/capacity-32b/sli.py' $scriptRoot
Copy-Item '_workspace/capacity-32b/p1_integrity.sql' $scriptRoot
Copy-Item '_workspace/capacity-32b/truncate10.sql' $scriptRoot
Copy-Item '_workspace/loadtest/order-spike-availability.js' $scriptRoot
Get-FileHash "$scriptRoot/cap_loadgen.sh", "$scriptRoot/sli.py", "$scriptRoot/p1_integrity.sql", "$scriptRoot/truncate10.sql", "$scriptRoot/order-spike-availability.js" -Algorithm SHA256 | Format-Table -AutoSize | Out-File -Encoding utf8 '_workspace/buy-order-index-remeasurement/script-sha256.txt'
```

`git diff --no-index`로 다섯 원본/복사본이 같음을 확인한다. workload 상수와 SLI 식은 수정하지 않는다.

- [ ] **Step 2: collector를 작성한다**

`collect_vm.sh start server index500r1 metrics` 같은 start 호출:

- `~/index-a/$phase/`가 이미 있거나 PID가 살아 있으면 실패한다.
- 모든 VM에서 32-B와 같은 `vmstat -t 5`를 `cpu-$role-$phase.txt`에 nohup 수집한다.
- `metrics` 모드(server만)는 `/metrics`를 15초마다 `snap-NNNN-UTC.txt.tmp`에 받은 뒤 성공 시 `.txt`로 rename한다.
- PID는 phase 디렉터리에 저장한다.

`collect_vm.sh stop server index500r1` 같은 stop 호출:

- 해당 phase PID 파일에 기록된 프로세스만 종료한다.
- 다른 phase나 시스템 프로세스를 탐색·종료하지 않는다.

shellcheck가 있으면 실행하고, 없으면 `bash -n collect_vm.sh`로 문법을 확인한다.

- [ ] **Step 3: 네 VM을 기동하고 고정 구성을 확인한다**

```powershell
$zone = 'asia-northeast3-a'
$instances = @('goexchange-stress-server','goexchange-stress-db','goexchange-stress-load-gen','goexchange-stress-load-gen-b')
gcloud compute instances start @instances --zone=$zone
gcloud compute instances list --filter="name:goexchange-stress-*" --format='table(name,zone.basename(),machineType.basename(),status)'
```

Expected:

| VM | type | state |
|---|---|---|
| server | e2-highcpu-4 | RUNNING |
| DB | e2-highcpu-8 | RUNNING |
| load-gen A | e2-standard-8 | RUNNING |
| load-gen B | e2-standard-8 | RUNNING |

모든 VM에서 `timedatectl show -p NTPSynchronized --value=yes`, load-gen에서 `Linger=yes`를 확인한다. 서버 `.env`에서 settlement concurrency 8, DB max-open 25를 값 자체만 비교해 확인한다.

token은 PowerShell 변수에만 읽어 A/B 파일과 ordinal comparison 후 즉시 `Remove-Variable`한다. 값이나 hash를 출력하지 않는다.

- [ ] **Step 4: 후보 SHA를 서버에서 build한다**

```powershell
$candidateSha = git rev-parse HEAD
```

서버 `~/go-exchange-back`에서 `git fetch origin`, exact detached checkout `$candidateSha`, `git rev-parse HEAD` 일치 확인 후 아래를 실행한다.

```bash
docker compose --env-file .env -f docker-compose.stress.yml build backend
```

main의 최신값을 다시 해석하거나 다른 SHA로 build하지 않는다.

- [ ] **Step 5: 도구를 복사하고 세 위치 checksum을 맞춘다**

- `collect_vm.sh`: 네 VM의 `~/collect_vm.sh`
- `cap_loadgen.sh`, `order-spike-availability.js`: 양 load-gen의 home
- `p1_integrity.sql`, `truncate10.sql`: DB home

`sha256sum`으로 local/A/B의 load script와 k6 script가 같아야 한다. 불일치하면 phase를 시작하지 않는다.

- [ ] **Step 6: 모든 GCP phase에 적용할 독립 실행 protocol을 사용한다**

각 phase는 Task 6에서 확정하는 새 이름과 VU 변수로 시작한다.

순서는 고정한다.

1. server backend stop.
2. DB VM stop→start.
3. DB VM의 `~/go-exchange-back`에서 DB compose up, Postgres healthy 대기.
4. `truncate10.sql` 실행.
5. server backend/node-exporter/prometheus up.
6. `/ping`, bootstrap `loaded=0`, `settlement partitions=10 concurrency=8` 확인.
7. goose version 6, index ready/valid/definition 확인.
8. 다섯 job count/duration metric 노출 확인.
9. 네 collector 시작.
10. 현재보다 5분 뒤 공통 `start_ms` 생성. `hold_start=start_ms/1000+30`, `hold_end=hold_start+600`을 phase `run.json`에 기록.
11. A/B load 시작.

reset 명령의 핵심은 다음과 같다.

```bash
# server
docker compose --env-file .env -f docker-compose.stress.yml stop backend
docker compose --env-file .env -f docker-compose.stress.yml up -d backend node-exporter prometheus

# DB
docker compose --env-file .env -f docker-compose.db.yml up -d
docker exec -i goexchange-db-postgres psql -X -v ON_ERROR_STOP=1 -U goexchange -d goexchange < ~/truncate10.sql
```

load 명령은 양쪽에서 같은 start epoch를 쓴다.

```powershell
gcloud compute ssh goexchange-stress-load-gen --zone=$zone --command="bash ~/cap_loadgen.sh 0 a $startMs $phase $vuPerGenerator 30s 10m $vuPerGenerator"
gcloud compute ssh goexchange-stress-load-gen-b --zone=$zone --command="bash ~/cap_loadgen.sh $offsetB b $startMs $phase $vuPerGenerator 30s 10m $vuPerGenerator"
```

실행한 완전한 명령과 확정 숫자/이름은 secret 없이 `run.json`에 저장한다.

- [ ] **Step 7: phase 중·후 수집 protocol을 고정한다**

- ramp/hold 중 약 45초마다 server admission/outstanding/worker busy/dispatch wait/job time, DB vmstat, 양 k6 PID를 별도 호출로 확인한다. 60초를 넘는 원격 wait loop를 만들지 않는다.
- 한 load-gen이 먼저 죽으면 phase 전체 FAIL이며 같은 phase를 재사용하지 않는다.
- 양 k6 종료 후 outstanding 합이 0일 때까지 30초 간격으로 확인한다.
- drain 후 final metrics와 DB `p1_integrity.sql` 결과를 저장하고 collector를 중지한다.
- backend restart 직전 epoch를 기록한다. restart 후 `reconciliation_last_run_timestamp_seconds`가 그 epoch 이상이고 4개 check가 0인 뒤 postrestart metrics를 저장한다.
- 이 증거를 모두 회수하기 전에 truncate하지 않는다.

phase별 로컬 디렉터리에 반드시 회수할 파일:

```text
run.json
summary-a.json, summary-b.json
stdout-a.log, stdout-b.log
cpu-server, cpu-db, cpu-loadgen-a, cpu-loadgen-b
snapshots/*.txt
metrics-final.txt, metrics-postrestart.txt
integrity.txt
sli.txt, hard-gate.json, gcp-gate-or-report.json
```

- [ ] **Step 8: phase 공통 hard gate를 실행한다**

```powershell
python "$scriptRoot/sli.py" $localPhase $phase 2>&1 | Tee-Object -FilePath "$localPhase/sli.txt"
python "$scriptRoot/hard_gate.py" $localPhase $phase --restart-epoch $restartEpoch 2>&1 | Tee-Object -FilePath "$localPhase/hard-gate.json"
if ($LASTEXITCODE -ne 0) { throw "$phase hard gate failed" }
```

Expected: 양쪽 response/business/cancel 100%, over-1s 0건, admission/failure/fallback/quarantine/duplicate/integrity 0, drain 0, 재기동 reconciliation 4개 0.

---

### Task 6: 500 1회와 750 최대 2회를 사전 등록 분기로 실행

- [ ] **Step 1: `index500r1`을 독립 실행한다**

Task 5의 공통 phase protocol에 다음 확정값을 넣는다.

```powershell
$phase = 'index500r1'
$vuPerGenerator = 250
$offsetB = 250
$totalVU = 500
$localPhase = "_workspace/buy-order-index-remeasurement/gcp/$phase"
```

protocol 종료 후 hard gate와 growth gate를 모두 실행한다.

```powershell
python "$scriptRoot/gcp_gate.py" "$localPhase/snapshots" --hold-start $holdStart --hold-end $holdEnd --max-growth 1.40 2>&1 | Tee-Object -FilePath "$localPhase/gcp-gate.json"
if ($LASTEXITCODE -ne 0) { throw '500 growth gate failed; do not run 750' }
```

Expected: hard gate PASS와 `job_growth≤1.40`. first/last-minute job ms와 trade/cancel/market_done/total count 및 mix를 모두 보존한다.

- [ ] **Step 2: 500 FAIL 분기를 즉시 닫는다**

hard gate 또는 growth gate 하나라도 실패하면 750은 실행하지 않는다. 결론은 다음으로 고정한다.

> 로컬 전수 스캔 제거는 확인됐더라도, 인덱스 하나가 32-B의 GCP 비용 성장 또는 기존 500 VU 계약을 충분히 설명·보존하지 못했다. 용량 경계가 이동했다고 주장하지 않고 GCP의 남은 성장 원인을 진단한다.

- [ ] **Step 3: 500 PASS일 때 `index750r1`을 새 환경에서 실행한다**

공통 protocol은 DB VM stop→start, 10-table truncate, backend 새 프로세스부터 다시 시작한다.

```powershell
$phase = 'index750r1'
$vuPerGenerator = 375
$offsetB = 375
$totalVU = 750
$localPhase = "_workspace/buy-order-index-remeasurement/gcp/$phase"
```

hard gate 후 growth는 report-only로 계산한다.

```powershell
python "$scriptRoot/gcp_gate.py" "$localPhase/snapshots" --hold-start $holdStart --hold-end $holdEnd 2>&1 | Tee-Object -FilePath "$localPhase/gcp-report.json"
if ($LASTEXITCODE -ne 0) { throw 'first 750 evidence is malformed; do not confirm' }
```

`job_growth_750`은 원인 설명 지표이며 별도 PASS threshold가 아니다.

- [ ] **Step 4: 첫 750 FAIL이면 반복하지 않는다**

hard gate 실패 또는 malformed evidence이면 `index750r2`를 만들지 않는다. 첫 실행을 평균이나 재시도로 구제하지 않고 최고 검증 용량을 500으로 유지한다.

- [ ] **Step 5: 첫 750 PASS일 때만 `index750r2`를 독립 확증한다**

다시 DB VM stop→start, 10-table truncate, backend 새 프로세스부터 시작한다.

```powershell
$phase = 'index750r2'
$vuPerGenerator = 375
$offsetB = 375
$totalVU = 750
$localPhase = "_workspace/buy-order-index-remeasurement/gcp/$phase"
```

hard gate와 report-only GCP 계산을 별도로 실행해 `hard-gate.json`, `gcp-report.json`을 만든다.

- [ ] **Step 6: 750 판정을 고정한다**

- 첫 실행 또는 확증 실행 FAIL: 최고 검증 용량은 500 VU.
- 두 실행 모두 PASS: 이 workload·구성·10분 hold에서 최고 검증 용량은 최소 750 VU. 그보다 높은 값은 외삽하지 않는다.
- FAIL과 함께 32-B의 DB CPU 고점, job 비용·worker busy 상승, admission shedding이 재현될 때만 DB가 남은 실제 용량 벽이라고 결론 낸다.
- 다른 실패 서명이면 그 증거를 우선하며 DB를 자동 원인으로 쓰지 않는다.

---

### Task 7: GCP 비용 종료·결과 문서화·최종 검증

**Files:**

- Create: `docs/benchmarks/34-2026-08-11-buy-order-index-remeasurement.md`
- Modify: `docs/refactor/README.md`
- Modify: `docs/ENGINEERING-SUMMARY.md`

- [ ] **Step 1: PASS/FAIL과 무관하게 네 VM을 먼저 정지한다**

Task 6의 어느 gate가 실패했어도 finally 작업으로 가장 먼저 실행한다.

```powershell
gcloud compute instances stop @instances --zone=$zone
gcloud compute instances list --filter="name:goexchange-stress-*" --format='table(name,zone.basename(),machineType.basename(),status)'
```

Expected: server, DB, load-gen A/B 모두 `TERMINATED`. 이후 문서 작성·테스트·CI 동안 GCP 비용이 계속 발생하지 않는다.

- [ ] **Step 2: 사전 등록 판정표에서 정확히 한 행을 선택한다**

| 로컬 | GCP 500 | GCP 750 | 고정 결론 |
|---|---|---|---|
| FAIL | 미실행 | 미실행 | 단일 인덱스로 33번 원인을 닫지 못함 |
| PASS | FAIL | 미실행 | 로컬 원인은 제거했지만 GCP 성장·기존 계약을 설명하지 못함 |
| PASS | PASS | 한 실행 이상 FAIL | 비용 개선은 유효하나 검증 경계는 500 |
| PASS | PASS | 두 실행 PASS | 검증 경계가 최소 750으로 이동; A 완료 |

실행하지 않은 단계는 성공이나 빈칸이 아니라 `미실행 — 이전 게이트 실패`로 쓴다.

- [ ] **Step 3: 34번 benchmark 보고서를 작성한다**

`docs/benchmarks/34-2026-08-11-buy-order-index-remeasurement.md` 순서:

1. 기준 SHA `82b4d7f`, 후보 SHA, migration/catalog 증거
2. 로컬 12셀 raw p50, 회차별 ratio, 중앙값 slope, EXPLAIN, integrity. 33번과 달리 run 사이에 Postgres를 재기동해 buffer cache·통계 상태를 초기화했다는 비교 조건을 명시한다.
3. raw 회차 불일치 여부와 주 판정 유지 규칙
4. GCP topology, runtime, workload/checksum 동등성
5. 500 first/last-minute job ms, growth, 종류별 count/mix, hard gate
6. 실제 실행된 750 각 회차의 같은 표와 CPU/worker busy/dispatch wait/체결률
7. drain·postrestart reconciliation
8. 선택한 판정 행과 고정 결론
9. A에서 바꾸지 않은 것과 B로 넘기는 제약

로컬/GCP 절대 지연을 직접 비교하지 않는다. job mix 이동이 혼합 평균에 미친 영향은 count evidence로 설명한다.

- [ ] **Step 4: 문서 인덱스 두 곳을 최소 갱신한다**

- `docs/refactor/README.md`: 33번 다음에 34번 링크와 실제 한 문장 결론.
- `docs/ENGINEERING-SUMMARY.md`: 경계 측정→원인 특정→수정→경계 재판정 이야기를 실제 결과로 닫는다.
- B를 완료된 작업으로 쓰지 않는다.

- [ ] **Step 5: 수치·placeholder·코드를 최종 검증한다**

```powershell
rg -n 'TODO|TBD|FIXME|미정' docs/benchmarks/34-2026-08-11-buy-order-index-remeasurement.md docs/refactor/README.md docs/ENGINEERING-SUMMARY.md
git diff --check
$env:GOEXCHANGE_TEST_DATABASE_DSN='host=localhost user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=55432 sslmode=disable'
go test ./...
go vet ./...
go build -trimpath -ldflags='-s -w' ./cmd
git diff 82b4d7f..HEAD -- '*.go' ':(exclude)*_test.go'
```

Expected: placeholder 검색 결과 없음, diff check/test/vet/build PASS, production Go diff 비어 있음, 문서 수치가 raw artifact와 일치.

- [ ] **Step 6: 결과 문서만 commit-message 검토 후 커밋·push한다**

```powershell
git add docs/benchmarks/34-2026-08-11-buy-order-index-remeasurement.md docs/refactor/README.md docs/ENGINEERING-SUMMARY.md
git commit -F _workspace/commit-draft.md
git push origin HEAD:main
$finalSha = git rev-parse HEAD
$runId = gh run list --workflow 'Backend CI' --commit $finalSha --limit 1 --json databaseId --jq '.[0].databaseId'
if (-not $runId) { throw 'Backend CI run not found for final SHA' }
gh run watch $runId --exit-status
```

권장 subject: `docs(benchmark): 매수 주문 인덱스 재측정 결과 기록`

- [ ] **Step 7: A를 닫는다**

최종 응답은 migration/index 상태, 로컬 slope, 500 growth·job mix, 실제 750 실행 횟수, 최고 검증 용량, report/raw artifact 경로, VM 종료 상태를 요약한다. B는 별도 승인 후 취소 command outbox부터 시작한다.
