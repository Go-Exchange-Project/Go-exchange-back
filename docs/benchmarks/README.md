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
