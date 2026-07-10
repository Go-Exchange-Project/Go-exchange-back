# 15번째 테스트 (2026-07-10): DB 인스턴스 CPU 증설 대조군 실험

## 커밋 해시

`a8a1762` (docs: DB 인스턴스 CPU 증설 대조군 실험 설계 문서 추가) — 이 테스트 자체는 인프라(GCP 인스턴스 리사이즈)만 바꿔서 진행했다. 애플리케이션 코드는 `7e287e9`(pg_stat_statements 프리로드) 이후 변경 없음.

## 왜 이 테스트를 했는지

`14-2026-07-10-pg-stat-statements-investigation.md`에서 Postgres CPU 포화(389%)의 원인이 느린 쿼리가 아니라 정산 트랜잭션당 쿼리 왕복량이었음을 확인했다. 코드 리팩터링(리스크 있음)보다 먼저 DB 인스턴스의 CPU를 늘려서(`e2-medium` 2 vCPU → `e2-highcpu-4` 4 vCPU) 이 순수 용량 문제가 해소되는지 실측했다.

## 왜 이 방식을 선택했는지

`docs/superpowers/specs/2026-07-10-db-cpu-scaling-control-design.md`에서 결정한 대로, 07/11번 테스트와 같은 절차(`gcloud`로 일시 리사이즈 → 실측 → 결과에 따라 Terraform 반영/되돌림)를 따랐다.

## 환경

- 서버: `e2-highcpu-4`(4 vCPU, 4GB) — 그대로
- **DB**: `e2-medium`(2 vCPU) → **`e2-highcpu-4`(4 vCPU, 4GB)** — 이번 실험 대상
- 부하생성기: `e2-standard-2`(2 vCPU, 8GB) — 그대로
- 리사이즈 후 재부팅 시 Postgres 컨테이너가 자동 기동되지 않는 걸 발견(재시작 정책 미설정) — 수동으로 `docker compose up -d`로 재기동. (별도 개선 과제로 기록)

## 실행한 정확한 커맨드

```bash
gcloud compute instances stop goexchange-stress-db --zone=asia-northeast3-a
gcloud compute instances set-machine-type goexchange-stress-db --zone=asia-northeast3-a --machine-type=e2-highcpu-4
gcloud compute instances start goexchange-stress-db --zone=asia-northeast3-a
# (컨테이너 수동 재기동 후)
k6 run -e BASE_URL=http://10.10.0.3:8080 -e DEV_TOOLS_TOKEN=<토큰> loadtest/order-submission-stress.js
```

## 핵심 결과: 명확하고 압도적인 개선

### 1. 최종 k6 요약 — 13번(분리 직후, 2vCPU DB)뿐 아니라 12번(분리 전)도 넘어섰다

| 지표 | 12번(분리 전) | 13번(분리, 2vCPU DB) | 15번(분리, **4vCPU DB**) | 변화(13→15) |
|---|---|---|---|---|
| 총 완료 iteration | 248,032 | 201,739 | **492,751** | **약 2.44배** |
| 처리량 | 220.88 iter/s | 178.25 iter/s | **438.04 iter/s** | **약 2.46배** |
| `http_req_duration` p95 | 7.05s | 9.52s | **2.70s** | **약 72% 개선** |
| `http_req_duration` max | - | 11.25s | **3.50s** | **개선** |
| `http_req_failed` | 1.16% | 0.00% | 0.59% | 약간 증가(여전히 낮음) |

**15번이 12번(Postgres 분리를 아예 안 했던 최초 상태)보다도 처리량 2배, p95 62% 개선이다 — "분리 + 양쪽 다 충분한 CPU"가 지금까지 시도한 모든 조합 중 최고 결과다.**

### 2. Postgres 자체의 여유도 뚜렷이 늘었다

| | 13번(2vCPU) | 15번(4vCPU) |
|---|---|---|
| Postgres 컨테이너 CPU (부하 중 관측) | 389.52% (2코어=200%를 훨씬 초과, 스로틀링 유발) | 318.85% (4코어=400% 중 80%, 여유 있음) |
| DB 인스턴스 load average | 12.86 (2코어 대비 6.4배) | 10.02 (4코어 대비 2.5배) |
| 부하 종료 후 load average | - | 2.49로 빠르게 안정화 |

### 3. `pg_stat_statements` — 처리량이 늘었는데도 평균 실행시간은 오히려 더 빨라졌다

| 쿼리 | 14번(2vCPU) 평균 | 15번(4vCPU) 평균 |
|---|---|---|
| 지갑 조회(1위) | 0.53ms | **0.12ms** |
| 주문 갱신 | 1.03ms | **0.32ms** |

호출 횟수는 훨씬 늘었는데(처리량이 2.4배가 됐으니 당연) 개별 쿼리 평균 실행시간은 더 짧아졌다 — CPU 코어 경합이 줄어서 각 쿼리가 대기 없이 더 빨리 끝난다는 뜻이다.

## 원본 출력 (최종 k6 요약, 15번)

```
  █ TOTAL RESULTS 

    checks_total.......: 492751  438.044665/s
    checks_succeeded...: 100.00% 492751 out of 492751
    checks_failed......: 0.00%   0 out of 492751

    HTTP
    http_req_duration..............: avg=776.92ms min=1.28ms   med=401.45ms max=3.5s  p(90)=2.32s p(95)=2.7s 
    http_req_failed................: 0.59%  3000 out of 501751
    http_reqs......................: 501751 446.045464/s

    EXECUTION
    iterations.....................: 492751 438.044665/s
    vus............................: 671    min=0              max=2994
```

## 해석

1. **가설이 정확히 맞았다.** 14번에서 "쿼리 자체는 빠른데 왕복 횟수가 많아서 CPU가 부족하다"고 결론지었는데, 코드를 전혀 안 건드리고 CPU만 늘렸더니 문제가 크게 해소됐다 — 순수 용량 문제였다는 게 재확인됐다.
2. **13번의 실패는 "분리 자체가 잘못"이 아니라 "분리 후 준 자원이 부족"이었다.** 13번에서 "병목을 옮긴 것"이라고 결론지었던 게 맞았지만, 그 병목(Postgres CPU 부족)을 해소하니 분리 구조 자체의 장점(backend가 Postgres와 안 싸움)이 제대로 발휘됐다.
3. **07번(CPU 핀닝)과 이번은 근본적으로 다른 종류의 "자원 분리"였다.** 07번은 "같은 파이를 억지로 나눈 것"(총 코어 수 그대로, 나눠 쓰기만 바꿈)이라 실패했지만, 이번(Postgres 분리+양쪽 증설)은 "파이 자체를 키운 것"(총 코어 수가 늘어남)이라 성공했다. 격리가 문제가 아니라 격리 후에 준 총 자원의 양이 핵심이었다.
4. **판단: 유지한다.** 개선 폭이 압도적이라 `e2-highcpu-4`를 유지하기로 결정 — Terraform 반영.

## 이력서/포트폴리오용 요약 문장

- "정산 트랜잭션당 쿼리 왕복량이 많다는 근본 원인을 pg_stat_statements로 규명한 뒤, 코드 리팩터링 없이 DB 인스턴스 CPU만 증설(2→4 vCPU)해 처리량을 2.46배, p95 응답시간을 72% 개선했다. Postgres 분리(13번)가 처음엔 오히려 손해였지만, 분리 후 양쪽 모두에 충분한 CPU를 준 뒤에는 분리 이전보다도 2배 나은 성능을 확인해, 아키텍처 변경의 효과가 자원 배분과 맞물려 있다는 걸 정량적으로 규명했다."

## 다음 작업 제안

1. Postgres 컨테이너에 `restart: unless-stopped` 정책 추가 — 이번 실험 중 재부팅 후 컨테이너가 자동 기동 안 되는 걸 발견했다.
2. `http_req_failed`가 0.59%로 소폭 나타난 원인(04~15번에서 계속 소량 관측된 문제)을 조사.
3. VU 3,000 최상단 구간(이번 테스트에서 마지막 VU까지는 다 안 봄)에서 추가 병목이 있는지 재확인.

## 범위 밖 (Out of Scope)

- 위 "다음 작업 제안"의 실제 실행 — 별도 브레인스토밍.
- 정산 트랜잭션 리팩터링 — 이번 CPU 증설로 충분히 해소됐다고 판단해 이번엔 보류.
