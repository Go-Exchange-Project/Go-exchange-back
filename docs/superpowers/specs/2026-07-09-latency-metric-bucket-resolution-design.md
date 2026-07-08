# 매칭/정산 지연 지표 히스토그램 버킷 해상도 확장 설계

## 배경 (왜 필요한가)

`04~07`번 스트레스 테스트 문서에서, `order_pipeline_match_latency_seconds`의 p95가 매번 정확히 10초로 관측됐다 — CPU 코어 핀닝 같은 서로 다른 실험을 거쳐도 값이 전혀 안 바뀐 게 이상해서 코드를 확인해보니, `internal/metrics/metrics.go`에서 이 지표(그리고 `order_settlement_duration_seconds`)가 `prometheus.DefBuckets`(`.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10`)를 쓰고 있었다. 이 버킷의 마지막 유한 경계가 10초라서, 실제 지연이 10초보다 크면 전부 `+Inf` 버킷에 뭉뚱그려지고 `histogram_quantile`은 그 안에서 보간을 못 해 10초로 표시된다.

즉 지금까지 본 "p95 10초"는 진짜 값이 아니라 **측정 상한에 눌려서 잘려 보이는 값**일 가능성이 높다. 이걸 먼저 확인하지 않으면, 앞으로 어떤 최적화를 해도 "10초에서 얼마나 줄었는지"를 정확히 판단할 수 없다.

## 왜 이 방식을 선택했는지

버킷 경계를 확장하는 것 외에 다른 대안(예: Summary 타입으로 바꾸기, 별도 트레이싱 도입)도 있지만, 이번엔 최소 변경으로 문제를 확인하는 게 목적이라 기존 Histogram의 `Buckets` 값만 확장하는 게 가장 간단하다. `HTTPRequestDuration`은 그대로 둔다 — 다른 문서들에서 HTTP 응답 자체는 이미 초 단위로 정상 관측됐고, 이번에 상한 문제가 의심되는 건 매칭/정산 지표뿐이다.

## 범위

- `internal/metrics/metrics.go`에서 `OrderPipelineMatchLatency`, `OrderSettlementDuration` 두 히스토그램의 `Buckets`를 확장한다.
- `order_settlement_duration_seconds`도 같은 Grafana 패널(p95)에서 `order_pipeline_match_latency_seconds`와 나란히 쓰이므로 함께 확장한다.
- `HTTPRequestDuration`(`http_request_duration_seconds`)은 이번 스코프가 아니다.
- 재배포/재측정/결과 문서화는 이 스펙 문서화 이후 사용자와 직접 진행한다(코드 준비까지만 계획에 담는다).

## 아키텍처

`internal/metrics/metrics.go`의 두 히스토그램 정의를 다음과 같이 바꾼다:

```go
var matchLatencyBuckets = append(prometheus.DefBuckets, 15, 20, 30, 45, 60)

OrderPipelineMatchLatency = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "order_pipeline_match_latency_seconds",
	Help:    "Time from order enqueue into the matching engine to completion of matching for that order.",
	Buckets: matchLatencyBuckets,
})

OrderSettlementDuration = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "order_settlement_duration_seconds",
	Help:    "Time to persist trade settlement (wallet/ledger updates) after a match event.",
	Buckets: matchLatencyBuckets,
})
```

`prometheus.DefBuckets`(`.005`~`10`)에 15/20/30/45/60초 버킷을 추가해서, 최대 60초까지는 실제 값을 정확히 구분해서 볼 수 있게 한다. 두 히스토그램이 같은 버킷 슬라이스(`matchLatencyBuckets`)를 공유하게 해서 Grafana에서 나란히 비교할 때 버킷 경계가 어긋나지 않게 한다.

### 검증 절차 (사용자와 직접 진행, 이 계획의 범위 밖)

1. 서버 인스턴스에 재배포, `docker compose -f docker-compose.stress.yml up -d --build --force-recreate`로 재기동 (Histogram 정의 변경은 새 바이너리가 필요).
2. k6 스트레스 테스트를 04~07번 테스트와 동일한 조건으로 재실행.
3. 같은 VU 구간(약 150)에서 `order_pipeline_match_latency_seconds` p95가 10초를 넘는지, 넘는다면 얼마나 큰지 확인.
4. 결과를 `docs/benchmarks/08-YYYY-MM-DD-latency-metric-bucket-resolution.md`에 기록 — "10초는 상한이었다" 또는 "10초가 진짜 값이었다" 둘 중 어느 쪽인지 명확히 결론짓는다.

## 성공 기준

- 코드가 빌드/기존 테스트를 통과한다.
- 재측정 후, `order_pipeline_match_latency_seconds`의 진짜 p95 값(10초를 초과하든 안 하든)을 정확히 알 수 있다.

## 범위 밖 (Out of Scope)

- `HTTPRequestDuration` 버킷 변경.
- 이번 조사 결과에 따른 실제 최적화 작업(예: 매칭 고루틴 핀닝) — 결과를 보고 별도로 브레인스토밍.
- VM 사양 변경 — 이 프로젝트의 목표에 따라 계속 배제.
