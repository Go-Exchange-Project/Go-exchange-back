# 매칭엔진 채널 길이 지표 노출 설계

## 배경 (왜 필요한가)

`internal/matching/engine.go`의 `Start()`는 `OrderCh`/`CancelCh`/`SnapshotReq`를 처리하는 **단일 `for-select` 루프**로 되어 있고, `Match()` 내부에서 체결이 나면 다음 두 전송이 이 루프 안에서 동기적으로 일어난다:

```go
me.ExecutionCh <- ExecutionEvent{Trade: trade}
me.SnapshotCh <- me.GetOrderBookSnapshot(order.CoinSymbol)
```

`ExecutionCh`(버퍼 1024)와 `SnapshotCh`(버퍼 256)는 각각 정산(DB 쓰기)과 웹소켓 브로드캐스트를 처리하는 별도 고루틴이 소비하는데, 그 소비 속도가 느려서 버퍼가 가득 차면 이 전송 자체가 **블로킹**된다. 이렇게 되면 매칭 루프가 멈추고, 그 뒤로 들어오는 모든 심볼의 모든 주문이 줄줄이 밀린다.

`08번 테스트`([[goexchange-resume-documentation-style]] 원칙에 따라 문서화됨)에서 실측한 매칭 지연 p95(14.2~27.5초)가 이 메커니즘 때문인지 아직 확인되지 않았다 — pprof(04번)에서 본 "CPU 스케줄링 경쟁"이라는 막연한 설명보다, "채널 버퍼가 가득 차서 블로킹된다"는 게 훨씬 구체적이고 검증 가능한 가설이다. 이번 작업은 이 가설을 실측으로 확인하기 위한 관측 지표를 먼저 추가한다.

## 왜 이 방식을 선택했는지

매칭엔진의 5개 채널(`OrderCh`, `CancelCh`, `SnapshotReq`, `ExecutionCh`, `SnapshotCh`) 길이를 전부 노출하기로 했다 — `ExecutionCh`/`SnapshotCh`가 가장 유력한 용의자지만, `OrderCh`(주문이 매칭 루프에 들어가기도 전에 밀리는지)도 다른 각도의 신호를 준다. `CancelCh`/`SnapshotReq`는 부하테스트에서 거의 안 쓰이지만, 나중에 다시 추가하는 수고를 줄이기 위해 지금 다 넣는다.

`metrics` 패키지가 `matching` 패키지를 직접 import하지 않는 기존 패턴(`MatchLatencyObserver` 콜백)을 그대로 따른다 — `metrics.go`는 `func() int` 클로저 5개를 받아 각각을 `promauto.NewGaugeFunc`로 등록하고, 실제 `len(me.XCh)` 호출은 `cmd/main.go`에서 클로저로 넘긴다. 이렇게 하면 `metrics` 패키지가 `matching.MatchingEngine` 타입을 몰라도 되고, 순환 참조도 생기지 않는다.

## 범위

- `internal/metrics/metrics.go`에 채널 길이 게이지 등록 함수를 추가한다.
- `cmd/main.go`에서 매칭엔진 생성 직후 이 함수를 호출해 5개 채널을 등록한다.
- Grafana 대시보드에 채널 길이를 보여주는 패널을 추가한다.
- 재배포/재측정/결과 문서화는 이 스펙 문서화 이후 사용자와 직접 진행한다(코드/설정 준비까지만 계획에 담는다).

## 아키텍처

### 1. `internal/metrics/metrics.go`에 게이지 등록 함수 추가

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

`promauto.NewGaugeFunc`는 Prometheus가 스크레이프할 때마다 넘겨준 함수를 호출해서 값을 읽는다 — 별도의 폴링 고루틴이나 동기화가 필요 없다.

### 2. `cmd/main.go`에서 등록

`me := matching.NewMatchingEngine()` 직후에 추가:

```go
metrics.RegisterMatchingEngineChannelLenGauges(
	func() int { return len(me.OrderCh) },
	func() int { return len(me.CancelCh) },
	func() int { return len(me.SnapshotReq) },
	func() int { return len(me.ExecutionCh) },
	func() int { return len(me.SnapshotCh) },
)
```

### 3. Grafana 패널

`monitoring/grafana/provisioning/dashboards/json/goexchange-stress.json`에 새 패널(`h:8, w:12`, 기존 패널들과 같은 2단 그리드) 추가:
- 쿼리: `matching_engine_channel_length{job="goexchange-backend"}` (5개 시계열, `channel` 레이블로 범례 구분)
- 패널 설명(description)에 각 채널의 버퍼 용량을 명시: `order/cancel/snapshot_request/execution=1024, snapshot=256` — 그래프가 이 상한에 붙는지 한눈에 판단할 수 있게.

### 검증 절차 (사용자와 직접 진행, 이 계획의 범위 밖)

1. 서버 인스턴스에 재배포, `docker compose -f docker-compose.stress.yml up -d --build --force-recreate`로 재기동.
2. k6 스트레스 테스트를 04~08번 테스트와 동일한 조건으로 재실행.
3. 매칭 지연(`order_pipeline_match_latency_seconds`)이 치솟는 시점에 `matching_engine_channel_length{channel="execution"}` 또는 `channel="snapshot"`이 버퍼 상한(1024/256) 근처까지 차는지 확인.
4. 결과를 `docs/benchmarks/09-YYYY-MM-DD-matching-engine-channel-length.md`에 기록 — 가설이 맞았는지 틀렸는지 명확히 결론짓는다.

## 성공 기준

- 코드가 빌드/기존 테스트를 통과한다.
- `/metrics` 엔드포인트에서 `matching_engine_channel_length` 5개 시계열이 노출된다.
- 재측정 후, "블로킹 채널 전송이 지연의 원인인가"에 대한 명확한 실측 근거(맞다/아니다 둘 중 하나)를 얻는다.

## 범위 밖 (Out of Scope)

- 이번 조사 결과에 따른 실제 수정(정산 워커 풀 확장, 논블로킹 전송 등) — 결과를 보고 별도로 브레인스토밍.
- Redis/Kafka 도입, VM 사이즈 조정 — 로드맵의 다음 단계, 이번 스코프 아님.
