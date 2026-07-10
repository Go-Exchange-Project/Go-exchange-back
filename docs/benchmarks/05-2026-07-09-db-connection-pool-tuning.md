# 5번째 테스트 (2026-07-09): DB 커넥션 풀 튜닝 전/후 비교

## 커밋 해시

`85e2bf8` (feat: 스트레스 환경에 DB 커넥션 풀 설정 반영)

## 왜 이 테스트를 했는지

`04-2026-07-08-matching-engine-cpu-profiling.md`의 pprof 조사에서, DB 커넥션 풀이 전혀 설정되지 않아 매 요청마다 새 커넥션을 맺고 SCRAM-SHA-256 인증(PBKDF2)을 반복하는 데 CPU의 37.9%가 소모되고 있다는 걸 확인했다. `docs/superpowers/plans/2026-07-08-db-connection-pool-tuning.md`로 이 문제를 실제로 고쳤고(`SetMaxOpenConns=25`, `SetMaxIdleConns=25`, `SetConnMaxLifetime=30m`), 이번 테스트는 그 수정이 **VM 사양을 전혀 바꾸지 않은 상태에서** 실제로 얼마나 개선됐는지 3번째 테스트(`03-2026-07-08-gcp-stress-test.md`)와 동일한 조건으로 재측정한 것이다.

## 환경 (3번째 테스트와 동일, VM 사양 변경 없음)

- 서버 인스턴스: GCP `e2-medium`(2 vCPU, 4GB), 서울 리전
- 부하생성 인스턴스: GCP `e2-small`(2 vCPU, 2GB)
- k6 스크립트: `loadtest/order-submission-stress.js` (50→100→200→400→800 VU, 각 2분)
- 적용된 설정: `GOEXCHANGE_DB_MAX_OPEN_CONNS=25`, `GOEXCHANGE_DB_MAX_IDLE_CONNS=25`, `GOEXCHANGE_DB_CONN_MAX_LIFETIME=30m`

## 실행한 정확한 커맨드

```bash
k6 run -e BASE_URL=http://10.10.0.3:8080 -e DEV_TOOLS_TOKEN=<토큰> loadtest/order-submission-stress.js
```

## 핵심 결과: 전/후 비교

### 1. 가장 중요한 차이: 테스트 완주 여부

| | 이전 (03번, 튜닝 전) | 이번 (05번, 튜닝 후) |
|---|---|---|
| 테스트 종료 방식 | **VU 254에서 수동 중단** (병목이 너무 심해서 계속할 이유가 없었음) | **800 VU까지 5단계 전체 완주** (10분 시나리오 자연 종료) |
| 최대 도달 VU | 254 / 800 | 797 / 800 |
| 총 완료 iteration | 18,320 | 65,620 (3.6배) |
| 처리량 | 39.4 iterations/s | 96.7 iterations/s (2.5배) |

**튜닝 전에는 VU 250 근처에서 시스템이 사실상 무너져서 테스트를 끝까지 돌릴 이유가 없었는데, 튜닝 후에는 목표했던 800 VU까지 완주했다.** 이게 이번 튜닝의 가장 확실한 증거다.

### 2. 같은 VU 구간(약 150)에서의 수치 비교

| 지표 | 튜닝 전 (VU 157) | 튜닝 후 (VU 153) | 변화 |
|---|---|---|---|
| CPU 사용률 | 93.6% | 85.6% | **93.6% → 85.6%** (소폭 개선) |
| `POST /orders` p95 (HTTP) | 9.09s | 4.30s | **9.09s → 4.30s** (약 53% 감소) |
| `order_pipeline_match_latency_seconds` p95 | 10s | 10s | **10s → 10s** (변화 없음) |
| `order_settlement_duration_seconds` p95 | 0.176s | 0.179s | **0.176s → 0.179s** (변화 없음, 원래도 문제 없었음) |
| 같은 시점 누적 처리 iteration | 15,943 | 38,916 | **15,943 → 38,916** (2.4배) |

### 3. VU를 더 밀어붙였을 때 (튜닝 후에만 관측 가능했던 구간)

| 지표 | VU 630 (10m20s 시점) |
|---|---|
| CPU 사용률 | 79.5% |
| `POST /orders` p95 | 9.64s |

**튜닝 전에는 VU 150~200에서 나타났던 "p95 9초대" 수준의 병목이, 튜닝 후에는 VU 600대까지 밀려서야 다시 나타났다.** 같은 "체감 한계"가 약 4배 더 높은 동시 사용자 수에서 발생한 셈이다.

### 4. 커넥션 풀 자체의 동작 확인

| 지표 (VU 153 시점) | 값 |
|---|---|
| `go_sql_open_connections` | 25 (설정한 최대치까지 차 있음 — 재사용 중) |
| `go_sql_idle_connections` | 21 |
| `go_sql_in_use_connections` | 4 |
| `go_sql_wait_count_total` 증가율 | 0.036/s (거의 대기 없음) |
| Postgres 활성 커넥션 (`pg_stat_activity_count`) | 27 |

이전 테스트에서는 커넥션이 계속 새로 열리고 닫히길 반복했는데, 이번엔 25개로 안정되어 재사용되고 있는 게 직접 확인된다.

### 5. 테스트 종료 후 (쿨다운) 스냅샷

| 지표 | 값 |
|---|---|
| CPU 사용률 | 49.2% |
| Go 고루틴 수 | 13 |
| GC 사이클 누적 횟수 (`go_gc_duration_seconds_count`) | 2,254 |

## 최종 k6 요약 (원본, 가공 없음)

```
  █ TOTAL RESULTS 

    checks_total.......: 65620   96.654285/s
    checks_succeeded...: 100.00% 65620 out of 65620
    checks_failed......: 0.00%   0 out of 65620

    ✓ order accepted (status 200)

    HTTP
    http_req_duration..............: avg=1.72s min=874.68µs med=115.75ms max=8.88s p(90)=5.81s p(95)=7.07s
      { expected_response:true }...: avg=1.74s min=2.77ms   med=128.41ms max=8.88s p(90)=5.87s p(95)=7.08s
    http_req_failed................: 1.17% 800 out of 68020
    http_reqs......................: 68020 100.189339/s

    EXECUTION
    iteration_duration.............: avg=2.13s min=204.58ms med=539.02ms max=9.31s p(90)=6.38s p(95)=7.44s
    iterations.....................: 65620 96.654285/s
    vus............................: 44      min=0            max=797
    vus_max........................: 800     min=800          max=800

    NETWORK
    data_received..................: 12 MB 18 kB/s
    data_sent......................: 26 MB 39 kB/s
```

`http_req_failed` 1.17%는 (이전과 마찬가지로) setup()의 register-or-login 폴백에서 나온 예상된 409를 포함한 전역 지표다. `checks_failed` 0.00%(`order accepted` 체크 65620/65620 통과)가 실제 주문 제출 성공률이다.

### 램프업 타임라인 (30초 간격 샘플, 튜닝 후)

```
running (01m12.4s), 000/800 VUs, 0 complete and 0 interrupted iterations
running (01m42.4s), 012/800 VUs, 481 complete and 0 interrupted iterations
running (02m12.4s), 025/800 VUs, 2012 complete and 0 interrupted iterations
running (02m42.4s), 037/800 VUs, 4592 complete and 0 interrupted iterations
running (03m12.4s), 050/800 VUs, 8259 complete and 0 interrupted iterations
running (03m42.4s), 062/800 VUs, 12895 complete and 0 interrupted iterations
running (04m12.4s), 075/800 VUs, 18567 complete and 0 interrupted iterations
running (04m42.4s), 087/800 VUs, 25316 complete and 0 interrupted iterations
running (05m12.4s), 100/800 VUs, 32397 complete and 0 interrupted iterations
running (05m42.4s), 125/800 VUs, 36149 complete and 0 interrupted iterations
running (06m12.4s), 150/800 VUs, 38916 complete and 0 interrupted iterations
running (06m42.4s), 175/800 VUs, 40488 complete and 0 interrupted iterations
running (07m12.4s), 200/800 VUs, 43238 complete and 0 interrupted iterations
running (07m42.4s), 250/800 VUs, 45978 complete and 0 interrupted iterations
running (08m12.4s), 300/800 VUs, 48563 complete and 0 interrupted iterations
running (08m42.4s), 350/800 VUs, 51314 complete and 0 interrupted iterations
running (09m12.4s), 400/800 VUs, 54013 complete and 0 interrupted iterations
running (09m42.4s), 500/800 VUs, 56761 complete and 0 interrupted iterations
running (10m12.4s), 600/800 VUs, 59381 complete and 0 interrupted iterations
running (10m42.4s), 700/800 VUs, 62147 complete and 0 interrupted iterations
running (11m12.4s), 796/800 VUs, 64824 complete and 0 interrupted iterations
```

## 해석

1. **가설이 맞았다: DB 커넥션 풀 미설정이 병목의 핵심 원인이었다.** `MaxOpenConns=25`/`MaxIdleConns=25`로 커넥션을 재사용하게 한 것만으로, 같은 하드웨어에서 시스템이 버틸 수 있는 부하가 최소 4배(VU 150→600대) 늘었다.
2. **다만 "CPU 사용률 자체"는 크게 줄지 않았다(93.6%→85.6%).** 이건 나쁜 신호가 아니라, 예전엔 CPU의 상당 부분(37.9%)이 SCRAM 인증이라는 낭비성 작업에 쓰였는데, 이제 그 자리를 실제 유효한 작업(더 많은 주문 처리)이 채웠다는 뜻이다. 같은 VU에서 처리한 iteration 수가 2.4배 늘어난 게 그 증거다.
3. **`order_pipeline_match_latency_seconds` p95(10초)가 전혀 개선되지 않았다.** 이는 매칭엔진의 큐잉 지연이 DB 커넥션 문제와는 별개의 원인(CPU 스케줄링 경쟁 자체)에서 온다는 걸 다시 확인해준다 — CPU가 여전히 85%+ 로 거의 포화 상태이기 때문에, 단일 매칭 고루틴은 여전히 스케줄링을 잘 못 받는다.
4. **다음 병목은 순수 CPU 한계로 보인다.** VM 사양을 안 바꾼다는 목표를 유지한다면, 다음으로 시도해볼 만한 방향은: (a) 스냅샷 브로드캐스트처럼 매 주문마다 반복되는 부수 작업을 줄이는 코드 최적화, (b) 모니터링 스택(Prometheus/Grafana/exporter들)을 별도 프로세스로 옮기지 않고도 리소스 사용을 줄이는 방법, (c) 애초에 CPU를 많이 쓰는 부분(JSON 직렬화, decimal 연산 등)을 다시 pprof로 확인.

## 다음 작업 제안

- 매칭엔진 파이프라인 지연(여전히 p95 10초)의 정확한 원인을 다시 pprof로 확인 — 이번엔 CPU가 85%대인 상황에서 어떤 함수가 상위를 차지하는지 재프로파일링.
- ulimit/nofile 등 OS 레벨 튜닝 검토 (VU 800 근처에서 `http_req_failed`가 소폭 존재 — 재시도/타임아웃 관련 원인인지 확인).
- VM 사양은 그대로 유지한다는 목표 하에, 코드 레벨 최적화(스냅샷 브로드캐스트 최적화 등)를 다음 우선순위로 검토.

## 범위 밖 (Out of Scope)

- 위에서 제안한 추가 최적화의 실제 구현 — 별도 브레인스토밍/계획으로 진행.
- VM 사양 변경 — 이번 프로젝트의 목표(고정된 인프라에서 최대 성능)에 따라 계속 배제.
