# Go DB 커넥션 풀 Prometheus 지표 설계

## 배경 (왜 필요한가)

`docs/benchmarks/04-2026-07-08-matching-engine-cpu-profiling.md`의 pprof 조사에서, 스트레스 테스트 CPU 포화의 실제 원인이 매칭엔진이 아니라 **`config/database.go`의 `ConnectDB()`가 DB 커넥션 풀을 전혀 설정하지 않아, 매 요청마다 새 커넥션을 맺고 그때마다 SCRAM-SHA-256 인증(PBKDF2)을 반복하는 것**(CPU의 37.9%)이라는 걸 콜스택 추적으로 확인했다.

지금 Grafana 대시보드의 "Postgres 커넥션 / 커밋 통계" 패널은 `pg_stat_activity_count`(Postgres 서버가 보는 활성 커넥션 수)만 보여준다. 이건 **DB 서버 쪽 시점**이라, "Go 앱이 커넥션을 얼마나 자주 새로 맺고 있는지"(=이번에 찾은 문제의 직접적 증거)는 보이지 않는다. Go 앱 자신의 `database/sql` 풀 상태(열린/유휴/사용중 커넥션 수, 새 커넥션을 기다린 횟수)를 봐야 이 문제를 실시간으로 관찰할 수 있다.

## 왜 이 방식을 선택했는지

커스텀 지표를 직접 계측하는 대신, Prometheus 공식 클라이언트 라이브러리가 제공하는 `prometheus/collectors.NewDBStatsCollector`를 쓰기로 했다. Go 표준 라이브러리의 `sql.DB.Stats()`가 이미 열린/유휴/사용중 커넥션 수, 대기 횟수, 대기 시간, 커넥션 종료 사유별 카운터까지 전부 제공하는데, 이 컬렉터는 그 값을 그대로 Prometheus 지표로 변환해줄 뿐이라 직접 히스토그램/카운터를 설계할 필요가 없다. 코드 변경은 `ConnectDB()`에 등록 한 줄을 추가하는 정도로 최소화된다.

## 범위

- `config/database.go`의 `ConnectDB()`에 `DBStatsCollector` 등록을 추가한다.
- `monitoring/grafana/provisioning/dashboards/json/goexchange-stress.json`에 새 패널 "Go DB 커넥션 풀"을 추가한다.
- DB 커넥션 풀 자체의 설정값(`SetMaxOpenConns` 등)을 바꾸는 것은 이번 스코프가 아니다 — 이건 관찰만 가능하게 하는 작업이고, 실제로 풀을 튜닝하는 건 이 지표로 문제를 재확인한 뒤 별도 작업으로 진행한다.

## 아키텍처

### 1. Go 앱 계측

`config/database.go`의 `ConnectDB()`, 현재 `db.DB()`로 `*sql.DB`를 얻는 부분 뒤에 다음을 추가한다:

```go
sqlDB, err := db.DB()
if err != nil {
    log.Fatal("DB handle retrieval failed: ", err)
}
prometheus.MustRegister(collectors.NewDBStatsCollector(sqlDB, "goexchange"))
```

이 컬렉터는 다음 지표를 자동으로 노출한다 (client_golang의 표준 이름):
- `go_sql_open_connections` — 현재 열려있는 커넥션 수
- `go_sql_in_use_connections` — 사용 중인 커넥션 수
- `go_sql_idle_connections` — 유휴 커넥션 수
- `go_sql_max_open_connections` — 설정된 최대 열린 커넥션 수 (현재는 0=무제한)
- `go_sql_wait_count_total` — 새 커넥션을 기다린 누적 횟수
- `go_sql_wait_duration_seconds_total` — 그 대기에 걸린 누적 시간
- `go_sql_max_idle_closed_total`, `go_sql_max_lifetime_closed_total` — 각각 유휴 제한/수명 제한으로 닫힌 커넥션 수

이 지표들은 이미 Task 2에서 만든 `/metrics` 엔드포인트(`internal/metrics`가 아니라 `cmd/main.go`가 직접 `promhttp.Handler()`를 등록)를 통해 그대로 노출된다 — 별도 라우팅 변경이 필요 없다.

### 2. Grafana 대시보드

`goexchange-stress.json`에 6번째 패널 "Go DB 커넥션 풀"을 추가한다. 쿼리:
- `go_sql_open_connections` (전체 열린 커넥션)
- `go_sql_in_use_connections` (사용 중)
- `go_sql_idle_connections` (유휴)
- `rate(go_sql_wait_count_total[1m])` (초당 새 커넥션 대기 횟수 — 이 값이 0보다 크게 지속되면 풀이 부족하다는 신호)

### 3. 기대하는 관찰 결과

풀이 설정되지 않은 현재 상태에서 스트레스 테스트를 다시 돌리면, `go_sql_open_connections`가 동시 요청 수에 비례해서 계속 오르내리고(유휴 커넥션이 거의 재사용되지 않음), `go_sql_wait_count_total`의 증가율도 관찰될 것으로 예상된다. 이 관찰 자체가 `04-2026-07-08-matching-engine-cpu-profiling.md`에서 pprof로 찾은 원인(반복적 재연결)의 두 번째 증거가 된다.

## 성공 기준

- `/metrics` 엔드포인트에서 `go_sql_*` 지표가 노출된다.
- Grafana에 커넥션 풀 상태를 실시간으로 볼 수 있는 패널이 생긴다.
- (선택, 이번 스코프 밖) 이 지표로 커넥션 풀 미설정 문제를 재확인하면, 다음 작업(풀 설정 추가)의 재측정 근거로 쓴다.

## 범위 밖 (Out of Scope)

- `SetMaxOpenConns`/`SetMaxIdleConns`/`SetConnMaxLifetime` 등 실제 풀 설정 변경 — 별도 작업.
- 매칭엔진 관련 추가 조사 — pprof 결과(04번 문서)에서 이미 낮은 우선순위로 정리됨.
