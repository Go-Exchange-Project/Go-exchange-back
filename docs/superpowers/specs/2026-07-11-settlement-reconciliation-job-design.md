# 정산 리컨실리에이션 잡 설계 (A-6)

## 왜 필요했는지

Fable5 코드 리뷰에서 "정합성 100%"는 주장이 아니라 증명이어야 한다는 지적이 나왔다:
원장(`ledger_entries`)과 지갑(`wallets`)이 항상 일치하는지, 자산 총량이 보존되는지를
**주기적으로 실제로 검사**하는 잡이 없으면, `b35839f`에서 고친 A-1/A-2 버그처럼 아직
발견 못 한 정합성 버그가 조용히 잔고를 틀어지게 만들어도 아무도 모른다.
이 프로젝트는 모든 지갑 변동(입금/락/해제/정산)이 예외 없이 `LedgerEntry`를 남기는
구조라, 리컨실리에이션에 필요한 데이터는 이미 갖춰져 있다 — 검증 로직만 없다.

## 왜 이 방식을 선택했는지

최초 설계안을 사용자가 다른 세션(Fable5)에 다시 검토를 맡겼고, 실제 함정이
코드로 확인됐다:

1. **원장 델타 합이 지갑 필드와 정확히 일치하지 않을 수 있다.** `internal/service/ledger.go:16`의
   `ledgerEntryFromWalletUpdate`가 델타를 `walletAvailableBalance(wallet)`(레거시
   `krw`/`quantity` 폴백 적용된 "유효 잔액") 기준으로 계산한다. `available_balance=0`인데
   `krw>0`인 레거시 지갑이 처음 거래되면, 원장에는 그 최초 구체화가 델타로 안 잡히고
   지갑 컬럼만 뛴다 — 원장 합과 지갑 필드 사이에 레거시 잔액만큼 영구 괴리가 생긴다.
   이건 버그가 아니라 범위 밖으로 미룬 "krw/quantity ↔ available/locked 이중 필드"
   문제의 실측 증거이므로, 위반이 아니라 별도 `legacy_mismatch`로 분류해 보고해야 한다.

   **최초 분류 휴리스틱(`krw != available+locked`)은 틀렸다** — `balance.go`의
   `walletBalanceUpdate`(196-213행)가 지갑을 갱신할 때마다 `krw`(또는 `quantity`)를
   `available+locked` 합으로 매번 재동기화한다. 즉 이 패턴은 **아직 한 번도 갱신
   안 된 지갑**에서만 관측되는데, 그 상태에선 원장 합(0)과 지갑 필드(0)가 이미
   일치해서 애초에 위반이 없다. 첫 거래(hold)가 나는 순간 `krw`가 재동기화되어
   신호가 사라지는데, 원장 괴리는 바로 그 순간 발생한다 — "위반 없는 지갑만
   레거시로, 레거시 때문에 위반한 지갑은 오히려 진짜 버그로" 분류하는 정반대
   동작이다. 대신 지갑별 **가장 이른 원장 항목**에서
   `available_balance_after - available_delta`(그 지갑이 원장에 처음 기록되던
   시점에 이미 존재했던, 추적 안 된 초기 잔액)를 구해서, 위반 gap이 이 값과
   정확히 일치하면 `legacy_mismatch`, 아니면 진짜 `ledger_wallet` 위반으로
   분류한다 — 레거시와 버그가 같은 지갑에 겹친 경우도 놓치지 않는다.

   **locked gap은 레거시로 설명되지 않는다.** `ledgerEntryFromWalletUpdate`가
   `available_delta`는 유효 잔액(폴백 적용) 기준으로, `locked_delta`는 원시
   `wallet.LockedBalance` 기준으로 계산한다(`ledger.go:16-17`) — 레거시 구체화는
   항상 `available` 쪽에서만 발생하고 `locked`는 처음부터 원장과 정확히 일치한다.
   따라서 `legacy_mismatch` 판정은 available gap이 `implied_initial_available`과
   일치하는 것만으로는 부족하고, **locked gap이 정확히 0**이어야 한다 — locked에도
   gap이 있다면 레거시로 설명 안 되는 별도 위반이 겹친 것이므로 `ledger_wallet`로
   분류한다. 또한 원장 항목이 하나도 없는 지갑은 `implied_initial_available`이
   NULL인데, 이 경우 0으로 취급한다(원장 기록 없이 잔액만 있는 지갑은 레거시
   패턴과 구분할 근거가 없으므로 안전하게 진짜 위반으로 분류).
2. **DEV_FUND는 KRW 전용이 아니다.** `internal/service/dev_wallet_service.go:61`의
   `normalizeFundWalletInput`이 임의 `coin_symbol`을 받는다 — BTC 등도 이 경로로
   충전된다. "KRW 총량 보존"을 KRW만 필터링 안 하고 검사하면 BTC 충전액이 섞여
   들어가 식이 깨진다. 자산별(`coin_symbol` GROUP BY)로 일반화하면 모든 자산의
   총량 보존을 공짜로 검증할 수 있다.

또한 "원장-지갑 합이 일치한다"는 **원장에 적힌 대로 지갑이 바뀌었는가**만 증명하지,
**바뀌어야 할 만큼만 바뀌었는가**(의미적 정합성)는 증명하지 않는다는 지적도 있었다.
예: 같은 hold가 버그로 두 번 release되면 지갑도 원장도 똑같이 두 번 정직하게
기록되므로 원장-지갑 검사는 통과하지만, 주문 관점에선 명백한 위반이다. 이를 완전히
잡으려면 미체결 주문별 잔여 hold를 재계산해 `locked_balance`와 대조해야 하는데,
hold 산식(지정가/시장가, 매수/매도 각각 다름)을 리컨실리에이션에 중복 구현해야 해서
리스크와 작업량이 크다 — 이번 스코프에서는 제외하고, 대신 훨씬 저렴하면서 A-1이
고친 버그 클래스를 직접 탐지하는 **오래된 시장가 주문 잔존 검사**(검사 3)를
안전망으로 추가한다.

## 아키텍처

### 검사 항목

**검사 1: 원장-지갑 일치 (`ledger_wallet`)**

`(user_id, coin_symbol)`별로 원장 델타 합이 현재 지갑 필드와 일치하는지. 지갑 읽기와
원장 집계를 별도 쿼리로 하면 그 사이에 정산이 끼어들어 가짜 위반이 뜬다(tolerance
0 설계라 위양성이 그대로 알람이 됨) — 반드시 **단일 SQL**(하나의 스냅샷)로 검사한다.
`LEFT JOIN (SELECT ... GROUP BY ...)` 형태는 Postgres가 조인 조건을 서브쿼리
안으로 밀어넣지 못해 배치(500지갑)마다 원장 전체(60만+ 행)를 다시 집계하므로,
지갑당 인덱스 레인지 스캔이 되는 `LEFT JOIN LATERAL`을 쓴다(`idx_ledger_entries_user_asset_created_at
(user_id, coin_symbol, ...)`가 이미 있어 그대로 탄다). 레거시 초기 잔액(가장
이른 원장 항목의 `available_balance_after - available_delta`)도 같은 LATERAL에서
함께 구한다:

```sql
SELECT
  w.id AS wallet_id, w.user_id, w.coin_symbol,
  w.available_balance, w.locked_balance,
  COALESCE(agg.available_delta_sum, 0) AS ledger_available_sum,
  COALESCE(agg.locked_delta_sum, 0) AS ledger_locked_sum,
  first_entry.implied_initial_available
FROM wallets w
LEFT JOIN LATERAL (
  SELECT SUM(available_delta) AS available_delta_sum,
         SUM(locked_delta)    AS locked_delta_sum
  FROM ledger_entries l
  WHERE l.user_id = w.user_id AND l.coin_symbol = w.coin_symbol
) agg ON true
LEFT JOIN LATERAL (
  SELECT available_balance_after - available_delta AS implied_initial_available
  FROM ledger_entries l
  WHERE l.user_id = w.user_id AND l.coin_symbol = w.coin_symbol
  ORDER BY l.id ASC
  LIMIT 1
) first_entry ON true
WHERE w.id > $1
ORDER BY w.id
LIMIT $2
```

각 행에서 `available_balance != ledger_available_sum` 또는
`locked_balance != ledger_locked_sum`이면 위반 후보다. 이때
`available_gap := available_balance - ledger_available_sum`,
`locked_gap := locked_balance - ledger_locked_sum`,
`implied := COALESCE(implied_initial_available, 0)`로 두고,
**`available_gap == implied AND locked_gap == 0`이면 `check="legacy_mismatch"`,
아니면 `check="ledger_wallet"`(진짜 버그)**로 분류한다 — 레거시와 진짜 버그가 같은
지갑에 겹친 경우, 또는 원장이 아예 없는 지갑(`implied`가 NULL→0으로 취급)에
잔액만 있는 경우 모두 `ledger_wallet`로 안전하게 잡힌다. 스트레스 테스트
DB는 dev-fund 경로로만 자금이 들어가므로 레거시 패턴이 없을 것으로 예상되지만,
확인 없이 가정하지 않고 실제로 분류해서 첫 실행 결과를 본다.

**검사 2: 자산별 총량 보존 (`asset_conservation`)**

자산(`coin_symbol`)별로 `Σ(available+locked) + (KRW일 때만 누적 수수료) == Σ(DEV_FUND delta)`.
수수료는 `internal/service/fee.go`에서 항상 KRW로만 부과되므로(`BuyerFeeAsset`/
`SellerFeeAsset`이 항상 `model.KRWAssetSymbol`), 코인 자산은 수수료 항이 0이 되어
동일한 쿼리 형태로 일반화된다. 하나의 CTE로 스냅샷 일관성을 확보한다:

```sql
WITH wallet_totals AS (
  SELECT coin_symbol, SUM(available_balance + locked_balance) AS total
  FROM wallets
  GROUP BY coin_symbol
),
fee_totals AS (
  SELECT 'KRW' AS coin_symbol,
         COALESCE(SUM(buyer_fee), 0) + COALESCE(SUM(seller_fee), 0) AS total
  FROM trades
),
funded_totals AS (
  SELECT coin_symbol, SUM(available_delta) AS total
  FROM ledger_entries
  WHERE entry_type = 'DEV_FUND'
  GROUP BY coin_symbol
)
SELECT
  COALESCE(w.coin_symbol, f.coin_symbol) AS coin_symbol,
  COALESCE(w.total, 0) AS wallet_total,
  COALESCE(fee.total, 0) AS fee_total,
  COALESCE(f.total, 0) AS funded_total
FROM wallet_totals w
FULL OUTER JOIN funded_totals f ON f.coin_symbol = w.coin_symbol
LEFT JOIN fee_totals fee ON fee.coin_symbol = COALESCE(w.coin_symbol, f.coin_symbol)
```

(`FULL OUTER JOIN`이라 `w.coin_symbol`이 NULL일 수 있으므로 `fee_totals` 조인 키는
`COALESCE(w.coin_symbol, f.coin_symbol)`을 써야 한다 — dev-fund가 지갑을 먼저
생성하므로 실제로 funded-only 자산이 나오긴 어렵지만, `FULL OUTER`를 쓴 이상
조인도 일관되게 처리한다.)

`wallet_total + fee_total != funded_total`이면 위반. (전체 자산을 한 번에 조회 —
지갑 배치 순회와 무관하게 매 실행 1회만 수행.)

**검사 3: 오래된 시장가 주문 잔존 (`stale_market_order`)**

```sql
SELECT id, user_id, coin_symbol, status, created_at
FROM orders
WHERE order_type = 'MARKET'
  AND status IN ('PENDING', 'PARTIAL')
  AND created_at < NOW() - INTERVAL '5 minutes'
```

한 건이라도 나오면 위반 — 시장가는 오더북에 rest하지 않으므로 5분 넘게 열려있으면
완료(A-1이 고친 `MarketOrderDone` 처리 또는 그 재시도)가 어딘가에서 유실됐다는 뜻이다.
`SettlementRetryWorker`의 `RetryCount` 소진 등으로 재시도 워커가 놓친 케이스의 최종
안전망 역할.

**검사 4 (범위 밖, 2단계)**: 미체결 주문별 잔여 hold 재계산 합 == `locked_balance`.
hold 산식 중복 구현 리스크 때문에 이번엔 제외.

### 실행 구조

`SettlementRetryWorker`와 같은 패턴으로 `ReconciliationWorker`를 신설한다
(`internal/service/reconciliation_worker.go`):

```go
type ReconciliationWorker struct {
    DB       *gorm.DB
    Interval time.Duration
    Logger   *log.Logger
}

func (w *ReconciliationWorker) Run(ctx context.Context) {
    w.RunOnce() // 기동 직후 1회 — 배포/재시작 직후가 정합성이 가장 의심스러운 시점
    ticker := time.NewTicker(w.interval())
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            w.RunOnce()
        }
    }
}

func (w *ReconciliationWorker) RunOnce() {
    violations := w.checkLedgerWallet()       // 검사 1, keyset 페이지네이션(500건씩)
    violations = append(violations, w.checkAssetConservation()...) // 검사 2
    violations = append(violations, w.checkStaleMarketOrders()...) // 검사 3
    w.recordAndReport(violations)
}
```

- **페이지네이션**: 검사 1은 `OFFSET`이 아니라 `WHERE id > $lastID ORDER BY id LIMIT 500`
  (keyset)으로 지갑 테이블을 순회한다.
- **인터벌**: `GOEXCHANGE_RECONCILIATION_INTERVAL` 환경변수(초 단위, 미설정 시 기본
  3600 = 1시간), `config/runtime.go`의 기존 패턴(`parsePositiveIntEnv` 재사용)을
  따른다. 통합 테스트에서 짧은 인터벌로 검증 가능.
- **내구 기록**: 위반이 발견된 경우에만 `reconciliation_violations` 테이블에
  insert한다(위반이 없으면 아무것도 안 씀 — 로그 전용이면 컨테이너 재시작 시
  유실되므로). 스키마:
  ```go
  const maxReconciliationViolationDetailLength = 2048

  type ReconciliationViolation struct {
      ID          uint      `gorm:"primaryKey"`
      CheckName   string    `gorm:"not null"` // ledger_wallet | asset_conservation | stale_market_order | legacy_mismatch
      SubjectKey  string    `gorm:"not null;index"` // 예: "wallet:123", "coin:BTC", "order:42" — grouping/dedup용
      Detail      string    `gorm:"type:text;not null"` // 어떤 유저/심볼/얼마나 차이나는지, 최대 2048자
      DetectedAt  time.Time `gorm:"not null"`
  }
  ```
  `SubjectKey`는 위반의 대상을 구조화해 담는다(`Detail`은 free-text로 두되, 나중에
  "같은 지갑이 언제부터 언제까지 위반 상태였나" 같은 분석이 가능하도록). 같은 위반이
  해소될 때까지 매 실행마다 새 행이 쌓이는 걸 감안한 설계다(이번 스코프에서는
  dedup하지 않음 — 위반 지속 이력 자체가 유용한 데이터).
  `internal/model/reconciliation_violation.go`에 정의하고, `cmd/main.go`와
  `internal/testdb/integration.go`의 `AutoMigrate(...)` 목록에 `&model.ReconciliationViolation{}`를
  추가한다(`FailedMarketCompletion` 추가 때와 동일한 절차, 별도 SQL 파일 불필요).
- **메트릭**: `reconciliation_violations{check="ledger_wallet"|"asset_conservation"|"stale_market_order"|"legacy_mismatch"}`
  게이지(`_total` 접미사는 카운터 전용이라 피함) + `reconciliation_last_run_timestamp_seconds`
  게이지. **매 실행마다 4개 라벨 전부를 그 회차의 위반 건수로 set한다**(위반이 0건인
  검사도 명시적으로 0으로 갱신 — 안 그러면 해소된 위반의 이전 값이 게이지에 계속
  남아 가짜 알람이 지속된다). 반대로 **검사 쿼리 자체가 에러로 실패하면 해당 라벨은
  갱신하지 않는다**(조용히 0으로 보고하면 "위반 없음"과 "검사를 못 돌림"이 구분 안
  돼 가짜 정상이 된다) — 대신 `reconciliation_check_errors_total{check=...}` 카운터를
  올리고 로그를 남긴다(이건 실패 횟수 누적이 맞는 진짜 카운터이므로 `_total`이 맞다).
  알람 조건은 `reconciliation_violations > 0`(단, `legacy_mismatch`는 알려진 항목이라
  별도 라벨로 분리해뒀으니 운영자가 다르게 취급 가능) 및 `reconciliation_check_errors_total`
  증가.
- **자동 교정은 하지 않는다** — 탐지/보고만. `decimal.Decimal`은 정확한 값이라
  오차 허용치 없이 정확히 일치해야 정상.
- `cmd/main.go`에서 `SettlementRetryWorker`와 나란히 기동, `context.Background()`
  기반으로 서버 생명주기 동안 계속 실행.

### 향후 확장 여지 (설계에만 기록, 이번 스코프 아님)

원장 테이블이 스트레스 테스트 1회에 60만 행 이상 쌓인다. 매시간 전체 집계는 당분간
문제없지만 비용이 데이터 증가에 비례해 계속 늘어난다 — 나중에는 "지갑별 마지막
검증 시점 이후의 원장만 증분 집계 + 스냅샷 테이블" 구조로 전환할 여지가 있다.

## 검증 방법

기존 통합 테스트(`seedSettlementRows` 등)는 원장 없이 지갑에 잔액을 직접 insert하는
경우가 있어 그 자체로 검사 1의 위반 후보이고, `go test ./...`는 여러 패키지의
통합 테스트를 같은 공유 테스트 DB에 병렬로 돌린다. 따라서 **"위반 0건"을 전역
단언하는 테스트는 다른 패키지가 남긴 데이터에 의해 flaky해진다** — 아래처럼
자기 테스트가 만든 대상만 걸러서 단언한다.

- **검사 1**: 결과를 자기 테스트가 사용한 `user_id`/`wallet_id`로 필터링해서
  "내 유저에 대한 위반 없음/있음"만 단언한다(전역 0건 단언 금지).
- **검사 2**: 전역 SUM 집계라 필터링으로는 격리가 안 된다 — 테스트 전용 유니크
  심볼(예: `"RCN" + 타임스탬프`)을 만들어 dev-fund 경로로만 충전하면, 검사 2가
  `coin_symbol` GROUP BY이므로 그 심볼 행만 조회해 단언할 수 있다(다른 테스트가
  건드리지 않는 심볼이라 완전히 격리됨).
- **검사 3**: 결과를 자기 테스트가 생성한 `order_id`로 필터링해서 단언한다.
- 픽스처 시나리오: 의도적으로 지갑 필드를 원장과 어긋나게 만들어 검사 1이 정확히
  잡는지(위 유저 필터 적용), 위 유니크 심볼로 KRW/코인을 섞어 충전한 뒤 검사 2가
  자산별로 올바르게 분리해 계산하는지, 5분 넘은 PENDING 시장가 주문 픽스처에서
  검사 3이 잡는지(위 주문 필터 적용) — 전부 실제 Postgres 대상 통합 테스트로 확인.
- `go test ./... -count=1` 전체 스위트 통과(위 격리 전략으로 flaky 없이).
- 서버 로컬 기동 후 로그로 첫 실행이 "위반 0건"을 보고하는지 수동 확인 — 이건
  스트레스 DB(dev-fund만 사용, 다른 테스트 데이터 없음) 기준이라 전역 단언이
  유효하다. legacy_mismatch도 0건이어야 정상, 아니면 원인 조사.

## 범위 밖 (Out of Scope)

- krw/quantity ↔ available/locked 이중 필드 통합 데이터 마이그레이션 — 별도 작업.
- 검사 4(미체결 주문별 잔여 hold 재계산) — 2단계.
- 위반 자동 복구/교정.
- 원장 증분 집계/스냅샷 테이블로의 전환.

## 검증 결과 (2026-07-11 구현 완료 후 실측)

- **단위 테스트** (`internal/service/reconciliation_worker_test.go`, DB 불필요):
  분류 로직 7건 — gap 없음/레거시로 설명되는 gap/locked gap 존재 시 항상 진짜
  위반/implied와 불일치하는 gap/원장 없는 지갑(NULL→ledger_wallet)/보존식
  성립·불성립 — 전부 통과. RunOnce 오케스트레이션 4건 — 위반 기록+게이지 set,
  위반 0건일 때 게이지 0으로 명시적 갱신, **검사 쿼리 실패 시 게이지 미갱신 +
  에러 카운터 증가**, 풀 페이지 후 다음 keyset 페이지 요청 — 전부 통과.
- **리포지토리 통합 테스트** (실제 Postgres 16): 원장-지갑 일치/불일치(gap 700-500=200
  정확히 산출)/원장 없는 지갑의 implied NULL/keyset 커서 동작/유니크 심볼
  자산 보존/오래된·신선한 시장가 주문 구분/위반 insert — 전부 통과.
- **워커 엔드투엔드 통합 테스트** (`reconciliation_worker_integration_test.go`):
  위반 3종(진짜 원장 괴리, 레거시 패턴, 10분 경과 시장가)을 주입하고 RunOnce 실행 →
  `reconciliation_violations` 테이블에 각각 `ledger_wallet` / `legacy_mismatch` /
  `stale_market_order`로 정확히 분류·기록되고, 정상 지갑은 기록되지 않음을 확인.
  위반을 해소한 뒤 재실행하면 새 행이 쌓이지 않는 것도 확인.
- **전체 스위트**: `go test ./... -count=1` (통합 포함) 전 패키지 통과.
- **구현 중 발견한 픽스처 버그**: 시장가 매수 주문 픽스처가 `amount=1`로 생성되어
  `ck_orders_shape_by_type`(003 마이그레이션: 시장가 매수는 `amount=0 AND
  quote_amount>0`) 제약에 걸렸다 — 검사 3용 테스트 픽스처 2곳을 올바른 형태로
  수정. DB 제약이 테스트 픽스처의 잘못된 데이터를 잡아낸 사례.
- **남은 수동 확인**: 스트레스 DB(dev-fund 경로만 사용)에서 서버 기동 후 첫 실행이
  전 검사 위반 0건(legacy_mismatch 포함)을 보고하는지 — 다음 배포 때 확인.
