# 13번째 테스트 (2026-07-10): Postgres 별도 인스턴스 분리 전/후 비교

## 커밋 해시

`1f7ac95` (fix(infra): allow_postgres 방화벽 규칙에 postgres-exporter 포트(9187) 추가) — Postgres 분리 관련 전체 변경은 `486de91`(Terraform DB 인스턴스+방화벽), `3ed5d4c`(docker-compose.db.yml), `dc7fc51`(스트레스 compose 분리), `e524037`(Prometheus 타겟 갱신), `1f7ac95`(방화벽 포트 추가 수정)로 이어진다.

## 왜 이 테스트를 했는지

`12-2026-07-10-stress-test-vu-ceiling-extension.md`에서 CPU가 VU 700~800부터 90%로 포화되는 걸 확인했다. backend와 Postgres가 같은 4코어를 나눠 쓰는 게 원인일 수 있다는 가설을 세우고, Postgres를 별도 GCP 인스턴스(`e2-medium`, 2 vCPU)로 분리해 실측으로 검증했다.

## 왜 이 방식을 선택했는지

`docs/superpowers/specs/2026-07-10-postgres-instance-separation-design.md`에서 결정한 대로, 서버/부하생성기와 같은 Terraform 패턴으로 DB 전용 인스턴스와 방화벽 규칙(서버 내부 IP로만 5432/9187 허용)을 추가했다. `docker-compose.stress.yml`에서 Postgres 관련 서비스를 빼고, 별도 `docker-compose.db.yml`을 DB 인스턴스에 배포했다.

## 환경

- 서버: `e2-highcpu-4`(4 vCPU, 4GB) — 그대로
- **DB(신규)**: `e2-medium`(2 vCPU, 4GB), 내부 IP `10.10.0.4`
- 부하생성기: `e2-standard-2`(2 vCPU, 8GB) — 그대로
- k6 `STRESS_STAGES`: 12번과 동일(800→1600→3000)

## 실행한 정확한 커맨드

```bash
k6 run -e BASE_URL=http://10.10.0.3:8080 -e DEV_TOOLS_TOKEN=<토큰> loadtest/order-submission-stress.js
```

## 배포 중 발견한 문제 (그 자체로 유의미한 기록)

1차 배포 직후 Prometheus의 `postgres-exporter` 타겟이 `down`(연결 타임아웃)이었다 — 방화벽 규칙(`allow_postgres`)이 Postgres 포트(5432)만 열어두고 postgres-exporter의 메트릭 포트(9187)는 빼먹었기 때문이다. `terraform apply`로 9187 포트를 추가해서 해결했다(`1f7ac95`).

## 핵심 결과: 부분적 성공 + 새로운 병목 발견 (솔직하게 기록)

### 1. Backend CPU는 확실히 좋아졌다 — 가설의 절반은 맞았다

| | 12번(Postgres 같은 인스턴스) | 13번(Postgres 분리) |
|---|---|---|
| VU~800 구간 CPU | **90%로 포화** | **24~47% (평균 30% 근처)** |

**backend가 더 이상 Postgres와 CPU를 두고 경쟁하지 않는다는 게 명확히 확인됐다.**

### 2. 하지만 전체 성능은 오히려 나빠졌다

| 지표 | 12번(분리 전) | 13번(분리 후) | 변화 |
|---|---|---|---|
| `http_req_duration` p95 | 7.05s | **9.52s** | **악화** |
| 총 완료 iteration | 248,032 | **201,739** | **약 19% 감소** |
| 처리량 | 220.88 iter/s | **178.25 iter/s** | **약 19% 감소** |
| `http_req_failed` | 1.16% | **0.00%** | **개선** |
| 매칭 지연 p95 최댓값 | 14.7s | **14.75s** | 거의 동일 |

### 3. 원인: Postgres 자체가 새 인스턴스에서 CPU 부족

배포 직후 DB 인스턴스에 직접 SSH로 접속해서 확인한 결과:

```
$ uptime
06:16:00 up 32 min, load average: 12.86, 9.23, 4.36    ← 2 vCPU인데 부하가 12.86!

$ docker stats --no-stream
goexchange-db-postgres   CPU 389.52%   ← 2코어(=400%)를 거의 다 씀
```

**Postgres가 자기 코어(2개)를 거의 다 쓰고 있었다.** `execution`(정산) 채널이 이 시점에 다시 1024까지 포화됐고, 매칭 지연도 그때부터 치솟기 시작했다.

## 원본 출력 (최종 k6 요약, 13번)

```
  █ TOTAL RESULTS 

    checks_total.......: 201739  178.254644/s
    checks_succeeded...: 100.00% 201739 out of 201739
    checks_failed......: 0.00%   0 out of 201739

    HTTP
    http_req_duration..............: avg=2.39s min=3.91ms   med=918.6ms max=11.25s p(90)=8.2s  p(95)=9.52s
    http_req_failed................: 0.00%  0 out of 207739
    http_reqs......................: 207739 183.556187/s

    EXECUTION
    iterations.....................: 201739 178.254644/s
    vus............................: 269    min=0           max=2996
```

## 해석

1. **가설의 절반은 정확히 맞았다.** backend와 Postgres가 CPU를 나눠 쓰는 게 문제였다는 부분은 실측으로 확인됐다 — backend CPU가 90%→30%로 확실히 줄었다.
2. **하지만 전체 시스템 관점에선 오히려 손해였다.** Postgres를 분리하면서 2 vCPU짜리 작은 인스턴스에 몰아넣었더니, 이번엔 Postgres 자신이 그 2코어로 감당을 못 했다. 게다가 backend↔Postgres 통신이 이제 도커 내부 네트워크가 아니라 실제 VM 간 네트워크를 거치게 됐다는 점도 약간의 지연을 더했을 수 있다(정확한 기여도는 이번 테스트로는 분리 측정 못함).
3. **07번(CPU 핀닝) 때와 비슷한 교훈이다.** "격리하면 좋아질 것"이라는 직관이 실제로는 "전체 파이를 나누는 방식만 바꾼 것"이었을 수 있다 — 이번엔 격리 자체는 잘 됐지만(backend는 확실히 여유로워짐), 그 대가로 Postgres가 받은 리소스(2 vCPU)가 원래 감당하던 부하보다 부족했다.
4. **`http_req_failed`가 0%로 개선된 것은 진짜 성과다.** 이건 04~12번에서 계속 남아있던 "원인 미상의 소량 네트워크 실패"가 이번엔 전혀 발생하지 않았다는 뜻이라, Postgres 분리가 이 문제엔 도움이 됐을 가능성이 있다(우연일 수도 있어 추가 검증 필요).

## 다음 작업 제안 (결정 필요)

1. **DB 인스턴스 CPU를 늘려서 재시도**: `e2-medium`(2 vCPU) → `e2-standard-4`나 `e2-highcpu-4`(4 vCPU) 등으로 올려서, backend 분리 이득과 Postgres 자체 용량을 동시에 확보했을 때 어떻게 되는지 확인.
2. **분리를 되돌린다**: 07번처럼 이번 방향이 순효과가 없다고 판단하면, Postgres를 다시 backend와 같은 인스턴스로 합친다.
3. **DB 쿼리 자체 최적화**: 인스턴스 사양을 더 올리기 전에, 실제로 어떤 쿼리가 Postgres CPU를 많이 쓰는지(pg_stat_statements 등) 먼저 조사하는 방향도 있다.

## 범위 밖 (Out of Scope)

- 위 "다음 작업 제안"의 실제 실행 — 사용자와 상의해서 결정.
- 네트워크 홉 추가로 인한 지연 기여도의 정밀 측정 — 이번 테스트로는 Postgres CPU 부족과 네트워크 지연을 분리해서 측정하지 못했다.