# 정산 리컨실리에이션 잡 설계 (A-6)

## 왜 필요했는지

Fable5 코드 리뷰에서 "정합성 100%"는 주장이 아니라 증명이어야 한다는 지적이 나왔다:
원장(`ledger_entries`)과 지갑(`wallets`)이 항상 일치하는지, 자산 총량이 보존되는지를
**주기적으로 실제로 검사**하는 잡이 없으면, `b35839f`에서 고친 A-1/A-2 버그처럼 아직
발견 못 한 정합성 버그가 조용히 잔고를 틀어지게 만들어도 아무도 모른다.
이 프로젝트는 모든 지갑 변동(입금/락/해제/정산)이 예외 없이 `LedgerEntry`를 남기는
구조라, 리컨실리에이션에 필요한 데이터는 이미 갖춰져 있다 — 검증 로직만 없다.

## 왜 이 방식을 선택했는지

최초 설계안을 사용자가 다른 세션(Fable5)에 다시 검토를 맡겼고, 두 가지 실제 함정이
코드로 확인됐다:

1. **원장 델타 합이 지갑 필드와 정확히 일치하지 않을 수 있다.** `internal/service/ledger.go:16`의
   `ledgerEntryFromWalletUpdate`가 델타를 `walletAvailableBalance(wallet)`(레거시
   `krw`/`quantity` 폴백 적용된 "유효 잔액") 기준으로 계산한다. `available_balance=0`인데
   `krw>0`인 레거시 지갑이 처음 거래되면, 원장에는 그 최초 구체화가 델타로 안 잡히고
   지갑 컬럼만 뛴다 — 원장 합과 지갑 필드 사이에 레거시 잔액만큼 영구 괴리가 생긴다.
   이건 버그가 아니라 범위 밖으로 미룬 "krw/quantity ↔ available/locked 이중 필드"
   문제의 실측 증거이므로, 이 패턴(`krw ≠ available+locked`)은 위반이 아니라 별도
   `legacy_mismatch`로 분류해 보고한다.
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
0 설계라 위양성이 그대로 알람이 됨) — 반드시 **단일 SQL**(하나의 스냅샷)로 검사한다:

```sql
SELECT
  w.id AS wallet_id, w.user_id, w.coin_symbol,
  w.available_balance, w.locked_balance, w.krw, w.quantity,
  COALESCE(l.available_delta_sum, 0) AS ledger_available_sum,
  COALESCE(l.locked_delta_sum, 0) AS ledger_locked_sum
FROM wallets w
LEFT JOIN (
  SELECT user_id, coin_symbol,
         SUM(available_delta) AS available_delta_sum,
         SUM(locked_delta) AS locked_delta_sum
  FROM ledger_entries
  GROUP BY user_id, coin_symbol
) l ON l.user_id = w.user_id AND l.coin_symbol = w.coin_symbol
WHERE w.id > $1
ORDER BY w.id
LIMIT $2
```

각 행에서 `available_balance != ledger_available_sum` 또는
`locked_balance != ledger_locked_sum`이면 위반 후보다. 단, 그 지갑이
`krw != available_balance + locked_balance`(KRW 지갑) 또는
`quantity != available_balance + locked_balance`(코인 지갑)인 **레거시 이중 필드
불일치 패턴**을 보이면, 이 위반을 `check="ledger_wallet"`이 아니라
`check="legacy_mismatch"`로 분류해 별도 카운트한다(운영자가 "진짜 버그"와
"알려진 마이그레이션 필요 항목"을 구분할 수 있도록). 스트레스 테스트 DB는 dev-fund
경로로만 자금이 들어가므로 레거시 패턴이 없을 것으로 예상되지만, 확인 없이
가정하지 않고 실제로 분류해서 첫 실행 결과를 본다.

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
LEFT JOIN fee_totals fee ON fee.coin_symbol = w.coin_symbol
```

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
  type ReconciliationViolation struct {
      ID          uint      `gorm:"primaryKey"`
      CheckName   string    `gorm:"not null"` // ledger_wallet | asset_conservation | stale_market_order | legacy_mismatch
      Detail      string    `gorm:"type:text;not null"` // 어떤 유저/심볼/얼마나 차이나는지
      DetectedAt  time.Time `gorm:"not null"`
  }
  ```
  `AutoMigrate`로 생성(이 프로젝트의 기존 마이그레이션 방식, 별도 SQL 파일 불필요).
- **메트릭**: `reconciliation_violations{check="ledger_wallet"|"asset_conservation"|"stale_market_order"|"legacy_mismatch"}`
  게이지(매 실행마다 해당 회차의 위반 건수로 갱신, `_total` 접미사는 카운터 전용이라
  피함) + `reconciliation_last_run_timestamp_seconds` 게이지. 알람 조건은
  `reconciliation_violations > 0`(단, `legacy_mismatch`는 알려진 항목이라 별도 라벨로
  분리해뒀으니 운영자가 다르게 취급 가능).
- **자동 교정은 하지 않는다** — 탐지/보고만. `decimal.Decimal`은 정확한 값이라
  오차 허용치 없이 정확히 일치해야 정상.
- `cmd/main.go`에서 `SettlementRetryWorker`와 나란히 기동, `context.Background()`
  기반으로 서버 생명주기 동안 계속 실행.

### 향후 확장 여지 (설계에만 기록, 이번 스코프 아님)

원장 테이블이 스트레스 테스트 1회에 60만 행 이상 쌓인다. 매시간 전체 집계는 당분간
문제없지만 비용이 데이터 증가에 비례해 계속 늘어난다 — 나중에는 "지갑별 마지막
검증 시점 이후의 원장만 증분 집계 + 스냅샷 테이블" 구조로 전환할 여지가 있다.

## 검증 방법

- 단위/통합 테스트: 정상 상태(위반 0건)에서 각 검사가 위반을 보고하지 않는지, 의도적으로
  지갑 필드를 원장과 어긋나게 만든 픽스처에서 검사 1이 정확히 잡는지, DEV_FUND로
  KRW/BTC를 섞어 충전한 뒤 검사 2가 자산별로 올바르게 분리해 계산하는지, 5분 넘은
  PENDING 시장가 주문 픽스처에서 검사 3이 잡는지, 실제 Postgres 대상 통합 테스트로 확인.
- `go test ./... -count=1` 전체 스위트 통과.
- 서버 로컬 기동 후 로그로 첫 실행이 "위반 0건"을 보고하는지 수동 확인(스트레스 DB
  기준 — legacy_mismatch도 0건이어야 정상, 아니면 원인 조사).

## 범위 밖 (Out of Scope)

- krw/quantity ↔ available/locked 이중 필드 통합 데이터 마이그레이션 — 별도 작업.
- 검사 4(미체결 주문별 잔여 hold 재계산) — 2단계.
- 위반 자동 복구/교정.
- 원장 증분 집계/스냅샷 테이블로의 전환.
