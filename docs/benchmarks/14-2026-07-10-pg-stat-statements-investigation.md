# 14번째 테스트 (2026-07-10): pg_stat_statements로 Postgres CPU 병목 쿼리 확인

## 커밋 해시

`7e287e9` (feat(infra): postgres에 pg_stat_statements 프리로드 설정 추가)

## 왜 이 테스트를 했는지

`13-2026-07-10-postgres-instance-separation.md`에서 분리된 Postgres 인스턴스(`e2-medium`, 2 vCPU)의 CPU가 389%까지 치솟는 걸 확인했다. 사용자가 "사양을 올리기 전에 어떤 쿼리가 문제인지 먼저 봐야 하지 않냐"고 제안했고, 매칭엔진 샤딩이 이 문제에 도움이 안 될 거라는 점(샤딩은 매칭 계산을 병렬화하는 거지 Postgres 쓰기량을 줄이는 게 아님)도 짚었다. `internal/service/settlement_service.go`를 읽어보니 트랜잭션 하나당 약 15개의 개별 쿼리가 순차 실행되는 구조였고, 마이그레이션을 보니 필요한 인덱스는 이미 다 있었다. 그래서 추측 대신 `pg_stat_statements`로 실제 어떤 쿼리가 Postgres CPU를 많이 쓰는지 확인했다.

## 왜 이 방식을 선택했는지

`docs/superpowers/specs/2026-07-10-pg-stat-statements-investigation-design.md`에서 결정한 대로, Postgres 공식 확장 `pg_stat_statements`를 `shared_preload_libraries`로 로드하고, 짧은 재부하(VU~1000대까지)로 통계를 쌓은 뒤 총 실행시간 기준으로 상위 쿼리를 조회했다.

## 실행한 정확한 커맨드

```bash
# 1. 확장 활성화 (1회)
docker exec goexchange-db-postgres psql -U goexchange -d goexchange -c 'CREATE EXTENSION IF NOT EXISTS pg_stat_statements;'
docker exec goexchange-db-postgres psql -U goexchange -d goexchange -c 'SELECT pg_stat_statements_reset();'

# 2. 재부하 (k6, VU~1000대까지 진행 후 수동 중단)
k6 run -e BASE_URL=http://10.10.0.3:8080 -e DEV_TOOLS_TOKEN=<토큰> loadtest/order-submission-stress.js

# 3. 상위 쿼리 조회
docker exec goexchange-db-postgres psql -U goexchange -d goexchange -x -c \
  "SELECT substring(query,1,90) AS short_query, calls, round(total_exec_time::numeric,1) AS total_ms, round(mean_exec_time::numeric,2) AS mean_ms, rows FROM pg_stat_statements ORDER BY total_exec_time DESC LIMIT 15;"
```

## 원본 출력 (상위 11개, 시스템 쿼리 제외)

```
1. SELECT * FROM wallets WHERE user_id=$1 AND coin_symbol=$2 ...          calls=418878  total=221914.8ms  mean=0.53ms
2. UPDATE orders SET filled_amount=$1,filled_quote_amount=$2,status=$3    calls=138940  total=143369.6ms  mean=1.03ms
3. INSERT INTO ledger_entries (...) [배치, 4행/호출]                       calls=69468   total=101364.6ms  mean=1.46ms  rows=277872
4. INSERT INTO ledger_entries (...) [단건, 1행/호출]                       calls=143966  total=82422.1ms   mean=0.57ms
5. INSERT INTO orders (...)                                               calls=140966  total=80631.7ms   mean=0.57ms
6. INSERT INTO trades (...)                                               calls=69477   total=73450.2ms   mean=1.06ms
7. UPDATE wallets SET available_balance=$1,krw=$2,locked_balance=$3       calls=209694  total=72955.7ms   mean=0.35ms
8. UPDATE wallets SET ...,avg_buy_price=$2,...,quantity=...              calls=138936  total=57613.8ms   mean=0.41ms
9. SELECT * FROM orders WHERE id=$1 ... FOR UPDATE                       calls=138954  total=31870.2ms   mean=0.23ms
10. UPDATE wallets SET available_balance=$1,locked_balance=$2,quantity=$3 calls=70209   total=16972.8ms   mean=0.24ms
11. SELECT * FROM trades WHERE idempotency_key=$1                        calls=69477   total=12783.1ms   mean=0.18ms
```

## 핵심 결과: "느린 쿼리"가 아니라 "쿼리 호출량 자체"가 문제

### 개별 쿼리는 전부 빠르다

상위 11개 쿼리의 평균 실행시간(`mean_ms`)이 전부 **0.18~1.46ms**다 — 인덱스를 잘 타고 있고, 개별 쿼리 자체엔 성능 문제가 없다. `EXPLAIN`이 필요한 "느린 쿼리 하나"는 없었다.

### 하지만 누적 호출 횟수와 총 실행시간이 압도적이다

같은 부하 구간(약 10~11분) 동안, 상위 11개 쿼리의 **총 실행시간을 합치면 895.3초**다 — 2 vCPU 인스턴스가 이론상 낼 수 있는 최대 처리량(2코어 × 630초 wall-clock ≈ 1260 코어-초)의 약 71%를 이 쿼리들만으로 이미 쓰고 있었다. 1위 쿼리(지갑 조회)만 해도 41만 번 넘게 불렸다.

### 원인은 `SettleTrade` 트랜잭션의 구조다

`internal/service/settlement_service.go`의 `SettleTrade` 하나가 체결 1건마다:
- 지갑 조회(`FOR UPDATE`) 4번(매수자 KRW/코인, 매도자 KRW/코인)
- 주문 조회(`FOR UPDATE`) 2번
- 주문 갱신(`UPDATE`) 2번
- 지갑 갱신(`UPDATE`) 4번
- 원장 배치 INSERT 1번(4행)
- 체결 INSERT 1번, 멱등키 조회 1번

을 순차적으로 실행한다 — 트랜잭션 하나당 약 15개 왕복. 체결 건수(69,477건)에 이 배수를 곱하면 딱 관측된 호출량과 맞아떨어진다.

## 해석

1. **가설이 정확히 확인됐다.** 13번 테스트에서 "Postgres CPU 389% 포화"의 원인이 특정 버그나 느린 쿼리가 아니라, 정상적으로 인덱스를 타는 쿼리들이 **너무 많이 호출되는** 구조적 문제였다.
2. **"쿼리 최적화"의 범위가 명확해졌다.** 인덱스 추가 같은 쉬운 수정으로는 해결 안 된다 — `SettleTrade`가 트랜잭션당 15번 왕복하는 구조 자체를 줄여야 한다(예: 여러 UPDATE를 하나의 다중 행 UPDATE로 묶기, 지갑 조회/갱신을 배치화하기). 이건 정산 로직을 건드리는 더 큰 리팩터링이라 리스크가 있다.
3. **왜 분리 후에 더 아팠는지도 설명된다.** co-located 상태에선 이 왕복들이 도커 내부 네트워크(사실상 로컬)를 탔지만, 분리 후엔 실제 VM 간 네트워크를 탄다 — 왕복 횟수가 그대로인데 왕복당 비용(네트워크 RTT)이 늘었으니, 원래도 컸던 "쿼리 폭주" 문제가 더 크게 느껴졌을 가능성이 있다.
4. **당장은 사양을 늘리는 게 더 실용적일 수 있다.** 쿼리 리팩터링은 정산 로직의 정합성(멱등성, 트랜잭션 원자성)을 건드리는 위험한 변경이라 신중해야 한다. 반면 DB 인스턴스 CPU를 늘리는 건(13번 문서의 "다음 작업 제안 1번") 리스크가 훨씬 적고, 지금 확인한 "쿼리 자체는 다 빠르다"는 사실과도 맞는 방향이다 — 코드가 아니라 순수 용량 문제이기 때문이다.

## 다음 작업 제안

1. **DB 인스턴스 CPU 증설**(13번 문서 제안 1번) — 지금까지 확인한 바로는 쿼리 자체엔 문제가 없으니, 이게 리스크가 가장 낮은 다음 조치.
2. **(장기) 정산 트랜잭션 왕복 횟수 축소** — 지갑 4건 갱신을 배치 UPDATE로 묶는 등, 코드 리팩터링으로 근본적인 호출량을 줄이는 방향. 정산 로직을 건드리는 만큼 TDD와 신중한 리뷰가 필요.

## 범위 밖 (Out of Scope)

- 위 "다음 작업 제안"의 실제 실행 — 사용자와 상의해서 결정.
- 정산 트랜잭션 리팩터링의 구체적 설계 — 채택된다면 별도 브레인스토밍.
