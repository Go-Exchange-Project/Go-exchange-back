# GCP 분리 환경 스트레스 테스트 설계

## 배경

이전 두 작업(`2026-07-05-matching-engine-concurrency-benchmark-tests-design.md`, `2026-07-05-k6-order-submission-baseline-design.md`)은 각각 매칭엔진 단독 로직과, 로컬 환경에서의 API+DB 기준선(정상 부하)을 측정했다. 이번 작업은 그 다음 단계로, **시스템이 한계 부하에서 어디서·어떻게 무너지는지**를 관찰하는 스트레스 테스트다.

로컬 1대(같은 머신)에서 부하생성기(k6)와 서버(Go 앱+DB)를 함께 돌리면, 극한 부하에서 k6 자체가 CPU/네트워크 자원을 서버와 경쟁하게 되어 "서버가 느려진 것"과 "부하생성기가 자원을 뺏은 것"을 구분하기 어렵다. 이 노이즈를 제거하기 위해 이번 테스트는 **GCP 인스턴스 2대(서버용, 부하생성용)로 물리적으로 분리된 환경**에서 진행한다. GCP는 사용자의 첫 가입 무료 크레딧을 활용한다.

또한 로컬 k6 실행 시 클라이언트 관점 지표(응답시간, 에러율)만 보였는데, 이번엔 서버 쪽 지표(CPU, 메모리, GC, DB 커넥션, 매칭엔진 내부 지연)를 실시간으로 함께 봐야 "어디서 무너졌는지"를 판단할 수 있으므로, Prometheus + Grafana를 도입한다.

## 범위

- GCP Compute Engine 인스턴스 2대(서버, 부하생성)를 Terraform으로 프로비저닝
- 서버 인스턴스에 Go 앱 + Postgres + Prometheus + Grafana + node_exporter + postgres_exporter를 docker-compose로 구성
- Go 앱에 Prometheus 메트릭(기본 런타임 지표 + HTTP 엔드포인트 지표 + 매칭/정산 파이프라인 지연 지표) 추가
- 기존 `loadtest/order-submission-baseline.js`를 확장한 스트레스 시나리오를 부하생성 인스턴스에서 실행
- 단계적 램프업(50→100→200→400→800 VU)으로 시스템이 무너지는 지점을 관찰하고 결과를 기록

## 아키텍처

### 1. 인프라 (Terraform, `infra/terraform/gcp/`)

기존 `infra/terraform/ec2/`(AWS, 코드만 존재하고 실제로 띄운 적 없음)를 대체한다.

- `versions.tf`: `google` provider
- `variables.tf`: `gcp_project_id`, `gcp_region`(기본값 `asia-northeast3`, 서울), `gcp_zone`, `server_machine_type`(기본값 `e2-medium`), `load_gen_machine_type`(기본값 `e2-small`), `allowed_admin_cidr`(SSH/Grafana/Prometheus를 허용할 내 공인 IP)
- `main.tf`:
  - `google_compute_network` + `google_compute_subnetwork`: 두 인스턴스가 속할 VPC
  - `google_compute_firewall`:
    - `allow-admin`: 22(SSH), 3000(Grafana), 9090(Prometheus) — `allowed_admin_cidr`에서만 허용
    - `allow-api`: 8080(Go API) — load-gen 인스턴스의 내부 IP + `allowed_admin_cidr`에서만 허용
  - `google_compute_instance "server"`: `server_machine_type`, Ubuntu 이미지, 외부 IP 부여
  - `google_compute_instance "load_gen"`: `load_gen_machine_type`, Ubuntu 이미지, 외부 IP 부여
- `outputs.tf`: 두 인스턴스의 외부/내부 IP

머신 타입은 나중에 언제든 변경 가능하다(인스턴스 중지 → `machine_type` 변경 → `terraform apply`로 재시작, AWS EC2 인스턴스 타입 변경과 동일한 절차). 이번엔 무난한 사양(server: 2 vCPU/4GB, load-gen: 2 vCPU/2GB)으로 시작한다.

### 2. 서버 인스턴스 스택

기존 `docker-compose.deploy.yml`을 기반으로 하되, 이번 스코프는 API 스트레스 테스트이므로 frontend 서비스는 제외한다. 대신 다음을 추가한다:

- `prometheus`: `prometheus.yml`에서 Go 앱의 `/metrics`, `node-exporter:9100`, `postgres-exporter:9187`를 스크레이핑
- `grafana`: Prometheus를 데이터소스로 하는 대시보드
- `node-exporter`: 머신 자체의 CPU/메모리/디스크 지표
- `postgres-exporter`: Postgres 커넥션 수, 쿼리 통계

### 3. Go 앱 계측 추가

- `promhttp.Handler()`를 `/metrics` 엔드포인트로 노출 (Go 런타임 기본 지표: goroutine 수, GC 정지시간, 힙 메모리 등 자동 포함)
- gin 미들웨어로 HTTP 엔드포인트별 요청 수(counter)와 응답시간(histogram) 수집, 라벨은 `method`, `path`, `status`
- 매칭/정산 파이프라인 지연 지표 (신규):
  - `order_pipeline_match_latency_seconds` (histogram): `internal/service/order_service.go`의 `s.MatchingEngine.OrderCh <- ...` 시점부터, `cmd/main.go`의 `for event := range me.ExecutionCh` 루프에서 해당 이벤트를 수신하는 시점까지. 매칭엔진 내부 큐잉+매칭 처리 시간을 나타낸다.
  - `order_settlement_duration_seconds` (histogram): `processExecutionEvent` 내부에서 정산(체결 반영, 지갑/원장 갱신) DB 트랜잭션이 완료되기까지 걸리는 시간.

이 두 지표는 `POST /orders`의 HTTP 응답시간(이미 k6가 측정)과는 별개다 — 주문 생성 API는 매칭엔진 채널에 넣자마자 바로 응답하므로, 매칭·정산은 요청 생명주기 밖에서 비동기로 진행된다. 따라서 이 지표들로 "API 지연"과 "비동기 매칭/정산 지연"을 구분해서 볼 수 있다.

### 4. Grafana 대시보드 구성

다음 5개 패널 그룹으로 구성한다:
1. Go 런타임: goroutine 수, GC 정지시간, 힙 메모리
2. HTTP 엔드포인트별 QPS 및 p50/p95/p99 응답시간 (특히 `POST /orders`)
3. 매칭/정산 파이프라인 지연: `order_pipeline_match_latency_seconds`, `order_settlement_duration_seconds`의 분포
4. 머신 자원(node_exporter): CPU 사용률, 메모리 사용률, 디스크 I/O
5. Postgres(postgres_exporter): 활성 커넥션 수, 커넥션 풀 대기, 느린 쿼리 통계

### 5. k6 스트레스 시나리오

`loadtest/order-submission-baseline.js`를 확장한 `loadtest/order-submission-stress.js`(신규 파일)로 작성한다. 기본 구조(주문 생성 로직, 홀/짝수 매수자/매도자 배정, 가입-또는-로그인 폴백)는 재사용하되:

- `setup()`에서 목표 최대 VU 수(800)만큼 유저를 미리 등록/자금 충전 (기존 50명 → 800명으로 확장)
- 부하 프로파일: `ramping-vus` executor로 50 → 100 → 200 → 400 → 800 VU까지 각 단계 2분씩 유지하며 단계적으로 램프업
- 사전에 정의된 목표 수치나 임계값(threshold)은 두지 않는다. Grafana 대시보드를 실시간으로 관찰하면서, 에러율 급증 또는 p95 응답시간의 급격한 저하가 관측되는 시점에 수동으로 테스트를 중단한다. 필요하면 800 VU 이후 단계를 추가해 계속 밀어붙일 수 있다.

### 6. 실행 순서

1. `infra/terraform/gcp/`에서 `terraform apply`로 인스턴스 2대 생성
2. 서버 인스턴스에 SSH 접속 → 코드 배포 → `docker-compose up`으로 백엔드+DB+모니터링 스택 기동
3. 로컬 브라우저에서 서버 인스턴스의 Grafana(3000)에 접속해 대시보드 확인
4. 부하생성 인스턴스에 SSH 접속 → k6 설치 → `order-submission-stress.js` 실행 (대상: 서버 인스턴스의 내부 또는 외부 IP:8080)
5. 테스트 진행 중 Grafana를 실시간으로 관찰하며 한계점(병목) 도달 시 k6 실행을 수동 중단
6. k6 결과 요약 + Grafana 대시보드 스크린샷 + 관찰된 병목 원인을 `docs/benchmarks/03-YYYY-MM-DD-gcp-stress-test.md`에 기록 (기존 컨벤션 유지)
7. 인스턴스는 며칠간 유지하며 필요시 대시보드를 추가로 들여다보고, 이후 수동으로 `terraform destroy`

## 성공 기준

특정 성능 수치(예: "800 VU까지 버텨야 함")를 목표로 하지 않는다. 이번 목표는 **시스템이 한계 부하에서 무너지는 지점과, 그 원인(CPU/메모리/GC/DB 커넥션/매칭엔진 큐잉 지연 중 무엇인지)을 최소 1가지 이상 식별 가능하게 만드는 것**이다.

- GCP 인스턴스 2대가 Terraform으로 재현 가능하게 프로비저닝된다.
- Grafana 대시보드에서 API/DB/매칭엔진/머신 자원 지표를 실시간으로 함께 볼 수 있다.
- 스트레스 테스트 실행 결과와 관찰된 병목이 `docs/benchmarks/`에 기록된다.

## 범위 밖 (Out of Scope)

- 발견된 병목을 실제로 고치는 것 (코드/아키텍처 개선) — 이번 결과가 나온 뒤 별도 작업으로 결정
- 혼합 시나리오(조회 트래픽 포함) 테스트
- 오토스케일링, 로드밸런서, 다중 서버 인스턴스 구성
- CI/CD 연동, 상시 운영용 모니터링 스택 구축
