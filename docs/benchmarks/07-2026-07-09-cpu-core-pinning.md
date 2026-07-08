# 7번째 테스트 (2026-07-09): CPU 코어 핀닝 전/후 비교

## 커밋 해시

`1db00ff` (feat: 스트레스 환경 backend 컨테이너를 코어 0에 CPU 핀닝)

## 왜 이 테스트를 했는지

4번째 테스트(pprof)에서, 매칭 로직 자체는 CPU의 10% 미만만 쓰는데도 매칭 큐잉 지연이 p95 10초까지 벌어지는 걸 봤다. 이게 "매칭 고루틴이 Postgres/모니터링 스택과 CPU를 경쟁하느라 스케줄링을 못 받아서"라는 가설을 세우고, `docs/superpowers/specs/2026-07-09-cpu-core-pinning-design.md`에서 이를 실제로 검증하기로 했다 — backend 컨테이너를 코어 0에, Postgres+모니터링 스택을 코어 1에 고정해서 재측정했다.

## 왜 이 방식을 선택했는지

VM이 2 vCPU뿐이라 "매칭 전용 코어 + HTTP 전용 코어"로 더 세밀하게 나누면 Postgres/모니터링에 줄 코어가 없어진다. 그래서 첫 실험으로 컨테이너 단위 분리(backend 전체 vs 나머지 전체)를 먼저 시도했다 — docker-compose 설정만으로 가능하고, 바로 재측정해서 효과를 확인할 수 있어서다.

## 환경 (05번 테스트와 동일 조건, VM 사양 변경 없음)

- 서버 인스턴스: GCP `e2-medium`(2 vCPU, 4GB), 서울 리전
- 적용된 설정: `backend` → `cpuset: "0"`, `GOMAXPROCS=1` / `postgres`, `prometheus`, `grafana`, `node-exporter`, `postgres-exporter` → `cpuset: "1"`
- DB 커넥션 풀 설정(05번 테스트에서 적용한 것)은 그대로 유지된 상태

## 실행한 정확한 커맨드

```bash
k6 run -e BASE_URL=http://10.10.0.3:8080 -e DEV_TOOLS_TOKEN=<토큰> loadtest/order-submission-stress.js
```

## 핵심 결과: 05번 테스트(코어 핀닝 없음) 대비 비교

### 1. 같은 VU 구간(150) 누적 처리량 — 악화됨

| | 05번 (핀닝 없음) | 07번 (핀닝 적용) | 변화 |
|---|---|---|---|
| VU 150 시점 누적 iteration | 38,916 | 33,991 | **38,916 → 33,991 (약 12.7% 감소)** |

### 2. 최종 k6 요약 비교

| 지표 | 05번 (핀닝 없음) | 07번 (핀닝 적용) | 변화 |
|---|---|---|---|
| `http_req_duration` med(중앙값) | 115.75ms | 819.07ms | **115.75ms → 819.07ms (약 7배 악화)** |
| `http_req_duration` p95 | 7.07s | 7.45s | **7.07s → 7.45s (소폭 악화)** |
| 총 완료 iteration | 65,620 | 60,219 | **65,620 → 60,219 (감소)** |
| 처리량 | 96.65 iter/s | 88.84 iter/s | **96.65 → 88.84 (감소)** |
| 최대 도달 VU | 797 | 797 (완주는 동일) | 동일 |

### 3. VU 175 시점 스냅샷 (참고용, 05번엔 정확히 같은 VU 지점 데이터 없음)

| 지표 | 값 |
|---|---|
| CPU (코어 0, backend) | 64.7% |
| CPU (코어 1, Postgres+모니터링) | 72.7% — **backend보다 더 바쁨** |
| `POST /orders` p95 (이 순간) | 2.42s |
| `order_pipeline_match_latency_seconds` p95 | 10s (변화 없음) |

이 스냅샷만 보면 개선된 것처럼 보이지만, 위 1·2번의 누적/최종 지표는 반대로 악화를 가리킨다 — 아래 해석 참고.

## 원본 출력 (최종 k6 요약, 07번)

```
  █ TOTAL RESULTS 

    checks_total.......: 60219   88.83648/s
    checks_succeeded...: 100.00% 60219 out of 60219
    checks_failed......: 0.00%   0 out of 60219

    ✓ order accepted (status 200)

    HTTP
    http_req_duration..............: avg=1.9s  min=810.83µs med=819.07ms max=9.87s p(90)=6.14s p(95)=7.45s
      { expected_response:true }...: avg=1.93s min=2.82ms   med=857.48ms max=9.87s p(90)=6.18s p(95)=7.48s
    http_req_failed................: 1.27% 800 out of 62619
    http_reqs......................: 62619 92.377016/s

    EXECUTION
    iteration_duration.............: avg=2.33s min=203.96ms med=1.28s    max=10.3s p(90)=6.57s p(95)=7.87s
    iterations.....................: 60219 88.83648/s
    vus............................: 52      min=0            max=797
    vus_max........................: 800     min=800          max=800
```

### 램프업 타임라인 (30초 간격 샘플)

```
running (01m11.4s), 000/800 VUs, 0 complete and 0 interrupted iterations
running (01m41.4s), 012/800 VUs, 508 complete and 0 interrupted iterations
running (02m11.4s), 025/800 VUs, 2048 complete and 0 interrupted iterations
running (02m41.4s), 037/800 VUs, 4632 complete and 0 interrupted iterations
running (03m11.4s), 050/800 VUs, 8271 complete and 0 interrupted iterations
running (03m41.4s), 062/800 VUs, 12912 complete and 0 interrupted iterations
running (04m11.4s), 075/800 VUs, 18649 complete and 0 interrupted iterations
running (04m41.4s), 087/800 VUs, 25354 complete and 0 interrupted iterations
running (05m11.4s), 100/800 VUs, 28693 complete and 0 interrupted iterations
running (05m41.4s), 125/800 VUs, 31326 complete and 0 interrupted iterations
running (06m11.4s), 150/800 VUs, 33991 complete and 0 interrupted iterations
running (06m41.4s), 175/800 VUs, 36685 complete and 0 interrupted iterations
running (07m11.4s), 200/800 VUs, 39354 complete and 0 interrupted iterations
running (07m41.4s), 250/800 VUs, 41208 complete and 0 interrupted iterations
running (08m11.4s), 300/800 VUs, 43805 complete and 0 interrupted iterations
running (08m41.4s), 350/800 VUs, 46473 complete and 0 interrupted iterations
running (09m11.4s), 401/800 VUs, 49056 complete and 0 interrupted iterations
running (09m41.4s), 501/800 VUs, 51706 complete and 0 interrupted iterations
running (10m11.4s), 601/800 VUs, 54375 complete and 0 interrupted iterations
running (10m41.4s), 701/800 VUs, 56970 complete and 0 interrupted iterations
running (11m11.4s), 769/800 VUs, 59450 complete and 0 interrupted iterations
```

## 해석 (솔직하게: 가설이 틀렸을 가능성이 높다)

1. **전체적으로는 개선이 아니라 악화였다.** 같은 VU 구간 누적 처리량이 12.7% 줄었고, 응답시간 중앙값은 약 7배(115ms→819ms) 나빠졌다. 최종 처리량도 96.65→88.84 iter/s로 줄었다.
2. **가장 유력한 원인: backend가 쓸 수 있는 CPU 용량 자체가 줄었다.** 핀닝 전에는 backend 프로세스가 2개 vCPU를 모두 쓸 수 있었다(Postgres/모니터링과 경쟁은 했지만). 핀닝 후에는 backend가 **코어 1개로 제한**됐고, `GOMAXPROCS=1`까지 줘서 Go 런타임 자체도 동시에 1개의 OS 스레드로만 스케줄링한다. 즉 "다른 프로세스와 경쟁을 없앤 대가로, 원래 쓸 수 있었던 컴퓨팅 용량의 절반을 스스로 포기한 것"이 된다 — 격리로 얻은 이득보다 용량 축소로 잃은 손해가 더 컸던 것으로 보인다.
3. **코어 1(Postgres+모니터링)이 오히려 더 바빴다(72.7% vs backend 64.7%).** Prometheus+Grafana+2개의 exporter+Postgres를 전부 코어 1 하나에 몰아넣은 것도 그쪽에서 새로운 병목을 만들었을 가능성이 있다.
4. **`order_pipeline_match_latency_seconds` p95는 여전히 10초로 고정.** 이 값이 4~7번 테스트에서 한 번도 안 바뀌었다는 점은 주목할 만하다 — Prometheus 기본 히스토그램 버킷 경계가 10초에서 끊기는 구조라(`histogram_quantile`의 마지막 정규 버킷이 10초), 실제 값이 10초보다 훨씬 커도 표시상 10초로 보일 수 있다. 이 지표 자체의 해상도 문제일 가능성을 다음 조사에서 짚어야 한다.
5. **결론: 이번 방식(컨테이너 전체를 코어 하나에 통째로 고정)은 기각한다.** VM 사양을 안 늘리면서 "격리"를 얻으려면, 코어를 통째로 떼어주는 것보다 훨씬 정교한 방법(예: 매칭 고루틴만 별도 OS 스레드로 묶고, HTTP 핸들러는 여전히 양쪽 코어를 다 쓰게 하는 방식)이 필요해 보인다.

## 이력서/포트폴리오용 요약 문장 (검증된 사실만)

- "컨테이너 단위 CPU 코어 격리(cpuset)를 실험했으나, 격리로 얻은 이득보다 가용 컴퓨팅 용량 축소로 인한 손해가 더 커서 전체 처리량이 오히려 12.7% 감소하는 결과를 얻었다 — 가설을 기각하고 더 정교한 접근(고루틴 단위 핀닝)이 필요하다는 걸 확인했다."

## 다음 작업 제안

1. **이번 cpuset 변경을 되돌린다** (05번 테스트 상태로 복원) — 지금까지 확인된 가장 나은 설정은 "DB 커넥션 풀 튜닝만 적용, 코어 핀닝 없음"이다.
2. **`order_pipeline_match_latency_seconds` 히스토그램 버킷을 10초 이상으로 확장**해서, 이 지표가 진짜 10초에 수렴하는지 아니면 버킷 해상도 문제로 잘려 보이는지부터 확인한다.
3. 코어 핀닝을 다시 시도한다면, 컨테이너 전체가 아니라 **매칭 고루틴만** `runtime.LockOSThread()` + Linux `sched_setaffinity`로 묶고, HTTP 핸들러는 여전히 양쪽 코어를 다 쓰게 하는 훨씬 정교한 방식을 검토한다 (설계 스펙의 "범위 밖" 항목이었던 방식).

## 범위 밖 (Out of Scope)

- 위 "다음 작업 제안"의 실제 구현 — 별도 브레인스토밍/계획으로 진행.
- VM 사양 변경 — 이 프로젝트의 목표에 따라 계속 배제.
