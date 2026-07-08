# DB 커넥션 풀 튜닝 설계

## 배경 (왜 필요한가)

`docs/benchmarks/04-2026-07-08-matching-engine-cpu-profiling.md`의 pprof 조사에서, 스트레스 테스트 CPU 포화의 실제 원인이 `config/database.go`의 `ConnectDB()`가 DB 커넥션 풀을 전혀 설정하지 않은 것이라는 걸 콜스택 추적으로 확인했다. Go `database/sql`의 기본값(유휴 커넥션 2개, 열린 커넥션 수 무제한)에서는 동시 요청이 2개를 넘는 순간마다 새 TCP 연결 + Postgres SCRAM-SHA-256 인증(PBKDF2 키 유도 포함)을 반복해야 하고, 이 인증 과정 하나가 CPU 샘플의 37.9%를 차지했다.

이번 작업은 이 문제를 실제로 고치는 것이다. (직전 작업인 `2026-07-08-db-connection-pool-metrics-design.md`는 "관찰만 가능하게" 하는 것이었고, 이번이 "실제로 고치는" 작업이다.)

**이 프로젝트의 성능 개선 목표**: VM 사양(서버 `e2-medium` 2vCPU/4GB, 부하생성 `e2-small`)은 그대로 두고, 그 안에서 최대 성능을 끌어낸다. 수단은 코드 수정, 네트워크/OS 설정, 캐싱(Redis 등), 아키텍처 변경 등 무엇이든 열려있다. 이번 작업은 그중 "코드 수정"(커넥션 풀 설정)에 해당한다.

## 왜 이 방식을 선택했는지

커넥션 풀 값을 하드코딩하지 않고 환경변수로 노출하는 이유는, 로컬 개발/테스트/스트레스 환경마다 적정 값이 다를 수 있고, 이번 스트레스 환경(e2-medium, Postgres와 같은 박스)에 맞춘 값이 다른 배포 환경(예: 별도 관리형 DB, 더 큰 인스턴스)에는 안 맞을 수 있기 때문이다. 기존 `config/database.go`가 `EnvDBHost`, `EnvDBPort` 등 이미 이 패턴(환경변수 우선, 기본값 폴백)을 쓰고 있어 일관성 있게 확장할 수 있다.

기본값(`MaxOpenConns=25`, `MaxIdleConns=25`, `ConnMaxLifetime=30m`)은:
- **유휴 커넥션 수를 열린 커넥션 수와 같게 맞춘 것**이 이번 문제의 직접적인 해결책이다 — 기본값 2에서 25로 늘려서, 동시 요청 25개까지는 매번 새로 연결하지 않고 기존 커넥션을 재사용하게 한다.
- `25`는 Postgres 기본 `max_connections`(100)에 여유를 남기면서도, e2-medium(2 vCPU)에서 감당 가능한 동시 커넥션 수다.
- `ConnMaxLifetime=30m`은 커넥션을 주기적으로 재활용해 장시간 유휴 상태의 커넥션이 쌓이는 걸 방지하는 일반적인 관행이다.

## 범위

- `config/database.go`에 커넥션 풀 설정용 환경변수 3개와 파싱 함수를 추가한다.
- `ConnectDB()`에서 `sqlDB.SetMaxOpenConns()`, `SetMaxIdleConns()`, `SetConnMaxLifetime()`을 호출한다.
- `.env.stress.example`과 `docker-compose.stress.yml`에 이 환경변수들을 기본값과 함께 추가한다.
- k6 스트레스 테스트를 재실행해 전/후 수치를 비교하고 결과를 기록한다.
- ulimit/nofile 같은 OS 레벨 튜닝은 이번 스코프가 아니다 — 이번 테스트에서 파일 디스크립터 관련 에러가 관측되지 않았으므로, 필요해지면 별도 작업으로 다룬다.

## 아키텍처

### 1. 환경변수 및 파싱 (`config/database.go`)

```go
const (
	EnvDBMaxOpenConns    = "GOEXCHANGE_DB_MAX_OPEN_CONNS"
	EnvDBMaxIdleConns    = "GOEXCHANGE_DB_MAX_IDLE_CONNS"
	EnvDBConnMaxLifetime = "GOEXCHANGE_DB_CONN_MAX_LIFETIME"
)

const (
	defaultDBMaxOpenConns    = 25
	defaultDBMaxIdleConns    = 25
	defaultDBConnMaxLifetime = 30 * time.Minute
)
```

- `MaxOpenConnsFromEnv() int`, `MaxIdleConnsFromEnv() int`: `strconv.Atoi`로 파싱, 미설정이거나 파싱 실패 또는 0 이하이면 기본값(25) 반환.
- `ConnMaxLifetimeFromEnv() time.Duration`: `time.ParseDuration`으로 파싱 (`"30m"` 형식), 미설정이거나 파싱 실패면 기본값(30분) 반환.

### 2. `ConnectDB()` 적용

`db.DB()`로 `*sql.DB`를 얻은 직후(기존 DBStatsCollector 등록과 같은 자리)에:

```go
sqlDB.SetMaxOpenConns(MaxOpenConnsFromEnv())
sqlDB.SetMaxIdleConns(MaxIdleConnsFromEnv())
sqlDB.SetConnMaxLifetime(ConnMaxLifetimeFromEnv())
```

### 3. 배포 설정

`.env.stress.example`에 추가:
```
GOEXCHANGE_DB_MAX_OPEN_CONNS=25
GOEXCHANGE_DB_MAX_IDLE_CONNS=25
GOEXCHANGE_DB_CONN_MAX_LIFETIME=30m
```

`docker-compose.stress.yml`의 `backend` 서비스 `environment`에 동일하게 3줄 추가 (기존 `GOEXCHANGE_ENABLE_PPROF` 패턴과 동일하게 `${VAR:-기본값}` 형식).

### 4. 검증 (재측정)

1. 서버 인스턴스에 재배포 (환경변수는 기본값 그대로 사용).
2. k6 스트레스 테스트(`order-submission-stress.js`)를 3번째 테스트(`03-2026-07-08-gcp-stress-test.md`)와 동일한 방식으로 재실행.
3. Grafana에서 다음을 이전 결과와 비교:
   - `go_sql_open_connections`/`go_sql_idle_connections`가 안정적으로 유지되는지 (더 이상 매 요청마다 오르내리지 않는지)
   - `go_sql_wait_count_total` 증가율이 낮아졌는지
   - CPU 사용률과 `POST /orders` p95가 같은 VU 구간에서 개선됐는지
4. 가능하면 pprof로 같은 VU 구간을 다시 프로파일링해서 `pbkdf2.Key`/`scramAuth` 비중이 실제로 줄었는지 확인.
5. 결과를 `docs/benchmarks/05-YYYY-MM-DD-db-connection-pool-tuning.md`에 기록 — 왜 고쳤는지, 전/후 수치 비교표, 실제로 개선된 정도.

## 성공 기준

- 커넥션 풀 설정이 환경변수로 노출되고, 기본값이 적용된 상태로 재배포된다.
- 재측정 결과, 같은 VU 구간에서 CPU 사용률 또는 `POST /orders` p95 중 최소 하나가 3번째 테스트 대비 뚜렷하게 개선된 것이 수치로 확인된다.
- 개선되지 않거나 미미하다면(예: 다른 병목이 새로 드러남), 그 사실도 있는 그대로 기록한다 — 수치를 과장하지 않는다.

## 범위 밖 (Out of Scope)

- ulimit/nofile 등 OS 레벨 튜닝 — 이번 테스트에서 필요성이 관측되지 않음, 필요해지면 별도 작업.
- Redis 캐싱, 매칭엔진 샤딩 등 더 큰 아키텍처 변경 — 이번 재측정 결과를 보고 다음 우선순위를 정한다.
- 로컬/프로덕션(`docker-compose.yml`, `docker-compose.deploy.yml`, `docker-compose.prod.yml`) 환경변수 반영 — 이번엔 스트레스 환경(`docker-compose.stress.yml`)에만 적용한다. 프로덕션에도 같은 설정이 필요한지는 별도로 검토한다.
