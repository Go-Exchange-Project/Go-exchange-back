# Postgres 별도 인스턴스 분리 설계

## 배경 (왜 필요한가)

`12-2026-07-10-stress-test-vu-ceiling-extension.md`에서, VU 700~800부터 CPU가 90%로 평탄화되고 그 이후로는 매칭 지연이 계속 쌓이는 걸 확인했다. 지금 `docker-compose.stress.yml`은 `backend`(Go 앱), `postgres`, `postgres-exporter`, `prometheus`, `grafana`, `node-exporter`가 전부 같은 4코어 서버 인스턴스 안에서 CPU를 나눠 쓴다. Postgres를 별도 인스턴스로 옮기면, backend가 이 4코어를 온전히 쓸 수 있게 되고 Postgres도 자기 코어를 독점하게 된다 — 이게 진짜 개선을 가져오는지, 아니면 07번(CPU 핀닝) 때처럼 기대와 다른 결과가 나오는지 실측으로 확인한다.

이 작업은 [[goexchange-performance-goal]]에 따라 GCP 무료 크레딧 여유를 활용한 "적당한 규모의 인프라 확장"에 해당한다 — 인스턴스 하나를 더 쓰는 것이지, 기존 인스턴스를 과하게 키우는 게 아니다.

## 왜 이 방식을 선택했는지

지금 이 프로젝트에는 이미 서버 인스턴스와 부하생성 인스턴스가 GCP 내부망(VPC, 내부 IP)으로 통신하는 패턴이 있다 — k6가 `10.10.0.3:8080`으로 서버에 접속하는 것과 같은 방식이다. Postgres 분리도 같은 패턴을 그대로 재사용한다: 새 인스턴스에 내부 IP를 할당하고, 방화벽 규칙으로 접근을 서버 인스턴스로만 제한하고, backend의 DB 접속 주소만 그 내부 IP로 바꾼다. 새로운 통신 방식을 도입하지 않고 기존 검증된 패턴을 확장하는 것이라 리스크가 적다.

인스턴스 자체는 서버/부하생성기와 마찬가지로 Terraform으로 관리한다 — 이번 건 "테스트 후 되돌릴 수도 있는 일시적 실험"(07번 CPU 핀닝, 11번 VM 사이즈 실험처럼 `gcloud`로 즉흥 조작)이 아니라, 애초에 아키텍처에 새 컴포넌트(별도 DB 인스턴스)를 들이는 구조적 변경이라 처음부터 IaC로 관리하는 게 맞다.

## 범위

- `infra/terraform/gcp/`: DB 전용 인스턴스(`e2-medium`)와 이를 겨냥한 방화벽 규칙을 추가한다.
- `docker-compose.stress.yml`에서 `postgres`/`postgres-exporter` 서비스를 제거하고, `backend`의 DB 접속 설정을 외부 인스턴스를 가리키도록 바꾼다.
- 새 `docker-compose.db.yml`을 만들어 DB 인스턴스에 배포한다.
- `monitoring/prometheus.yml`의 `postgres-exporter` 타겟 주소를 갱신한다.
- 실제 프로비저닝, 배포, k6 재실행, 결과 문서화는 이 스펙 문서화 이후 사용자와 직접 진행한다.

## 아키텍처

### 1. Terraform — DB 인스턴스 + 방화벽 규칙

`infra/terraform/gcp/variables.tf`에 추가:
```hcl
variable "db_machine_type" {
  description = "Postgres 전용 인스턴스 머신 타입."
  type        = string
  default     = "e2-medium"
}
```

`infra/terraform/gcp/main.tf`에 추가 (기존 `server`/`load_gen` 리소스와 동일한 패턴):
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

`allow_postgres`는 `allow_ssh`(관리자 CIDR)와 다르게 **서버 인스턴스의 내부 IP로만** 5432 포트를 열어준다 — 기존 `allow_api`가 부하생성기 IP로만 8080을 열어준 것과 같은 최소 권한 패턴. 외부 인터넷은 물론, 부하생성기에서도 DB에 직접 접속할 수 없다.

### 2. `docker-compose.db.yml` 신규 — DB 인스턴스에 배포

`docker-compose.stress.yml`의 `postgres`/`postgres-exporter` 서비스를 그대로 옮기되, `postgres`에 포트 매핑을 추가한다(지금은 같은 docker 네트워크 안에서만 접근했지만, 이제 다른 VM에서 접근해야 하므로):

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

포트가 `0.0.0.0`에 바인딩되긴 하지만, 실제 접근 제어는 위의 GCP 방화벽 규칙(`allow_postgres`)이 담당한다 — 지금까지 이 프로젝트가 pprof 포트 등에서 써온 것과 같은 "compose는 넓게 열고 방화벽이 진짜 게이트" 패턴이다.

### 3. `docker-compose.stress.yml` 수정

- `postgres`, `postgres-exporter` 서비스 블록 제거.
- `backend` 서비스에서 `depends_on: postgres: condition: service_healthy` 제거(더 이상 같은 compose 안에 없으므로).
- `backend`의 `GOEXCHANGE_DB_HOST` 값을 하드코딩된 `postgres` 대신 환경변수로:
  ```yaml
  GOEXCHANGE_DB_HOST: ${GOEXCHANGE_DB_HOST:?GOEXCHANGE_DB_HOST is required}
  ```

### 4. `.env.stress.example` / `monitoring/prometheus.yml` 갱신

- `.env.stress.example`에 `GOEXCHANGE_DB_HOST=<db-instance-internal-ip>` 주석과 함께 추가(실제 IP는 Terraform apply 후 확인해서 배포 시점에 채워 넣는다).
- `monitoring/prometheus.yml`의 `postgres-exporter` 타겟을 `postgres-exporter:9187`에서 `<db-instance-internal-ip>:9187`로 변경(DB 인스턴스 IP 확정 후 반영 — 지금까지 인스턴스 리사이즈 때마다 IP를 확인해서 수동 반영해온 것과 같은 절차).

### 검증 절차 (사용자와 직접 진행, 이 스펙의 범위 밖)

1. `terraform apply`로 DB 인스턴스 생성, 내부 IP 확인.
2. `docker-compose.db.yml`을 DB 인스턴스에 배포.
3. `docker-compose.stress.yml`/`.env.stress.example`/`monitoring/prometheus.yml`을 DB 인스턴스의 실제 IP로 채워서 서버 인스턴스에 재배포.
4. 서버 컨테이너 안에서 DB 인스턴스로의 연결 확인(`docker exec ... wget --spider <db-ip>:5432` 또는 앱 자체의 헬스체크 통과 여부).
5. k6 재실행, 04~12번과 동일 조건에서 CPU/채널 길이/매칭 지연을 비교.
6. 결과를 `docs/benchmarks/13-YYYY-MM-DD-postgres-instance-separation.md`에 기록.

## 성공 기준

- `terraform plan`/`apply`가 에러 없이 DB 인스턴스와 방화벽 규칙을 생성한다.
- backend가 분리된 DB 인스턴스에 정상 연결되고 기존 기능(로그인/주문/체결)이 그대로 동작한다.
- 재측정 결과(개선이든 아니든)를 있는 그대로 기록해서, 이 분리가 실제로 효과가 있었는지 명확히 결론짓는다.

## 범위 밖 (Out of Scope)

- 매칭엔진 심볼별 샤딩 — 이번 결과를 보고 별도로 브레인스토밍.
- Postgres 자체의 쿼리 최적화(인덱스, N+1 등) — 이번엔 "CPU 경합 해소"만 검증하고, DB 쿼리 자체가 느리다면 그건 별개 문제로 남겨둔다.
