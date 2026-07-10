# 벤치마크/테스트 결과 기록

성능 비교(전/후)를 위해 벤치마크 및 주요 테스트 실행 결과를 이 디렉토리에 순서대로 저장합니다.

## 파일 명명 규칙

`NN-YYYY-MM-DD-<대상>-benchmarks.md` (예: `01-2026-07-05-matching-engine-benchmarks.md`)

- `NN`: 몇 번째 기록인지 (01, 02, 03, ... 두 자리, 100개 넘으면 자릿수 확장). 파일명이 정렬 순서 = 기록 순서가 되도록 함
- 파일 제목(H1)에도 `N번째 테스트 (날짜)` 형식으로 명시

## 각 파일에 포함할 내용

1. 커밋 해시 (결과가 어느 시점의 코드인지)
2. 실행한 정확한 커맨드
3. 원본 출력 (raw output) — 가공하지 않은 그대로
4. 요약 테이블 (사람이 읽기 쉽게)
5. 해석 (특이사항, 예상과 다른 결과 등)

## 기록 목록

- [01-2026-07-05-matching-engine-benchmarks.md](01-2026-07-05-matching-engine-benchmarks.md) — 1번째 테스트: 매칭엔진 동시성/벤치마크 테스트 추가 작업 직후 첫 벤치마크 결과 (커밋 `24f8cb2`)
- [02-2026-07-06-k6-order-submission-baseline.md](02-2026-07-06-k6-order-submission-baseline.md) — 2번째 테스트: POST /orders 부하테스트(k6) 기준선, VU 10~50명 (커밋 `d2bf50a`)
- [03-2026-07-08-gcp-stress-test.md](03-2026-07-08-gcp-stress-test.md) — 3번째 테스트: GCP 분리 환경(서버+부하생성 인스턴스 2대) 스트레스 테스트, VU 150~200 구간에서 CPU 포화로 인한 병목 확인 (커밋 `3583176`)
- [04-2026-07-08-matching-engine-cpu-profiling.md](04-2026-07-08-matching-engine-cpu-profiling.md) — 4번째 테스트: pprof CPU 프로파일링. 병목은 매칭엔진이 아니라 미설정된 DB 커넥션 풀로 인한 반복적 SCRAM 인증(CPU의 38%)으로 확인 (커밋 `3122c33`)
- [05-2026-07-09-db-connection-pool-tuning.md](05-2026-07-09-db-connection-pool-tuning.md) — 5번째 테스트: DB 커넥션 풀 튜닝 전/후 비교. 동일 VM에서 완주 가능 VU가 250→800(4배+)으로, p95가 9.09s→4.30s(VU150 기준)로 개선 (커밋 `85e2bf8`)
- [06-2026-07-09-matching-engine-pure-tps-benchmark.md](06-2026-07-09-matching-engine-pure-tps-benchmark.md) — 6번째 테스트: 순수 매칭엔진 TPS 벤치마크(API/DB 제외). 지정가 465,036 TPS, 시장가 163,926 TPS, 혼합 216,731 TPS — 전체 스택(55.2 TPS)과 측정 대상이 다름을 확인 (커밋 `b025055`)
- [07-2026-07-09-cpu-core-pinning.md](07-2026-07-09-cpu-core-pinning.md) — 7번째 테스트: CPU 코어 핀닝(backend=코어0, 나머지=코어1) 실험. 가설과 달리 처리량이 12.7% 감소해 기각 — 격리 이득보다 가용 용량 축소 손해가 더 컸음 (커밋 `1db00ff`)
- [08-2026-07-09-latency-metric-bucket-resolution.md](08-2026-07-09-latency-metric-bucket-resolution.md) — 8번째 테스트: 매칭 지연 지표 버킷 상한(10초) 수정 후 재측정. 진짜 p95는 10초가 아니라 14.2~27.5초로, 기존 관측치보다 최대 2.75배 심각했음을 확인 (커밋 `6dfced6`)
- [09-2026-07-09-matching-engine-channel-length.md](09-2026-07-09-matching-engine-channel-length.md) — 9번째 테스트: 매칭엔진 채널 길이 노출로 병목 메커니즘 확인. 단일 정산 워커가 못 따라가 ExecutionCh/OrderCh가 버퍼 상한(1024)까지 포화 → 매칭 루프 블로킹이 지연의 진짜 원인임을 실측 확인 (커밋 `a3d1226`)
- [10-2026-07-09-settlement-worker-pool.md](10-2026-07-09-settlement-worker-pool.md) — 10번째 테스트: 정산 워커 풀(1개→10개) 전/후 비교. 처리량 9% 개선됐지만 채널 포화·매칭 지연 최댓값은 거의 그대로 — CPU가 99.3%까지 치솟아 병목이 "정산 워커 부족"에서 "CPU 물리적 한계"로 이동한 것으로 해석 (커밋 `d50e383`)
- [11-2026-07-09-vm-cpu-scaling-control.md](11-2026-07-09-vm-cpu-scaling-control.md) — 11번째 테스트: VM 코어 수 대조군 실험(2vCPU→4vCPU). 처리량 2.9배, p95 78% 개선(6.02s→1.32s), 채널 포화 대부분 해소 — CPU가 진짜 병목이었음을 확인하고 유지 결정, Terraform 반영 (커밋 `893d548`)
- [12-2026-07-10-stress-test-vu-ceiling-extension.md](12-2026-07-10-stress-test-vu-ceiling-extension.md) — 12번째 테스트: VU 상한 800→3,000 확장. 부하생성기가 먼저 OOM으로 죽는 걸 발견해 리사이즈 후 재시도 — CPU가 VU 700~800에서 90%로 포화된 뒤로는 크래시 없이 큐잉 지연만 선형 증가함을 확인(p95 14.7s까지) (커밋 `0c1df36`)
