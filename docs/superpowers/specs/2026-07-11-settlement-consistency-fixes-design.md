# 정산 정합성 버그 2건 수정: 심볼 파티셔닝 워커 + 지갑 락 정렬/재시도 (2026-07-11)

## 왜 필요했는지

아키텍처 전수 검토에서 "정합성 100%" 목표에 위배되는 살아있는 버그 2건이 발견됐다.

1. **MarketOrderDone 레이스 → 고객 자금 영구 동결.** 정산 워커 10개가 단일
   `ExecutionCh`를 경쟁 소비해서, 같은 시장가 주문의 trade 정산이 끝나기 전에
   완료(Done) 이벤트가 다른 워커에서 먼저 처리될 수 있었다. 이때
   `CompleteMarketOrder`가 conflict를 반환하면 로그만 남기고 버렸다 — 시장가는
   취소가 불가능하므로 잔여 hold가 영구 locked 상태로 남는다.
2. **지갑 락 순서 미정렬 → 데드락 → 체결 영구 미정산.** `SettleTrade`가 지갑을
   buyer KRW → buyer coin → seller KRW → seller coin 순서로 잠가서, 두 유저가
   서로 반대 역할인 두 체결이 동시에 정산되면 AB-BA 데드락이 성립했다. abort된
   정산은 `failed_settlements`에 기록만 되고 자동 재시도가 없어서, 일시적
   오류인데도 체결이 영구 미정산으로 남았다.

## 왜 이 방식을 선택했는지

### A-1. 심볼 파티셔닝 정산 워커 (cmd/main.go)

- 워커 N개(`GOEXCHANGE_SETTLEMENT_WORKERS`, 기본 10)에 각각 전용 채널(버퍼 256)을
  배정하고, 디스패처 goroutine이 `ExecutionCh`를 읽어 `FNV-1a(CoinSymbol) % N`으로
  라우팅한다. 같은 심볼의 이벤트는 항상 같은 워커가 Go 채널 FIFO로 처리하므로
  엔진이 만든 순서(trade들 → Done)가 보존된다. 엔진 코드는 무수정 — 디스패처가
  뒤에서 팬아웃만 하므로 결합도가 낮다.
- 핫 심볼은 워커 하나에서 직렬 처리되는데, 이는 순서 보장의 본질적 비용이고
  같은 주문의 DB 행 락이 어차피 직렬화하므로 실질 손해가 아니다. 쏠림 관측용으로
  `settlement_worker_queue_length{worker=N}` 게이지를 추가했다.
- 보조 방어선: 완료가 그래도 conflict/transient로 실패하면 in-place 백오프
  (50/100/200ms) 재시도 → 그래도 실패하면 **`failed_market_completions` 테이블에
  내구 기록** → 재시도 워커가 처리. Done 이벤트는 엔진 메모리에만 존재하므로
  로그로 버리면 복구 불가능하다는 게 내구 기록을 추가한 이유다. 특히 "trade
  정산이 실패해 `failed_settlements`로 간 경우"는 재시도 워커가 trade를 성공시킨
  뒤(10초+)에야 완료가 가능해서, ms 단위 in-place 재시도만으로는 못 덮는다.

### A-2. 지갑 락 ID 정렬 + transient 자동 재시도 (internal/service, internal/repository)

- `SettleTrade`의 지갑 획득을 2단계로 분리: 1차로 락 없이 4개 지갑의 ID만 확보
  (없는 지갑은 생성), 2차로 `WHERE id IN (...) ORDER BY id FOR UPDATE`로 한 번에
  잠근다(`WalletRepository.LockByIDs`). 모든 정산이 같은 순서로 잠그므로 지갑 간
  AB-BA 데드락이 성립하지 않는다. 잔고 산술은 반드시 2차에서 잠근 행으로만 한다
  (1차 값은 stale). 락 계층은 "orders 먼저 → wallets(ID 오름차순)"이며 주문 락
  (buy→sell)은 주문의 side가 고정이라 순환이 성립하지 않아 변경하지 않았다.
- transient 판정은 에러 메시지 문자열이 아니라 **SQLSTATE**(40P01 데드락, 40001
  직렬화 실패, 55P03 락 타임아웃)로 한다 — 메시지는 `lc_messages`에 따라 번역될
  수 있어서다. 저장된 실패 기록도 재분류할 수 있도록 기록 시점에 메시지 앞에
  `[SQLSTATE xxxxx]` 태그를 붙인다.
- 정산 경로에도 transient in-place 재시도(50/100/200ms)를 넣었다. 데드락 abort는
  보통 ms 단위로 해소되므로 대부분 여기서 끝나고, 10초 주기
  `SettlementRetryWorker`는 2차 방어선이다(OPEN + transient + RetryCount<5만
  재시도, 성공 시 RESOLVED, 실패 시 기존 upsert가 retry_count 증가). 5회 초과는
  기존대로 운영자 확인으로 넘긴다.
- 재시도용 trade 복원을 위해 `failed_settlements`에 `engine_sequence` /
  `engine_event_id` / `traded_at` 컬럼을 추가했다. 멱등성 키는 원본을 그대로
  보존하므로 이중 정산은 구조적으로 불가능하고, 재시도 정산이 워커 스트림 순서
  밖에서 실행돼도 fill/잔고 산술이 전부 가산적(commutative)이라 안전하다.
  수수료는 price×quantity에서 결정적으로 재계산된다 — **수수료율이 사용자별로
  달라지면 실패 기록에 수수료도 저장해야 한다**(현재는 고정 요율이라 무해).

## 검증 결과

- 회귀 테스트 `TestIntegrationConcurrentReversedSettlementsDoNotDeadlock`
  (역할이 교차하는 두 정산을 30라운드 동시 실행, 실제 Postgres 16):
  - **수정 전 코드(HEAD)로 스왑해 실행 → `deadlock detected (SQLSTATE 40P01)`
    반복 발생** — 테스트가 실제 버그를 잡는 것을 확인.
  - **수정 후 → 데드락 0건, 60건 전부 정산 성공.**
- `TestDispatchExecutionEventsRoutesSameSymbolInOrder`: 같은 심볼의 trade와
  Done이 같은 워커 큐에 순서대로 도착.
- `TestProcessMarketOrderDone*`: conflict 재시도 → 성공 시 기록 없음, 소진 시
  내구 기록, validation 오류는 재시도 없이 즉시 기록.
- `SettlementRetryWorker` 단위 테스트 7건: transient만 재시도, permanent/
  RetryCount 소진 스킵, 성공 시 resolve, 실패 시 재기록.
- 전체 스위트(통합 포함): `go test ./... -count=1` 전부 통과. 기존 정산/취소/
  멱등성 통합 테스트 회귀 없음.

## 범위 밖 (Out of Scope)

- trade outbox(엔진→정산 내구 로그) — 다음 단계. 이번 수정으로 "정산이 실패하는
  경우"는 닫혔지만 "정산 요청 자체가 크래시로 증발하는 경우"는 outbox가 필요하다.
- k6 스트레스 재측정 — 파티셔닝이 처리량에 주는 영향은 별도 벤치마크로.
- graceful shutdown, 스냅샷 코얼레싱.
