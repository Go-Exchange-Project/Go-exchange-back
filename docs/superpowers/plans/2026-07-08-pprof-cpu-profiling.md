# 매칭엔진 CPU 프로파일링(pprof) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 스트레스 테스트 중 CPU 포화 구간에서 실제 CPU 프로파일을 캡처할 수 있도록, 환경변수로 게이팅된 `net/http/pprof` 엔드포인트를 Go 앱에 추가하고, 배포/실행 문서를 갱신한다.

**Architecture:** `config.PprofEnabledFromEnv()`(기존 `DevToolsEnabledFromEnv`/`UpbitEnabledFromEnv`와 동일한 패턴)가 true일 때만, `cmd/main.go`에서 `127.0.0.1:6060`에 바인딩된 별도 HTTP 서버(`net/http/pprof`가 등록하는 `http.DefaultServeMux` 사용)를 고루틴으로 띄운다. 기존 gin 라우터(8080)와는 완전히 분리된 서버라 라우팅에 영향이 없다.

**Tech Stack:** Go 표준 라이브러리 `net/http/pprof`, 기존 `config` 패키지의 env 기반 플래그 패턴, docker-compose, SSH 터널링.

## Global Constraints

- pprof 엔드포인트는 `127.0.0.1:6060`에만 바인딩한다 — 외부 노출 없음, GCP 방화벽 규칙 변경 없음.
- 접근은 SSH 터널링(`ssh -L 6060:localhost:6060`)만 사용한다.
- 인스턴스 사양/대수는 늘리지 않는다.
- 환경변수 이름: `GOEXCHANGE_ENABLE_PPROF` (기본값 false/비활성).
- 실제 GCP 재배포, k6 재실행, 프로파일 캡처, 결과 문서화(`docs/benchmarks/04-...md`)는 이 계획의 범위 밖이다 — 이 계획은 코드/설정/문서 준비까지이며, 실제 클라우드 재배포와 프로파일 캡처는 사용자와 함께 직접 진행한다.

---

### Task 1: `GOEXCHANGE_ENABLE_PPROF` 설정 및 pprof 서버 연결

**Files:**
- Modify: `config/runtime.go`
- Modify: `config/runtime_test.go`
- Modify: `cmd/main.go`

**Interfaces:**
- Produces: `config.PprofEnabledFromEnv() bool` — Task 2에서 `.env.stress.example`에 설정할 환경변수 이름(`GOEXCHANGE_ENABLE_PPROF`)과 짝을 이룬다.

- [ ] **Step 1: 실패하는 테스트 작성**

`config/runtime_test.go` 파일 끝에 추가:

```go
func TestPprofEnabledFromEnv(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected bool
	}{
		{name: "enabled", value: "true", expected: true},
		{name: "disabled", value: "false", expected: false},
		{name: "unset", value: "", expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvGOExchangeEnablePprof, tt.value)
			assert.Equal(t, tt.expected, PprofEnabledFromEnv())
		})
	}
}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./config/... -run TestPprofEnabledFromEnv -v`
Expected: FAIL (컴파일 에러 — `EnvGOExchangeEnablePprof`, `PprofEnabledFromEnv`가 아직 존재하지 않음)

- [ ] **Step 3: `config.PprofEnabledFromEnv()` 구현**

`config/runtime.go`의 `const` 블록, 현재:

```go
const (
	EnvGOExchangeEnableDevTools = "GOEXCHANGE_ENABLE_DEV_TOOLS"
	EnvGOExchangeDevToolsToken  = "GOEXCHANGE_DEV_TOOLS_TOKEN"
	EnvGOExchangeEnableUpbit    = "GOEXCHANGE_ENABLE_UPBIT"
	EnvGOExchangeCORSOrigins    = "GOEXCHANGE_CORS_ALLOWED_ORIGINS"
)
```

다음과 같이 수정한다:

```go
const (
	EnvGOExchangeEnableDevTools = "GOEXCHANGE_ENABLE_DEV_TOOLS"
	EnvGOExchangeDevToolsToken  = "GOEXCHANGE_DEV_TOOLS_TOKEN"
	EnvGOExchangeEnableUpbit    = "GOEXCHANGE_ENABLE_UPBIT"
	EnvGOExchangeCORSOrigins    = "GOEXCHANGE_CORS_ALLOWED_ORIGINS"
	EnvGOExchangeEnablePprof    = "GOEXCHANGE_ENABLE_PPROF"
)
```

`UpbitEnabledFromEnv` 함수 뒤에 새 함수를 추가한다:

```go
func PprofEnabledFromEnv() bool {
	return parseBoolEnv(os.Getenv(EnvGOExchangeEnablePprof))
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go test ./config/... -v`
Expected: PASS (기존 테스트 포함 전부)

- [ ] **Step 5: `cmd/main.go`에 pprof 서버 연결**

import 블록, 현재:

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
```

다음과 같이 수정한다 (`net/http` 다음 줄에 blank import 추가):

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"time"
```

`main()` 함수, 현재:

```go
func main() {
	if err := config.LoadLocalEnvFiles(); err != nil {
		log.Fatal("load local env failed: ", err)
	}
	config.ConnectDB()

	if err := config.DB.AutoMigrate(
```

다음과 같이 수정한다:

```go
func main() {
	if err := config.LoadLocalEnvFiles(); err != nil {
		log.Fatal("load local env failed: ", err)
	}
	config.ConnectDB()

	if config.PprofEnabledFromEnv() {
		go func() {
			log.Println("pprof listening on 127.0.0.1:6060:", http.ListenAndServe("127.0.0.1:6060", nil))
		}()
	}

	if err := config.DB.AutoMigrate(
```

- [ ] **Step 6: 빌드 및 전체 테스트 확인**

```bash
go build ./...
go test ./... 2>&1 | tail -40
```

Expected: 빌드 성공, 모든 테스트 PASS.

- [ ] **Step 7: 커밋**

```bash
git add config/runtime.go config/runtime_test.go cmd/main.go
git commit -m "$(cat <<'MSG'
feat: GOEXCHANGE_ENABLE_PPROF로 게이팅된 pprof 서버 추가

스트레스 테스트 CPU 포화 구간의 실제 원인(매칭 로직 자체 vs
스케줄링/부수 작업)을 프로파일로 확인하기 위해 127.0.0.1:6060에만
바인딩되는 pprof 엔드포인트를 환경변수로 켜고 끌 수 있게 추가한다.
MSG
)"
```

---

### Task 2: docker-compose / 환경변수 / 런북 갱신

**Files:**
- Modify: `docker-compose.stress.yml`
- Modify: `.env.stress.example`
- Modify: `docs/gcp-stress-test-runbook.md`

**Interfaces:**
- Consumes: Task 1의 `GOEXCHANGE_ENABLE_PPROF` 환경변수, `127.0.0.1:6060` 포트

- [ ] **Step 1: `docker-compose.stress.yml`에 포트 매핑 추가**

`backend` 서비스 블록, 현재:

```yaml
    ports:
      - "8080:8080"
```

다음과 같이 수정한다:

```yaml
    ports:
      - "8080:8080"
      - "127.0.0.1:6060:6060"
```

`backend` 서비스의 `environment` 블록에 다음 한 줄을 추가한다 (`GOEXCHANGE_ENABLE_UPBIT` 줄 다음):

```yaml
      GOEXCHANGE_ENABLE_PPROF: ${GOEXCHANGE_ENABLE_PPROF:-false}
```

- [ ] **Step 2: `.env.stress.example`에 플래그 추가**

`.env.stress.example` 파일 끝에 추가:

```bash
GOEXCHANGE_ENABLE_PPROF=true
```

- [ ] **Step 3: 문법 검증**

```bash
docker compose -f docker-compose.stress.yml --env-file .env.stress.example config >/dev/null
```

Expected: 에러 없이 종료.

- [ ] **Step 4: 런북에 프로파일링 절차 추가**

`docs/gcp-stress-test-runbook.md`의 "## 6. 부하생성 인스턴스에서 k6 실행" 섹션과 "## 7. 실시간 관찰과 수동 종료" 섹션 사이에 다음 섹션을 새로 추가한다:

```markdown
## 6.5. (선택) CPU 프로파일 캡처

이전 실행에서 CPU 포화가 관측된 VU 구간(예: 150~200)이 있다면, 그 구간에서 30초 CPU 프로파일을 캡처해 실제 병목 함수를 확인할 수 있다.

1. `.env`에 `GOEXCHANGE_ENABLE_PPROF=true`가 설정된 채로 서버가 기동 중인지 확인한다 (기본값은 `false`이므로 명시적으로 켜야 한다).
2. 로컬에서 서버 인스턴스로 SSH 터널을 연다:
   ```bash
   ssh -L 6060:localhost:6060 -i ~/.ssh/goexchange-gcp goexchange@<server_external_ip>
   ```
3. k6가 목표 VU 구간에 진입한 시점에, 로컬의 또 다른 터미널에서 프로파일을 받는다:
   ```bash
   go tool pprof -seconds=30 -output=cpu.prof http://localhost:6060/debug/pprof/profile
   ```
4. 캡처가 끝나면 분석한다:
   ```bash
   go tool pprof -top cpu.prof
   go tool pprof -svg cpu.prof > cpu.svg
   ```
5. 결과를 `docs/benchmarks/04-YYYY-MM-DD-matching-engine-cpu-profiling.md`에 기록한다 (왜 이 조사를 했는지, `-top` 원본 출력, 상위 CPU 소비 함수 요약, 다음 작업 제안 포함).
```

- [ ] **Step 5: 커밋**

```bash
git add docker-compose.stress.yml .env.stress.example docs/gcp-stress-test-runbook.md
git commit -m "$(cat <<'MSG'
docs: 런북에 pprof CPU 프로파일 캡처 절차 추가

GOEXCHANGE_ENABLE_PPROF 플래그와 127.0.0.1:6060 포트 매핑을 docker-compose에
반영하고, SSH 터널로 30초 CPU 프로파일을 캡처하는 절차를 런북에 남긴다.
MSG
)"
```
