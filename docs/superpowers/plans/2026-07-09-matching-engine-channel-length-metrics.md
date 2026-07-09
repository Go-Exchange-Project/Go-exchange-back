# 매칭엔진 채널 길이 지표 노출 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 매칭엔진의 `ExecutionCh`/`SnapshotCh` 버퍼가 가득 차 매칭 루프가 블로킹된다는 가설을 검증하기 위해, 매칭엔진 채널 5개의 길이를 Prometheus 게이지로 노출하고 Grafana에서 볼 수 있게 한다.

**Architecture:** `internal/metrics`는 `matching` 패키지를 직접 import하지 않고, `func() int` 클로저 5개를 받아 `promauto.NewGaugeFunc`로 등록하는 함수를 제공한다. `cmd/main.go`가 매칭엔진 생성 직후 이 함수를 호출해 실제 채널 길이를 클로저로 넘긴다. Grafana 대시보드 JSON에 새 패널을 추가한다.

**Tech Stack:** `github.com/prometheus/client_golang/prometheus`(`promauto.NewGaugeFunc`), Grafana 프로비저닝 JSON.

## Global Constraints

- 노출할 채널은 5개: `OrderCh`, `CancelCh`, `SnapshotReq`, `ExecutionCh`, `SnapshotCh`.
- 게이지 이름은 `matching_engine_channel_length`, 레이블은 `channel`(값: `order`, `cancel`, `snapshot_request`, `execution`, `snapshot`).
- `metrics` 패키지는 `matching` 패키지를 import하지 않는다 — 클로저 기반 콜백 패턴을 유지한다.

---

### Task 1: 채널 길이 게이지 등록 함수 추가

**Files:**
- Modify: `internal/metrics/metrics.go`

**Interfaces:**
- Produces: `RegisterMatchingEngineChannelLenGauges(orderLen, cancelLen, snapshotReqLen, executionLen, snapshotLen func() int)` — Task 2에서 `cmd/main.go`가 이 정확한 시그니처로 호출한다.

- [ ] **Step 1: `internal/metrics/metrics.go` 끝에 함수 추가**

파일 끝(`)` 닫는 괄호 다음)에 추가:

```go

func RegisterMatchingEngineChannelLenGauges(orderLen, cancelLen, snapshotReqLen, executionLen, snapshotLen func() int) {
	gauges := []struct {
		channel string
		lenFn   func() int
	}{
		{"order", orderLen},
		{"cancel", cancelLen},
		{"snapshot_request", snapshotReqLen},
		{"execution", executionLen},
		{"snapshot", snapshotLen},
	}
	for _, g := range gauges {
		g := g
		promauto.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "matching_engine_channel_length",
			Help:        "Current number of buffered items in a matching engine channel.",
			ConstLabels: prometheus.Labels{"channel": g.channel},
		}, func() float64 { return float64(g.lenFn()) })
	}
}
```

- [ ] **Step 2: 빌드 확인**

```bash
go build ./internal/metrics/...
go vet ./internal/metrics/...
```

Expected: 둘 다 에러 없이 종료.

- [ ] **Step 3: 커밋**

```bash
git add internal/metrics/metrics.go
git commit -m "$(cat <<'MSG'
feat(metrics): 매칭엔진 채널 길이 게이지 등록 함수 추가

ExecutionCh/SnapshotCh 버퍼가 가득 차 매칭 루프가 블로킹된다는 가설을
검증하기 위해, 매칭엔진 채널 5개의 현재 길이를 읽는 클로저를 받아
Prometheus 게이지로 등록하는 함수를 추가한다. matching 패키지를 직접
import하지 않고 기존 MatchLatencyObserver와 같은 콜백 패턴을 따른다.
MSG
)"
```

---

### Task 2: `cmd/main.go`에서 게이지 등록

**Files:**
- Modify: `cmd/main.go:61-65`

**Interfaces:**
- Consumes: `metrics.RegisterMatchingEngineChannelLenGauges(orderLen, cancelLen, snapshotReqLen, executionLen, snapshotLen func() int)` (Task 1에서 정의).

- [ ] **Step 1: 매칭엔진 생성 직후에 등록 코드 추가**

`cmd/main.go`의 현재:

```go
	me := matching.NewMatchingEngine()
	me.MatchLatencyObserver = func(d time.Duration) {
		metrics.OrderPipelineMatchLatency.Observe(d.Seconds())
	}
	me.Start()
```

다음과 같이 수정한다:

```go
	me := matching.NewMatchingEngine()
	me.MatchLatencyObserver = func(d time.Duration) {
		metrics.OrderPipelineMatchLatency.Observe(d.Seconds())
	}
	metrics.RegisterMatchingEngineChannelLenGauges(
		func() int { return len(me.OrderCh) },
		func() int { return len(me.CancelCh) },
		func() int { return len(me.SnapshotReq) },
		func() int { return len(me.ExecutionCh) },
		func() int { return len(me.SnapshotCh) },
	)
	me.Start()
```

- [ ] **Step 2: 빌드 및 전체 테스트 회귀 확인**

```bash
go build ./...
go vet ./...
go test ./... 2>&1 | tail -40
```

Expected: 전부 에러 없이 종료, 모든 패키지 PASS.

- [ ] **Step 3: `/metrics` 엔드포인트에서 게이지가 실제로 노출되는지 로컬 확인**

```bash
go run ./cmd &
sleep 3
curl -s http://localhost:8080/metrics | grep matching_engine_channel_length
kill %1
```

Expected: 다음 5줄이 출력됨 (값은 0):
```
matching_engine_channel_length{channel="cancel"} 0
matching_engine_channel_length{channel="execution"} 0
matching_engine_channel_length{channel="order"} 0
matching_engine_channel_length{channel="snapshot"} 0
matching_engine_channel_length{channel="snapshot_request"} 0
```

(로컬에 `.env`/DB 설정이 없어 `go run ./cmd`가 즉시 종료되면, 이 단계는 GCP 스트레스 서버에 재배포한 뒤 `curl http://localhost:8080/metrics | grep matching_engine_channel_length`로 대신 확인한다 — 실제 재배포/재측정은 이 계획의 범위 밖이므로, 로컬에서 안 되면 Step 3은 "빌드로 확인됨(런타임 확인은 배포 후)"로 기록하고 넘어간다.)

- [ ] **Step 4: 커밋**

```bash
git add cmd/main.go
git commit -m "$(cat <<'MSG'
feat: 매칭엔진 채널 길이 게이지를 main에서 등록

RegisterMatchingEngineChannelLenGauges를 매칭엔진 생성 직후 호출해,
OrderCh/CancelCh/SnapshotReq/ExecutionCh/SnapshotCh 5개 채널의 실시간
길이를 /metrics에 노출한다.
MSG
)"
```

---

### Task 3: Grafana 패널 추가

**Files:**
- Modify: `monitoring/grafana/provisioning/dashboards/json/goexchange-stress.json`

**Interfaces:**
- 없음 (JSON 설정 파일만 변경).

- [ ] **Step 1: 패널 배열 끝에 새 패널 추가**

`monitoring/grafana/provisioning/dashboards/json/goexchange-stress.json`의 `panels` 배열 마지막 항목(`id: 8`, "GC STW 횟수") 다음에 콤마를 추가하고 새 패널을 넣는다. 현재:

```json
    {
      "id": 8,
      "title": "GC STW 횟수",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 12, "y": 24 },
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "targets": [
        { "refId": "A", "expr": "rate(go_gc_duration_seconds_count{job=\"goexchange-backend\"}[1m])", "legendFormat": "GC cycles/sec" },
        { "refId": "B", "expr": "go_gc_duration_seconds_count{job=\"goexchange-backend\"}", "legendFormat": "GC cycles (누적)" }
      ]
    }
  ]
}
```

다음과 같이 수정한다:

```json
    {
      "id": 8,
      "title": "GC STW 횟수",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 12, "y": 24 },
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "targets": [
        { "refId": "A", "expr": "rate(go_gc_duration_seconds_count{job=\"goexchange-backend\"}[1m])", "legendFormat": "GC cycles/sec" },
        { "refId": "B", "expr": "go_gc_duration_seconds_count{job=\"goexchange-backend\"}", "legendFormat": "GC cycles (누적)" }
      ]
    },
    {
      "id": 9,
      "title": "매칭엔진 채널 길이 (버퍼 용량: order/cancel/snapshot_request/execution=1024, snapshot=256)",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 0, "y": 32 },
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "targets": [
        { "refId": "A", "expr": "matching_engine_channel_length{job=\"goexchange-backend\"}", "legendFormat": "{{channel}}" }
      ]
    }
  ]
}
```

- [ ] **Step 2: JSON 문법 검증**

```bash
python3 -m json.tool monitoring/grafana/provisioning/dashboards/json/goexchange-stress.json > /dev/null
```

Expected: 에러 없이 종료(유효한 JSON).

- [ ] **Step 3: 커밋**

```bash
git add monitoring/grafana/provisioning/dashboards/json/goexchange-stress.json
git commit -m "$(cat <<'MSG'
feat: Grafana에 매칭엔진 채널 길이 패널 추가

matching_engine_channel_length 게이지를 채널별로 나눠 보여주는 패널을
추가한다. 패널 제목에 각 채널의 버퍼 용량을 명시해, 그래프가 상한에
붙는지 한눈에 판단할 수 있게 한다.
MSG
)"
```
