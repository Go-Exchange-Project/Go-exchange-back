# DB 커넥션 풀 튜닝 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** pprof로 찾은 DB 커넥션 풀 미설정 문제를 실제로 고친다 — `ConnectDB()`가 `SetMaxOpenConns`/`SetMaxIdleConns`/`SetConnMaxLifetime`을 환경변수 기반으로 설정하게 하고, 스트레스 환경에 반영한다.

**Architecture:** `config/database.go`에 3개의 환경변수 파싱 함수(기본값 폴백 포함)를 추가하고, `ConnectDB()`에서 이 값들을 `*sql.DB`에 적용한다. `docker-compose.stress.yml`/`.env.stress.example`에 기본값과 함께 노출한다.

**Tech Stack:** Go 표준 라이브러리 `database/sql`(`SetMaxOpenConns`/`SetMaxIdleConns`/`SetConnMaxLifetime`), 기존 `config` 패키지의 env-파싱 패턴.

## Global Constraints

- 환경변수 이름: `GOEXCHANGE_DB_MAX_OPEN_CONNS`, `GOEXCHANGE_DB_MAX_IDLE_CONNS`, `GOEXCHANGE_DB_CONN_MAX_LIFETIME`.
- 기본값: `MaxOpenConns=25`, `MaxIdleConns=25`, `ConnMaxLifetime=30m`.
- 미설정이거나 파싱 실패 시 기본값으로 폴백한다 (에러를 내지 않는다).
- ulimit 등 OS 레벨 튜닝, Redis 캐싱, 매칭엔진 아키텍처 변경은 이번 스코프가 아니다.
- 실제 GCP 재배포, k6 재실행, 결과 문서화(`docs/benchmarks/05-...md`)는 이 계획의 범위 밖이다 — 코드/설정 준비까지만 다루고, 실제 재측정은 사용자와 함께 직접 진행한다.

---

### Task 1: 커넥션 풀 환경변수 파싱 및 `ConnectDB()` 적용

**Files:**
- Modify: `config/database.go`
- Modify: `config/database_test.go`

**Interfaces:**
- Produces: `MaxOpenConnsFromEnv() int`, `MaxIdleConnsFromEnv() int`, `ConnMaxLifetimeFromEnv() time.Duration` — Task 2의 배포 설정이 참조하는 환경변수 이름과 짝을 이룬다.

- [ ] **Step 1: 실패하는 테스트 작성**

`config/database_test.go` 파일 끝(`clearDatabaseEnv` 함수 앞)에 추가:

```go
func TestMaxOpenConnsFromEnvDefaultsWhenUnset(t *testing.T) {
	t.Setenv(EnvDBMaxOpenConns, "")

	got := MaxOpenConnsFromEnv()
	want := 25

	if got != want {
		t.Fatalf("MaxOpenConnsFromEnv() = %d, want %d", got, want)
	}
}

func TestMaxOpenConnsFromEnvUsesExplicitValue(t *testing.T) {
	t.Setenv(EnvDBMaxOpenConns, "10")

	got := MaxOpenConnsFromEnv()
	want := 10

	if got != want {
		t.Fatalf("MaxOpenConnsFromEnv() = %d, want %d", got, want)
	}
}

func TestMaxOpenConnsFromEnvFallsBackOnInvalidValue(t *testing.T) {
	t.Setenv(EnvDBMaxOpenConns, "not-a-number")

	got := MaxOpenConnsFromEnv()
	want := 25

	if got != want {
		t.Fatalf("MaxOpenConnsFromEnv() = %d, want %d", got, want)
	}
}

func TestMaxIdleConnsFromEnvDefaultsWhenUnset(t *testing.T) {
	t.Setenv(EnvDBMaxIdleConns, "")

	got := MaxIdleConnsFromEnv()
	want := 25

	if got != want {
		t.Fatalf("MaxIdleConnsFromEnv() = %d, want %d", got, want)
	}
}

func TestMaxIdleConnsFromEnvUsesExplicitValue(t *testing.T) {
	t.Setenv(EnvDBMaxIdleConns, "5")

	got := MaxIdleConnsFromEnv()
	want := 5

	if got != want {
		t.Fatalf("MaxIdleConnsFromEnv() = %d, want %d", got, want)
	}
}

func TestConnMaxLifetimeFromEnvDefaultsWhenUnset(t *testing.T) {
	t.Setenv(EnvDBConnMaxLifetime, "")

	got := ConnMaxLifetimeFromEnv()
	want := 30 * time.Minute

	if got != want {
		t.Fatalf("ConnMaxLifetimeFromEnv() = %s, want %s", got, want)
	}
}

func TestConnMaxLifetimeFromEnvUsesExplicitValue(t *testing.T) {
	t.Setenv(EnvDBConnMaxLifetime, "5m")

	got := ConnMaxLifetimeFromEnv()
	want := 5 * time.Minute

	if got != want {
		t.Fatalf("ConnMaxLifetimeFromEnv() = %s, want %s", got, want)
	}
}

func TestConnMaxLifetimeFromEnvFallsBackOnInvalidValue(t *testing.T) {
	t.Setenv(EnvDBConnMaxLifetime, "not-a-duration")

	got := ConnMaxLifetimeFromEnv()
	want := 30 * time.Minute

	if got != want {
		t.Fatalf("ConnMaxLifetimeFromEnv() = %s, want %s", got, want)
	}
}
```

`config/database_test.go`의 import 블록, 현재:

```go
import "testing"
```

다음과 같이 수정한다:

```go
import (
	"testing"
	"time"
)
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./config/... -run "MaxOpenConns|MaxIdleConns|ConnMaxLifetime" -v`
Expected: FAIL (컴파일 에러 — `EnvDBMaxOpenConns`, `MaxOpenConnsFromEnv` 등이 아직 존재하지 않음)

- [ ] **Step 3: 환경변수 상수 및 파싱 함수 구현**

`config/database.go`의 import 블록, 현재:

```go
import (
	"log"
	"os"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)
```

다음과 같이 수정한다:

```go
import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)
```

`const` 블록, 현재:

```go
const (
	EnvDatabaseDSN = "GOEXCHANGE_DATABASE_DSN"
	EnvDBHost      = "GOEXCHANGE_DB_HOST"
	EnvDBUser      = "GOEXCHANGE_DB_USER"
	EnvDBPassword  = "GOEXCHANGE_DB_PASSWORD"
	EnvDBName      = "GOEXCHANGE_DB_NAME"
	EnvDBPort      = "GOEXCHANGE_DB_PORT"
	EnvDBSSLMode   = "GOEXCHANGE_DB_SSLMODE"
	EnvDBTimeout   = "GOEXCHANGE_DB_CONNECT_TIMEOUT"
)
```

다음과 같이 수정한다:

```go
const (
	EnvDatabaseDSN       = "GOEXCHANGE_DATABASE_DSN"
	EnvDBHost            = "GOEXCHANGE_DB_HOST"
	EnvDBUser            = "GOEXCHANGE_DB_USER"
	EnvDBPassword        = "GOEXCHANGE_DB_PASSWORD"
	EnvDBName            = "GOEXCHANGE_DB_NAME"
	EnvDBPort            = "GOEXCHANGE_DB_PORT"
	EnvDBSSLMode         = "GOEXCHANGE_DB_SSLMODE"
	EnvDBTimeout         = "GOEXCHANGE_DB_CONNECT_TIMEOUT"
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

`DatabaseDSNFromEnv` 함수 앞에 세 함수를 추가한다:

```go
func MaxOpenConnsFromEnv() int {
	return parsePositiveIntEnv(EnvDBMaxOpenConns, defaultDBMaxOpenConns)
}

func MaxIdleConnsFromEnv() int {
	return parsePositiveIntEnv(EnvDBMaxIdleConns, defaultDBMaxIdleConns)
}

func ConnMaxLifetimeFromEnv() time.Duration {
	value := strings.TrimSpace(os.Getenv(EnvDBConnMaxLifetime))
	if value == "" {
		return defaultDBConnMaxLifetime
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return defaultDBConnMaxLifetime
	}
	return parsed
}

func parsePositiveIntEnv(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go test ./config/... -v`
Expected: PASS (기존 테스트 포함 전부)

- [ ] **Step 5: `ConnectDB()`에 적용**

`ConnectDB()` 함수, 현재:

```go
func ConnectDB() {
	dsn := DatabaseDSNFromEnv()

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("DB connection failed: ", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		log.Fatal("DB handle retrieval failed: ", err)
	}
	prometheus.MustRegister(collectors.NewDBStatsCollector(sqlDB, "goexchange"))

	DB = db
	log.Println("DB connection established")
}
```

다음과 같이 수정한다:

```go
func ConnectDB() {
	dsn := DatabaseDSNFromEnv()

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("DB connection failed: ", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		log.Fatal("DB handle retrieval failed: ", err)
	}
	sqlDB.SetMaxOpenConns(MaxOpenConnsFromEnv())
	sqlDB.SetMaxIdleConns(MaxIdleConnsFromEnv())
	sqlDB.SetConnMaxLifetime(ConnMaxLifetimeFromEnv())
	prometheus.MustRegister(collectors.NewDBStatsCollector(sqlDB, "goexchange"))

	DB = db
	log.Println("DB connection established")
}
```

- [ ] **Step 6: 빌드 및 전체 테스트 확인**

```bash
go build ./...
go vet ./...
go test ./... 2>&1 | tail -40
```

Expected: 빌드 성공, `go vet` 통과, 모든 테스트 PASS.

- [ ] **Step 7: 커밋**

```bash
git add config/database.go config/database_test.go
git commit -m "$(cat <<'MSG'
fix: DB 커넥션 풀 크기/수명을 환경변수로 설정

pprof 조사에서 커넥션 풀 미설정으로 인한 반복적 SCRAM 인증이 CPU의
37.9%를 차지한다는 걸 확인했다. SetMaxOpenConns/SetMaxIdleConns/
SetConnMaxLifetime을 환경변수 기반(기본 25/25/30m)으로 설정해
커넥션이 재사용되도록 한다.
MSG
)"
```

---

### Task 2: 스트레스 배포 설정에 반영

**Files:**
- Modify: `docker-compose.stress.yml`
- Modify: `.env.stress.example`

**Interfaces:**
- Consumes: Task 1의 `GOEXCHANGE_DB_MAX_OPEN_CONNS`, `GOEXCHANGE_DB_MAX_IDLE_CONNS`, `GOEXCHANGE_DB_CONN_MAX_LIFETIME` 환경변수 이름.

- [ ] **Step 1: `docker-compose.stress.yml`에 환경변수 추가**

`backend` 서비스의 `environment` 블록, 현재:

```yaml
      GOEXCHANGE_ENABLE_UPBIT: "false"
      GOEXCHANGE_ENABLE_PPROF: ${GOEXCHANGE_ENABLE_PPROF:-false}
      GOEXCHANGE_CORS_ALLOWED_ORIGINS: ${GOEXCHANGE_CORS_ALLOWED_ORIGINS:-http://localhost:3000}
```

다음과 같이 수정한다:

```yaml
      GOEXCHANGE_ENABLE_UPBIT: "false"
      GOEXCHANGE_ENABLE_PPROF: ${GOEXCHANGE_ENABLE_PPROF:-false}
      GOEXCHANGE_DB_MAX_OPEN_CONNS: ${GOEXCHANGE_DB_MAX_OPEN_CONNS:-25}
      GOEXCHANGE_DB_MAX_IDLE_CONNS: ${GOEXCHANGE_DB_MAX_IDLE_CONNS:-25}
      GOEXCHANGE_DB_CONN_MAX_LIFETIME: ${GOEXCHANGE_DB_CONN_MAX_LIFETIME:-30m}
      GOEXCHANGE_CORS_ALLOWED_ORIGINS: ${GOEXCHANGE_CORS_ALLOWED_ORIGINS:-http://localhost:3000}
```

- [ ] **Step 2: `.env.stress.example`에 추가**

`.env.stress.example` 파일 끝에 추가:

```bash
GOEXCHANGE_DB_MAX_OPEN_CONNS=25
GOEXCHANGE_DB_MAX_IDLE_CONNS=25
GOEXCHANGE_DB_CONN_MAX_LIFETIME=30m
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
feat: 스트레스 환경에 DB 커넥션 풀 설정 반영

Task 1에서 추가한 GOEXCHANGE_DB_MAX_OPEN_CONNS/MAX_IDLE_CONNS/
CONN_MAX_LIFETIME을 docker-compose.stress.yml과 .env.stress.example에
기본값과 함께 노출한다.
MSG
)"
```
