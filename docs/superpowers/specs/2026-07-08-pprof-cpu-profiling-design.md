# 매칭엔진 CPU 프로파일링 (pprof) 설계

## 배경 (왜 필요한가)

`docs/benchmarks/03-2026-07-08-gcp-stress-test.md`의 GCP 스트레스 테스트에서 VU 150~200 구간부터 CPU가 93%까지 포화되며 처리량이 정체됐다. 이때 `order_pipeline_match_latency_seconds`(매칭엔진 큐잉+처리 지연) p95가 10초까지 치솟았는데, 이 지표는 "채널에서 대기한 시간"과 "실제 매칭 연산에 걸린 시간"을 구분하지 않고 하나로 합쳐서 잰다. 그래서 다음 두 가설 중 무엇이 맞는지 지금 데이터로는 판단할 수 없었다:

- **가설 A**: 매칭 로직 자체(BTree 탐색 등)가 느려졌다.
- **가설 B**: 매칭 로직은 여전히 빠르지만, 단일 소비자 고루틴이 CPU 스케줄링을 충분히 못 받거나(다른 고루틴/프로세스와 경쟁), 주문마다 실행되는 오더북 스냅샷 생성(`GetOrderBookSnapshot`) 같은 부수 작업이 매칭 루프 자체를 무겁게 만들었다.

이전 매칭엔진 단독 벤치마크(`docs/benchmarks/01-2026-07-05-matching-engine-benchmarks.md`)에서는 매칭 자체가 마이크로초 단위로 매우 빨랐기 때문에 가설 B 쪽에 무게가 실리지만, 추측이 아니라 실제 CPU 프로파일로 확인해야 다음에 무엇을 고칠지 근거를 갖고 결정할 수 있다.

**왜 pprof인가 (다른 방법과의 비교):** 이 가설을 검증하는 대안으로 "파이프라인 지표를 더 잘게 쪼개서 재계측"하는 방법(큐 대기시간과 매칭 연산시간을 별도 히스토그램으로 분리)도 있었지만, 이는 코드 계측 지점을 미리 정확히 짚어야 하고 새로 재현 테스트를 한 번 더 돌려야 한다. pprof는 코드를 미리 특정하지 않고도 "CPU 시간이 실제로 어느 함수에서 소비됐는지"를 통째로 보여주므로, 원인을 모르는 지금 단계에서 가설 공간을 좁히는 더 빠른 방법이다. 파이프라인 지표 세분화는 pprof로 원인을 좁힌 뒤, 그 특정 구간만 정밀 계측하는 다음 단계로 남겨둔다.

## 범위

- Go 앱에 `net/http/pprof` 기반 프로파일링 엔드포인트를 환경변수로 게이팅해서 추가한다.
- 기존 GCP 인스턴스(`infra/terraform/gcp`로 이미 생성된 서버/부하생성 인스턴스)를 재사용해 k6 스트레스 테스트를 다시 실행하고, CPU가 포화되는 구간(VU 150~200 부근)에서 30초 CPU 프로파일을 캡처한다.
- `go tool pprof`로 캡처한 프로파일을 분석하고, 결과를 왜 이 조사를 했는지/무엇을 확인했는지/다음에 무엇을 할지가 드러나도록 `docs/benchmarks/`에 기록한다.
- 인프라(인스턴스 사양, 대수)는 늘리지 않는다. 방화벽 규칙도 변경하지 않는다 — pprof 접근은 SSH 터널링만으로 해결한다.

## 아키텍처

### 1. pprof 서버 추가

`cmd/main.go`에 다음을 추가한다:

- import: `net/http/pprof` (blank import로 `http.DefaultServeMux`에 핸들러 자동 등록), `strconv`
- `config.PprofEnabledFromEnv()`(신규, `config` 패키지에 기존 `UpbitEnabledFromEnv()`와 같은 패턴으로 추가) — `GOEXCHANGE_ENABLE_PPROF` 환경변수가 `"true"`일 때만 true
- `PprofEnabledFromEnv()`가 true면, `main()` 안에서 별도 고루틴으로 `http.ListenAndServe("127.0.0.1:6060", nil)`을 실행 (기존 gin 라우터가 쓰는 8080과는 별개의 서버, `DefaultServeMux`를 그대로 사용하므로 gin 라우터에 영향 없음)
- 127.0.0.1에만 바인딩하므로 컨테이너 밖에서는 도달 불가 — 방화벽 규칙 추가가 필요 없다.

### 2. docker-compose / env 설정

- `docker-compose.stress.yml`의 `backend` 서비스에 포트 매핑 `"127.0.0.1:6060:6060"` 추가 (호스트의 루프백에만 노출, GCP 방화벽에 규칙이 없으므로 외부에서는 도달 불가)
- `.env.stress.example`에 `GOEXCHANGE_ENABLE_PPROF=true` 추가 (스트레스 테스트 환경 전용 플래그, 기본 배포에는 영향 없음)

### 3. 실행 절차

1. 로컬에서 Go 코드 변경 후 서버 인스턴스에 재배포 (`git archive` + `docker compose up -d --build`, 기존 런북과 동일한 방식)
2. 서버 인스턴스로 SSH 터널을 연다: `ssh -L 6060:localhost:6060 -i ~/.ssh/goexchange-gcp goexchange@<server_external_ip>`
3. 부하생성 인스턴스에서 k6 스트레스 스크립트를 다시 실행
4. VU가 150~200 구간(이전 테스트에서 CPU 93%를 찍었던 지점)에 도달하면, 로컬에서 터널을 통해 프로파일을 받는다:
   ```bash
   go tool pprof -seconds=30 -output=cpu.prof http://localhost:6060/debug/pprof/profile
   ```
5. `go tool pprof -top cpu.prof`로 상위 CPU 소비 함수를 확인하고, 가능하면 `go tool pprof -svg cpu.prof > cpu.svg`로 그래프도 남긴다.
6. k6는 이전과 동일하게 병목 신호(에러율 급증 또는 p95 급격 저하)가 보이면 수동 중단한다.

### 4. 결과 기록

`docs/benchmarks/04-YYYY-MM-DD-matching-engine-cpu-profiling.md`에 다음을 남긴다:

- **왜 이 조사를 했는지**: 3번째 테스트에서 나온 미해결 질문(큐 대기 vs 실제 매칭 연산)
- **무엇을 확인했는지**: `go tool pprof -top` 결과 전체(원본 출력), 상위 CPU 소비 함수 요약 테이블, 가설 A/B 중 어느 쪽이 데이터로 뒷받침되는지
- **다음에 무엇을 할지**: 확인된 원인에 따라 구체적인 다음 작업(예: 스냅샷 생성 최적화, 큐 구조 변경, 심볼별 샤딩 검토 등)을 우선순위와 함께 제안

## 성공 기준

- 스트레스 테스트 중 CPU 포화 구간의 실제 CPU 프로파일을 확보한다.
- 프로파일 데이터로 "매칭 로직 자체가 느린가, 아니면 다른 요인(스냅샷 생성, GC, 스케줄링 경쟁 등)이 매칭 루프를 무겁게 만드는가"라는 질문에 추측이 아닌 근거 있는 답을 낸다.
- 결과 문서가 이력서/포트폴리오에 그대로 쓸 수 있을 만큼 왜/어떻게/결과가 명확하게 정리된다.

## 범위 밖 (Out of Scope)

- 프로파일링 결과로 확인된 병목을 실제로 고치는 것 — 이번 조사 결과가 나온 뒤 별도 작업으로 결정한다.
- 파이프라인 지표 세분화(`order_engine_queue_wait_duration` 등) — pprof로 원인을 좁힌 뒤 필요하면 별도 브레인스토밍.
- 인스턴스 사양 확장, 모니터링 스택 분리, 심볼별 매칭엔진 샤딩 — 모두 이번 조사 결과에 따라 판단할 다음 단계다.
- k6 스크립트의 setup/create_order 지표 분리(A안) — 이번 스코프와 독립적인 별도 위생 작업.
