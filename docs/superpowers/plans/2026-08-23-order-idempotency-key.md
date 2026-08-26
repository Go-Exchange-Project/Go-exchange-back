# 주문 생성 idempotency key 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 클라이언트가 준 키로 주문 생성을 멱등하게 만들어, 응답이 유실된 뒤의 재시도가 두 번째 주문을 만들지 않게 한다.

**Architecture:** `POST /orders`가 `Idempotency-Key` 헤더를 필수로 받는다. 주문·hold·멱등성 레코드를 한 트랜잭션에 커밋하고, `(user_id, idempotency_key)` UNIQUE가 중복을 직렬화한다. 배치 경로에서는 키를 먼저 INSERT해 owner와 follower를 가르고 **owner만 엔진에 제출**한다. 커밋 시점의 `outcome`은 `PENDING`이고, 엔진 제출 결과에 따라 `ACCEPTED`/`REJECTED`/`UNKNOWN`으로 전이한다. 전이가 실패하면 `PENDING`에 머물며 stale gauge로 관측한다.

**Tech Stack:** Go 1.25.7, Gin 1.12, GORM 1.31, PostgreSQL, goose 3.27, Prometheus client_golang, React 18, TypeScript 5.8, Vitest, Playwright, k6

**Spec:** [`docs/superpowers/specs/2026-08-23-order-idempotency-key-design.md`](../specs/2026-08-23-order-idempotency-key-design.md)

## Global Constraints

- `Idempotency-Key` 헤더는 **필수**다. 누락·공백은 **400**. 길이는 공백 제외 **1~128자**
  (바이트가 아니라 **문자 수**. 서버는 rune, DB CHECK는 `length()`로 같은 단위를 쓴다).
- UNIQUE 범위는 **`(user_id, idempotency_key)`** 다. 전역이 아니다.
- 주문 INSERT·지갑 UPDATE·원장 INSERT·멱등성 레코드가 **한 트랜잭션**에서 커밋된다.
- **owner만 `TrySubmitOrder`를 호출한다.** follower는 저장된 `outcome`으로 응답한다.
- hold 커밋 시점의 `outcome`은 **`PENDING`** 이다. `ACCEPTED`로 앞당겨 쓰지 않는다.
- **`REJECTED` 기록은 보상 트랜잭션 안에서** hold 해제·주문 종결과 함께 커밋된다.
- **`UNKNOWN`은 best-effort다.** 기록에 실패하면 `PENDING`에 머문다.
- 모든 outcome 변경은 **`outcome`과 `updated_at`을 한 UPDATE 문에서** 갱신한다.
- **주문이 커밋된 뒤에는 어떤 실패에서도 키를 삭제하지 않는다.** 예외는 hold 검증에 실패해 주문이 만들어지지 않은 미커밋 키뿐이다.
- 지문은 **버전 + 길이-prefix 인코딩**이다. DTO 통째 직렬화 금지, decimal은 문자열로 넣되
  **자릿수를 자르지 않는다**(자르면 그 아래 자리만 다른 주문이 같은 지문을 받는다).
- `order_idempotency_keys`는 **AutoMigrate에 등록하지 않는다**(007과 같은 이유 — GORM이 UNIQUE를 자기 명명규칙으로 DROP하려 해 두 번째 부팅부터 SQLSTATE 42704).
- migration 008은 부분 인덱스를 **같은 Up에서 카탈로그로 검증**하고 어긋나면 `RAISE EXCEPTION`한다(006과 같은 방식).
- **보장 범위**: 중복 방지 + 같은 `order_id` + 저장된 최선의 결과. **첫 HTTP 응답 재생은 보장하지 않는다.**
- 산출물은 [gcp-stress-test-runbook §7.5](../../gcp-stress-test-runbook.md)의 시크릿 게이트를 통과한 정리본만 동기화 경로에 넣는다.
- 백엔드와 프런트는 별도 Git 저장소다. 각 저장소에서 관련 파일만 stage하고 커밋 전 `commit-message` 스킬을 쓴다.
- 각 코드 작업은 RED → GREEN 순서다. 백엔드 단위 게이트는 `go test ./... -race`, 통합 게이트는 DSN을 설정한 `go test -run Integration -p 1`, 프런트 게이트는 `npm test && npm run lint && npm run build`.

---

## File Structure

| 파일 | 책임 | 태스크 |
|---|---|---|
| `internal/service/order_fingerprint.go` | 버전형 길이-prefix 지문 계산 | 1 |
| `internal/service/order_fingerprint_test.go` | 정규화·경계 모호성·버전 | 1 |
| `migrations/008_order_idempotency_keys.sql` | 테이블·UNIQUE·CHECK·부분 인덱스·카탈로그 검증 | 2 |
| `internal/model/order_idempotency_key.go` | 모델 + outcome 상수 4종 | 2 |
| `internal/dbmigration/runner_test.go` | 008 정적 계약 | 2 |
| `internal/dbmigration/order_idempotency_integration_test.go` | 008 카탈로그 + 잘못된 동명 인덱스 실패 | 2 |
| `internal/repository/order_idempotency_repository.go` | 배치 INSERT-or-conflict, 조회, outcome UPDATE, 미커밋 정리, stale count | 3 |
| `internal/repository/order_idempotency_repository_integration_test.go` | repository SQL 계약 | 3 |
| `internal/service/hold_coordinator.go` | 그룹화·owner/follower·트랜잭션 순서·미커밋 키 정리 | 4 |
| `internal/service/hold_coordinator_test.go` | 그룹화 결정성·역할 분배 | 4 |
| `internal/service/order_service.go` | 키 검증, 지문, follower 엔진 제출 차단, outcome 전이, 5xx 매핑 | 5 |
| `internal/handler/order_handler.go` | 헤더 파싱, 400/409/202, `idempotent_replay` | 6 |
| `internal/handler/order_handler_integration_test.go` | HTTP 계약 | 6 |
| `internal/metrics/metrics.go` | 지표 4종 | 7 |
| `internal/service/order_idempotency_monitor.go` | stale PENDING gauge 갱신 | 7 |
| `internal/service/order_idempotency_monitor_test.go` | 즉시 1회·주기·실패 시 gauge 유지 | 7 |
| `cmd/main.go` | monitor 배선(lifecycle) | 7 |
| `internal/service/order_idempotency_integration_test.go` | 동시성·혼합 배치·상태 전이 통합 검증 | 8 |
| `src/lib/api.ts`, `OrderForm.tsx` (front) | 헤더 전송·키 수명·202 처리 | 9 |
| `tests/e2e/exchange.spec.ts` (front) | 중복 제출 eventual 계약 | 10 |
| `_workspace/loadtest/*.js`, `loadtest/order-spike-single-symbol.js` | iteration마다 새 키 | 10 |
| `docs/benchmarks/36-*.md`, README, refactor, ENGINEERING-SUMMARY | 결과·문서 | 11 |

---

### Task 1: 버전형 지문

**Files:**
- Create: `internal/service/order_fingerprint.go`
- Create: `internal/service/order_fingerprint_test.go`

**Interfaces:**
- Produces:
  - `const CurrentOrderFingerprintVersion = 1` (알고리즘은 `computeOrderFingerprintV1`로 분리)
  - `service.OrderFingerprintInput{UserID uint; CoinSymbol, Side, OrderType string; Price, Amount, QuoteAmount decimal.Decimal}`
  - `service.ComputeOrderFingerprint(in OrderFingerprintInput, version int) (string, error)`

- [ ] **Step 1: 실패하는 테스트 작성**

```go
package service

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fingerprintInput() OrderFingerprintInput {
	return OrderFingerprintInput{
		UserID:      7,
		CoinSymbol:  "BTC",
		Side:        "SELL",
		OrderType:   "LIMIT",
		Price:       decimal.RequireFromString("100.50"),
		Amount:      decimal.RequireFromString("1.5"),
		QuoteAmount: decimal.Zero,
	}
}

// 1.50과 1.5는 같은 주문이다. 표현 차이가 다른 지문이 되면 재시도가 409가 된다.
func TestComputeOrderFingerprintNormalizesDecimals(t *testing.T) {
	a := fingerprintInput()
	a.Price = decimal.RequireFromString("100.50")
	b := fingerprintInput()
	b.Price = decimal.RequireFromString("100.5")

	fa, err := ComputeOrderFingerprint(a, CurrentOrderFingerprintVersion)
	require.NoError(t, err)
	fb, err := ComputeOrderFingerprint(b, CurrentOrderFingerprintVersion)
	require.NoError(t, err)

	assert.Equal(t, fa, fb)
}

// 단순 연결이면 ("BTC","SELL")과 ("BTCS","ELL")이 같은 입력 문자열이 된다.
func TestComputeOrderFingerprintIsUnambiguousAcrossFieldBoundaries(t *testing.T) {
	a := fingerprintInput()
	a.CoinSymbol, a.Side = "BTC", "SELL"
	b := fingerprintInput()
	b.CoinSymbol, b.Side = "BTCS", "ELL"

	fa, err := ComputeOrderFingerprint(a, CurrentOrderFingerprintVersion)
	require.NoError(t, err)
	fb, err := ComputeOrderFingerprint(b, CurrentOrderFingerprintVersion)
	require.NoError(t, err)

	assert.NotEqual(t, fa, fb, "필드 경계가 모호하면 서로 다른 요청이 같은 지문을 갖는다")
}

func TestComputeOrderFingerprintDiffersPerField(t *testing.T) {
	base, err := ComputeOrderFingerprint(fingerprintInput(), CurrentOrderFingerprintVersion)
	require.NoError(t, err)

	mutations := map[string]func(*OrderFingerprintInput){
		"user":  func(in *OrderFingerprintInput) { in.UserID = 8 },
		"coin":  func(in *OrderFingerprintInput) { in.CoinSymbol = "ETH" },
		"side":  func(in *OrderFingerprintInput) { in.Side = "BUY" },
		"type":  func(in *OrderFingerprintInput) { in.OrderType = "MARKET" },
		"price": func(in *OrderFingerprintInput) { in.Price = decimal.RequireFromString("101") },
		"amt":   func(in *OrderFingerprintInput) { in.Amount = decimal.RequireFromString("2") },
		"quote": func(in *OrderFingerprintInput) { in.QuoteAmount = decimal.RequireFromString("5") },
	}
	for name, mutate := range mutations {
		in := fingerprintInput()
		mutate(&in)
		got, err := ComputeOrderFingerprint(in, CurrentOrderFingerprintVersion)
		require.NoError(t, err)
		assert.NotEqual(t, base, got, "%s가 지문에 반영되지 않았다", name)
	}
}

// 저장된 버전의 규칙으로 비교해야 배포만으로 기존 재시도가 409가 되지 않는다.
func TestComputeOrderFingerprintRejectsUnknownVersion(t *testing.T) {
	_, err := ComputeOrderFingerprint(fingerprintInput(), 99)
	require.Error(t, err)
}

// 자릿수를 잘라내는 것은 정규화가 아니라 정보 손실이다. 입력이 소수 18자리로 제한되지
// 않으므로, 19번째 자리만 다른 두 주문이 같은 지문을 받으면 서로를 재시도로 오인한다.
func TestComputeOrderFingerprintKeepsDigitsBeyondEighteenPlaces(t *testing.T) {
	a := fingerprintInput()
	a.Amount = decimal.RequireFromString("1.0000000000000000001")
	b := fingerprintInput()
	b.Amount = decimal.RequireFromString("1.0000000000000000002")

	fa, err := ComputeOrderFingerprint(a, CurrentOrderFingerprintVersion)
	require.NoError(t, err)
	fb, err := ComputeOrderFingerprint(b, CurrentOrderFingerprintVersion)
	require.NoError(t, err)

	assert.NotEqual(t, fa, fb)
}

// v1은 DB에 저장된 값이다. CurrentOrderFingerprintVersion을 2로 올려도 v1 계산은
// 그대로여야 한다 — 이 값이 바뀌면 배포만으로 기존 키의 재시도가 409가 된다.
// 버전을 올릴 때 이 테스트를 고치면 안 되고, 새 버전용 golden을 추가해야 한다.
func TestComputeOrderFingerprintV1IsFrozen(t *testing.T) {
	got, err := ComputeOrderFingerprint(fingerprintInput(), 1)
	require.NoError(t, err)

	assert.Equal(t, "95798288d827b6cccfc97d5bd57abb442f1f00047f1c9433b4f57463f699c398", got)
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/service -run OrderFingerprint -v`
Expected: FAIL — `undefined: OrderFingerprintInput`

- [ ] **Step 3: 최소 구현**

```go
package service

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/shopspring/decimal"
)

// CurrentOrderFingerprintVersion은 새 레코드를 저장할 때 쓰는 버전이다. 지문에 들어가는
// 필드 목록이 바뀌면 올린다.
//
// 이 상수를 올려도 computeOrderFingerprintV1은 그대로 남아야 한다. 비교는 항상 레코드에
// 저장된 버전의 알고리즘으로 하므로, 그래야 배포만으로 기존 키의 재시도가 다른 지문을
// 얻지 않는다.
const CurrentOrderFingerprintVersion = 1

type OrderFingerprintInput struct {
	UserID      uint
	CoinSymbol  string
	Side        string
	OrderType   string
	Price       decimal.Decimal
	Amount      decimal.Decimal
	QuoteAmount decimal.Decimal
}

// ComputeOrderFingerprint는 version이 지정한 알고리즘으로 지문을 계산한다.
func ComputeOrderFingerprint(in OrderFingerprintInput, version int) (string, error) {
	switch version {
	case 1:
		return computeOrderFingerprintV1(in), nil
	default:
		return "", fmt.Errorf("unsupported order fingerprint version %d", version)
	}
}

// computeOrderFingerprintV1은 요청을 결정하는 값만 모아 해시한다.
//
// DTO를 통째로 직렬화하지 않는다 — 필드 추가·키 순서·JSON 표현 변경만으로 기존 키가
// 전부 깨진다. 필드는 명시적으로 나열하고, 각 값은 길이-prefix로 이어 붙여 경계를
// 모호하지 않게 만든다("BTC"+"SELL"과 "BTCS"+"ELL"이 같은 입력이 되면 안 된다).
func computeOrderFingerprintV1(in OrderFingerprintInput) string {
	hash := sha256.New()
	write := func(value string) {
		var prefix [4]byte
		binary.BigEndian.PutUint32(prefix[:], uint32(len(value)))
		hash.Write(prefix[:])
		hash.Write([]byte(value))
	}

	write("v1")
	write(strconv.FormatUint(uint64(in.UserID), 10))
	write(in.CoinSymbol)
	write(in.Side)
	write(in.OrderType)
	// decimal은 JSON 숫자나 부동소수점이 아니라 문자열로 넣는다. String()은 후행 0을
	// 제거하므로 100.50과 100.5가 같은 지문이 된다. 자릿수는 자르지 않는다 — 자르면
	// 그 아래 자리만 다른 주문이 같은 지문이 된다.
	write(in.Price.String())
	write(in.Amount.String())
	write(in.QuoteAmount.String())

	return hex.EncodeToString(hash.Sum(nil))
}
```

- [ ] **Step 4: 통과 확인**

Run: `go test ./internal/service -run OrderFingerprint -v`
Expected: PASS 6개

- [ ] **Step 5: 커밋**

```bash
git add internal/service/order_fingerprint.go internal/service/order_fingerprint_test.go
git commit -F _workspace/commit-draft.md
```

권장 subject: `feat(order): 주문 요청의 버전형 지문 계산 추가`

**커버하는 검증**: 6, 6b, 6c, 6d

---

### Task 2: migration 008과 모델

**Files:**
- Create: `migrations/008_order_idempotency_keys.sql`
- Create: `internal/model/order_idempotency_key.go`
- Create: `internal/dbmigration/order_idempotency_integration_test.go`
- Modify: `internal/dbmigration/runner_test.go`

**Interfaces:**
- Produces:
  - `model.OrderIdempotencyOutcomePending|Accepted|Rejected|Unknown`
  - `model.OrderIdempotencyKey`

- [ ] **Step 1: 정적 계약 RED 테스트 작성**

`internal/dbmigration/runner_test.go` 끝에 추가한다.

```go
// 008은 gauge 조회용 부분 인덱스를 만든다. IF NOT EXISTS는 "같은 이름의 다른 인덱스"도
// 조용히 통과시키므로(006에서 확인한 구멍), 같은 Up 안의 카탈로그 검증이 한 세트다.
func TestOrderIdempotencyMigrationDeclaresContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(migrationsDir(), "008_order_idempotency_keys.sql"))
	require.NoError(t, err)
	sql := string(raw)

	assert.Contains(t, sql, "CREATE TABLE IF NOT EXISTS order_idempotency_keys")
	assert.Contains(t, sql, "order_idempotency_keys_user_key_unique")
	assert.Contains(t, sql, "UNIQUE (user_id, idempotency_key)")
	assert.Contains(t, sql, "fingerprint_version")
	assert.Contains(t, sql, "'PENDING','ACCEPTED','REJECTED','UNKNOWN'")
	assert.Contains(t, sql, "NOT NULL DEFAULT 'PENDING'")

	// 제약도 conname 존재만으로는 부족하다 — 실제 정의를 확인해야 한다.
	assert.Contains(t, sql, "pg_get_constraintdef")

	assert.Contains(t, sql, "CREATE INDEX IF NOT EXISTS order_idempotency_pending_updated_at")
	assert.Contains(t, sql, "WHERE outcome = 'PENDING'")

	// 카탈로그 방어 — 셋이 한 세트다.
	assert.Contains(t, sql, "indisready")
	assert.Contains(t, sql, "indisvalid")
	assert.Contains(t, sql, "RAISE EXCEPTION")
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/dbmigration -run OrderIdempotency -v`
Expected: FAIL — 008 파일이 없다

- [ ] **Step 3: 모델 작성**

```go
package model

import "time"

type OrderIdempotencyOutcome string

// PENDING은 "아직 진행 중"만 뜻하지 않는다. 이후 UPDATE가 실패해도 여기 머물므로,
// "이 시점 이후를 서버가 durable하게 알지 못한다"는 뜻이다.
const (
	OrderIdempotencyOutcomePending  OrderIdempotencyOutcome = "PENDING"
	OrderIdempotencyOutcomeAccepted OrderIdempotencyOutcome = "ACCEPTED"
	OrderIdempotencyOutcomeRejected OrderIdempotencyOutcome = "REJECTED"
	OrderIdempotencyOutcomeUnknown  OrderIdempotencyOutcome = "UNKNOWN"
)

// OrderIdempotencyKey는 주문 생성 요청의 재시도를 식별한다.
//
// 이 테이블은 AutoMigrate 대상이 아니다. 스키마는 migration 008이 전부 소유한다 —
// AutoMigrate에 넣으면 008이 만든 UNIQUE를 GORM이 자기 명명규칙
// (uni_order_idempotency_keys_...)으로 DROP하려 해 두 번째 부팅부터 실패한다.
type OrderIdempotencyKey struct {
	ID                 uint64                  `gorm:"primaryKey"`
	UserID             uint                    `gorm:"not null"`
	IdempotencyKey     string                  `gorm:"not null"`
	Fingerprint        string                  `gorm:"not null"`
	FingerprintVersion int                     `gorm:"not null"`
	OrderID            *uint // 1단계 INSERT 시점에는 모른다
	// DB는 NOT NULL DEFAULT 'PENDING'이다. 커밋 시점의 outcome은 이미 PENDING으로
	// 확정되므로 nullable로 두지 않는다 — 값 타입과 NULL 컬럼이 섞이면 GORM이 빈
	// 문자열을 넣어 CHECK와 충돌한다. INSERT 시 명시적으로 PENDING을 채운다.
	Outcome            OrderIdempotencyOutcome
	CreatedAt          time.Time
	UpdatedAt          time.Time
}
```

- [ ] **Step 4: migration 008 작성**

```sql
-- +goose Up
-- 주문 생성 재시도를 식별하는 키. 스키마는 이 migration이 단독으로 소유한다
-- (AutoMigrate에 넣으면 GORM이 아래 UNIQUE를 자기 명명규칙으로 DROP하려 한다).

CREATE TABLE IF NOT EXISTS order_idempotency_keys (
    id                  BIGSERIAL PRIMARY KEY,
    user_id             BIGINT      NOT NULL,
    idempotency_key     TEXT        NOT NULL,
    fingerprint         TEXT        NOT NULL,
    fingerprint_version INT         NOT NULL,
    order_id            BIGINT,
    -- 커밋 시점의 outcome은 이미 PENDING으로 확정된다. NULL을 허용하면 Go 모델의
    -- 값 타입과 어긋나 GORM이 빈 문자열을 넣는 경로가 생긴다.
    outcome             TEXT        NOT NULL DEFAULT 'PENDING',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'order_idempotency_keys'::regclass
          AND conname = 'order_idempotency_keys_user_key_unique'
    ) THEN
        ALTER TABLE order_idempotency_keys
            ADD CONSTRAINT order_idempotency_keys_user_key_unique UNIQUE (user_id, idempotency_key);
    END IF;

    -- HTTP 계약(공백 제외 1~128자)과 같은 범위를 DB에서도 막는다.
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'order_idempotency_keys'::regclass
          AND conname = 'order_idempotency_keys_key_length'
    ) THEN
        ALTER TABLE order_idempotency_keys
            ADD CONSTRAINT order_idempotency_keys_key_length
            CHECK (length(btrim(idempotency_key)) BETWEEN 1 AND 128);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'order_idempotency_keys'::regclass
          AND conname = 'order_idempotency_keys_outcome_check'
    ) THEN
        ALTER TABLE order_idempotency_keys
            ADD CONSTRAINT order_idempotency_keys_outcome_check
            CHECK (outcome IN ('PENDING','ACCEPTED','REJECTED','UNKNOWN'));
    END IF;
END $$;
-- +goose StatementEnd

-- 위 블록은 conname 존재만 본다. 같은 이름의 잘못된 제약이 이미 있으면 조용히 통과하므로
-- (인덱스의 IF NOT EXISTS와 같은 구멍), 실제 정의를 확인하고 어긋나면 실패시킨다.
-- 기대 문자열은 PostgreSQL 16.14와 18.4의 pg_get_constraintdef 출력이 동일함을 확인했다.
-- +goose StatementBegin
DO $$
DECLARE
    expected CONSTANT text[][] := ARRAY[
        ARRAY['order_idempotency_keys_user_key_unique',
              $def$UNIQUE (user_id, idempotency_key)$def$],
        ARRAY['order_idempotency_keys_key_length',
              $def$CHECK (((length(btrim(idempotency_key)) >= 1) AND (length(btrim(idempotency_key)) <= 128)))$def$],
        ARRAY['order_idempotency_keys_outcome_check',
              $def$CHECK ((outcome = ANY (ARRAY['PENDING'::text, 'ACCEPTED'::text, 'REJECTED'::text, 'UNKNOWN'::text])))$def$]
    ];
    constraint_name text;
    want text;
    got text;
BEGIN
    FOR i IN 1 .. array_length(expected, 1) LOOP
        constraint_name := expected[i][1];
        want := expected[i][2];

        SELECT pg_get_constraintdef(oid) INTO got
        FROM pg_constraint
        WHERE conrelid = 'order_idempotency_keys'::regclass
          AND conname = constraint_name;

        IF got IS NULL THEN
            RAISE EXCEPTION 'constraint % is missing on order_idempotency_keys', constraint_name;
        END IF;

        -- 공백만 다른 경우는 같은 제약으로 본다.
        IF regexp_replace(got, '\s+', '', 'g') <> regexp_replace(want, '\s+', '', 'g') THEN
            RAISE EXCEPTION 'constraint % has an unexpected definition: %', constraint_name, got;
        END IF;
    END LOOP;
END $$;
-- +goose StatementEnd


-- stale PENDING gauge 조회 전용. 정상 상태에서는 거의 비어 있다.
CREATE INDEX IF NOT EXISTS order_idempotency_pending_updated_at
    ON order_idempotency_keys (updated_at)
    WHERE outcome = 'PENDING';

-- IF NOT EXISTS는 같은 이름의 잘못된 인덱스도 조용히 통과시킨다(006에서 확인).
-- 카탈로그로 확인하고 어긋나면 실패시켜 goose version 8이 기록되지 않게 한다.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_class index_rel
        JOIN pg_index index_meta ON index_meta.indexrelid = index_rel.oid
        JOIN pg_am access_method ON access_method.oid = index_rel.relam
        JOIN pg_attribute column_meta
          ON column_meta.attrelid = index_meta.indrelid
         AND column_meta.attnum = index_meta.indkey[0]
        WHERE index_rel.relname = 'order_idempotency_pending_updated_at'
          AND index_meta.indrelid = 'order_idempotency_keys'::regclass
          AND index_meta.indisready
          AND index_meta.indisvalid
          AND NOT index_meta.indisunique
          AND access_method.amname = 'btree'
          AND index_meta.indnkeyatts = 1
          AND index_meta.indnatts = 1
          AND column_meta.attname = 'updated_at'
          AND index_meta.indexprs IS NULL
          AND pg_get_expr(index_meta.indpred, index_meta.indrelid) = '(outcome = ''PENDING''::text)'
    ) THEN
        RAISE EXCEPTION 'order_idempotency_pending_updated_at is missing, invalid, or has the wrong definition';
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
-- data-bearing 키를 자동으로 지우지 않는다. rollback이 필요하면 별도 운영 절차에서 처리한다.
SELECT 1;
```

- [ ] **Step 5: 카탈로그 통합 테스트 작성**

```go
// package dbmigration_test인 이유: testdb가 dbmigration을 import하므로 내부 테스트
// 패키지에서 testdb를 쓰면 import cycle이 된다.
package dbmigration_test

import (
	"strings"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/dbmigration"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestOrderIdempotencyKeysIntegration(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)

	t.Run("UNIQUE는 (user_id, idempotency_key)다", func(t *testing.T) {
		var definition string
		require.NoError(t, db.Raw(`
SELECT pg_get_constraintdef(c.oid)
FROM pg_constraint c
JOIN pg_class t ON t.oid = c.conrelid
WHERE t.relname = 'order_idempotency_keys'
  AND c.conname = 'order_idempotency_keys_user_key_unique'`).Scan(&definition).Error)
		require.NotEmpty(t, definition)
		assert.Equal(t, "UNIQUE (user_id, idempotency_key)", definition)
	})

	// 008의 카탈로그 검증이 보는 조건을 그대로 단언한다. 하나라도 느슨하면
	// 검증 조건이 빠져도 이 테스트가 통과한다.
	t.Run("부분 인덱스 정의가 정확하다", func(t *testing.T) {
		var got struct {
			AccessMethod    string
			FirstColumn     string
			Indisready      bool
			Indisvalid      bool
			Indisunique     bool
			Indnkeyatts     int
			Indnatts        int
			HasNoExpression bool
			Predicate       *string
		}
		require.NoError(t, db.Raw(`
SELECT am.amname AS access_method,
       a.attname AS first_column,
       i.indisready, i.indisvalid, i.indisunique, i.indnkeyatts, i.indnatts,
       (i.indexprs IS NULL) AS has_no_expression,
       pg_get_expr(i.indpred, i.indrelid) AS predicate
FROM pg_class c
JOIN pg_index i ON i.indexrelid = c.oid
JOIN pg_am am ON am.oid = c.relam
JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
WHERE c.relname = 'order_idempotency_pending_updated_at'`).Scan(&got).Error)

		require.Equal(t, "btree", got.AccessMethod, "인덱스가 없다 — goose version과 008 Up 로그를 먼저 확인한다")
		assert.Equal(t, "updated_at", got.FirstColumn)
		assert.True(t, got.Indisready)
		assert.True(t, got.Indisvalid)
		assert.False(t, got.Indisunique)
		assert.Equal(t, 1, got.Indnkeyatts)
		assert.Equal(t, 1, got.Indnatts, "INCLUDE 컬럼이 붙으면 008의 검증이 실패한다")
		assert.True(t, got.HasNoExpression, "표현식 인덱스가 아니어야 한다")
		require.NotNil(t, got.Predicate)
		assert.Equal(t, "(outcome = 'PENDING'::text)", *got.Predicate)
	})

	// gauge 조회는 이 CHECK를 신뢰한다. HTTP 검증만으로는 다른 경로의 INSERT를 못 막는다.
	t.Run("키 길이 CHECK가 공백 제외 1~128자를 강제한다", func(t *testing.T) {
		insert := func(key string) error {
			return db.Exec(`
INSERT INTO order_idempotency_keys (user_id, idempotency_key, fingerprint, fingerprint_version)
VALUES (?, ?, ?, ?)`, 999999, key, "fp", 1).Error
		}

		valid := strings.Repeat("k", 128)
		require.NoError(t, insert(valid))
		t.Cleanup(func() {
			require.NoError(t, db.Exec(
				`DELETE FROM order_idempotency_keys WHERE user_id = ?`, 999999).Error)
		})

		assert.Error(t, insert("   "), "공백만 있는 키가 통과했다")
		assert.Error(t, insert(strings.Repeat("k", 129)), "129자 키가 통과했다")

		// length()는 바이트가 아니라 문자를 센다. 서버 검증도 rune으로 세야 두 단위가 맞는다.
		multibyte := strings.Repeat("가", 128) // 384바이트
		require.NoError(t, insert(multibyte), "128자 멀티바이트 키가 거부됐다 — CHECK가 바이트를 센다")
		assert.Error(t, insert(strings.Repeat("가", 129)), "129자 멀티바이트 키가 통과했다")
	})

	// 커밋 시점 outcome은 PENDING으로 확정된다. NULL을 허용하면 Go 모델의 값 타입과
	// 어긋나 GORM이 빈 문자열을 넣는 경로가 생긴다.
	t.Run("outcome은 NOT NULL이고 기본값이 PENDING이다", func(t *testing.T) {
		var got struct {
			IsNullable    string
			ColumnDefault *string
		}
		require.NoError(t, db.Raw(`
SELECT is_nullable, column_default
FROM information_schema.columns
WHERE table_schema = current_schema()
  AND table_name = 'order_idempotency_keys'
  AND column_name = 'outcome'`).Scan(&got).Error)

		assert.Equal(t, "NO", got.IsNullable)
		require.NotNil(t, got.ColumnDefault)
		assert.Contains(t, *got.ColumnDefault, "'PENDING'")
	})

	t.Run("goose version이 8이다", func(t *testing.T) {
		var version int64
		require.NoError(t, db.Raw(
			`SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&version).Error)
		assert.GreaterOrEqual(t, version, int64(8))
	})
}

// 제약은 conname 존재만 보고 조건부로 만든다. 같은 이름의 잘못된 제약이 이미 있으면
// 이름만으로 통과하므로, 실제 정의 검증이 없으면 틀린 스키마가 version 8로 기록된다.
func TestOrderIdempotencyMigrationFailsOnWrongSameNamedConstraint(t *testing.T) {
	wrong := map[string]struct{ name, definition string }{
		"UNIQUE 범위가 전역이다": {
			"order_idempotency_keys_user_key_unique", "UNIQUE (idempotency_key)"},
		"키 길이 상한이 다르다": {
			"order_idempotency_keys_key_length",
			"CHECK (length(btrim(idempotency_key)) BETWEEN 1 AND 1280)"},
		"outcome 목록이 다르다": {
			"order_idempotency_keys_outcome_check",
			"CHECK (outcome IN ('PENDING','ACCEPTED','REJECTED'))"},
	}

	for name, tc := range wrong {
		t.Run(name, func(t *testing.T) {
			db := testdb.OpenIntegrationDB(t)

			require.NoError(t, db.Exec(
				`ALTER TABLE order_idempotency_keys DROP CONSTRAINT `+tc.name).Error)
			require.NoError(t, db.Exec(
				`ALTER TABLE order_idempotency_keys ADD CONSTRAINT `+tc.name+` `+tc.definition).Error)
			t.Cleanup(func() {
				require.NoError(t, db.Exec(
					`ALTER TABLE order_idempotency_keys DROP CONSTRAINT IF EXISTS `+tc.name).Error)
				reapply008(t, db)
			})

			require.NoError(t, db.Exec(`DELETE FROM goose_db_version WHERE version_id = 8`).Error)

			err := dbmigration.Up(db)

			require.Error(t, err, "잘못된 동명 제약인데 migration이 성공했다")
			var applied int64
			require.NoError(t, db.Raw(
				`SELECT count(*) FROM goose_db_version WHERE version_id = 8 AND is_applied`).Scan(&applied).Error)
			assert.Zero(t, applied, "실패했는데 version 8이 기록됐다")
		})
	}
}

// 같은 이름의 잘못된 인덱스가 있으면 migration이 실패하고 version 8이 기록되지 않아야
// 한다. IF NOT EXISTS만으로는 조용히 통과한다.
func TestOrderIdempotencyMigrationFailsOnWrongSameNamedIndex(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)

	require.NoError(t, db.Exec(`DROP INDEX IF EXISTS order_idempotency_pending_updated_at`).Error)
	// predicate 없는 전체 인덱스를 같은 이름으로 만든다.
	require.NoError(t, db.Exec(
		`CREATE INDEX order_idempotency_pending_updated_at ON order_idempotency_keys (updated_at)`).Error)
	// 이 테스트는 goose version 8 행을 지우고 008을 실패시킨다. 다음 테스트가 우연히
	// 복구해 주기를 기대하지 않고, 여기서 008을 다시 적용해 인덱스와 version을 모두 되돌린다.
	t.Cleanup(func() {
		require.NoError(t, db.Exec(`DROP INDEX IF EXISTS order_idempotency_pending_updated_at`).Error)
		reapply008(t, db)
	})

	require.NoError(t, db.Exec(`DELETE FROM goose_db_version WHERE version_id = 8`).Error)

	err := dbmigration.Up(db)

	require.Error(t, err, "잘못된 동명 인덱스인데 migration이 성공했다")
	var applied int64
	require.NoError(t, db.Raw(
		`SELECT count(*) FROM goose_db_version WHERE version_id = 8 AND is_applied`).Scan(&applied).Error)
	assert.Zero(t, applied, "실패했는데 version 8이 기록됐다")
}

// reapply008은 스키마를 훼손한 테스트가 끝난 뒤 008을 다시 적용한다.
//
// version 8 행을 먼저 지우는 것이 핵심이다. migration이 성공해 버린 경우에는 version 8이
// 남아 goose가 "no migrations to run"으로 건너뛰고, 훼손된 스키마가 그대로 남는다.
func reapply008(t *testing.T, db *gorm.DB) {
	t.Helper()

	require.NoError(t, db.Exec(`DELETE FROM goose_db_version WHERE version_id = 8`).Error)
	require.NoError(t, dbmigration.Up(db), "cleanup에서 008 재적용이 실패했다")

	var applied int64
	require.NoError(t, db.Raw(
		`SELECT count(*) FROM goose_db_version WHERE version_id = 8 AND is_applied`).Scan(&applied).Error)
	require.EqualValues(t, 1, applied, "cleanup 후에도 version 8이 복구되지 않았다")
}
```

- [ ] **Step 6: 통과 확인**

```powershell
$env:GOEXCHANGE_TEST_DATABASE_DSN='host=localhost user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=55432 sslmode=disable'
go test ./internal/dbmigration -run OrderIdempotency -v -p 1
```

Expected: 정적 1개 + 카탈로그 5개 서브테스트 + 실패 케이스 4개(동명 오제약 3 + 동명 오인덱스 1) PASS.
`testdb/integration.go`는 **바꾸지 않는다**.

스키마를 훼손하는 실패 케이스의 cleanup은 반드시 `reapply008` 헬퍼처럼 **goose version 8 행을
먼저 지운 뒤** `dbmigration.Up`을 호출해야 한다. migration이 성공해 버린 경우에는 version 8이
남아 goose가 건너뛰고, 훼손된 스키마가 공유 테스트 DB에 그대로 남는다.

- [ ] **Step 7: 커밋**

권장 subject: `feat(order): 멱등성 키 테이블과 카탈로그 검증 migration 추가`

**커버하는 검증**: 9d, 9e, 9f

---

### Task 3: repository

**Files:**
- Create: `internal/repository/order_idempotency_repository.go`
- Create: `internal/repository/order_idempotency_repository_integration_test.go`

**Interfaces:**
- Consumes: Task 2의 `model.OrderIdempotencyKey`
- Produces:
  - `repository.NewOrderIdempotencyRepository(db)`
  - `(*OrderIdempotencyRepository).WithTx(tx)`
  - `InsertNew(records []*model.OrderIdempotencyKey) (inserted []uint64, err error)` — `ON CONFLICT DO NOTHING RETURNING id`, 반환은 **실제로 들어간 레코드의 ID**
  - `FindByUserKeys(pairs []UserKeyPair) ([]model.OrderIdempotencyKey, error)`
  - `SetOrderAndOutcome(id uint64, orderID uint, outcome model.OrderIdempotencyOutcome) error`
  - `UpdateOutcome(id uint64, outcome model.OrderIdempotencyOutcome) error`
  - `DeleteByIDs(ids []uint64) error`
  - `CountStalePending(olderThan time.Time) (int64, error)`
  - `repository.UserKeyPair{UserID uint; Key string}`

- [ ] **Step 1: RED 통합 테스트 작성**

```go
package repository

import (
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func uniqueIdemUserID() uint {
	return uint(time.Now().UnixNano() % 1_000_000_000)
}

func seedIdemRecord(userID uint, key string) *model.OrderIdempotencyKey {
	return &model.OrderIdempotencyKey{
		UserID:             userID,
		IdempotencyKey:     key,
		Fingerprint:        "fp-" + key,
		FingerprintVersion: 1,
		// outcome은 NOT NULL이다. 비워 두면 GORM이 빈 문자열을 넣어 CHECK에 걸린다.
		Outcome: model.OrderIdempotencyOutcomePending,
	}
}

func cleanupIdemRecords(t *testing.T, db *gorm.DB, userID uint) {
	t.Helper()
	require.NoError(t, db.Where("user_id = ?", userID).Delete(&model.OrderIdempotencyKey{}).Error)
}

// 배치 INSERT는 "어느 것이 실제로 들어갔는지"를 한 왕복에 알려줘야 한다.
// 요청마다 INSERT하면 배치의 존재 이유(왕복 절감)가 사라진다.
func TestIntegrationOrderIdempotencyInsertNewReturnsOnlyInserted(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewOrderIdempotencyRepository(db)
	userID := uniqueIdemUserID()
	defer cleanupIdemRecords(t, db, userID)

	first := seedIdemRecord(userID, "k1")
	inserted, err := repo.InsertNew([]*model.OrderIdempotencyKey{first})
	require.NoError(t, err)
	require.Len(t, inserted, 1)
	assert.Equal(t, first.ID, inserted[0])

	// 같은 키 재시도 + 새 키 → 새 키만 들어간다.
	dup := seedIdemRecord(userID, "k1")
	fresh := seedIdemRecord(userID, "k2")
	inserted, err = repo.InsertNew([]*model.OrderIdempotencyKey{dup, fresh})
	require.NoError(t, err)
	require.Len(t, inserted, 1)

	// ID는 실제로 들어간 레코드에 붙어야 한다. 반환 행을 슬라이스 순서대로 채우면
	// 충돌한 dup이 fresh의 ID를 갖고, follower가 owner의 행을 가리키게 된다.
	assert.Zero(t, dup.ID, "삽입되지 않은 레코드에 ID가 채워졌다")
	assert.NotZero(t, fresh.ID, "삽입된 레코드에 ID가 채워지지 않았다")
	assert.Equal(t, fresh.ID, inserted[0])

	var count int64
	require.NoError(t, db.Model(&model.OrderIdempotencyKey{}).
		Where("user_id = ?", userID).Count(&count).Error)
	assert.EqualValues(t, 2, count)
}

// 같은 배치에 같은 키가 두 번 오면 한 건만 들어가고, ID는 그중 하나에만 붙어야 한다.
// 둘 다 ID를 받으면 뒤쪽 요청이 owner처럼 행동해 엔진에 두 번 제출된다.
func TestIntegrationOrderIdempotencyInsertNewDeduplicatesWithinBatch(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewOrderIdempotencyRepository(db)
	userID := uniqueIdemUserID()
	defer cleanupIdemRecords(t, db, userID)

	first := seedIdemRecord(userID, "same")
	second := seedIdemRecord(userID, "same")
	inserted, err := repo.InsertNew([]*model.OrderIdempotencyKey{first, second})
	require.NoError(t, err)

	require.Len(t, inserted, 1)
	assert.NotZero(t, first.ID)
	assert.Zero(t, second.ID, "같은 배치의 중복 요청이 owner가 됐다")
	assert.Equal(t, first.ID, inserted[0])

	var count int64
	require.NoError(t, db.Model(&model.OrderIdempotencyKey{}).
		Where("user_id = ?", userID).Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

// 다른 사용자의 같은 키는 충돌하지 않는다. 전역 UNIQUE였다면 여기서 막힌다.
func TestIntegrationOrderIdempotencyKeyScopeIsPerUser(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewOrderIdempotencyRepository(db)
	userA := uniqueIdemUserID()
	userB := userA + 1
	defer cleanupIdemRecords(t, db, userA)
	defer cleanupIdemRecords(t, db, userB)

	inserted, err := repo.InsertNew([]*model.OrderIdempotencyKey{
		seedIdemRecord(userA, "shared"),
		seedIdemRecord(userB, "shared"),
	})
	require.NoError(t, err)
	assert.Len(t, inserted, 2)
}

func TestIntegrationOrderIdempotencyFindByUserKeys(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewOrderIdempotencyRepository(db)
	userID := uniqueIdemUserID()
	defer cleanupIdemRecords(t, db, userID)

	_, err := repo.InsertNew([]*model.OrderIdempotencyKey{
		seedIdemRecord(userID, "k1"), seedIdemRecord(userID, "k2"),
	})
	require.NoError(t, err)

	found, err := repo.FindByUserKeys([]UserKeyPair{{UserID: userID, Key: "k1"}})
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "k1", found[0].IdempotencyKey)
	assert.Equal(t, 1, found[0].FingerprintVersion)

	empty, err := repo.FindByUserKeys(nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// outcome과 updated_at은 한 UPDATE 문에서 함께 바뀌어야 한다.
func TestIntegrationOrderIdempotencyOutcomeUpdatesTouchUpdatedAt(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewOrderIdempotencyRepository(db)
	userID := uniqueIdemUserID()
	defer cleanupIdemRecords(t, db, userID)

	record := seedIdemRecord(userID, "k1")
	_, err := repo.InsertNew([]*model.OrderIdempotencyKey{record})
	require.NoError(t, err)

	var before model.OrderIdempotencyKey
	require.NoError(t, db.First(&before, record.ID).Error)

	time.Sleep(10 * time.Millisecond)
	require.NoError(t, repo.SetOrderAndOutcome(record.ID, 4242, model.OrderIdempotencyOutcomePending))

	var afterSet model.OrderIdempotencyKey
	require.NoError(t, db.First(&afterSet, record.ID).Error)
	require.NotNil(t, afterSet.OrderID)
	assert.EqualValues(t, 4242, *afterSet.OrderID)
	assert.Equal(t, model.OrderIdempotencyOutcomePending, afterSet.Outcome)
	assert.True(t, afterSet.UpdatedAt.After(before.UpdatedAt), "updated_at이 함께 갱신되지 않았다")

	time.Sleep(10 * time.Millisecond)
	require.NoError(t, repo.UpdateOutcome(record.ID, model.OrderIdempotencyOutcomeAccepted))

	var afterOutcome model.OrderIdempotencyKey
	require.NoError(t, db.First(&afterOutcome, record.ID).Error)
	assert.Equal(t, model.OrderIdempotencyOutcomeAccepted, afterOutcome.Outcome)
	assert.True(t, afterOutcome.UpdatedAt.After(afterSet.UpdatedAt))
}

// hold 검증에 실패한 미커밋 키는 지워야 재사용할 수 있다.
func TestIntegrationOrderIdempotencyDeleteByIDs(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewOrderIdempotencyRepository(db)
	userID := uniqueIdemUserID()
	defer cleanupIdemRecords(t, db, userID)

	keep := seedIdemRecord(userID, "keep")
	drop := seedIdemRecord(userID, "drop")
	_, err := repo.InsertNew([]*model.OrderIdempotencyKey{keep, drop})
	require.NoError(t, err)

	require.NoError(t, repo.DeleteByIDs([]uint64{drop.ID}))
	require.NoError(t, repo.DeleteByIDs(nil))

	found, err := repo.FindByUserKeys([]UserKeyPair{
		{UserID: userID, Key: "keep"}, {UserID: userID, Key: "drop"},
	})
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "keep", found[0].IdempotencyKey)
}

// 0행 변경을 성공으로 돌려주면 호출자는 계약이 깨진 것을 알 수 없다.
// DB 오류가 나지 않는 경로라 오직 RowsAffected 검사만이 이를 잡는다.
func TestIntegrationOrderIdempotencyStateChangesRejectZeroRows(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewOrderIdempotencyRepository(db)
	userID := uniqueIdemUserID()
	defer cleanupIdemRecords(t, db, userID)

	const missingID = uint64(1 << 62)

	t.Run("SetOrderAndOutcome은 없는 ID에서 실패한다", func(t *testing.T) {
		err := repo.SetOrderAndOutcome(missingID, 1, model.OrderIdempotencyOutcomeAccepted)
		require.Error(t, err)
	})

	t.Run("UpdateOutcome은 없는 ID에서 실패한다", func(t *testing.T) {
		err := repo.UpdateOutcome(missingID, model.OrderIdempotencyOutcomeAccepted)
		require.Error(t, err)
	})

	t.Run("DeleteByIDs는 일부만 존재하면 실패한다", func(t *testing.T) {
		record := seedIdemRecord(userID, "partial")
		_, err := repo.InsertNew([]*model.OrderIdempotencyKey{record})
		require.NoError(t, err)

		err = repo.DeleteByIDs([]uint64{record.ID, missingID})
		require.Error(t, err, "요청한 키 중 하나가 없는데 성공으로 처리됐다")
	})

	t.Run("DeleteByIDs는 전부 없으면 실패한다", func(t *testing.T) {
		err := repo.DeleteByIDs([]uint64{missingID})
		require.Error(t, err)
	})

	// 0은 "지울 것이 없다"가 아니라 ID 전달이 깨졌다는 신호다. 조용히 버리면 그 키가
	// PENDING으로 남는다 — 이 메서드가 막으려던 바로 그 상태다.
	t.Run("DeleteByIDs는 0 ID를 거부한다", func(t *testing.T) {
		require.Error(t, repo.DeleteByIDs([]uint64{0}))

		record := seedIdemRecord(userID, "with-zero")
		_, err := repo.InsertNew([]*model.OrderIdempotencyKey{record})
		require.NoError(t, err)

		require.Error(t, repo.DeleteByIDs([]uint64{record.ID, 0}),
			"0이 섞였는데 실제 ID만 지우고 성공했다")

		var count int64
		require.NoError(t, db.Model(&model.OrderIdempotencyKey{}).
			Where("id = ?", record.ID).Count(&count).Error)
		assert.EqualValues(t, 1, count, "0을 버리고 실제 행을 지웠다")
	})

	// 부분 누락에서 error를 돌려주는 것만으로는 부족하다. 호출자 트랜잭션 안에서
	// 실제로 지워진 행까지 함께 롤백되어야 "키를 소비하지 않는다"가 성립한다.
	t.Run("호출자 트랜잭션에서 부분 삭제가 롤백된다", func(t *testing.T) {
		record := seedIdemRecord(userID, "rollback")
		_, err := repo.InsertNew([]*model.OrderIdempotencyKey{record})
		require.NoError(t, err)

		txErr := db.Transaction(func(tx *gorm.DB) error {
			return repo.WithTx(tx).DeleteByIDs([]uint64{record.ID, missingID})
		})
		require.Error(t, txErr)

		var count int64
		require.NoError(t, db.Model(&model.OrderIdempotencyKey{}).
			Where("id = ?", record.ID).Count(&count).Error)
		assert.EqualValues(t, 1, count, "부분 삭제가 롤백되지 않았다")
	})
}

func TestIntegrationOrderIdempotencyCountStalePending(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewOrderIdempotencyRepository(db)
	userID := uniqueIdemUserID()
	defer cleanupIdemRecords(t, db, userID)

	stale := seedIdemRecord(userID, "stale")
	fresh := seedIdemRecord(userID, "fresh")
	done := seedIdemRecord(userID, "done")
	_, err := repo.InsertNew([]*model.OrderIdempotencyKey{stale, fresh, done})
	require.NoError(t, err)

	require.NoError(t, repo.SetOrderAndOutcome(stale.ID, 1, model.OrderIdempotencyOutcomePending))
	require.NoError(t, repo.SetOrderAndOutcome(fresh.ID, 2, model.OrderIdempotencyOutcomePending))
	require.NoError(t, repo.SetOrderAndOutcome(done.ID, 3, model.OrderIdempotencyOutcomeAccepted))

	require.NoError(t, db.Model(&model.OrderIdempotencyKey{}).Where("id = ?", stale.ID).
		Update("updated_at", time.Now().Add(-time.Hour)).Error)

	before, err := repo.CountStalePending(time.Now().Add(-30 * time.Minute))
	require.NoError(t, err)

	// 다른 테스트 데이터가 섞일 수 있어 절대값이 아니라 증분으로 본다.
	require.NoError(t, db.Model(&model.OrderIdempotencyKey{}).Where("id = ?", fresh.ID).
		Update("updated_at", time.Now().Add(-time.Hour)).Error)
	after, err := repo.CountStalePending(time.Now().Add(-30 * time.Minute))
	require.NoError(t, err)

	assert.Equal(t, before+1, after, "PENDING이면서 임계보다 오래된 것만 세야 한다")

	// ACCEPTED는 아무리 오래돼도 잡히지 않는다.
	require.NoError(t, db.Model(&model.OrderIdempotencyKey{}).Where("id = ?", done.ID).
		Update("updated_at", time.Now().Add(-time.Hour)).Error)
	withDone, err := repo.CountStalePending(time.Now().Add(-30 * time.Minute))
	require.NoError(t, err)
	assert.Equal(t, after, withDone, "PENDING이 아닌 레코드가 gauge에 잡혔다")
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/repository -run IntegrationOrderIdempotency`
Expected: FAIL — `undefined: NewOrderIdempotencyRepository`

- [ ] **Step 3: 구현**

```go
package repository

import (
	"fmt"
	"strings"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"gorm.io/gorm"
)

type OrderIdempotencyRepository struct {
	DB *gorm.DB
}

type UserKeyPair struct {
	UserID uint
	Key    string
}

func NewOrderIdempotencyRepository(db *gorm.DB) *OrderIdempotencyRepository {
	return &OrderIdempotencyRepository{DB: db}
}

func (r *OrderIdempotencyRepository) WithTx(tx *gorm.DB) *OrderIdempotencyRepository {
	return &OrderIdempotencyRepository{DB: tx}
}

// InsertNew는 배치를 한 문장으로 넣고 실제로 삽입된 레코드의 ID만 돌려줍니다.
//
// 반환되지 않은 요청은 기존 키(follower)입니다. 요청마다 INSERT하면 배치의 존재
// 이유(왕복 절감)가 사라지므로 ON CONFLICT DO NOTHING + RETURNING을 씁니다.
//
// GORM의 Create(&records)를 쓰지 않는 이유: DO NOTHING으로 일부 행이 빠지면 반환 행 수가
// 구조체 수보다 적은데, GORM은 반환 행을 슬라이스 **순서대로** 채운다. 그러면 충돌해서
// 삽입되지 않은 앞쪽 구조체가 뒤쪽 행의 ID를 갖게 되고, follower가 owner의 ID를 들고
// 다니게 된다(통합 테스트로 실제 확인). 그래서 (user_id, idempotency_key)로 명시적으로
// 되짚어 채운다.
func (r *OrderIdempotencyRepository) InsertNew(records []*model.OrderIdempotencyKey) ([]uint64, error) {
	if len(records) == 0 {
		return nil, nil
	}

	placeholders := make([]string, 0, len(records))
	args := make([]any, 0, len(records)*5)
	for _, record := range records {
		placeholders = append(placeholders, "(?, ?, ?, ?, ?)")
		args = append(args, record.UserID, record.IdempotencyKey,
			record.Fingerprint, record.FingerprintVersion, record.Outcome)
	}

	// created_at·updated_at은 DB 기본값(now())에 맡긴다.
	query := `
INSERT INTO order_idempotency_keys (user_id, idempotency_key, fingerprint, fingerprint_version, outcome)
VALUES ` + strings.Join(placeholders, ", ") + `
ON CONFLICT (user_id, idempotency_key) DO NOTHING
RETURNING id, user_id, idempotency_key`

	var rows []struct {
		ID             uint64
		UserID         uint
		IdempotencyKey string
	}
	if err := r.DB.Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}

	byKey := make(map[UserKeyPair]uint64, len(rows))
	for _, row := range rows {
		byKey[UserKeyPair{UserID: row.UserID, Key: row.IdempotencyKey}] = row.ID
	}

	inserted := make([]uint64, 0, len(rows))
	for _, record := range records {
		id, ok := byKey[UserKeyPair{UserID: record.UserID, Key: record.IdempotencyKey}]
		if !ok {
			continue
		}
		// 같은 배치에 같은 키가 두 번 있으면 한 행만 삽입된다. 그 ID는 앞선 구조체가
		// 이미 가져갔으므로 뒤쪽 구조체는 follower로 남겨 둔다.
		delete(byKey, UserKeyPair{UserID: record.UserID, Key: record.IdempotencyKey})
		record.ID = id
		inserted = append(inserted, id)
	}
	return inserted, nil
}

func (r *OrderIdempotencyRepository) FindByUserKeys(pairs []UserKeyPair) ([]model.OrderIdempotencyKey, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	tuples := make([][]any, 0, len(pairs))
	for _, pair := range pairs {
		tuples = append(tuples, []any{pair.UserID, pair.Key})
	}

	var records []model.OrderIdempotencyKey
	err := r.DB.Where("(user_id, idempotency_key) IN ?", tuples).Find(&records).Error
	return records, err
}

// SetOrderAndOutcome은 order_id·outcome·updated_at을 한 UPDATE 문에서 갱신합니다.
//
// 0행 변경은 성공이 아니라 오류입니다. 대상 행이 없는데 성공을 돌려주면 주문·hold는
// 커밋됐는데 키에 order_id가 연결되지 않은 채로 남고, 호출자는 그 사실을 알지 못합니다.
func (r *OrderIdempotencyRepository) SetOrderAndOutcome(id uint64, orderID uint, outcome model.OrderIdempotencyOutcome) error {
	result := r.DB.Model(&model.OrderIdempotencyKey{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"order_id":   orderID,
			"outcome":    outcome,
			"updated_at": time.Now().UTC(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("set order and outcome affected %d rows for idempotency key %d, expected 1",
			result.RowsAffected, id)
	}
	return nil
}

// UpdateOutcome은 outcome과 updated_at을 한 UPDATE 문에서 갱신합니다.
// outcome이 바뀌면 부분 인덱스에서 빠지고, updated_at은 전이 시각을 보존합니다.
//
// SetOrderAndOutcome과 같은 이유로 0행 변경은 오류입니다. 성공으로 처리하면 outcome
// 전이가 유실됐는데 실패 counter도 오르지 않아 관측조차 되지 않습니다.
func (r *OrderIdempotencyRepository) UpdateOutcome(id uint64, outcome model.OrderIdempotencyOutcome) error {
	result := r.DB.Model(&model.OrderIdempotencyKey{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"outcome":    outcome,
			"updated_at": time.Now().UTC(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("update outcome affected %d rows for idempotency key %d, expected 1",
			result.RowsAffected, id)
	}
	return nil
}

// DeleteByIDs는 이번 트랜잭션에서 삽입했지만 hold 검증에 실패한 키를 지웁니다.
// 커밋된 주문을 가리키는 키에는 절대 쓰지 않습니다.
//
// 요청한 수만큼 지워지지 않으면 오류입니다. 지우지 못한 키는 PENDING으로 남아 그
// 사용자의 재시도를 영구히 막습니다 — 검증 실패는 키를 소비하지 않는다는 계약이 깨집니다.
// 이 메서드는 호출자의 트랜잭션 안에서 실행되므로, 오류를 돌려주면 부분 삭제도 롤백됩니다.
func (r *OrderIdempotencyRepository) DeleteByIDs(ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}

	// 0은 걸러내지 않고 오류로 만든다. 여기 오는 값은 모두 방금 삽입한 레코드의 ID이므로,
	// 0이 섞였다는 것은 ID 전달이 깨졌다는 뜻이다. 조용히 버리면 그 키가 PENDING으로 남아
	// "검증 실패는 키를 소비하지 않는다"는 계약이 다시 소리 없이 깨진다.
	for _, id := range ids {
		if id == 0 {
			return fmt.Errorf("refusing to delete idempotency keys: id 0 in %d ids", len(ids))
		}
	}

	// 0은 위에서 걸렀으므로 여기서는 중복 제거만 한다.
	deduped := dedupeNonzeroUint64(ids)

	result := r.DB.Where("id IN ?", deduped).Delete(&model.OrderIdempotencyKey{})
	if result.Error != nil {
		return result.Error
	}
	if int(result.RowsAffected) != len(deduped) {
		return fmt.Errorf("delete idempotency keys removed %d rows, expected %d",
			result.RowsAffected, len(deduped))
	}
	return nil
}

// CountStalePending은 stale PENDING gauge의 원천입니다.
// order_idempotency_pending_updated_at 부분 인덱스가 이 조회를 받칩니다.
func (r *OrderIdempotencyRepository) CountStalePending(olderThan time.Time) (int64, error) {
	var count int64
	err := r.DB.Model(&model.OrderIdempotencyKey{}).
		Where("outcome = ? AND updated_at < ?", model.OrderIdempotencyOutcomePending, olderThan).
		Count(&count).Error
	return count, err
}
```

- [ ] **Step 4: 통과 확인**

```powershell
$env:GOEXCHANGE_TEST_DATABASE_DSN='host=localhost user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=55432 sslmode=disable'
go test ./internal/repository -run IntegrationOrderIdempotency -v -p 1
```

Expected: 8개 PASS(서브테스트 6개 포함)

> `DeleteByIDs`에 들어오는 값은 모두 방금 삽입한 레코드의 ID다. **0은 걸러내지 말고
> 오류로 만든다** — 조용히 버리면 ID 전달이 깨진 키가 `PENDING`으로 남아, 이 메서드가
> 막으려던 상태가 그대로 발생한다. 빈 슬라이스만 no-op이다.
>
> 상태 변경 메서드는 **0행 변경을 성공으로 돌려주면 안 된다**. 잘못된 ID로도 DB 오류가
> 나지 않으므로, `RowsAffected` 검사가 빠지면 "주문은 커밋됐는데 키에 `order_id`가 없는"
> 상태나 "검증 실패 키가 `PENDING`으로 소비된" 상태를 아무도 관측하지 못한다.
> `SetOrderAndOutcome`·`UpdateOutcome`은 정확히 1행, `DeleteByIDs`는 요청한 ID 수만큼
> 지워지지 않으면 error다.

> **확인 결과(가정이 깨진 지점):** GORM의 `Create(&records)` + `OnConflict{DoNothing}`은
> 삽입되지 않은 행의 `ID`를 0으로 남기지 **않는다**. 반환 행 수가 구조체 수보다 적으면
> 반환 행을 슬라이스 **순서대로** 채우므로, 충돌한 앞쪽 구조체가 뒤쪽 행의 ID를 가져간다
> (`dup.ID = fresh의 ID`, `fresh.ID = 0`). follower가 owner의 행을 가리키게 되므로
> Task 4·5의 owner/follower 판정이 통째로 어긋난다.
>
> 그래서 명시적 `INSERT ... ON CONFLICT DO NOTHING RETURNING id, user_id, idempotency_key`로
> 바꾸고, 반환 행을 `(user_id, idempotency_key)`로 되짚어 구조체에 채운다.

- [ ] **Step 5: 커밋**

권장 subject: `feat(order): 멱등성 키 저장소 추가`

**커버하는 검증**: 9c

---

### Task 4: 배치 그룹화와 owner/follower

**Files:**
- Modify: `internal/service/hold_coordinator.go`
- Create: `internal/service/hold_coordinator_idempotency_test.go`
- Create: `internal/service/hold_coordinator_idempotency_integration_test.go`
- Modify: `cmd/main.go` (코디네이터에 `IdemRepo` 배선)

**Interfaces:**
- Consumes: Task 1 지문, Task 3 repository
- Produces:
  - `service.holdRole` (`holdRoleOwner`, `holdRoleFollower`)
  - `holdRequest`에 `idem *idempotencyContext` 추가
  - `holdResult`에 `Role holdRole`, `Existing *model.OrderIdempotencyKey` 추가
  - `service.idempotencyContext{Key string; Fingerprint string; Version int; RecordID uint64}`
  - `service.groupIdempotentRequests(reqs []holdRequest) (owners []int, followers map[int]int, conflicts []int)` — 결정적
  - `(*HoldCoordinator).SubmitWithIdempotency(order *model.Order, idem *idempotencyContext) (holdResult, error)`

- [ ] **Step 1: 그룹화 결정성 RED 테스트 작성**

```go
package service

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func idemReq(userID uint, key, fingerprint string) holdRequest {
	return holdRequest{
		order: &model.Order{UserID: userID},
		idem:  &idempotencyContext{Key: key, Fingerprint: fingerprint, Version: CurrentOrderFingerprintVersion},
	}
}

// 같은 키·같은 지문이면 하나가 owner, 나머지는 follower다. 둘 다 owner가 되면
// hold는 한 번인데 엔진 제출이 두 번이 된다.
func TestGroupIdempotentRequestsAssignsOneOwnerPerKey(t *testing.T) {
	reqs := []holdRequest{
		idemReq(1, "k1", "fp"),
		idemReq(1, "k1", "fp"),
		idemReq(2, "k1", "fp"), // 다른 사용자 — 별개다
	}

	owners, followers, conflicts := groupIdempotentRequests(reqs)

	assert.Equal(t, []int{0, 2}, owners)
	assert.Equal(t, map[int]int{1: 0}, followers, "인덱스 1은 0을 따라야 한다")
	assert.Empty(t, conflicts)
}

// 같은 키·다른 지문이면 하나만 진행하고 나머지는 409다.
func TestGroupIdempotentRequestsMarksFingerprintConflicts(t *testing.T) {
	reqs := []holdRequest{
		idemReq(1, "k1", "fp-a"),
		idemReq(1, "k1", "fp-b"),
	}

	owners, followers, conflicts := groupIdempotentRequests(reqs)

	assert.Equal(t, []int{0}, owners)
	assert.Empty(t, followers)
	assert.Equal(t, []int{1}, conflicts)
}

// map 순회 순서에 맡기면 같은 입력이 실행마다 다른 결과를 낸다.
func TestGroupIdempotentRequestsIsDeterministic(t *testing.T) {
	build := func() []holdRequest {
		return []holdRequest{
			idemReq(1, "k1", "fp-a"),
			idemReq(1, "k1", "fp-b"),
			idemReq(1, "k2", "fp-c"),
			idemReq(2, "k1", "fp-d"),
		}
	}

	owners, followers, conflicts := groupIdempotentRequests(build())
	for i := 0; i < 50; i++ {
		o, f, c := groupIdempotentRequests(build())
		require.Equal(t, owners, o, "%d회차 owner가 달라졌다", i)
		require.Equal(t, followers, f, "%d회차 follower가 달라졌다", i)
		require.Equal(t, conflicts, c, "%d회차 conflict가 달라졌다", i)
	}
}

// 키 없는 요청은 그룹화 대상이 아니다 — 전부 owner로 남아야 한다.
func TestGroupIdempotentRequestsKeepsUnkeyedRequestsAsOwners(t *testing.T) {
	reqs := []holdRequest{
		{order: &model.Order{UserID: 1}},
		{order: &model.Order{UserID: 1}},
	}

	owners, followers, conflicts := groupIdempotentRequests(reqs)

	assert.Equal(t, []int{0, 1}, owners)
	assert.Empty(t, followers)
	assert.Empty(t, conflicts)
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/service -run GroupIdempotentRequests -v`
Expected: FAIL — `undefined: groupIdempotentRequests`

- [ ] **Step 3: 타입과 그룹화 구현**

`internal/service/hold_coordinator.go`의 기존 타입을 바꾸고 함수를 추가한다.

```go
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
	Existing *model.OrderIdempotencyKey // follower일 때 저장된 결과
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
```

- [ ] **Step 4: 통과 확인**

Run: `go test ./internal/service -run GroupIdempotentRequests -v`
Expected: 4개 PASS

- [ ] **Step 5: `HoldBatch`에 트랜잭션 순서 반영**

`HoldBatch`의 시그니처를 `HoldBatch(reqs []holdRequest) ([]holdResult, error)`로 바꾸고,
트랜잭션 맨 앞과 끝에 다음을 넣는다. 기존 지갑 락·검증·INSERT 로직은 그대로 두고
**대상 목록만 owner로 좁힌다.**

```go
// 1) 멱등성 레코드를 먼저 넣어 owner/follower를 가른다. 중복이 지갑 hold를
//    소비하지 않게 하려면 이 단계가 hold 계산보다 앞서야 한다.
idemRepo := c.IdemRepo.WithTx(tx)
newRecords := make([]*model.OrderIdempotencyKey, 0, len(owners))
recordIdx := make([]int, 0, len(owners))
for _, i := range owners {
	if reqs[i].idem == nil {
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
if _, err := idemRepo.InsertNew(newRecords); err != nil {
	return err
}

// 삽입되지 않은 것 = 기존 키. 그 요청은 배치에서 빼고 저장된 결과를 돌려준다.
insertedOwners := make([]int, 0, len(owners))
var lookup []repository.UserKeyPair
existingIdx := map[repository.UserKeyPair]int{}
for j, i := range recordIdx {
	if newRecords[j].ID != 0 {
		reqs[i].idem.RecordID = newRecords[j].ID
		insertedOwners = append(insertedOwners, i)
		continue
	}
	pair := repository.UserKeyPair{UserID: reqs[i].order.UserID, Key: reqs[i].idem.Key}
	lookup = append(lookup, pair)
	existingIdx[pair] = i
}
if len(lookup) > 0 {
	found, err := idemRepo.FindByUserKeys(lookup)
	if err != nil {
		return err
	}
	for k := range found {
		pair := repository.UserKeyPair{UserID: found[k].UserID, Key: found[k].IdempotencyKey}
		if i, ok := existingIdx[pair]; ok {
			record := found[k]
			results[i] = holdResult{Role: holdRoleFollower, Existing: &record}
		}
	}
}
```

실제 구현에서는 이 단계를 `claimIdempotencyKeys(tx, reqs, owners, results)`로 뽑았다.
반환값이 `activeOwners`(실제로 키를 선점한 인덱스)이고, 지갑 키 수집과 hold 검증 루프는
이 목록만 순회한다. 기존 키로 밀려난 요청은 `results[i] = {Role: follower, Existing: record}`로
채워져 배치에서 빠진다.

> 충돌한 키가 곧바로 조회되지 않으면(다른 트랜잭션이 지운 경우) 조용히 넘어가지 않고
> 오류를 낸다. 넘어가면 그 요청은 결과가 채워지지 않은 채 남는다.

hold 검증 루프는 실패한 요청의 키를 모은다.

```go
// 4) hold 검증에 실패한 owner의 키는 이번 트랜잭션에서 지운다.
//    HoldBatch는 실패분을 results에 격리하고 나머지와 함께 커밋하므로,
//    지우지 않으면 "검증 실패는 키를 소비하지 않는다"가 깨진다.
if len(failedRecordIDs) > 0 {
	if err := idemRepo.DeleteByIDs(failedRecordIDs); err != nil {
		return err
	}
}

// 전원 실패 조기 반환 경로에도 같은 정리가 필요하다.
if len(passing) == 0 {
	return nil
}

// 5) 성공한 owner의 레코드에 order_id와 PENDING을 기록한다.
//    ACCEPTED로 앞당겨 쓰지 않는다 — 엔진 제출은 이 트랜잭션 밖이다.
for _, ph := range passing {
	if reqs[ph.idx].idem == nil {
		continue
	}
	if err := idemRepo.SetOrderAndOutcome(
		reqs[ph.idx].idem.RecordID, ph.order.ID, model.OrderIdempotencyOutcomePending,
	); err != nil {
		return err
	}
}
```

`conflicts`의 요청에는 `results[i] = holdResult{Err: NewConflictErrorf("idempotency key reused with a different request")}`를 채운다.

- [ ] **Step 6: fallback 경로 반영**

`processBatch`의 fallback은 `persistAndHold`를 호출한다. 같은 순서를 적용한 메서드
`(*HoldCoordinator).persistAndHoldOne(req holdRequest) holdResult`로 바꾼다(코디네이터의
필드를 그대로 쓰므로 인자 6개를 다시 넘기지 않는다).

단건 경로에서는 검증 실패가 트랜잭션 전체를 롤백하므로 키도 함께 사라진다 — 배치 경로의
`DeleteByIDs`와 같은 효과다. 롤백 시 `order.ID`와 `idem.RecordID`를 0으로 되돌린다.

**폴백도 먼저 그룹화한다.** 요청을 전부 독립 처리하면 배치가 내렸을 판정이 뒤집힌다 —
같은 키의 앞 요청이 검증 실패로 롤백되면, 뒤의 다른 지문 요청이 409 대신 owner가 되어
주문을 만든다.

```go
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
```

- [ ] **Step 7: 회귀 확인**

Run: `go test ./internal/service -race -p 1`
Expected: 기존 hold coordinator 테스트 전부 PASS

배치 경로의 owner/follower 판정은 단위 테스트만으로는 증명되지 않는다. 통합 테스트 4개를
`hold_coordinator_idempotency_integration_test.go`에 둔다.

| 테스트 | 확인하는 것 |
|---|---|
| `SameKeyCreatesOneOrder` | 주문 1건, 키 1건, **hold 1회**(locked=100.05), follower가 leader의 주문을 받음 |
| `SameKeyDifferentFingerprintConflicts` | 하나만 성공, 나머지는 `ErrorKindConflict`, 주문 1건 |
| `FailedValidationReleasesKey` | 실패한 owner의 키가 **DB에서 사라짐**, 성공한 키는 남음 |
| `ExistingKeyReturnsStoredRecord` | 재시도가 follower + 저장된 `OrderID`를 받고 새 주문 없음 |
| `FallbackKeepsIdempotencyGrouping` | 폴백에서도 다른 지문은 409, 중복은 follower, 실패 키는 소비되지 않음 |

- [ ] **Step 8: 커밋**

권장 subject: `feat(order): hold 배치에 멱등성 키와 owner/follower 분리 도입`

**커버하는 검증**: 4b, 4c

---

### Task 5: 서비스 계약

**Files:**
- Modify: `internal/service/order_service.go`
- Modify: `internal/metrics/metrics.go`
- Modify: `internal/handler/order_handler.go` (새 반환 타입에 맞춘 최소 변경, 상태 매핑은 Task 6)
- Modify: `internal/service/service_integration_test.go` (+ acceptance·cancellation 통합 테스트)
- Create: `internal/service/order_idempotency_service_integration_test.go`

**Interfaces:**
- Consumes: Task 1·3·4
- Produces:
  - `CreateOrderInput.IdempotencyKey string`
  - `service.CreateOrderResult{Order *model.Order; Replay bool; Outcome model.OrderIdempotencyOutcome}`
  - `OrderService.CreateOrder(input CreateOrderInput) (*CreateOrderResult, error)`
  - `OrderService.OrderIdempotencyRepository *repository.OrderIdempotencyRepository`
  - `metrics.OrderIdempotencyUnknownTotal`, `metrics.OrderIdempotencyOutcomeUpdateFailuresTotal`

- [ ] **Step 0: 이 태스크가 쓰는 지표 두 개를 먼저 추가**

`internal/metrics/metrics.go`의 `var (...)` 블록에 넣는다. gauge와 monitor 오류 counter는
Task 7에서 추가한다.

```go
	// 보상 실패 후 UNKNOWN 기록에 성공한 건수. 실패해서 PENDING에 머문 경우는
	// 여기 잡히지 않으므로 아래 counter가 함께 필요하다.
	OrderIdempotencyUnknownTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "order_idempotency_unknown_total",
		Help: "Order idempotency records marked UNKNOWN after a failed compensation.",
	})

	OrderIdempotencyOutcomeUpdateFailuresTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "order_idempotency_outcome_update_failures_total",
		Help: "Failed attempts to record ACCEPTED/REJECTED/UNKNOWN on an idempotency record.",
	})
```

- [ ] **Step 1: 키 검증 RED 테스트 작성**

```go
func TestCreateOrderRequiresIdempotencyKey(t *testing.T) {
	svc := &OrderService{}

	for name, key := range map[string]string{
		"빈 값":  "",
		"공백":   "   ",
		"초과 길이": strings.Repeat("k", 129),
	} {
		t.Run(name, func(t *testing.T) {
			result, err := svc.CreateOrder(CreateOrderInput{
				UserID: 1, CoinSymbol: "BTC", Side: "BUY", OrderType: "LIMIT",
				Price: "100", Amount: "1", IdempotencyKey: key,
			})
			require.Error(t, err)
			assert.Nil(t, result)
			kind, ok := DomainErrorKind(err)
			require.True(t, ok)
			assert.Equal(t, ErrorKindValidation, kind)
		})
	}
}

// 128자 계약은 문자 수 기준이다. len()으로 세면 128자 한글 키가 384바이트라 거절돼
// DB CHECK(length() = 문자 수)와 어긋난다.
func TestNormalizeIdempotencyKeyCountsCharactersNotBytes(t *testing.T) {
	key, err := normalizeIdempotencyKey(strings.Repeat("가", 128))
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("가", 128), key)

	_, err = normalizeIdempotencyKey(strings.Repeat("가", 129))
	require.Error(t, err)
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/service -run CreateOrderRequiresIdempotencyKey -v`
Expected: FAIL — `unknown field IdempotencyKey`

- [ ] **Step 3: 키 검증·지문·follower 분기 구현**

`BuildOrderWithRegistry`를 **구문 파싱(`parseOrderRequest`)** 과 **시장 정책 검증
(`validateMarketPolicy`)** 으로 쪼갠다. `CreateOrder`는 파싱만 먼저 하고, 정책은 새 키에만
적용한다 — 정책은 시간이 지나 바뀌므로, 주문 생성 뒤 시장이 멈추면 같은 키의 정상 재시도가
replay 대신 4xx로 끝난다. `BuildOrderWithRegistry`는 두 함수를 순서대로 부르는 얇은 껍데기로
남겨 기존 호출부(테스트 포함)를 그대로 둔다.

```go
const maxIdempotencyKeyLength = 128

// 계약은 "공백 제외 1~128자"다. len()은 바이트를 세므로 멀티바이트 키에서 DB CHECK의
// length()(문자 수)와 단위가 어긋난다. 두 곳이 같은 단위를 쓰도록 rune으로 센다.
// import에 "unicode/utf8"을 추가한다.
func normalizeIdempotencyKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" || utf8.RuneCountInString(key) > maxIdempotencyKeyLength {
		return "", NewValidationErrorf("idempotency_key is required and must be 1..%d characters", maxIdempotencyKeyLength)
	}
	return key, nil
}

func (s *OrderService) CreateOrder(input CreateOrderInput) (*CreateOrderResult, error) {
	key, err := normalizeIdempotencyKey(input.IdempotencyKey)
	if err != nil {
		return nil, err
	}

	// 구문 파싱만 먼저 한다. 시장 정책은 시간이 지나 바뀌므로, 정책 검증을 여기서 하면
	// 이미 커밋된 요청의 재시도가 정책 변경 하나로 replay 대신 4xx가 된다.
	order, err := parseOrderRequest(input)
	if err != nil {
		return nil, err
	}

	// 지문 입력은 그대로 들고 다닌다. 기존 레코드는 저장된 버전의 규칙으로 다시 계산해야
	// 하므로, 현재 버전으로 계산한 문자열 하나만으로는 비교할 수 없다.
	fingerprintInput := OrderFingerprintInput{
		UserID:      order.UserID,
		CoinSymbol:  order.CoinSymbol,
		Side:        string(order.Side),
		OrderType:   string(order.OrderType),
		Price:       order.Price,
		Amount:      order.Amount,
		QuoteAmount: order.QuoteAmount,
	}
	fingerprint, err := ComputeOrderFingerprint(fingerprintInput, CurrentOrderFingerprintVersion)
	if err != nil {
		return nil, err
	}
	idem := &idempotencyContext{
		Key: key, Fingerprint: fingerprint, Version: CurrentOrderFingerprintVersion,
	}

	// 여기부터는 "지금 새 주문을 받아도 되는가"를 본다. 어느 검사에 걸리든 거절 직전에
	// 기존 키를 확인한다 — 이미 결정된 요청의 결과를 지금의 상황으로 덮어쓰면 안 된다.
	// 정상 경로(새 키)에서는 조회하지 않으므로 왕복이 늘지 않는다.
	if err := validateMarketPolicy(order, s.marketRulesRegistry()); err != nil {
		return s.rejectUnlessReplay(order.UserID, key, fingerprintInput, err)
	}

	// 엔진이 없으면 주문을 받을 수 없다. hold부터 잡으면 주문·자금이 처리될 경로 없이
	// 영구히 묶인다 — 주문에는 취소와 달리 command outbox가 없다.
	if s.MatchingEngine == nil {
		return s.rejectUnlessReplay(order.UserID, key, fingerprintInput,
			NewUnavailableErrorf("matching engine is not configured"))
	}

	// 입장 게이트: 엔진 유입이 포화면 DB 작업 전에 빠른 거절(503).
	if !s.MatchingEngine.IsIntakeAdmissible(order.CoinSymbol) {
		replay, err := s.replayExistingKey(order.UserID, key, fingerprintInput)
		if err != nil {
			return nil, err
		}
		if replay != nil {
			return replay, nil
		}

		metrics.OrdersAdmissionRejectedTotal.WithLabelValues("engine_gate").Inc()
		return nil, NewUnavailableErrorf("order intake is saturated, please retry shortly")
	}

	res, err := s.holdWithIdempotency(order, idem)
	if err != nil {
		return nil, err
	}

	// follower는 엔진에 제출하지 않는다. 제출하면 hold는 한 번인데 주문이 두 번 들어간다.
	if res.Role == holdRoleFollower {
		return s.followerResult(res, fingerprintInput)
	}
	order = res.Order

	// 바운디드 핸드오프: 매칭 처리량에 응답이 매달리지 않게. 주문은 이미
	// 영속화+홀드로 내구·정합 확정 상태다. 바운드 내 접수 못 하면(레이스로 포화)
	// 보상으로 홀드를 풀고 REJECTED로 종결한 뒤 503.
	submitted := s.MatchingEngine.TrySubmitOrder(&matching.Order{
		ID:                order.ID,
		UserID:            order.UserID,
		CoinSymbol:        order.CoinSymbol,
		Side:              order.Side,
		Price:             order.Price,
		Amount:            order.Amount,
		QuoteAmount:       matchingQuoteAmountForOrder(order),
		CreatedAt:         order.CreatedAt,
		EnqueuedAt:        time.Now(),
		OrderType:         order.OrderType,
		FilledAmount:      order.FilledAmount,
		FilledQuoteAmount: order.FilledQuoteAmount,
	}, s.acceptanceTimeout())
	if !submitted {
		metrics.OrdersAdmissionRejectedTotal.WithLabelValues("engine_handoff").Inc()
		return nil, s.rejectAcceptedOrderWithIdempotency(order, idem.RecordID)
	}
	// 엔진 접수 성공 — ACCEPTED로 전이한다. 실패하면 PENDING에 머문다(재요청은 202).
	if err := s.OrderIdempotencyRepository.UpdateOutcome(
		idem.RecordID, model.OrderIdempotencyOutcomeAccepted,
	); err != nil {
		metrics.OrderIdempotencyOutcomeUpdateFailuresTotal.Inc()
		log.Printf("order idempotency: ACCEPTED update failed for record %d: %v", idem.RecordID, err)
	}

	return &CreateOrderResult{Order: order, Outcome: model.OrderIdempotencyOutcomeAccepted}, nil
}

// followerResult는 follower 두 종류를 가른다.
//
//   - Existing != nil: 이전 요청이 이미 커밋한 키다. 저장된 레코드로 replay한다.
//   - Existing == nil && Order != nil: 같은 배치의 중복이다. leader가 이번에 주문을
//     만들었으므로 별도로 조회해 온 레코드가 없다. 엔진 제출 없이 202 PENDING이다.
//     여기서 replayResult를 부르면 정상적인 중복 요청이 503이 된다.
//   - 그 외: 결과를 만들지 못한 것이므로 503이다.
func (s *OrderService) followerResult(res holdResult, in OrderFingerprintInput) (*CreateOrderResult, error) {
	if res.Existing != nil {
		return s.replayResult(res.Existing, in)
	}
	if res.Order != nil {
		return &CreateOrderResult{
			Order:   res.Order,
			Replay:  true,
			Outcome: model.OrderIdempotencyOutcomePending,
		}, nil
	}
	return nil, NewUnavailableErrorf("idempotency record is unavailable, please retry")
}

// rejectUnlessReplay는 거절 직전의 마지막 확인이다. 기존 키면 저장된 결과를 돌려주고,
// 새 키면 주어진 거절 사유를 그대로 낸다.
//
// 조회는 거절할 때만 한다. 요청마다 미리 조회하면 정상 경로에 SELECT가 하나 는다.
func (s *OrderService) rejectUnlessReplay(
	userID uint, key string, in OrderFingerprintInput, rejection error,
) (*CreateOrderResult, error) {
	replay, err := s.replayExistingKey(userID, key, in)
	if err != nil {
		return nil, err
	}
	if replay != nil {
		return replay, nil
	}
	return nil, rejection
}

// replayExistingKey는 이미 커밋된 키가 있으면 그 결과를 돌려준다. 없으면 (nil, nil)이다.
func (s *OrderService) replayExistingKey(userID uint, key string, in OrderFingerprintInput) (*CreateOrderResult, error) {
	found, err := s.OrderIdempotencyRepository.FindByUserKeys(
		[]repository.UserKeyPair{{UserID: userID, Key: key}})
	if err != nil {
		// raw error를 그대로 내면 serviceErrorStatus의 default가 400으로 매핑한다.
		return nil, NewUnavailableErrorf("idempotency lookup failed, please retry")
	}
	if len(found) == 0 {
		return nil, nil
	}
	return s.replayResult(&found[0], in)
}

// replayResult는 저장된 결과로 응답을 재구성한다. 지문이 다르면 409다.
//
// 지문은 **레코드에 저장된 버전의 규칙으로** 다시 계산한다. 현재 버전으로만 비교하면
// 버전을 올리는 배포 하나로 기존 키의 정상 재시도가 전부 409가 된다.
func (s *OrderService) replayResult(record *model.OrderIdempotencyKey, in OrderFingerprintInput) (*CreateOrderResult, error) {
	if record == nil {
		return nil, NewUnavailableErrorf("idempotency record is unavailable, please retry")
	}

	stored, err := ComputeOrderFingerprint(in, record.FingerprintVersion)
	if err != nil {
		// 이 서버가 모르는 버전으로 저장된 레코드다(더 새 서버가 썼다). 비교할 수 없으므로
		// 409로 단정하지 않는다 — 정상 재시도일 수 있다.
		return nil, NewUnavailableErrorf(
			"idempotency record uses fingerprint version %d, which this server cannot verify",
			record.FingerprintVersion)
	}
	if record.Fingerprint != stored {
		return nil, NewConflictErrorf("idempotency key was used with a different request")
	}

	order := &model.Order{}
	if record.OrderID != nil {
		order.ID = *record.OrderID
	}
	return &CreateOrderResult{Order: order, Replay: true, Outcome: record.Outcome}, nil
}
```

- [ ] **Step 4: 보상 트랜잭션에 `REJECTED` 포함**

```go
// rejectAcceptedOrderWithIdempotency는 hold 해제·주문 REJECTED·outcome REJECTED를
// 한 트랜잭션에 넣는다. outcome을 밖에 두면 "hold는 풀렸는데 outcome은 PENDING"인
// 상태가 생기고, 재요청이 202를 받아 아직 진행 중인 것처럼 보인다.
func (s *OrderService) rejectAcceptedOrderWithIdempotency(order *model.Order, recordID uint64) error {
	// 트랜잭션이 롤백된 이유가 outcome 갱신 실패인지 구분한다. 그러지 않으면 REJECTED
	// 기록 실패가 counter에 잡히지 않고, 뒤이은 UNKNOWN이 성공하면 아무 흔적도 남지 않는다.
	rejectedUpdateFailed := false

	err := s.OrderRepository.DB.Transaction(func(tx *gorm.DB) error {
		orderRepo := s.OrderRepository.WithTx(tx)
		walletRepo := s.WalletRepository.WithTx(tx)
		ledgerRepo := s.LedgerRepository.WithTx(tx)

		if err := releaseInitialHold(walletRepo, ledgerRepo, order); err != nil {
			return err
		}
		if err := orderRepo.UpdateOrderExecution(
			order.ID, order.FilledAmount, order.FilledQuoteAmount, model.OrderStatusRejected,
		); err != nil {
			return err
		}
		if err := s.OrderIdempotencyRepository.WithTx(tx).UpdateOutcome(
			recordID, model.OrderIdempotencyOutcomeRejected); err != nil {
			rejectedUpdateFailed = true
			return err
		}
		return nil
	})
	if err == nil {
		return NewUnavailableErrorf("order intake is saturated, please retry shortly")
	}
	if rejectedUpdateFailed {
		metrics.OrderIdempotencyOutcomeUpdateFailuresTotal.Inc()
	}

	// 보상 실패 — hold가 잡힌 채 남는다. UNKNOWN은 best-effort 기록이다.
	if uerr := s.OrderIdempotencyRepository.UpdateOutcome(
		recordID, model.OrderIdempotencyOutcomeUnknown,
	); uerr != nil {
		metrics.OrderIdempotencyOutcomeUpdateFailuresTotal.Inc()
		log.Printf("order idempotency: UNKNOWN update failed for record %d: %v", recordID, uerr)
	} else {
		metrics.OrderIdempotencyUnknownTotal.Inc()
	}

	// 요청자 잘못이 아니다. raw error를 그대로 내면 serviceErrorStatus의 default가
	// 400으로 매핑한다(CancelOrder에서 e0ef22a로 고친 것과 같은 클래스).
	return NewUnavailableErrorf(
		"order intake saturated and hold release failed for order %d, retry is safe with the same key", order.ID)
}

func (s *OrderService) acceptanceTimeout() time.Duration {
	if s.AcceptanceTimeout > 0 {
		return s.AcceptanceTimeout
	}
	return defaultAcceptanceTimeout
}
```

- [ ] **Step 5: 기존 통합 테스트를 새 계약으로 갱신**

기존 호출부 13곳은 키 계약이 아니라 주문·홀드 동작을 본다. 호출마다 손으로 키를 넣는 대신
`service_integration_test.go`에 헬퍼를 두고 `orderService.CreateOrder(CreateOrderInput{` →
`createTestOrder(orderService, CreateOrderInput{`로 바꾼다. 반환 모양이 그대로라 나머지
코드는 손대지 않는다.

```go
// testIdemKeySeq는 테스트마다 고유한 멱등성 키를 만든다. 같은 키를 재사용하면
// 서로 다른 주문이 재시도로 오인된다.
var testIdemKeySeq atomic.Uint64

func createTestOrder(svc *OrderService, input CreateOrderInput) (*model.Order, error) {
	if input.IdempotencyKey == "" {
		// 공유 테스트 DB에는 이전 실행의 키가 남는다. 순번만 쓰면 실행마다 1부터 다시
		// 시작해 같은 키가 쌓이고, (user_id, key) UNIQUE 밖의 검사가 그 중복에 걸린다.
		input.IdempotencyKey = fmt.Sprintf("test-key-%d-%d", time.Now().UnixNano(), testIdemKeySeq.Add(1))
	}
	result, err := svc.CreateOrder(input)
	if err != nil {
		return nil, err
	}
	return result.Order, nil
}
```

**`cleanupServiceUsers`도 함께 고친다.** 주문 생성이 이제 항상 멱등성 키를 남기므로, 기존
정리 헬퍼는 불완전해졌다. 남겨 두면 공유 DB가 계속 커지고, 스키마를 검사하는 다른 테스트가
이전 실행의 행에 걸린다(실제로 `dbmigration`의 전역 UNIQUE 검사가 이 중복으로 실패했다).

```go
	// 주문 생성이 멱등성 키를 남기므로 함께 지운다.
	require.NoError(t, db.Where("user_id IN ?", userIDs).
		Delete(&model.OrderIdempotencyKey{}).Error)
```

- [ ] **Step 5b: 계약 통합 테스트 작성**

`order_idempotency_service_integration_test.go`에 넣는다. 엔진 제출 횟수를 세는
`countingAcceptanceEngine`이 필요하다 — 상태만 보면 "두 번 제출됐지만 결과가 같아 보이는"
경우를 구분할 수 없다.

| 테스트 | 확인하는 것 |
|---|---|
| `RetryWithSameKeyReplays` | 재시도가 `Replay=true`·같은 `order_id`, **엔진 제출 1회**, `ORDER_HOLD` 1건 |
| `SameKeyDifferentRequestConflicts` | 409, 주문 1건, 원래 주문 무변경 |
| `FailedValidationDoesNotConsumeKey` | 잔고 부족 실패 후 키가 DB에 없고, 잔고를 채우면 같은 키로 성공 |
| `SameBatchDuplicateIsNotUnavailable` | owner `ACCEPTED` + follower `PENDING`(**503 없음**), 엔진 제출 1회, `ORDER_HOLD` 1건 |

마지막 테스트는 `BatchSize=2`, `FlushInterval=2s`로 두 요청이 같은 배치에 들어가게 한 뒤
goroutine 2개로 같은 키를 동시에 보낸다. 도착 순서는 정해지지 않으므로 "owner 1건 +
follower 1건"으로 판정한다.

- [ ] **Step 6: 통과 확인**

```powershell
$env:GOEXCHANGE_TEST_DATABASE_DSN='host=localhost user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=55432 sslmode=disable'
go test -run 'Integration|CreateOrder' -v -p 1 ./internal/service
go test ./internal/service -race
```

- [ ] **Step 7: 커밋**

권장 subject: `feat(order): 주문 생성에 멱등성 키 계약 적용`

> **같은 배치 중복은 503이 아니다.** `HoldBatch`는 같은 배치의 follower에 leader의
> `Order`만 복사하고 `Existing`은 nil로 남긴다(레코드를 읽어 온 적이 없다). 이 경우를
> `replayResult`로 보내면 `record == nil` 분기에 걸려 정상 요청이 503이 된다.
>
> 통합 테스트로 고정한다: 같은 키·같은 지문 2건을 **한 배치에 확실히 들어가게** 제출하면
> owner는 엔진 제출까지 마치고 `Outcome=ACCEPTED`(핸들러에서 **200**), 같은 배치
> follower는 **503 없이** `Outcome=PENDING`(핸들러에서 **202**)이다. `ORDER_HOLD` 원장은
> 1건, fake engine의 `TrySubmitOrder` 호출 수는 **1**이다.
>
> 서비스 계층 테스트에서는 HTTP 코드가 아니라 `CreateOrderResult.Outcome`을 단언한다 —
> 상태 매핑은 Task 6 핸들러의 책임이다.

**커버하는 검증**: 1, 2, 7

---

### Task 6: HTTP 계약

**Files:**
- Modify: `internal/handler/order_handler.go`
- Modify: `internal/httpapi/response.go` (`WriteErrorWithData` 추가)
- Create: `internal/handler/order_idempotency_handler_integration_test.go`

**Interfaces:**
- Consumes: Task 5
- Produces: HTTP 200/202/400/409 계약, `idempotent_replay` 필드

- [ ] **Step 1: RED 테스트 작성**

```go
package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/auth"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/testdb"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// acceptingEngine은 주문을 항상 접수하는 최소 더블이다. 핸들러 계약만 보므로
// 매칭 동작은 필요 없다.
type acceptingEngine struct{}

func (acceptingEngine) SubmitOrder(*matching.Order) {}

func (acceptingEngine) TrySubmitOrder(*matching.Order, time.Duration) bool { return true }

func (acceptingEngine) IsIntakeAdmissible(string) bool { return true }

func (acceptingEngine) CancelOrder(matching.CancelOrderCommand) matching.CancelOrderResult {
	return matching.CancelOrderResult{}
}

func (acceptingEngine) RequestOrderBookSnapshot(string, int) (matching.OrderBookSnapshot, error) {
	return matching.OrderBookSnapshot{}, nil
}

func newCreateOrderHandler(db *gorm.DB) *OrderHandler {
	return NewOrderHandler(service.NewOrderService(
		repository.NewOrderRepository(db), repository.NewWalletRepository(db), acceptingEngine{}))
}

func seedFundedUser(t *testing.T, db *gorm.DB, userID uint) uint {
	t.Helper()

	require.NoError(t, db.Create(&model.User{ID: userID, Name: fmt.Sprintf("idem-%d", userID)}).Error)
	require.NoError(t, db.Create(&model.Wallet{
		UserID: userID, CoinSymbol: model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(100000),
		AvailableBalance: decimal.NewFromInt(100000),
		LockedBalance:    decimal.Zero,
	}).Error)

	t.Cleanup(func() {
		require.NoError(t, db.Where("user_id = ?", userID).Delete(&model.OrderIdempotencyKey{}).Error)
		require.NoError(t, db.Where("user_id = ?", userID).Delete(&model.LedgerEntry{}).Error)
		require.NoError(t, db.Where("user_id = ?", userID).Delete(&model.Order{}).Error)
		require.NoError(t, db.Where("user_id = ?", userID).Delete(&model.Wallet{}).Error)
		require.NoError(t, db.Delete(&model.User{}, userID).Error)
	})
	return userID
}

func validOrderBody() CreateOrderRequest {
	return CreateOrderRequest{
		CoinSymbol: "BTC", Side: "BUY", OrderType: "LIMIT",
		Price: "100", Amount: "1",
	}
}

func postOrder(t *testing.T, handler *OrderHandler, userID uint, key string, body CreateOrderRequest) *httptest.ResponseRecorder {
	t.Helper()

	payload, err := json.Marshal(body)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(auth.UserIDContextKey, userID)
	c.Request = httptest.NewRequest(http.MethodPost, "/orders", bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")
	if key != "" {
		c.Request.Header.Set("Idempotency-Key", key)
	}

	handler.CreateOrder(c)
	return recorder
}

// orderIDOf는 성공 응답(data)과 오류 응답(error+data) 양쪽에서 order_id를 꺼낸다.
func orderIDOf(t *testing.T, recorder *httptest.ResponseRecorder) uint64 {
	t.Helper()

	var body struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body), "body=%s", recorder.Body.String())
	require.Contains(t, body.Data, "order_id", "body=%s", recorder.Body.String())

	id, ok := body.Data["order_id"].(float64)
	require.True(t, ok, "body=%s", recorder.Body.String())
	return uint64(id)
}

// 키 누락은 400이다. 서비스의 ErrorKindValidation은 422로 매핑되므로 핸들러가 먼저 본다.
func TestIntegrationCreateOrderHandlerRequiresIdempotencyKey(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	userID := seedFundedUser(t, db, 919001)
	handler := newCreateOrderHandler(db)

	for name, key := range map[string]string{"헤더 없음": "", "공백만": "   "} {
		t.Run(name, func(t *testing.T) {
			recorder := postOrder(t, handler, userID, key, validOrderBody())
			assert.Equal(t, http.StatusBadRequest, recorder.Code, "body=%s", recorder.Body.String())
		})
	}

	var count int64
	require.NoError(t, db.Model(&model.Order{}).Where("user_id = ?", userID).Count(&count).Error)
	assert.Zero(t, count, "키 없는 요청이 주문을 만들었다")
}

func TestIntegrationCreateOrderHandlerReplaysSameKey(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	userID := seedFundedUser(t, db, 919002)
	handler := newCreateOrderHandler(db)

	first := postOrder(t, handler, userID, "key-1", validOrderBody())
	require.Equal(t, http.StatusOK, first.Code, "body=%s", first.Body.String())

	second := postOrder(t, handler, userID, "key-1", validOrderBody())
	require.Equal(t, http.StatusOK, second.Code, "body=%s", second.Body.String())

	assert.Equal(t, orderIDOf(t, first), orderIDOf(t, second))
	assert.Contains(t, second.Body.String(), "idempotent_replay")
	assert.NotContains(t, first.Body.String(), "idempotent_replay", "최초 요청에 replay 표시가 붙었다")
}

func TestIntegrationCreateOrderHandlerRejectsReusedKeyWithDifferentBody(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	userID := seedFundedUser(t, db, 919003)
	handler := newCreateOrderHandler(db)

	first := postOrder(t, handler, userID, "key-2", validOrderBody())
	require.Equal(t, http.StatusOK, first.Code, "body=%s", first.Body.String())

	changed := validOrderBody()
	changed.Amount = "2"
	second := postOrder(t, handler, userID, "key-2", changed)

	assert.Equal(t, http.StatusConflict, second.Code, "body=%s", second.Body.String())
}

// 네 outcome이 서로 다른 상태 코드로 나가야 한다. PENDING만 분기하고 나머지를 200으로
// 흘리면 접수되지 않은 주문이 "order accepted"로 표시된다.
func TestIntegrationCreateOrderHandlerMapsOutcomeToStatus(t *testing.T) {
	cases := map[string]struct {
		outcome    model.OrderIdempotencyOutcome
		wantStatus int
	}{
		"ACCEPTED는 200": {model.OrderIdempotencyOutcomeAccepted, http.StatusOK},
		"PENDING은 202":  {model.OrderIdempotencyOutcomePending, http.StatusAccepted},
		"REJECTED는 503": {model.OrderIdempotencyOutcomeRejected, http.StatusServiceUnavailable},
		"UNKNOWN은 503":  {model.OrderIdempotencyOutcomeUnknown, http.StatusServiceUnavailable},
	}

	userID := uint(919010)
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			userID++
			db := testdb.OpenIntegrationDB(t)
			seedFundedUser(t, db, userID)
			handler := newCreateOrderHandler(db)

			first := postOrder(t, handler, userID, "outcome-key", validOrderBody())
			require.Equal(t, http.StatusOK, first.Code, "body=%s", first.Body.String())

			// 저장된 outcome을 바꿔 재요청이 그 상태를 그대로 반영하는지 본다.
			require.NoError(t, db.Model(&model.OrderIdempotencyKey{}).
				Where("user_id = ? AND idempotency_key = ?", userID, "outcome-key").
				Update("outcome", tc.outcome).Error)

			replay := postOrder(t, handler, userID, "outcome-key", validOrderBody())
			require.Equal(t, tc.wantStatus, replay.Code, "body=%s", replay.Body.String())

			// 어떤 상태든 order_id는 준다 — 클라이언트가 주문 상태를 조회할 수 있어야 한다.
			assert.Equal(t, orderIDOf(t, first), orderIDOf(t, replay))
		})
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/handler -run IntegrationCreateOrderHandler`
Expected: FAIL — 헬퍼와 헤더 처리가 없다

- [ ] **Step 3: 핸들러 구현**

```go
func (h *OrderHandler) CreateOrder(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		httpapi.WriteError(c, http.StatusUnauthorized, httpapi.CodeAuthRequired, "authenticated user is required")
		return
	}

	var req CreateOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeBindingError(c, err)
		return
	}

	// 키 누락은 400이다. 서비스는 키 형식 오류를 ErrorKindValidation으로 내는데
	// serviceErrorStatus가 그것을 422로 매핑하므로, 여기서 먼저 본다.
	if strings.TrimSpace(c.GetHeader("Idempotency-Key")) == "" {
		httpapi.WriteError(c, http.StatusBadRequest, httpapi.CodeBadRequest,
			"Idempotency-Key header is required")
		return
	}

	result, err := h.OrderService.CreateOrder(service.CreateOrderInput{
		UserID:         userID,
		CoinSymbol:     req.CoinSymbol,
		Side:           req.Side,
		OrderType:      req.OrderType,
		Price:          req.Price,
		Amount:         req.Amount,
		QuoteAmount:    req.QuoteAmount,
		IdempotencyKey: c.GetHeader("Idempotency-Key"),
	})
	if err != nil {
		writeServiceError(c, err)
		return
	}

	// 네 outcome을 모두 분기한다. PENDING만 특별 처리하고 나머지를 200으로 흘리면
	// REJECTED·UNKNOWN 재요청이 "order accepted"로 표시된다 — 접수되지 않은 주문을
	// 접수됐다고 말하는 것이다.
	switch result.Outcome {
	case model.OrderIdempotencyOutcomeAccepted:
		body := gin.H{"message": "order accepted", "order_id": result.Order.ID}
		if result.Replay {
			body["idempotent_replay"] = true
		}
		httpapi.WriteData(c, http.StatusOK, body)

	// PENDING은 "주문은 있는데 그 뒤를 durable하게 알지 못한다"이다. 200은 "접수됐다"는
	// 거짓이 되고 503은 "없다"는 거짓이 되므로 202를 쓴다.
	case model.OrderIdempotencyOutcomePending:
		httpapi.WriteData(c, http.StatusAccepted, gin.H{
			"order_id":          result.Order.ID,
			"status":            string(result.Outcome),
			"idempotent_replay": result.Replay,
		})

	// REJECTED는 "접수되지 않았고 되돌렸다", UNKNOWN은 "되돌리다 실패했다"이다. 둘 다
	// 성공이 아니므로 503이되, order_id는 준다 — 클라이언트가 상태를 조회할 수 있어야 한다.
	case model.OrderIdempotencyOutcomeRejected, model.OrderIdempotencyOutcomeUnknown:
		httpapi.WriteErrorWithData(c, http.StatusServiceUnavailable, httpapi.CodeUnavailable,
			"order was not accepted, retry is safe with the same key",
			gin.H{"order_id": result.Order.ID, "status": string(result.Outcome)})

	default:
		httpapi.WriteError(c, http.StatusInternalServerError, httpapi.CodeInternal,
			"unknown idempotency outcome")
	}
}
```

> **네 outcome을 모두 덮는 handler 테스트가 필요하다** — 특히 `REJECTED`·`UNKNOWN`이
> 200이 아니라 `503 + order_id`인지. 엔진 실패를 주입하지 않고, 첫 요청 성공 뒤 저장된
> 레코드의 `outcome`을 직접 바꾸고 같은 키로 재요청하면 네 경우를 모두 만들 수 있다.

키 형식 오류는 서비스가 `ErrorKindValidation`으로 내는데 `serviceErrorStatus`는 그것을
422로 매핑한다. **키 누락은 400이어야 하므로** 위 구현처럼 핸들러에서 먼저 검사한다.

`httpapi`에는 오류에 데이터를 함께 싣는 헬퍼가 없으므로 `response.go`에 최소한으로 추가한다.
기존 `ErrorResponse`의 모양은 건드리지 않는다.

```go
type ErrorDetailResponse struct {
	Error Error       `json:"error"`
	Data  interface{} `json:"data"`
}

func WriteErrorWithData(c *gin.Context, status int, code string, message string, data interface{}) {
	setRetryAfterForOverload(c, status)
	c.JSON(status, ErrorDetailResponse{
		Error: Error{Code: normalizeCode(code), Message: normalizeMessage(message)},
		Data:  data,
	})
}
```

- [ ] **Step 4: 통과 확인**

```powershell
$env:GOEXCHANGE_TEST_DATABASE_DSN='host=localhost user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=55432 sslmode=disable'
go test ./internal/handler -run IntegrationCreateOrderHandler -v -p 1
```

- [ ] **Step 5: 커밋**

권장 subject: `feat(api): 주문 생성 HTTP 계약에 멱등성 키 반영`

**커버하는 검증**: 1, 2, 5

---

### Task 7: 지표와 monitor

**Files:**
- Modify: `internal/metrics/metrics.go`
- Create: `internal/service/order_idempotency_monitor.go`
- Create: `internal/service/order_idempotency_monitor_test.go`
- Modify: `cmd/main.go`

**Interfaces:**
- Produces:
  - `metrics.OrderIdempotencyStalePending`(Gauge), `metrics.OrderIdempotencyMonitorErrorsTotal`
  - (counter 두 개는 Task 5 Step 0에서 추가됨)
  - `service.NewOrderIdempotencyMonitor(counter stalePendingCounter)` with `Interval`, `Threshold`
  - `(*OrderIdempotencyMonitor).Run(ctx)`

- [ ] **Step 1: RED 테스트 작성**

```go
package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeStaleCounter struct {
	mu     sync.Mutex
	counts []int64
	errs   []error
	calls  int
}

func (f *fakeStaleCounter) CountStalePending(time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return 0, err
		}
	}
	if len(f.counts) == 0 {
		return 0, nil
	}
	value := f.counts[0]
	if len(f.counts) > 1 {
		f.counts = f.counts[1:]
	}
	return value, nil
}

func (f *fakeStaleCounter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// 30초를 먼저 기다리면 재기동 직후 창에서 stale PENDING이 보이지 않는다.
func TestOrderIdempotencyMonitorQueriesImmediately(t *testing.T) {
	counter := &fakeStaleCounter{counts: []int64{7}}
	monitor := NewOrderIdempotencyMonitor(counter)
	monitor.Interval = time.Hour // ticker가 돌기 전에 첫 조회가 있어야 한다

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { monitor.Run(ctx); close(done) }()

	require.Eventually(t, func() bool { return counter.callCount() >= 1 }, time.Second, 5*time.Millisecond)
	assert.EqualValues(t, 7, monitor.LastValue())

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("context 취소로 정지하지 않았다")
	}
}

// 조회 실패 시 gauge를 0으로 덮으면 "문제가 사라졌다"로 읽힌다. 실제로는 관측이
// 사라진 것이다.
func TestOrderIdempotencyMonitorKeepsLastValueOnError(t *testing.T) {
	counter := &fakeStaleCounter{counts: []int64{5}, errs: []error{nil, errors.New("db down")}}
	monitor := NewOrderIdempotencyMonitor(counter)
	monitor.Interval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go monitor.Run(ctx)

	require.Eventually(t, func() bool { return counter.callCount() >= 2 }, time.Second, 5*time.Millisecond)
	assert.EqualValues(t, 5, monitor.LastValue(), "조회 실패가 gauge를 0으로 덮었다")
	assert.Positive(t, monitor.ErrorCount())
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/service -run OrderIdempotencyMonitor -v`
Expected: FAIL — `undefined: NewOrderIdempotencyMonitor`

- [ ] **Step 3: gauge와 monitor 오류 counter 추가**

Task 5 Step 0에서 counter 두 개는 이미 넣었다. `internal/metrics/metrics.go`에 나머지를
추가한다.

```go
	// counter는 "그 순간 코드가 살아 있었다"를 전제한다. 프로세스가 hold 커밋 직후
	// 죽으면 아무 counter도 오르지 않으므로 gauge가 필요하다.
	OrderIdempotencyStalePending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "order_idempotency_stale_pending",
		Help: "Idempotency records still PENDING past the staleness threshold.",
	})

	OrderIdempotencyMonitorErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "order_idempotency_monitor_errors_total",
		Help: "Failed stale-pending queries. The gauge keeps its last value on failure.",
	})
```

- [ ] **Step 4: monitor 구현**

```go
package service

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
)

const (
	defaultStalePendingInterval  = 30 * time.Second
	defaultStalePendingThreshold = 5 * time.Minute
)

type stalePendingCounter interface {
	CountStalePending(olderThan time.Time) (int64, error)
}

// OrderIdempotencyMonitor는 stale PENDING 수를 gauge로 노출한다.
//
// 정산 worker에 얹지 않고 전용 컴포넌트로 둔다 — 책임이 섞이면 한쪽 장애가 다른 쪽
// 관측을 멈춘다.
type OrderIdempotencyMonitor struct {
	counter   stalePendingCounter
	Interval  time.Duration
	Threshold time.Duration
	Logger    *log.Logger

	lastValue atomic.Int64
	errors    atomic.Int64
}

func NewOrderIdempotencyMonitor(counter stalePendingCounter) *OrderIdempotencyMonitor {
	return &OrderIdempotencyMonitor{counter: counter}
}

func (m *OrderIdempotencyMonitor) LastValue() int64 { return m.lastValue.Load() }
func (m *OrderIdempotencyMonitor) ErrorCount() int64 { return m.errors.Load() }

// Run은 시작 직후 한 번 조회한 뒤 주기 ticker로 전환한다. 먼저 기다리면 재기동 직후
// 창에서 stale PENDING이 보이지 않는데, hold 커밋 직후 죽어서 생긴 레코드가 정확히
// 그 창에 있다.
func (m *OrderIdempotencyMonitor) Run(ctx context.Context) {
	m.observe()

	ticker := time.NewTicker(m.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.observe()
		}
	}
}

func (m *OrderIdempotencyMonitor) observe() {
	count, err := m.counter.CountStalePending(time.Now().Add(-m.threshold()))
	if err != nil {
		// gauge를 0으로 덮지 않는다. DB가 불안정할 때 0으로 떨어지면 "문제가
		// 사라졌다"로 읽히지만 실제로는 관측이 사라진 것이다.
		m.errors.Add(1)
		metrics.OrderIdempotencyMonitorErrorsTotal.Inc()
		m.logf("order idempotency monitor: stale pending query failed: %v", err)
		return
	}
	m.lastValue.Store(count)
	metrics.OrderIdempotencyStalePending.Set(float64(count))
}

func (m *OrderIdempotencyMonitor) logf(format string, args ...any) {
	if m.Logger != nil {
		m.Logger.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (m *OrderIdempotencyMonitor) interval() time.Duration {
	if m.Interval > 0 {
		return m.Interval
	}
	return defaultStalePendingInterval
}

func (m *OrderIdempotencyMonitor) threshold() time.Duration {
	if m.Threshold > 0 {
		return m.Threshold
	}
	return defaultStalePendingThreshold
}
```

- [ ] **Step 5: `cmd/main.go` 배선**

`reconciliationWorker` 기동 바로 뒤에 넣는다.

```go
	idempotencyMonitor := service.NewOrderIdempotencyMonitor(
		repository.NewOrderIdempotencyRepository(config.DB))
	go idempotencyMonitor.Run(backgroundCtx)
```

- [ ] **Step 6: 통과 확인**

```powershell
go test ./internal/service -run OrderIdempotencyMonitor -count=20 -race -v
go build ./...
go vet ./...
```

- [ ] **Step 7: 커밋**

권장 subject: `feat(order): 멱등성 지표와 stale PENDING monitor 추가`

**커버하는 검증**: 9b

---

### Task 8: 통합 검증

**Files:**
- Create: `internal/service/order_idempotency_integration_test.go`

**Interfaces:**
- Consumes: Task 1~7 전체

- [ ] **Step 1: 하니스와 동시성 검증 작성**

핵심은 **판정 기준**이다. 주문 상태가 아니라 **`ORDER_HOLD` 원장 건수**와
**fake engine의 `TrySubmitOrder` 호출 수**로 판정한다.

```go
// countingEngine은 엔진 제출 횟수를 센다. 원장만 보면 "hold 1회 · 엔진 제출 2회"를
// 놓친다 — owner/follower 분리가 없으면 정확히 그 상태가 된다.
type countingEngine struct {
	*matching.MatchingEngine
	mu      sync.Mutex
	submits int
}

func (e *countingEngine) TrySubmitOrder(order *matching.Order, within time.Duration) bool {
	e.mu.Lock()
	e.submits++
	e.mu.Unlock()
	return e.MatchingEngine.TrySubmitOrder(order, within)
}

func (e *countingEngine) submitCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.submits
}

func holdEntryCount(t *testing.T, db *gorm.DB, userID uint) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&model.LedgerEntry{}).
		Where("user_id = ? AND entry_type = ?", userID, model.LedgerEntryTypeOrderHold).
		Count(&count).Error)
	return count
}

// 검증 4. 애플리케이션 lock 없이 DB UNIQUE가 직렬화한다.
func TestIntegrationOrderIdempotencyConcurrentSameKeyCreatesOneOrder(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(60)
	defer cleanupServiceUsers(t, db, userID)
	seedKRWWallet(t, db, userID, decimal.NewFromInt(100_000_000))

	engine := &countingEngine{MatchingEngine: matching.NewMatchingEngine()}
	engine.Start()
	svc := newIdempotentOrderService(db, engine)

	const concurrency = 100
	results := make([]*CreateOrderResult, concurrency)
	errs := make([]error, concurrency)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = svc.CreateOrder(CreateOrderInput{
				UserID: userID, CoinSymbol: "BTC", Side: "BUY", OrderType: "LIMIT",
				Price: "100", Amount: "1", IdempotencyKey: "concurrent-key",
			})
		}(i)
	}
	close(start)
	wg.Wait()

	var orderID uint
	for i := range results {
		require.NoError(t, errs[i], "goroutine %d", i)
		require.NotNil(t, results[i])
		if orderID == 0 {
			orderID = results[i].Order.ID
		}
		assert.Equal(t, orderID, results[i].Order.ID, "goroutine %d가 다른 주문을 받았다", i)
	}

	// 판정은 상태가 아니라 원장 건수와 엔진 제출 횟수다.
	// 원장만 보면 "hold 1회 · 엔진 제출 2회"를 놓친다.
	assert.EqualValues(t, 1, holdEntryCount(t, db, userID), "hold가 두 번 잡혔다")
	assert.Equal(t, 1, engine.submitCount(), "엔진에 두 번 제출됐다")

	var orders int64
	require.NoError(t, db.Model(&model.Order{}).Where("user_id = ?", userID).Count(&orders).Error)
	assert.EqualValues(t, 1, orders)
}

// 검증 7b. HoldBatch는 잔액 부족을 격리하고 나머지와 함께 커밋하므로,
// 명시적으로 지우지 않으면 실패한 요청의 키까지 커밋된다.
func TestIntegrationOrderIdempotencyMixedBatchDoesNotConsumeFailedKey(t *testing.T) {
	db := openServiceIntegrationDB(t)
	richID := serviceTestUserID(61)
	poorID := serviceTestUserID(62)
	defer cleanupServiceUsers(t, db, richID, poorID)
	seedKRWWallet(t, db, richID, decimal.NewFromInt(100_000_000))
	seedKRWWallet(t, db, poorID, decimal.Zero) // 잔액 부족

	engine := &countingEngine{MatchingEngine: matching.NewMatchingEngine()}
	engine.Start()
	svc := newIdempotentOrderService(db, engine)

	var wg sync.WaitGroup
	var poorErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = svc.CreateOrder(CreateOrderInput{
			UserID: richID, CoinSymbol: "BTC", Side: "BUY", OrderType: "LIMIT",
			Price: "100", Amount: "1", IdempotencyKey: "rich-key",
		})
	}()
	go func() {
		defer wg.Done()
		_, poorErr = svc.CreateOrder(CreateOrderInput{
			UserID: poorID, CoinSymbol: "BTC", Side: "BUY", OrderType: "LIMIT",
			Price: "100", Amount: "1", IdempotencyKey: "poor-key",
		})
	}()
	wg.Wait()

	require.Error(t, poorErr, "잔액 부족이 성공했다")

	idemRepo := repository.NewOrderIdempotencyRepository(db)
	failed, err := idemRepo.FindByUserKeys([]repository.UserKeyPair{{UserID: poorID, Key: "poor-key"}})
	require.NoError(t, err)
	assert.Empty(t, failed, "검증 실패한 요청의 키가 커밋됐다 — 사용자가 그 키를 다시 쓸 수 없다")

	kept, err := idemRepo.FindByUserKeys([]repository.UserKeyPair{{UserID: richID, Key: "rich-key"}})
	require.NoError(t, err)
	assert.Len(t, kept, 1, "성공한 요청의 키까지 지워졌다")

	// 같은 키로 다시 시도하면 이번엔 성공해야 한다.
	seedKRWWallet(t, db, poorID, decimal.NewFromInt(100_000_000))
	retry, err := svc.CreateOrder(CreateOrderInput{
		UserID: poorID, CoinSymbol: "BTC", Side: "BUY", OrderType: "LIMIT",
		Price: "100", Amount: "1", IdempotencyKey: "poor-key",
	})
	require.NoError(t, err)
	assert.False(t, retry.Replay, "재사용 가능해야 할 키가 replay로 처리됐다")
}
```

나머지 7개는 같은 하니스 위에서 다음 단언을 갖는다.

> **실패 주입은 repository wrapper로 할 수 없다.** `OrderService`의 저장소 필드는 인터페이스가
> 아니라 concrete pointer(`*repository.OrderRepository` 등)라, B-1의 `blockableOutboxRepo`
> 패턴을 그대로 쓰려면 운영 코드를 인터페이스로 추상화해야 한다. 테스트 하나 때문에 운영
> 경로를 추상화하는 대신, **테스트용 PostgreSQL trigger**로 특정 UPDATE에서만 오류를 던진다.
>
> **trigger 함수도 영구 객체다.** 이름을 고정하면 다음 실행의 `CREATE FUNCTION`이 충돌하고,
> `DROP TRIGGER`만 하면 함수가 공유 DB에 계속 쌓인다. 테스트마다 고유한 이름을 쓰고
> cleanup에서 **trigger와 function을 모두** 지운다.
>
> ```go
> suffix := fmt.Sprintf("%d", time.Now().UnixNano()) // 테스트마다 고유
> fn := "fail_order_reject_" + suffix
> trg := fn + "_trg"
>
> require.NoError(t, db.Exec(fmt.Sprintf(`
> CREATE FUNCTION %s() RETURNS trigger AS $$
> BEGIN RAISE EXCEPTION 'injected failure'; END $$ LANGUAGE plpgsql`, fn)).Error)
>
> // 함수 생성 직후에 등록한다. 트리거 생성이 실패하면 require.NoError가 테스트를
> // 중단하므로, 뒤에 등록하면 함수가 공유 DB에 그대로 남는다.
> t.Cleanup(func() {
> 	require.NoError(t, db.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON orders`, trg)).Error)
> 	require.NoError(t, db.Exec(fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn)).Error)
> })
>
> require.NoError(t, db.Exec(fmt.Sprintf(`
> CREATE TRIGGER %s BEFORE UPDATE ON orders
> FOR EACH ROW WHEN (NEW.status = 'REJECTED' AND NEW.id = %d)
> EXECUTE FUNCTION %s()`, trg, orderID, fn)).Error)
> ```
>
> `order_idempotency_keys`의 `outcome` 전이도 같은 방식으로 막는다(`NEW.outcome = 'REJECTED'`,
> `'UNKNOWN'`, `'ACCEPTED'`). 주문 ID·레코드 ID로 조건을 좁혀 다른 테스트에 영향을 주지 않는다.

| 테스트 | 준비 | 단언 |
|---|---|---|
| `...SameBatchSameKey` (4b) | `HoldCoordinator.BatchSize`를 키워 두 요청이 한 배치에 들어가게 함 | 두 결과의 `Order.ID` 동일, `holdEntryCount == 1`, `engine.submitCount() == 1` |
| `...AllFailingBatchCleansKeys` (7c) | 잔액 0인 사용자 2명이 한 배치 | 두 키 모두 `FindByUserKeys`가 빈 결과, 주문 0건 |
| `...RejectedReplaysSameOrder` (8) | `TrySubmitOrder`가 항상 false인 fake engine | 첫 호출 503, 레코드 `outcome=REJECTED`, 같은 키 재호출이 **같은 `order_id`**, 주문 1건, `holdEntryCount == 1` |
| `...CompensationIsAtomic` (8b) | `orders`의 `REJECTED` UPDATE에서만 실패하는 trigger | hold가 풀리지 않음(지갑 `locked_balance` 불변) **AND** 주문이 `REJECTED`가 아님 — 부분 반영 0. outcome은 트랜잭션 밖 best-effort 기록이 성공하므로 `UNKNOWN`이다(`PENDING`이 아니다) |
| `...UnknownUpdateFailureKeepsPending` (8d) | `orders` REJECTED UPDATE와 `outcome='UNKNOWN'` UPDATE를 모두 막는 trigger | `outcome == PENDING`(UNKNOWN 기록까지 실패한 경우다), 주문 1건, `OrderIdempotencyOutcomeUpdateFailuresTotal` 증가 |
| `...AcceptedUpdateFailureKeepsPending` (8e) | 엔진 제출은 성공, `outcome='ACCEPTED'` UPDATE만 막는 trigger | `outcome == PENDING`, 같은 키 재호출이 `Replay == true`이고 `Outcome == PENDING`, `holdEntryCount == 1`, `engine.submitCount() == 1` |
| `...StalePendingIsObserved` (8g) | hold 커밋 후 `UpdateOutcome`을 건너뛰고, 레코드의 `updated_at`을 1시간 전으로 밀어 프로세스 종료를 모사 | `NewOrderIdempotencyMonitor(repo)`의 `Run`을 짧은 `Interval`로 돌린 뒤 `LastValue() >= 1` |

`8f`(`PENDING` 창의 재요청 → 202)는 Task 6의 handler 테스트에서 `outcome`을 `PENDING`으로
고정한 뒤 재요청해 **202**를 확인한다.

- [ ] **Step 2: 각 테스트를 RED로 확인한 뒤 통과시킨다**

각 시나리오마다 관련 구현을 되돌려 실패를 먼저 본다. 특히:
- owner/follower 분리를 제거하면 `submitCount()`가 2가 되는지
- 미커밋 키 정리를 제거하면 실패 키가 남는지

- [ ] **Step 3: 전체 확인**

```powershell
$env:GOEXCHANGE_TEST_DATABASE_DSN='host=localhost user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=55432 sslmode=disable'
go test -run Integration -v -p 1 ./internal/dbmigration ./internal/repository ./internal/service ./internal/handler
Remove-Item Env:GOEXCHANGE_TEST_DATABASE_DSN
go test ./... -race
```

- [ ] **Step 4: 커밋**

권장 subject: `test(order): 멱등성 키의 동시성·배치·상태 전이 통합 검증`

**커버하는 검증**: 3, 4, 4b, 7b, 7c, 8, 8b, 8c, 8d, 8e, 8f, 8g, 9

---

### Task 9: 프런트 계약

**Repository:** `C:\Users\dksco\OneDrive\Desktop\GoExchange\Go-exchange-front`

**Files:**
- Modify: `src/lib/api.ts`, `src/lib/api.test.ts`
- Modify: `src/components/trading/OrderForm.tsx`, `OrderForm.test.tsx`

**Interfaces:**
- Produces: `createOrder(token, input, idempotencyKey)`

- [ ] **Step 1: RED 테스트 작성**

```ts
it("createOrder가 Idempotency-Key 헤더를 보낸다", async () => {
  const fetchMock = vi.fn(async () =>
    new Response(JSON.stringify({ data: { message: "order accepted", order_id: 1 } }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    }),
  );
  vi.stubGlobal("fetch", fetchMock);

  await createOrder("token", validInput, "key-1");

  const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
  expect(new Headers(init.headers).get("Idempotency-Key")).toBe("key-1");
});
```

- [ ] **Step 2: 실패 확인**

Run: `npm test -- --run src/lib/api.test.ts`
Expected: FAIL — 인자가 3개가 아니다

- [ ] **Step 3: `api.ts` 구현**

```ts
export async function createOrder(
  token: string,
  input: CreateOrderInput,
  idempotencyKey: string,
): Promise<CreateOrderResponse> {
  return apiRequest<CreateOrderResponse>("/orders", {
    method: "POST",
    token,
    headers: { "Idempotency-Key": idempotencyKey },
    body: JSON.stringify(input),
  });
}
```

`apiRequest`가 `options.headers`를 이미 `new Headers(options.headers)`로 받으므로 추가
변경은 필요 없다.

- [ ] **Step 4: `OrderForm` 키 수명 RED 테스트**

```ts
// 사용자 주문 시도마다 키를 한 번 생성한다. 네트워크 재시도는 같은 키를 쓰고,
// 사용자가 다시 제출하면 새 키다(새 주문 의도).
it("네트워크 재시도는 같은 키를 재사용한다", async () => { /* ... */ });
it("사용자가 다시 제출하면 새 키를 만든다", async () => { /* ... */ });
it("202 응답을 실패로 표시하지 않는다", async () => { /* ... */ });
```

- [ ] **Step 5: `OrderForm` 구현**

`crypto.randomUUID()`로 키를 만들고 `useRef`에 보관한다. 제출 성공·사용자 재제출 시
새 키로 교체한다.

- [ ] **Step 6: 프런트 게이트**

```powershell
npm test
npm run lint
npm run build
```

- [ ] **Step 7: 커밋**(프런트 저장소)

권장 subject: `feat(trading): 주문 생성에 멱등성 키 전송`

**커버하는 검증**: 클라이언트 계약

---

### Task 10: E2E와 k6

**Files:**
- Modify (front): `tests/e2e/exchange.spec.ts`
- Modify (back): `_workspace/loadtest/order-spike-availability.js`, `_workspace/loadtest/crossing-flood.js`, `_workspace/loadtest/stress-hold3000.js`, `loadtest/order-spike-single-symbol.js`

- [ ] **Step 1: k6 헬퍼 추가**

각 스크립트에 iteration마다 새 키를 만드는 헬퍼를 넣는다.

```js
// iteration마다 새 키를 만든다. 재사용하면 두 번째부터 전부 replay가 되어
// 주문이 생성되지 않고 측정이 무의미해진다. 반대로 iteration 안의 재시도마다
// 새 키를 만들면 멱등성을 전혀 검증하지 못한다.
function newIdempotencyKey() {
  return `${__VU}-${__ITER}-${Date.now()}`;
}
```

주문 POST의 헤더에 `'Idempotency-Key': key`를 추가하고, 같은 iteration의 재시도에는
같은 `key` 변수를 쓴다.

- [ ] **Step 2: E2E 중복 제출 시나리오 추가**

```ts
test("duplicate order submission with the same key creates one order", async ({ request }) => {
  // 같은 키로 두 번 → 같은 order_id, 두 번째에 idempotent_replay
  // 주문 목록에 그 주문이 1건
});
```

- [ ] **Step 3: 확인**

```powershell
k6 run _workspace/loadtest/sli-classify.selftest.js
npx playwright test --grep "idempot|duplicate order"
```

- [ ] **Step 4: 커밋 두 개**(백엔드·프런트 각각)

권장 백엔드 subject: `test(load): 부하 하니스에 iteration별 멱등성 키 적용`
권장 프런트 subject: `test(e2e): 같은 키 중복 제출이 주문을 하나만 만드는지 검증`

---

### Task 11: 측정과 문서

**Files:**
- Create: `docs/benchmarks/36-YYYY-MM-DD-order-idempotency-key.md`
- Modify: `README.md`, `docs/refactor/README.md`, `docs/ENGINEERING-SUMMARY.md`

- [ ] **Step 1: 로컬 전체 게이트**

```powershell
$env:GOEXCHANGE_TEST_DATABASE_DSN='host=localhost user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=55432 sslmode=disable'
go test -run Integration -p 1 ./internal/dbmigration ./internal/repository ./internal/service ./internal/handler
Remove-Item Env:GOEXCHANGE_TEST_DATABASE_DSN
go test ./... -race
go vet ./...
git diff --check
```

- [ ] **Step 2: push·CI 초록 확인 후 측정 SHA 고정**

- [ ] **Step 3: 유료 GCP 실행 승인 요청**

35번과 같은 topology·게이트. **추가로 비용 실측 항목**을 부하 전후로 기록한다.

```sql
SELECT pg_size_pretty(pg_relation_size('order_idempotency_keys')) AS table_size,
       pg_size_pretty(pg_relation_size('order_idempotency_keys_user_key_unique')) AS unique_index,
       pg_size_pretty(pg_relation_size('order_idempotency_pending_updated_at')) AS partial_index;
SELECT pg_current_wal_lsn();
```

- [ ] **Step 4: 산출물 시크릿 게이트**

runbook §7.5 순서를 따른다. `_workspace`·`_artifacts`에는 정리본만 넣는다.

- [ ] **Step 5: 36번 문서 작성**

측정 SHA와 최종 문서 SHA를 구분한다. 인덱스 크기·WAL 증가량을 **기준선으로만** 기록하고
보존 정책 결정의 근거로 남긴다.

- [ ] **Step 6: 커밋·push·CI**

**커버하는 검증**: 10, 11

---

## Plan Completion Gate

- 설계 §6의 27개 검증이 테스트 이름과 원본 출력으로 추적된다
- 같은 키 동시 100회에서 **`ORDER_HOLD` 1건 AND 엔진 제출 1회**
- 혼합 배치·전원 실패 배치에서 실패 키가 소비되지 않는다
- 어떤 outcome UPDATE 실패에서도 중복 주문이 없고 `PENDING`이 유지된다
- `REJECTED` 기록이 보상 트랜잭션과 함께 커밋/롤백된다
- migration 008이 카탈로그 검증으로 잘못된 동명 인덱스를 막고, 실패 시 version 8을 남기지 않는다
- monitor가 시작 직후 1회 조회하고, 조회 실패가 gauge를 0으로 덮지 않는다
- 프런트·E2E·k6가 모두 키를 보내고, k6는 iteration마다 새 키를 쓴다
- 인덱스 크기·WAL 증가량이 기록됐다
- VM 4대가 TERMINATED이고 backend/frontend CI가 모두 PASS다
