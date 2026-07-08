# Go DB 커넥션 풀 Prometheus 지표 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** pprof로 찾은 DB 커넥션 풀 미설정 문제를 Grafana에서 실시간으로 관찰할 수 있도록, Go `database/sql` 커넥션 풀 상태를 Prometheus 지표로 노출하고 대시보드에 패널을 추가한다.

**Architecture:** `config.ConnectDB()`에서 `*sql.DB`를 얻은 직후 Prometheus 공식 `collectors.NewDBStatsCollector`를 등록해 `go_sql_*` 지표를 노출한다. 이미 존재하는 `/metrics` 엔드포인트를 통해 별도 라우팅 변경 없이 그대로 수집되며, Grafana 대시보드 JSON에 새 패널을 하나 추가한다.

**Tech Stack:** `github.com/prometheus/client_golang/prometheus/collectors`(이미 `go.mod`에 있는 `client_golang` v1.23.2에 포함), 기존 Grafana 대시보드 프로비저닝 구조.

## Global Constraints

- 노출 지표 이름은 정확히: `go_sql_open_connections`, `go_sql_in_use_connections`, `go_sql_idle_connections`, `go_sql_max_open_connections`, `go_sql_wait_count_total`, `go_sql_wait_duration_seconds_total`, `go_sql_max_idle_closed_total`, `go_sql_max_idle_time_closed_total`, `go_sql_max_lifetime_closed_total` (모두 `db_name="goexchange"` 라벨 포함, 코드에서 하드코딩하지 않고 컬렉터가 자동 부여).
- 이번 스코프는 지표 노출/관찰까지만 — `SetMaxOpenConns` 등 실제 풀 설정 변경은 하지 않는다.
- 새 패널은 기존 5개 패널과 같은 2열 그리드 레이아웃(`h:8, w:12`)을 따른다.

---

### Task 1: `ConnectDB()`에 DB 커넥션 풀 지표 등록

**Files:**
- Modify: `config/database.go`

**Interfaces:**
- Produces: `/metrics` 엔드포인트에 `go_sql_open_connections` 등 9개 지표 노출 (Task 2가 Grafana 쿼리에서 이 이름들을 그대로 참조).

- [ ] **Step 1: import 추가**

`config/database.go`의 import 블록, 현재:

```go
import (
	"log"
	"os"
	"strings"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)
```

다음과 같이 수정한다:

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

- [ ] **Step 2: `ConnectDB()`에 컬렉터 등록**

`ConnectDB()` 함수, 현재:

```go
func ConnectDB() {
	dsn := DatabaseDSNFromEnv()

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("DB connection failed: ", err)
	}

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
	prometheus.MustRegister(collectors.NewDBStatsCollector(sqlDB, "goexchange"))

	DB = db
	log.Println("DB connection established")
}
```

- [ ] **Step 3: 빌드 및 전체 테스트 확인**

`ConnectDB()`는 실제 Postgres 연결이 있어야 실행되므로(기존에도 이 함수를 직접 호출하는 단위 테스트는 없음 — `config/database_test.go`는 `DatabaseDSNFromEnv()`만 테스트한다), 이 스텝은 빌드/정적 검사와 기존 테스트 회귀만 확인한다.

```bash
go build ./...
go vet ./...
go test ./... 2>&1 | tail -40
```

Expected: 빌드 성공, `go vet` 통과, 기존 테스트 전부 PASS (새로 실행되는 테스트는 없음 — `ConnectDB()` 자체가 단위 테스트 대상이 아니었으므로 회귀만 확인).

- [ ] **Step 4: 커밋**

```bash
git add config/database.go
git commit -m "$(cat <<'MSG'
feat: DB 커넥션 풀 상태를 Prometheus 지표로 노출

pprof 조사에서 DB 커넥션 풀 미설정으로 인한 반복적 재연결이 CPU의
상당 부분을 차지한다는 걸 확인했다. collectors.NewDBStatsCollector로
Go database/sql의 풀 상태(열린/유휴/사용중 커넥션, 대기 횟수)를
/metrics에 노출해 이 문제를 실시간으로 관찰할 수 있게 한다.
MSG
)"
```

---

### Task 2: Grafana 대시보드에 커넥션 풀 패널 추가

**Files:**
- Modify: `monitoring/grafana/provisioning/dashboards/json/goexchange-stress.json`

**Interfaces:**
- Consumes: Task 1의 `go_sql_open_connections`, `go_sql_in_use_connections`, `go_sql_idle_connections`, `go_sql_wait_count_total` 지표 이름.

- [ ] **Step 1: 새 패널 추가**

`monitoring/grafana/provisioning/dashboards/json/goexchange-stress.json`의 `panels` 배열, 현재 마지막 패널(id 5) 다음:

```json
    {
      "id": 5,
      "title": "Postgres 커넥션 / 커밋 통계",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 0, "y": 16 },
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "targets": [
        { "refId": "A", "expr": "pg_stat_activity_count", "legendFormat": "{{state}} 커넥션" },
        { "refId": "B", "expr": "rate(pg_stat_database_xact_commit{datname=\"goexchange\"}[1m])", "legendFormat": "commits/sec" }
      ]
    }
  ]
}
```

다음과 같이 수정한다 (id 5 다음에 id 6 추가):

```json
    {
      "id": 5,
      "title": "Postgres 커넥션 / 커밋 통계",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 0, "y": 16 },
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "targets": [
        { "refId": "A", "expr": "pg_stat_activity_count", "legendFormat": "{{state}} 커넥션" },
        { "refId": "B", "expr": "rate(pg_stat_database_xact_commit{datname=\"goexchange\"}[1m])", "legendFormat": "commits/sec" }
      ]
    },
    {
      "id": 6,
      "title": "Go DB 커넥션 풀",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 12, "y": 16 },
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "targets": [
        { "refId": "A", "expr": "go_sql_open_connections", "legendFormat": "open" },
        { "refId": "B", "expr": "go_sql_in_use_connections", "legendFormat": "in use" },
        { "refId": "C", "expr": "go_sql_idle_connections", "legendFormat": "idle" },
        { "refId": "D", "expr": "rate(go_sql_wait_count_total[1m])", "legendFormat": "new conn waits/sec" }
      ]
    }
  ]
}
```

- [ ] **Step 2: 문법 검증**

```bash
python3 -c "import json; json.load(open('monitoring/grafana/provisioning/dashboards/json/goexchange-stress.json'))"
```

Expected: 에러 없이 종료.

- [ ] **Step 3: 커밋**

```bash
git add monitoring/grafana/provisioning/dashboards/json/goexchange-stress.json
git commit -m "$(cat <<'MSG'
feat: Grafana에 Go DB 커넥션 풀 패널 추가

Task 1에서 노출한 go_sql_* 지표(열린/유휴/사용중 커넥션 수, 새 커넥션
대기율)를 대시보드에서 실시간으로 볼 수 있도록 6번째 패널을 추가한다.
MSG
)"
```
