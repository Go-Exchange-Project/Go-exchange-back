# Postgres 별도 인스턴스 분리 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Postgres를 backend와 같은 4코어를 나눠 쓰는 지금 구조에서 분리해, 별도 GCP 인스턴스에서 독립적으로 실행되게 한다.

**Architecture:** 서버/부하생성기와 동일한 Terraform 패턴으로 DB 전용 인스턴스와 방화벽 규칙을 추가한다. `docker-compose.stress.yml`에서 Postgres 관련 서비스를 제거하고 새 `docker-compose.db.yml`로 분리한다. backend는 환경변수로 DB 인스턴스의 내부 IP를 받는다.

**Tech Stack:** Terraform(`google_compute_instance`, `google_compute_firewall`), Docker Compose.

## Global Constraints

- DB 인스턴스 머신 타입: `e2-medium`(2 vCPU/4GB).
- 방화벽 규칙 `allow_postgres`는 서버 인스턴스의 내부 IP로만 5432 포트를 연다 — 관리자 CIDR이나 부하생성기에는 열지 않는다.
- `docker-compose.stress.yml`, `.env.stress.example`, `monitoring/prometheus.yml`만 수정한다 — `docker-compose.yml`, `docker-compose.deploy.yml`, `docker-compose.prod.yml` 등 다른 compose 파일은 건드리지 않는다.
- 실제 `terraform apply`, DB 인스턴스 배포, 서버 재배포, k6 재실행, 결과 문서화는 이 계획의 범위 밖이다 — 코드/설정 준비까지만 다룬다.

---

### Task 1: Terraform에 DB 인스턴스와 방화벽 규칙 추가

**Files:**
- Modify: `infra/terraform/gcp/variables.tf`
- Modify: `infra/terraform/gcp/main.tf`

**Interfaces:**
- Produces: `google_compute_instance.db`(리소스), `google_compute_firewall.allow_postgres`(리소스) — Task 2/3의 배포 절차에서 `google_compute_instance.db`의 내부 IP를 참조하게 된다(실제 실행은 이 계획 범위 밖).

- [ ] **Step 1: `variables.tf`에 DB 머신 타입 변수 추가**

`infra/terraform/gcp/variables.tf`의 `load_gen_machine_type` 변수 블록 다음에 추가:

```hcl
variable "db_machine_type" {
  description = "Postgres 전용 인스턴스 머신 타입."
  type        = string
  default     = "e2-medium"
}
```

- [ ] **Step 2: `main.tf`에 DB 인스턴스 리소스 추가**

`infra/terraform/gcp/main.tf`의 `google_compute_instance "load_gen"` 리소스 블록 다음(파일 끝)에 추가:

```hcl

resource "google_compute_instance" "db" {
  name         = "${local.name_prefix}-db"
  machine_type = var.db_machine_type
  zone         = var.gcp_zone
  tags         = ["goexchange-db"]

  boot_disk {
    initialize_params {
      image = "ubuntu-os-cloud/ubuntu-2404-lts-amd64"
      size  = var.root_volume_size_gb
      type  = "pd-ssd"
    }
  }

  network_interface {
    network    = google_compute_network.this.id
    subnetwork = google_compute_subnetwork.this.id
    access_config {}
  }

  metadata = {
    ssh-keys = local.ssh_keys
  }
}
```

- [ ] **Step 3: `main.tf`에 방화벽 규칙 추가**

같은 파일, `google_compute_instance "db"` 리소스 블록 다음에 추가:

```hcl

resource "google_compute_firewall" "allow_postgres" {
  name    = "${local.name_prefix}-allow-postgres"
  network = google_compute_network.this.id
  source_ranges = [
    "${google_compute_instance.server.network_interface[0].network_ip}/32",
  ]
  target_tags = ["goexchange-db"]

  allow {
    protocol = "tcp"
    ports    = ["5432"]
  }
}
```

- [ ] **Step 4: `allow_ssh` 방화벽 규칙에 DB 태그 추가 (관리 접속용)**

`infra/terraform/gcp/main.tf`의 `google_compute_firewall "allow_ssh"` 현재:

```hcl
resource "google_compute_firewall" "allow_ssh" {
  name          = "${local.name_prefix}-allow-ssh"
  network       = google_compute_network.this.id
  source_ranges = [var.allowed_admin_cidr]
  target_tags   = ["goexchange-server", "goexchange-loadgen"]

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}
```

다음과 같이 수정한다(`goexchange-db` 태그 추가 — DB 인스턴스도 SSH로 배포/점검해야 하므로):

```hcl
resource "google_compute_firewall" "allow_ssh" {
  name          = "${local.name_prefix}-allow-ssh"
  network       = google_compute_network.this.id
  source_ranges = [var.allowed_admin_cidr]
  target_tags   = ["goexchange-server", "goexchange-loadgen", "goexchange-db"]

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}
```

- [ ] **Step 5: Terraform 문법 검증**

```bash
cd infra/terraform/gcp
terraform fmt -check
terraform validate
```

Expected: `terraform fmt -check`는 포맷팅 문제 없으면 출력 없이 종료(문제 있으면 `terraform fmt`로 자동 수정 후 재검증). `terraform validate`는 `Success! The configuration is valid.` 출력.

(주의: `terraform validate`는 GCP 자격 증명 없이도 문법/타입 검증만 수행하므로 이 샌드박스에서 실행 가능하다. 실제 `plan`/`apply`는 이 태스크의 범위 밖이다.)

- [ ] **Step 6: 커밋**

```bash
git add infra/terraform/gcp/variables.tf infra/terraform/gcp/main.tf
git commit -m "$(cat <<'MSG'
feat(infra): Postgres 전용 인스턴스와 방화벽 규칙 추가

backend와 Postgres가 같은 4코어를 나눠 쓰는 게 CPU 포화의 원인일 수
있다는 가설을 검증하기 위해, 서버/부하생성기와 같은 패턴으로 DB
전용 인스턴스(e2-medium)를 추가한다. allow_postgres 방화벽 규칙은
서버 인스턴스의 내부 IP로만 5432 포트를 연다.
MSG
)"
```

---

### Task 2: `docker-compose.db.yml` 신규 작성

**Files:**
- Create: `docker-compose.db.yml`

**Interfaces:**
- 없음 (Task 1에서 만든 GCP 인스턴스에 배포될 설정 파일, 다른 태스크와 코드 레벨 의존성 없음).

- [ ] **Step 1: 새 compose 파일 작성**

`docker-compose.db.yml` 생성:

```yaml
name: goexchange-db

services:
  postgres:
    image: postgres:18-alpine
    container_name: goexchange-db-postgres
    environment:
      POSTGRES_DB: ${POSTGRES_DB:-goexchange}
      POSTGRES_USER: ${POSTGRES_USER:-goexchange}
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}
    volumes:
      - goexchange-db-postgres-data:/var/lib/postgresql
    ports:
      - "5432:5432"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ${POSTGRES_USER:-goexchange} -d ${POSTGRES_DB:-goexchange}"]
      interval: 10s
      timeout: 5s
      retries: 10

  postgres-exporter:
    image: quay.io/prometheuscommunity/postgres-exporter:v0.15.0
    container_name: goexchange-db-postgres-exporter
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      DATA_SOURCE_NAME: postgresql://${POSTGRES_USER:-goexchange}:${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}@postgres:5432/${POSTGRES_DB:-goexchange}?sslmode=disable
    ports:
      - "9187:9187"

volumes:
  goexchange-db-postgres-data:
```

- [ ] **Step 2: 문법 검증**

```bash
docker compose -f docker-compose.db.yml --env-file .env.stress.example config >/dev/null
```

Expected: 에러 없이 종료. (`.env.stress.example`에 `POSTGRES_DB`/`POSTGRES_USER`/`POSTGRES_PASSWORD`가 이미 있으므로 이 시점엔 통과해야 한다 — Task 3에서 `.env.stress.example`에 `GOEXCHANGE_DB_HOST`를 추가해도 이 파일 자체 검증에는 영향 없다.)

- [ ] **Step 3: 커밋**

```bash
git add docker-compose.db.yml
git commit -m "$(cat <<'MSG'
feat: Postgres 전용 인스턴스용 docker-compose 파일 추가

docker-compose.stress.yml에 있던 postgres/postgres-exporter 서비스를
분리해 새 파일로 만든다. postgres에 5432 포트 매핑을 추가해 다른
인스턴스(backend)에서 접근 가능하게 하되, 실제 접근 제어는 GCP
방화벽 규칙(allow_postgres)이 담당한다.
MSG
)"
```

---

### Task 3: `docker-compose.stress.yml` 분리 및 관련 설정 갱신

**Files:**
- Modify: `docker-compose.stress.yml`
- Modify: `.env.stress.example`
- Modify: `monitoring/prometheus.yml`

**Interfaces:**
- Consumes: Task 2에서 만든 `docker-compose.db.yml`이 배포될 DB 인스턴스가 노출하는 포트(`5432`=Postgres, `9187`=postgres-exporter) — 이 태스크는 그 포트들을 가리키는 설정만 준비한다(실제 IP 값은 배포 시점에 채운다).

- [ ] **Step 1: `docker-compose.stress.yml`에서 `postgres`/`postgres-exporter` 서비스 제거, `backend` 설정 변경**

`docker-compose.stress.yml`의 현재 전체를 다음과 같이 수정한다:

```yaml
name: goexchange-stress

services:
  backend:
    build:
      context: .
      dockerfile: Dockerfile
    image: goexchange-back:stress
    container_name: goexchange-stress-backend
    environment:
      GOEXCHANGE_DB_HOST: ${GOEXCHANGE_DB_HOST:?GOEXCHANGE_DB_HOST is required}
      GOEXCHANGE_DB_USER: ${POSTGRES_USER:-goexchange}
      GOEXCHANGE_DB_PASSWORD: ${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}
      GOEXCHANGE_DB_NAME: ${POSTGRES_DB:-goexchange}
      GOEXCHANGE_DB_PORT: "5432"
      GOEXCHANGE_DB_SSLMODE: disable
      GOEXCHANGE_DB_CONNECT_TIMEOUT: "10"
      GOEXCHANGE_JWT_SECRET: ${GOEXCHANGE_JWT_SECRET:?GOEXCHANGE_JWT_SECRET is required}
      GOEXCHANGE_MARKET_RULES_PATH: /app/config/market_rules.json
      GOEXCHANGE_MIGRATIONS_DIR: /app/migrations
      GOEXCHANGE_ENABLE_DEV_TOOLS: "true"
      GOEXCHANGE_DEV_TOOLS_TOKEN: ${GOEXCHANGE_DEV_TOOLS_TOKEN:?GOEXCHANGE_DEV_TOOLS_TOKEN is required}
      GOEXCHANGE_ENABLE_UPBIT: "false"
      GOEXCHANGE_ENABLE_PPROF: ${GOEXCHANGE_ENABLE_PPROF:-false}
      GOEXCHANGE_DB_MAX_OPEN_CONNS: ${GOEXCHANGE_DB_MAX_OPEN_CONNS:-25}
      GOEXCHANGE_DB_MAX_IDLE_CONNS: ${GOEXCHANGE_DB_MAX_IDLE_CONNS:-25}
      GOEXCHANGE_DB_CONN_MAX_LIFETIME: ${GOEXCHANGE_DB_CONN_MAX_LIFETIME:-30m}
      GOEXCHANGE_SETTLEMENT_WORKERS: ${GOEXCHANGE_SETTLEMENT_WORKERS:-10}
      GOEXCHANGE_CORS_ALLOWED_ORIGINS: ${GOEXCHANGE_CORS_ALLOWED_ORIGINS:-http://localhost:3000}
      GOEXCHANGE_WS_ALLOWED_ORIGINS: ${GOEXCHANGE_WS_ALLOWED_ORIGINS:-http://localhost:3000}
    ports:
      - "8080:8080"
      - "127.0.0.1:6060:6060"
    healthcheck:
      test: ["CMD-SHELL", "wget -qO- http://localhost:8080/ping >/dev/null || exit 1"]
      interval: 30s
      timeout: 3s
      start_period: 20s
      retries: 5
    networks:
      - goexchange-stress

  node-exporter:
    image: prom/node-exporter:v1.8.2
    container_name: goexchange-stress-node-exporter
    pid: host
    command:
      - '--path.rootfs=/host'
    volumes:
      - /:/host:ro,rslave
    networks:
      - goexchange-stress

  prometheus:
    image: prom/prometheus:v2.55.1
    container_name: goexchange-stress-prometheus
    depends_on:
      - backend
      - node-exporter
    volumes:
      - ./monitoring/prometheus.yml:/etc/prometheus/prometheus.yml:ro
    ports:
      - "9090:9090"
    networks:
      - goexchange-stress

  grafana:
    image: grafana/grafana:11.3.1
    container_name: goexchange-stress-grafana
    depends_on:
      - prometheus
    environment:
      GF_SECURITY_ADMIN_PASSWORD: ${GRAFANA_ADMIN_PASSWORD:?GRAFANA_ADMIN_PASSWORD is required}
      GF_AUTH_ANONYMOUS_ENABLED: "false"
    volumes:
      - ./monitoring/grafana/provisioning:/etc/grafana/provisioning:ro
      - goexchange-stress-grafana-data:/var/lib/grafana
    ports:
      - "3000:3000"
    networks:
      - goexchange-stress

networks:
  goexchange-stress:
    name: goexchange-stress

volumes:
  goexchange-stress-grafana-data:
```

(변경 요약: `postgres`/`postgres-exporter` 서비스 제거, `backend`의 `depends_on: postgres` 제거, `GOEXCHANGE_DB_HOST`를 필수 환경변수로 변경, `prometheus`의 `depends_on`에서 `postgres-exporter` 제거, `volumes`에서 `goexchange-stress-postgres-data` 제거.)

- [ ] **Step 2: `.env.stress.example`에 `GOEXCHANGE_DB_HOST` 추가**

`.env.stress.example`의 현재:

```
POSTGRES_DB=goexchange
POSTGRES_USER=goexchange
POSTGRES_PASSWORD=change-me-postgres-password
```

다음과 같이 수정한다:

```
POSTGRES_DB=goexchange
POSTGRES_USER=goexchange
POSTGRES_PASSWORD=change-me-postgres-password
# terraform apply 후 DB 인스턴스의 내부 IP로 교체할 것 (예: 10.10.0.x)
GOEXCHANGE_DB_HOST=change-me-db-instance-internal-ip
```

- [ ] **Step 3: `monitoring/prometheus.yml`의 postgres-exporter 타겟 갱신**

`monitoring/prometheus.yml`의 현재:

```yaml
  - job_name: postgres-exporter
    static_configs:
      - targets: ["postgres-exporter:9187"]
```

다음과 같이 수정한다:

```yaml
  - job_name: postgres-exporter
    static_configs:
      # terraform apply 후 DB 인스턴스의 내부 IP로 교체할 것 (예: 10.10.0.x:9187)
      - targets: ["change-me-db-instance-internal-ip:9187"]
```

- [ ] **Step 4: 문법 검증**

```bash
docker compose -f docker-compose.stress.yml --env-file .env.stress.example config >/dev/null
```

Expected: 에러 없이 종료(예시 값이라도 `GOEXCHANGE_DB_HOST`가 비어있지 않으므로 `:?` 필수 검증은 통과한다).

- [ ] **Step 5: 커밋**

```bash
git add docker-compose.stress.yml .env.stress.example monitoring/prometheus.yml
git commit -m "$(cat <<'MSG'
feat: 스트레스 환경에서 Postgres 서비스 분리, 외부 DB 인스턴스 연결로 전환

docker-compose.stress.yml에서 postgres/postgres-exporter 서비스를
제거하고, backend가 GOEXCHANGE_DB_HOST 환경변수로 별도 DB 인스턴스에
접속하도록 바꾼다. Prometheus의 postgres-exporter 스크레이프 타겟도
같은 방식으로 외부 주소를 가리키게 한다. 실제 IP는 배포 시점에
terraform apply 결과로 채운다.
MSG
)"
```
