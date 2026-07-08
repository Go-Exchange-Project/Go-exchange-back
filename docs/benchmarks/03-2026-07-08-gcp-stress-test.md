# 3번째 테스트 (2026-07-08): GCP 분리 환경 스트레스 테스트

## 커밋 해시

`3583176` (fix: k6 setup() 타임아웃으로 인한 스트레스 테스트 실패 수정)

## 환경

- 서버 인스턴스: GCP `e2-medium`(2 vCPU, 4GB), 서울 리전(`asia-northeast3-a`), 외부 IP `8.230.15.177`
  - `docker-compose.stress.yml`로 Go 백엔드 + Postgres + Prometheus + Grafana + node_exporter + postgres_exporter를 한 인스턴스에서 함께 기동
- 부하생성 인스턴스: GCP `e2-small`(2 vCPU, 2GB), 같은 리전, 외부 IP `34.50.55.115`
  - k6 v2.0.0

## 실행한 정확한 커맨드

```bash
k6 run -e BASE_URL=http://10.10.0.3:8080 -e DEV_TOOLS_TOKEN=<토큰> loadtest/order-submission-stress.js
```

(서버 내부 IP `10.10.0.3:8080`을 대상으로, 부하생성 인스턴스에서 실행)

## 진행 중 발견하고 고친 버그 2건

이번이 처음으로 실제 GCP 배포 환경에서 전체 파이프라인을 돌려본 것이라, 로컬/코드 리뷰 단계에서는 드러나지 않았던 런타임 버그 2개를 발견해서 즉시 수정했다.

1. **`docker-compose.stress.yml`의 Postgres 볼륨 마운트 경로 오류** (커밋 `37532f3`): `postgres:18-alpine`부터 데이터 디렉토리가 버전별 서브디렉토리 구조로 바뀌어 `/var/lib/postgresql/data`가 아닌 `/var/lib/postgresql`에 마운트해야 하는데, 잘못된 경로로 설정되어 있어 컨테이너가 시작 직후 종료(exit 1)했다.
2. **k6 `setup()` 기본 타임아웃(60초) 초과** (커밋 `3583176`): `TOTAL_USERS=800`명을 순차로 가입+자금충전하는 `setup()`이 k6 기본 60초 제한을 넘겨 `setup() execution timed out after 60 seconds` 에러로 테스트 전체가 죽었다. `options.setupTimeout: '5m'`을 추가해 해결했다.

## 원본 출력

전체 로그가 매초 단위 진행률 라인(1442줄)으로 이루어져 있어, 30초 간격으로 샘플링한 램프업 타임라인 + 최종 요약 전체를 그대로 남긴다.

### 램프업 타임라인 (30초 간격 샘플)

```
running (01m12.6s), 000/800 VUs, 0 complete and 0 interrupted iterations
running (01m42.6s), 012/800 VUs, 511 complete and 0 interrupted iterations
running (02m12.6s), 025/800 VUs, 2064 complete and 0 interrupted iterations
running (02m42.6s), 037/800 VUs, 4538 complete and 0 interrupted iterations
running (03m12.6s), 050/800 VUs, 7448 complete and 0 interrupted iterations
running (03m42.6s), 062/800 VUs, 9923 complete and 0 interrupted iterations
running (04m12.6s), 075/800 VUs, 11843 complete and 0 interrupted iterations
running (04m42.6s), 087/800 VUs, 13046 complete and 0 interrupted iterations
running (05m12.6s), 100/800 VUs, 13954 complete and 0 interrupted iterations
running (05m42.6s), 125/800 VUs, 14835 complete and 0 interrupted iterations
running (06m12.6s), 150/800 VUs, 15687 complete and 0 interrupted iterations
running (06m42.6s), 175/800 VUs, 16558 complete and 0 interrupted iterations
running (07m12.6s), 201/800 VUs, 17415 complete and 0 interrupted iterations
running (07m42.6s), 251/800 VUs, 18272 complete and 0 interrupted iterations
```

### 수동 중단 시점 최종 요약 (k6 raw output, 가공 없음)

```
  █ TOTAL RESULTS 

    checks_total.......: 18321   39.408676/s
    checks_succeeded...: 100.00% 18321 out of 18321
    checks_failed......: 0.00%   0 out of 18321

    ✓ order accepted (status 200)

    HTTP
    http_req_duration..............: avg=1.44s min=827.74µs med=96.2ms   max=9.42s p(90)=5.31s p(95)=6.41s
      { expected_response:true }...: avg=1.49s min=3ms      med=132.11ms max=9.42s p(90)=5.41s p(95)=6.45s
    http_req_failed................: 3.27%  674 out of 20595
    http_reqs......................: 20595  44.300075/s

    EXECUTION
    iteration_duration.............: avg=1.97s min=204.98ms med=599.96ms max=9.85s p(90)=5.9s  p(95)=6.89s
    iterations.....................: 18320  39.406525/s
    vus............................: 254    min=0            max=254
    vus_max........................: 800    min=800          max=800

    NETWORK
    data_received..................: 3.9 MB 8.4 kB/s
    data_sent......................: 7.8 MB 17 kB/s

running (07m44.9s), 000/800 VUs, 18320 complete and 255 interrupted iterations
order_submission_stress ✗ [  66% ] 250/800 VUs  06m33.1s/10m00.0s
time="2026-07-08T10:22:27Z" level=error msg="test run was aborted because k6 received a 'interrupt' signal"
```

`http_req_failed` 3.27%는 `create_order` 실패가 아니라 setup()의 register-or-login 폴백에서 나온 예상된 409(재실행 시 발생)가 포함된 전역 지표다 — `checks_failed` 0.00%(=`order accepted (status 200)` 체크가 18321/18321 모두 통과)가 실제 주문 제출 성공률이다.

### 중단 시점 Prometheus 스냅샷

| 지표 | 값 |
|---|---|
| CPU 사용률 | 93.3% (VU 157 시점, 3단계 진입 직후) → 77.5% (중단 직후, 하강 중) |
| 메모리 사용률 | 23.3% |
| `POST /orders` p95 (HTTP) | 9.09초 (VU 157 시점) |
| `order_pipeline_match_latency_seconds` p95 | 10초 (매칭엔진 큐잉+처리 지연) |
| `order_settlement_duration_seconds` p95 | 0.176초 |
| Go 런타임 goroutine 수 | 7 (중단 후 안정화) |
| Postgres 활성 커넥션 | 3 |

## 요약 테이블

| 구간 (경과 시간) | VU | 누적 완료 iteration | 1분당 처리량(대략) |
|---|---|---|---|
| 1m12s → 5m12s (4분) | 0 → 100 | 0 → 13,954 | ~3,489/분 |
| 5m12s → 6m12s (1분) | 100 → 150 | 13,954 → 15,687 | ~1,733/분 |
| 6m12s → 7m12s (1분) | 150 → 201 | 15,687 → 17,415 | ~1,728/분 |

| 항목 | 값 |
|---|---|
| 최종 VU (중단 시점) | 254 / 800 목표 |
| HTTP 요청 성공률(`create_order` 체크 기준) | 100% (0건 실패) |
| `http_req_duration` p95 (전체 구간 평균) | 6.41초 |
| `http_req_duration` max | 9.42초 |
| CPU 최고치 | 93.3% |
| 병목 관측 VU 구간 | 약 150~200 VU |

## 해석

1. **병목은 VU 150~200 구간에서 이미 나타났다.** VU가 100→150→201로 늘어나는 동안 분당 처리량은 3,489/분에서 1,733/분, 1,728/분으로 급락하며 사실상 정체됐다 — VU를 늘려도 처리량이 더 늘지 않는 전형적인 포화(saturation) 신호다.
2. **CPU 포화가 원인으로 보인다.** VU 157 시점에 CPU 사용률이 93%까지 올라간 반면, 메모리(23%)와 Postgres 커넥션(3개)은 전혀 압박받지 않았다. `order_settlement_duration_seconds` p95도 0.176초로 정산 자체는 여전히 빠르다.
3. **`order_pipeline_match_latency_seconds` p95(10초)가 HTTP 응답 지연(p95 9초)과 거의 같이 움직였다.** 이는 매칭엔진의 단일 고루틴 소비 루프가 CPU를 못 받아 `OrderCh`가 밀리면서, 그 큐잉 지연이 그대로 정산 이전 단계 전체 지연으로 이어졌다는 뜻이다. 즉 "매칭 로직 자체가 느려서"가 아니라 "CPU를 나눠 쓰는 다른 프로세스(특히 Go 앱 자체의 HTTP 처리와 Postgres, 그리고 Prometheus/Grafana/exporter들)와 경쟁하느라" 매칭 루프가 스케줄링을 못 받은 것으로 해석하는 게 더 정확하다.
4. **한 인스턴스에 앱+DB+전체 모니터링 스택을 몰아넣은 구성 자체가 한계를 앞당겼을 가능성이 크다.** `e2-medium`(2 vCPU)에 Go 앱, Postgres, Prometheus, Grafana, node_exporter, postgres_exporter가 모두 CPU를 나눠 쓰고 있었다. 다음 이터레이션에서 병목 원인을 더 정확히 분리하려면 모니터링 스택을 별도 인스턴스로 옮기거나, 서버 인스턴스 자체의 vCPU를 늘려 재측정해보는 것이 좋다.
5. **에러율은 0%였다** — 시스템이 크래시하거나 요청을 거부하지 않고, 응답이 느려지는 형태(graceful degradation)로 한계를 드러냈다. 이는 이 프로젝트 CLAUDE.md의 "목표 중심 실행" 관점에서 나쁘지 않은 신호다.
6. **이전 로컬 k6 기준선(`02-2026-07-06`)과 비교**: 로컬 기준선(VU 10~50)에서는 `create_order` p95가 26.74ms였다. 이번 GCP 스트레스 테스트는 VU 150 부근부터 이미 p95가 초 단위로 치솟았는데, 로컬 기준선이 VU 50 이하 저부하 구간만 다뤘다는 점을 감안하면 직접 비교보다는 "저부하에서는 문제없고, 특정 동시성 임계점을 넘으면 급격히 무너진다"는 정성적 결론으로 받아들이는 게 맞다.

## 범위 밖으로 남긴 것

- 병목을 실제로 고치는 것(모니터링 스택 분리, 인스턴스 확장, 매칭엔진 스케줄링 개선 등)은 이번 스코프가 아니다 — 이 결과를 근거로 다음 이터레이션에서 결정한다.
- 800 VU까지의 완주는 하지 않았다 (150~200 VU 구간에서 이미 명확한 병목 신호를 확인했으므로, `docs/gcp-stress-test-runbook.md`의 수동 중단 기준에 따라 종료).
