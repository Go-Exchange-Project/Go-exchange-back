# pg_stat_statements로 Postgres CPU 병목 쿼리 조사 설계

## 배경 (왜 필요한가)

`13-2026-07-10-postgres-instance-separation.md`에서 Postgres를 별도 `e2-medium`(2 vCPU) 인스턴스로 분리한 뒤, Postgres 컨테이너 CPU가 389%(2코어 거의 전부)까지 치솟고 전체 처리량이 오히려 19% 줄어드는 걸 확인했다. 다음 조치를 정하기 전에, "무엇이 Postgres CPU를 이렇게 많이 쓰는지"부터 실측으로 확인한다.

`internal/service/settlement_service.go`의 `SettleTrade`를 읽어보니, 트랜잭션 하나당 대략 15개의 개별 쿼리(멱등키 조회, 체결 INSERT, 주문 2건 `SELECT ... FOR UPDATE`, 지갑 4건 `SELECT ... FOR UPDATE`, 주문 갱신 2건, 지갑 갱신 4건, 원장 배치 INSERT)가 순차적으로 실행된다. 마이그레이션(`migrations/001_constraints.sql`)을 확인한 결과 필요한 인덱스(`idx_wallets_user_id_coin_symbol`, `idx_trades_idempotency_key` 등)는 이미 다 있어서, "인덱스 누락" 같은 명백한 버그는 아닌 것으로 보인다. 그래서 추측 대신 Postgres 표준 도구로 실제 데이터를 확인한다.

## 왜 이 방식을 선택했는지

`pg_stat_statements`는 Postgres 공식 확장으로, 실행된 SQL을 정규화해서 호출 횟수·누적 실행시간·평균 실행시간을 추적해준다. 이 프로젝트가 지금까지 지켜온 "추측하지 말고 실측하라"는 원칙(pprof, 채널 길이 노출 등)과 정확히 같은 접근이다. 코드를 먼저 리팩터링하거나 Postgres 설정을 먼저 튜닝하는 대신, 어떤 쿼리가 실제로 비싼지부터 확인해서 조치의 우선순위를 데이터로 정한다.

## 범위

- DB 인스턴스의 Postgres에 `pg_stat_statements` 확장을 활성화한다(`docker-compose.db.yml`과 Postgres 설정 갱신).
- 짧은 재부하 테스트(VU 800~1000 정도, 13번 테스트에서 문제가 뚜렷했던 구간)를 진행해서 통계를 쌓는다.
- `pg_stat_statements`를 총 실행시간 기준으로 조회해서 상위 쿼리를 확인하고, 그 결과를 `docs/benchmarks/14-...md`에 기록한다.
- 이번 조사 결과에 따른 실제 코드/설정 변경은 이 스펙의 범위 밖이다 — 조사 후 별도로 브레인스토밍한다.

## 아키텍처

### 1. `pg_stat_statements` 활성화

`docker-compose.db.yml`의 `postgres` 서비스에 `command`를 추가해 확장을 로드한다:

```yaml
  postgres:
    image: postgres:18-alpine
    container_name: goexchange-db-postgres
    command: ["postgres", "-c", "shared_preload_libraries=pg_stat_statements", "-c", "pg_stat_statements.track=all"]
    environment:
      ...
```

컨테이너 기동 후, 최초 1회 확장을 실제로 생성해야 한다:
```sql
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
```

### 2. 재부하 테스트

기존 k6 스크립트(`loadtest/order-submission-stress.js`)를 그대로 쓰되, VU 800~1000 정도까지만 진행되면 충분하다 — 13번 테스트에서 이미 이 구간부터 Postgres CPU 문제가 뚜렷했으므로, 3,000까지 갈 필요 없이 통계를 쌓기에 충분한 부하다. (별도 스크립트 변경 없이, 필요하면 실행 중 수동으로 중단한다.)

### 3. 조사 쿼리

부하 테스트 후, DB 인스턴스에 SSH로 접속해 `docker exec`로 Postgres에 접속, 다음 쿼리로 상위 쿼리를 확인한다:

```sql
SELECT
  substring(query, 1, 100) AS short_query,
  calls,
  total_exec_time,
  mean_exec_time,
  rows
FROM pg_stat_statements
ORDER BY total_exec_time DESC
LIMIT 20;
```

## 성공 기준

- `pg_stat_statements`가 정상 동작하고, 부하 테스트 중 실행된 쿼리들의 통계가 쌓인다.
- 상위 쿼리 목록을 보고, "무엇이 Postgres CPU를 많이 쓰는지"에 대한 명확한 결론(특정 쿼리 하나가 지배적인지, 아니면 여러 쿼리가 고르게 분산되어 있는지)을 얻는다.
- 이 결과를 근거로 다음 조치(쿼리 배치화, Postgres 설정 튜닝, DB 사양 증설 등)의 우선순위를 정할 수 있게 된다.

## 범위 밖 (Out of Scope)

- 조사 결과에 따른 실제 쿼리/코드 리팩터링 — 별도 브레인스토밍.
- Postgres 설정(shared_buffers, synchronous_commit 등) 튜닝 — 이번엔 원인 파악까지만.
- DB 인스턴스 사양 증설 — 이번 조사 결과를 보고 필요성을 재평가.
