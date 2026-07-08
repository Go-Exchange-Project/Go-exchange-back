# 백엔드 컨테이너 CPU 코어 핀닝 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 매칭엔진 고루틴이 Postgres/모니터링 스택과 CPU를 경쟁해서 큐잉 지연이 벌어진다는 가설을 검증하기 위해, `docker-compose.stress.yml`에서 `backend` 컨테이너를 코어 0에, 나머지 서비스를 코어 1에 고정한다.

**Architecture:** docker-compose의 `cpuset` 옵션으로 컨테이너별 CPU 코어를 제한하고, `backend`에는 `GOMAXPROCS=1`을 명시해 Go 런타임이 실제 가용 코어 수에 맞게 동작하게 한다. Go 코드 변경은 없다.

**Tech Stack:** Docker Compose `cpuset` 옵션, Go `GOMAXPROCS` 환경변수.

## Global Constraints

- `backend` 서비스: `cpuset: "0"`, `GOMAXPROCS: "1"`.
- `postgres`, `prometheus`, `grafana`, `node-exporter`, `postgres-exporter` 서비스: `cpuset: "1"`.
- 이번엔 `docker-compose.stress.yml`에만 적용한다 — 다른 compose 파일(`docker-compose.yml`, `docker-compose.deploy.yml`, `docker-compose.prod.yml`)은 건드리지 않는다.
- 실제 GCP 재배포, k6 재실행, 결과 문서화(`docs/benchmarks/07-...md`)는 이 계획의 범위 밖이다.

---

### Task 1: `docker-compose.stress.yml`에 cpuset 적용

**Files:**
- Modify: `docker-compose.stress.yml`

**Interfaces:**
- 없음 (설정 파일만 변경, 다른 태스크가 이어지지 않음 — 단일 태스크 계획)

- [ ] **Step 1: `postgres` 서비스에 cpuset 추가**

`docker-compose.stress.yml`의 `postgres` 서비스, 현재:

```yaml
  postgres:
    image: postgres:18-alpine
    container_name: goexchange-stress-postgres
    environment:
```

다음과 같이 수정한다:

```yaml
  postgres:
    image: postgres:18-alpine
    container_name: goexchange-stress-postgres
    cpuset: "1"
    environment:
```

- [ ] **Step 2: `backend` 서비스에 cpuset + GOMAXPROCS 추가**

`backend` 서비스, 현재:

```yaml
  backend:
    build:
      context: .
      dockerfile: Dockerfile
    image: goexchange-back:stress
    container_name: goexchange-stress-backend
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      GOEXCHANGE_DB_HOST: postgres
```

다음과 같이 수정한다:

```yaml
  backend:
    build:
      context: .
      dockerfile: Dockerfile
    image: goexchange-back:stress
    container_name: goexchange-stress-backend
    cpuset: "0"
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      GOMAXPROCS: "1"
      GOEXCHANGE_DB_HOST: postgres
```

- [ ] **Step 3: `postgres-exporter` 서비스에 cpuset 추가**

`postgres-exporter` 서비스, 현재:

```yaml
  postgres-exporter:
    image: quay.io/prometheuscommunity/postgres-exporter:v0.15.0
    container_name: goexchange-stress-postgres-exporter
    depends_on:
```

다음과 같이 수정한다:

```yaml
  postgres-exporter:
    image: quay.io/prometheuscommunity/postgres-exporter:v0.15.0
    container_name: goexchange-stress-postgres-exporter
    cpuset: "1"
    depends_on:
```

- [ ] **Step 4: `node-exporter` 서비스에 cpuset 추가**

`node-exporter` 서비스, 현재:

```yaml
  node-exporter:
    image: prom/node-exporter:v1.8.2
    container_name: goexchange-stress-node-exporter
    pid: host
```

다음과 같이 수정한다:

```yaml
  node-exporter:
    image: prom/node-exporter:v1.8.2
    container_name: goexchange-stress-node-exporter
    cpuset: "1"
    pid: host
```

- [ ] **Step 5: `prometheus` 서비스에 cpuset 추가**

`prometheus` 서비스, 현재:

```yaml
  prometheus:
    image: prom/prometheus:v2.55.1
    container_name: goexchange-stress-prometheus
    depends_on:
```

다음과 같이 수정한다:

```yaml
  prometheus:
    image: prom/prometheus:v2.55.1
    container_name: goexchange-stress-prometheus
    cpuset: "1"
    depends_on:
```

- [ ] **Step 6: `grafana` 서비스에 cpuset 추가**

`grafana` 서비스, 현재:

```yaml
  grafana:
    image: grafana/grafana:11.3.1
    container_name: goexchange-stress-grafana
    depends_on:
```

다음과 같이 수정한다:

```yaml
  grafana:
    image: grafana/grafana:11.3.1
    container_name: goexchange-stress-grafana
    cpuset: "1"
    depends_on:
```

- [ ] **Step 7: 문법 검증**

```bash
docker compose -f docker-compose.stress.yml --env-file .env.stress.example config >/dev/null
```

Expected: 에러 없이 종료 (`cpuset`은 표준 Compose 옵션이라 로컬 검증에는 실제로 2코어 이상의 머신이 필요 없다 — 문법만 확인).

- [ ] **Step 8: 커밋**

```bash
git add docker-compose.stress.yml
git commit -m "$(cat <<'MSG'
feat: 스트레스 환경 backend 컨테이너를 코어 0에 CPU 핀닝

매칭엔진 고루틴이 Postgres/모니터링 스택과 CPU를 경쟁해 큐잉 지연이
벌어진다는 가설을 검증하기 위해, backend를 cpuset 0에, 나머지 서비스를
cpuset 1에 고정한다. GOMAXPROCS=1로 Go 런타임을 실제 가용 코어 수에
맞춘다.
MSG
)"
```
