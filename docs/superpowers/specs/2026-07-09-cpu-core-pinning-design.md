# 백엔드 컨테이너 CPU 코어 핀닝 설계

## 배경 (왜 필요한가)

`04-2026-07-08-matching-engine-cpu-profiling.md`의 pprof 조사와 `06-2026-07-09-matching-engine-pure-tps-benchmark.md`의 순수 벤치마크에서, 매칭 로직 자체는 전체 CPU의 10% 미만만 쓰고 초당 수십만 건을 처리할 수 있을 만큼 빠르다는 게 확인됐다. 그런데 `03-2026-07-08-gcp-stress-test.md`에서는 CPU가 포화된 상황에서 `order_pipeline_match_latency_seconds`(매칭 큐잉+처리 지연)가 p95 10초까지 벌어졌다. 이건 매칭 로직이 느려서가 아니라, **매칭엔진 고루틴이 다른 작업(HTTP 핸들러, Postgres, Prometheus, Grafana 등)과 같은 CPU 자원을 놓고 경쟁하느라 실행될 차례를 못 받는 것**으로 해석된다.

이번 작업은 이 가설을 실제로 검증한다 — backend 프로세스를 다른 프로세스(Postgres, 모니터링 스택)로부터 CPU 레벨에서 분리하면, 매칭 큐잉 지연이 실제로 줄어드는지 확인한다.

이 작업은 **VM 사양을 바꾸지 않는다**는 이 프로젝트의 성능 개선 원칙([[goexchange-performance-goal]])을 그대로 따른다 — 코어를 늘리는 게 아니라, 이미 있는 2개 코어를 어떻게 나눠 쓸지를 우리가 직접 통제하는 방법이다.

## 왜 이 방식을 선택했는지

CPU 핀닝은 두 갈래로 할 수 있다: (1) 컨테이너/프로세스 단위로 cpuset을 나누는 것(OS 레벨, 코드 변경 없음), (2) `runtime.LockOSThread()` + Linux `sched_setaffinity`로 매칭 고루틴 하나만 진짜로 전용 코어에 고정하는 것(Go 코드 변경 필요, Linux 전용).

이번엔 (1) 컨테이너 단위 분리를 먼저 시도하기로 했다. VM이 2 vCPU뿐이라 "매칭 전용 코어 1개 + HTTP 전용 코어 1개"로 더 세밀하게 나누면 Postgres/모니터링에 줄 코어가 아예 없어진다 — 이건 더 복잡하고 리스크가 큰 트레이드오프다. 컨테이너 단위 분리는 docker-compose 설정만으로 가능하고, 바로 재측정해서 효과를 확인할 수 있어 첫 실험으로 적합하다. 이걸로 효과가 있는지 없는지 확인한 뒤, 필요하면 더 정교한 (2)번 방식을 별도로 검토한다.

## 범위

- `docker-compose.stress.yml`에 `cpuset` 설정을 추가한다 — `backend`는 코어 0, `postgres`/`prometheus`/`grafana`/`node-exporter`/`postgres-exporter`는 코어 1을 공유한다.
- `backend` 서비스에 `GOMAXPROCS=1` 환경변수를 추가해 Go 런타임이 실제 가용 코어 수에 맞게 스케줄링하게 한다.
- Go 코드 변경(`runtime.LockOSThread()` 등)은 이번 스코프가 아니다 — 컨테이너 단위 분리 효과를 먼저 확인한다.
- 재배포/재측정/결과 문서화는 이 스펙 문서화 이후 사용자와 직접 진행한다(코드/설정 준비까지만 계획에 담는다).

## 아키텍처

### 1. `docker-compose.stress.yml` cpuset 설정

`backend` 서비스에 추가:
```yaml
    cpuset: "0"
```

`postgres`, `prometheus`, `grafana`, `node-exporter`, `postgres-exporter` 서비스에 각각 추가:
```yaml
    cpuset: "1"
```

### 2. `GOMAXPROCS` 명시

`backend` 서비스의 `environment` 블록에 추가:
```yaml
      GOMAXPROCS: "1"
```

`cpuset: "0"`은 백엔드 프로세스가 실제로 실행될 수 있는 코어를 하나로 제한하지만, Go 런타임 자체는(버전에 따라) 호스트의 전체 코어 수를 보고 더 많은 OS 스레드를 만들려고 시도할 수 있다. `GOMAXPROCS=1`을 명시해서 Go 스케줄러가 처음부터 "코어 1개만 있다"고 알고 동작하게 한다.

### 3. 검증 절차 (사용자와 직접 진행, 이 계획의 범위 밖)

1. 서버 인스턴스에 재배포, `docker compose -f docker-compose.stress.yml up -d --build`로 재기동.
2. k6 스트레스 테스트를 3/5번 테스트와 동일한 조건(같은 스크립트, 같은 VU 프로파일)으로 재실행.
3. 같은 VU 구간(약 150)에서 Grafana의 CPU, `POST /orders` p95, `order_pipeline_match_latency_seconds` p95를 05번 문서(튜닝 후 기준선)와 비교.
4. **반드시 확인할 것: 병목이 이동했는지.** 매칭 지연은 줄었는데 API 응답(HTTP p95)이나 Postgres 커넥션 대기가 오히려 늘었다면, "코어 1개를 Postgres+모니터링이 나눠 쓰게 된 것"이 새로운 병목이 된 것이므로 그것도 결과에 그대로 기록한다.
5. 결과를 `docs/benchmarks/07-YYYY-MM-DD-cpu-core-pinning.md`에 기록 — 05번 문서를 기준선으로 삼아 전/후 비교.

## 성공 기준

- `docker compose -f docker-compose.stress.yml config`가 에러 없이 통과한다 (cpuset 문법 검증).
- 재측정 결과가 (개선이든 아니든, 병목 이동이든) 있는 그대로 `docs/benchmarks/07-...md`에 기록된다 — 수치를 과장하지 않는다.

## 범위 밖 (Out of Scope)

- `runtime.LockOSThread()` + `sched_setaffinity` 기반의 진짜 고루틴 단위 핀닝 — 이번 실험 결과를 보고 필요성을 재평가한다.
- 로컬/프로덕션(`docker-compose.yml`, `docker-compose.deploy.yml`, `docker-compose.prod.yml`) 반영 — 이번엔 스트레스 환경에만 적용한다.
- VM 사양 변경 — 이 프로젝트의 목표에 따라 계속 배제한다.
