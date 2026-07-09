# 정산 워커 풀 도입 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 9번 테스트에서 확인한 매칭 지연 병목(단일 정산 고루틴이 `ExecutionCh` 소비 속도를 못 따라감)을 해소하기 위해, 정산 소비자를 환경변수로 설정 가능한 워커 풀(기본 10개)로 늘린다.

**Architecture:** `config/runtime.go`에 `SettlementWorkersFromEnv() int` 함수를 추가하고, `cmd/main.go`의 `ExecutionCh` 소비 고루틴을 1개에서 이 함수가 반환하는 개수만큼으로 늘린다. `SettleTrade`가 이미 DB 트랜잭션+멱등키로 동시 실행에 안전하므로 애플리케이션 레벨 동기화는 추가하지 않는다.

**Tech Stack:** Go 표준 고루틴/채널, `config` 패키지의 기존 `parsePositiveIntEnv` 헬퍼.

## Global Constraints

- 환경변수 이름: `GOEXCHANGE_SETTLEMENT_WORKERS`, 기본값: `10`.
- `config/database.go`에 이미 있는 `parsePositiveIntEnv(key string, fallback int) int`를 재사용한다(같은 `config` 패키지이므로 새로 만들지 않는다).
- 스트레스 환경(`docker-compose.stress.yml`, `.env.stress.example`)에만 이번 설정을 추가한다 — 다른 compose 파일은 건드리지 않는다.
- 실제 GCP 재배포, k6 재실행, 결과 문서화(`docs/benchmarks/10-...md`)는 이 계획의 범위 밖이다.

---

### Task 1: `config/runtime.go`에 워커 개수 설정 추가

**Files:**
- Modify: `config/runtime.go`
- Test: `config/runtime_test.go`

**Interfaces:**
- Produces: `SettlementWorkersFromEnv() int` — Task 2에서 `cmd/main.go`가 이 함수를 호출한다.

- [ ] **Step 1: 실패하는 테스트 작성**

`config/runtime_test.go` 파일 끝에 추가:

```go
func TestSettlementWorkersFromEnvDefault(t *testing.T) {
	requireUnsetEnv(t, EnvGOExchangeSettlementWorkers)

	assert.Equal(t, 10, SettlementWorkersFromEnv())
}

func TestSettlementWorkersFromEnvCustomValue(t *testing.T) {
	t.Setenv(EnvGOExchangeSettlementWorkers, "3")

	assert.Equal(t, 3, SettlementWorkersFromEnv())
}

func TestSettlementWorkersFromEnvInvalidValueFallsBackToDefault(t *testing.T) {
	t.Setenv(EnvGOExchangeSettlementWorkers, "not-a-number")

	assert.Equal(t, 10, SettlementWorkersFromEnv())
}
```

(`requireUnsetEnv`는 이 파일에 이미 정의되어 있는 테스트 헬퍼다 — `TestUpbitEnabledFromEnvDefaultsToEnabled`에서 쓰는 것과 동일하게 재사용한다.)

- [ ] **Step 2: 테스트 실행해서 실패 확인**

Run: `go test ./config/... -run TestSettlementWorkersFromEnv -v`
Expected: FAIL — `EnvGOExchangeSettlementWorkers`/`SettlementWorkersFromEnv`가 아직 정의되지 않아 컴파일 에러.

- [ ] **Step 3: `config/runtime.go`에 구현 추가**

`config/runtime.go`의 현재:

```go
const (
	EnvGOExchangeEnableDevTools = "GOEXCHANGE_ENABLE_DEV_TOOLS"
	EnvGOExchangeDevToolsToken  = "GOEXCHANGE_DEV_TOOLS_TOKEN"
	EnvGOExchangeEnableUpbit    = "GOEXCHANGE_ENABLE_UPBIT"
	EnvGOExchangeCORSOrigins    = "GOEXCHANGE_CORS_ALLOWED_ORIGINS"
	EnvGOExchangeEnablePprof    = "GOEXCHANGE_ENABLE_PPROF"
)
```

다음과 같이 수정한다:

```go
const (
	EnvGOExchangeEnableDevTools    = "GOEXCHANGE_ENABLE_DEV_TOOLS"
	EnvGOExchangeDevToolsToken     = "GOEXCHANGE_DEV_TOOLS_TOKEN"
	EnvGOExchangeEnableUpbit       = "GOEXCHANGE_ENABLE_UPBIT"
	EnvGOExchangeCORSOrigins       = "GOEXCHANGE_CORS_ALLOWED_ORIGINS"
	EnvGOExchangeEnablePprof       = "GOEXCHANGE_ENABLE_PPROF"
	EnvGOExchangeSettlementWorkers = "GOEXCHANGE_SETTLEMENT_WORKERS"
)

const defaultSettlementWorkers = 10
```

그리고 파일 끝(`parseBoolEnv` 함수 다음)에 추가:

```go

func SettlementWorkersFromEnv() int {
	return parsePositiveIntEnv(EnvGOExchangeSettlementWorkers, defaultSettlementWorkers)
}
```

- [ ] **Step 4: 테스트 실행해서 통과 확인**

Run: `go test ./config/... -run TestSettlementWorkersFromEnv -v`
Expected: PASS (3개 서브테스트 전부)

- [ ] **Step 5: 빌드 및 회귀 확인**

```bash
go build ./config/...
go vet ./config/...
go test ./config/... -v
```

Expected: 전부 에러 없이 종료, `config` 패키지의 모든 테스트 PASS.

- [ ] **Step 6: 커밋**

```bash
git add config/runtime.go config/runtime_test.go
git commit -m "$(cat <<'MSG'
feat(config): 정산 워커 개수 환경변수 설정 추가

GOEXCHANGE_SETTLEMENT_WORKERS(기본값 10)로 정산 소비자 고루틴 개수를
설정할 수 있게 한다. 기존 parsePositiveIntEnv 헬퍼를 재사용해 DB 풀
튜닝 설정과 같은 패턴을 따른다.
MSG
)"
```

---

### Task 2: `cmd/main.go`에서 정산 소비자를 워커 풀로 전환

**Files:**
- Modify: `cmd/main.go:99-105`

**Interfaces:**
- Consumes: `config.SettlementWorkersFromEnv() int` (Task 1에서 정의).

- [ ] **Step 1: 단일 고루틴을 워커 풀로 교체**

`cmd/main.go`의 현재:

```go
	go func() {
		for event := range me.ExecutionCh {
			processExecutionEvent(event, settlementService, failedSettlementService, orderService, func(msg []byte) {
				hub.Broadcast <- msg
			}, log.Default())
		}
	}()
```

다음과 같이 수정한다:

```go
	for i := 0; i < config.SettlementWorkersFromEnv(); i++ {
		go func() {
			for event := range me.ExecutionCh {
				processExecutionEvent(event, settlementService, failedSettlementService, orderService, func(msg []byte) {
					hub.Broadcast <- msg
				}, log.Default())
			}
		}()
	}
```

- [ ] **Step 2: 빌드 및 전체 테스트 회귀 확인**

```bash
go build ./...
go vet ./...
go test ./... 2>&1 | tail -40
```

Expected: 전부 에러 없이 종료, 모든 패키지 PASS — 특히 `internal/service`의 정산 통합 테스트(`TestIntegrationSettleTrade...`)가 계속 통과하는지 확인한다(이 태스크는 정산 로직 자체를 바꾸지 않으므로 영향 없어야 한다).

- [ ] **Step 3: 커밋**

```bash
git add cmd/main.go
git commit -m "$(cat <<'MSG'
feat: 정산 소비자를 워커 풀로 전환

ExecutionCh를 소비하는 고루틴을 1개에서 GOEXCHANGE_SETTLEMENT_WORKERS
개수만큼으로 늘린다. SettleTrade가 이미 DB 트랜잭션과 멱등키로 동시
실행에 안전하므로 애플리케이션 레벨 동기화는 추가하지 않는다.
MSG
)"
```

---

### Task 3: 스트레스 환경 설정에 워커 개수 추가

**Files:**
- Modify: `docker-compose.stress.yml`
- Modify: `.env.stress.example`

**Interfaces:**
- 없음 (설정 파일만 변경).

- [ ] **Step 1: `docker-compose.stress.yml`의 `backend` 서비스 `environment`에 추가**

`docker-compose.stress.yml`의 현재:

```yaml
      GOEXCHANGE_DB_CONN_MAX_LIFETIME: ${GOEXCHANGE_DB_CONN_MAX_LIFETIME:-30m}
      GOEXCHANGE_CORS_ALLOWED_ORIGINS: ${GOEXCHANGE_CORS_ALLOWED_ORIGINS:-http://localhost:3000}
```

다음과 같이 수정한다:

```yaml
      GOEXCHANGE_DB_CONN_MAX_LIFETIME: ${GOEXCHANGE_DB_CONN_MAX_LIFETIME:-30m}
      GOEXCHANGE_SETTLEMENT_WORKERS: ${GOEXCHANGE_SETTLEMENT_WORKERS:-10}
      GOEXCHANGE_CORS_ALLOWED_ORIGINS: ${GOEXCHANGE_CORS_ALLOWED_ORIGINS:-http://localhost:3000}
```

- [ ] **Step 2: `.env.stress.example`에 추가**

`.env.stress.example`의 현재:

```
GOEXCHANGE_DB_CONN_MAX_LIFETIME=30m
```

다음과 같이 수정한다:

```
GOEXCHANGE_DB_CONN_MAX_LIFETIME=30m
GOEXCHANGE_SETTLEMENT_WORKERS=10
```

- [ ] **Step 3: 문법 검증**

```bash
docker compose -f docker-compose.stress.yml --env-file .env.stress.example config >/dev/null
```

Expected: 에러 없이 종료.

- [ ] **Step 4: 커밋**

```bash
git add docker-compose.stress.yml .env.stress.example
git commit -m "$(cat <<'MSG'
feat: 스트레스 환경에 정산 워커 개수 설정 추가

GOEXCHANGE_SETTLEMENT_WORKERS(기본값 10)를 docker-compose.stress.yml과
.env.stress.example에 반영해, 재배포 시 정산 워커 풀 크기를 조정할 수
있게 한다.
MSG
)"
```
