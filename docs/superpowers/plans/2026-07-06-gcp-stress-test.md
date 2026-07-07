# GCP 분리 환경 스트레스 테스트 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** GCP 인스턴스 2대(서버/부하생성)로 물리적으로 분리된 환경에서, Prometheus+Grafana로 실시간 관찰하며 `POST /orders` 스트레스 테스트를 수행할 수 있도록 인프라 코드, 메트릭 계측, 모니터링 스택, k6 스크립트, 실행 문서를 갖춘다.

**Architecture:** Terraform으로 GCP VPC+방화벽+인스턴스 2대를 정의한다. Go 앱에는 `promhttp` 기반 HTTP/런타임 지표와, 매칭엔진 큐잉·정산 DB 반영 지연을 재는 2개의 신규 히스토그램을 추가한다. 서버 인스턴스에서는 docker-compose로 백엔드+Postgres+Prometheus+Grafana+node_exporter+postgres_exporter를 함께 띄운다. 부하생성 인스턴스에서는 기존 `order-submission-baseline.js`를 확장한 단계적 램프업 k6 스크립트를 실행한다.

**Tech Stack:** Go 1.25 (gin, gorm), `github.com/prometheus/client_golang`, Terraform (`google` provider), Docker Compose, Prometheus, Grafana, k6.

## Global Constraints

- 리전: `asia-northeast3`(서울). 서버 인스턴스: `e2-medium`(2 vCPU/4GB). 부하생성 인스턴스: `e2-small`(2 vCPU/2GB). 머신 타입은 나중에 변경 가능(중지 → `terraform apply`).
- 방화벽: SSH(22)·Grafana(3000)·Prometheus(9090)는 관리자 공인 IP(`allowed_admin_cidr`)만 허용. API(8080)는 관리자 공인 IP + 부하생성 인스턴스 내부 IP만 허용. 그 외 포트는 외부에 노출하지 않는다.
- k6 스트레스 시나리오는 사전 정의된 목표 수치나 임계값(threshold)을 두지 않는다. `ramping-vus`로 50→100→200→400→800 VU까지 각 단계 2분씩 단계적으로 램프업한다.
- 기존 AWS `infra/terraform/ec2/`는 실제로 띄운 적 없는 코드이므로 그대로 삭제하고 GCP로 대체한다 (`infra/terraform/gcp/`).
- 결과 기록은 기존 `docs/benchmarks/NN-YYYY-MM-DD-<주제>.md` 컨벤션(`docs/benchmarks/README.md` 참고)을 그대로 따른다.
- 실제 GCP 리소스 생성(`terraform apply`)과 실제 부하 실행은 이 계획의 범위 밖이다 — 이 계획의 모든 작업은 코드/설정/문서를 준비하는 것이며, 실제 클라우드 비용이 발생하는 단계(터널링/apply/실행)는 사용자가 `docs/gcp-stress-test-runbook.md`를 보며 직접 수행한다.

---

### Task 1: GCP Terraform 인프라 (AWS 대체)

**Files:**
- Delete: `infra/terraform/ec2/main.tf`, `infra/terraform/ec2/variables.tf`, `infra/terraform/ec2/outputs.tf`, `infra/terraform/ec2/versions.tf`, `infra/terraform/ec2/README.md`, `infra/terraform/ec2/.terraform.lock.hcl`
- Delete (untracked local artifacts, not in git but present on disk): `infra/terraform/ec2/.terraform/`, `infra/terraform/ec2/terraform.tfstate`, `infra/terraform/ec2/terraform.tfstate.backup`, `infra/terraform/ec2/terraform.tfvars`, `infra/terraform/ec2/tfplan`
- Create: `infra/terraform/gcp/versions.tf`
- Create: `infra/terraform/gcp/variables.tf`
- Create: `infra/terraform/gcp/main.tf`
- Create: `infra/terraform/gcp/outputs.tf`
- Create: `infra/terraform/gcp/terraform.tfvars.example`
- Create: `infra/terraform/gcp/README.md`

**Interfaces:**
- Consumes: 없음 (독립 인프라 코드)
- Produces: Terraform 리소스 이름 규칙 `google_compute_instance.server`, `google_compute_instance.load_gen` — Task 6(runbook)에서 이 리소스 이름과 output 이름을 참조한다.

- [ ] **Step 1: 기존 AWS terraform 삭제**

`infra/terraform/ec2/`에 실제로 띄운 EC2 인스턴스는 없음을 이미 확인했다(`terraform.tfstate`의 `resources` 배열이 비어 있음). 아래 명령으로 추적 파일과 로컬 잔여 파일을 모두 제거한다.

```bash
git rm infra/terraform/ec2/main.tf infra/terraform/ec2/variables.tf infra/terraform/ec2/outputs.tf infra/terraform/ec2/versions.tf infra/terraform/ec2/README.md infra/terraform/ec2/.terraform.lock.hcl
rm -rf infra/terraform/ec2
```

- [ ] **Step 2: `versions.tf` 작성**

`infra/terraform/gcp/versions.tf`:

```hcl
terraform {
  required_version = ">= 1.5.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 5.30"
    }
  }
}

provider "google" {
  project = var.gcp_project_id
  region  = var.gcp_region
  zone    = var.gcp_zone
}
```

- [ ] **Step 3: `variables.tf` 작성**

`infra/terraform/gcp/variables.tf`:

```hcl
variable "gcp_project_id" {
  description = "리소스를 생성할 GCP 프로젝트 ID."
  type        = string
}

variable "gcp_region" {
  description = "스트레스 테스트 리소스를 생성할 GCP 리전."
  type        = string
  default     = "asia-northeast3"
}

variable "gcp_zone" {
  description = "스트레스 테스트 인스턴스를 생성할 GCP 존."
  type        = string
  default     = "asia-northeast3-a"
}

variable "project_name" {
  description = "GCP 리소스 이름에 쓰일 프로젝트 이름."
  type        = string
  default     = "goexchange"
}

variable "environment" {
  description = "배포 환경 이름."
  type        = string
  default     = "stress"
}

variable "server_machine_type" {
  description = "서버 인스턴스(Go 앱 + Postgres + Prometheus + Grafana) 머신 타입."
  type        = string
  default     = "e2-medium"
}

variable "load_gen_machine_type" {
  description = "k6 부하생성 인스턴스 머신 타입."
  type        = string
  default     = "e2-small"
}

variable "root_volume_size_gb" {
  description = "두 인스턴스 공통 루트 디스크 크기(GiB)."
  type        = number
  default     = 30

  validation {
    condition     = var.root_volume_size_gb >= 10
    error_message = "root_volume_size_gb must be at least 10."
  }
}

variable "allowed_admin_cidr" {
  description = "SSH/Grafana/Prometheus/API에 접근을 허용할 내 공인 IP. 예: 203.0.113.10/32"
  type        = string

  validation {
    condition     = can(cidrhost(var.allowed_admin_cidr, 0))
    error_message = "allowed_admin_cidr must be a valid CIDR block, for example 203.0.113.10/32."
  }
}

variable "ssh_public_key_path" {
  description = "두 인스턴스에 등록할 로컬 SSH 공개키 경로."
  type        = string
  default     = "~/.ssh/goexchange-gcp.pub"
}

variable "ssh_username" {
  description = "SSH 공개키에 연결될 사용자 이름."
  type        = string
  default     = "goexchange"
}
```

- [ ] **Step 4: `main.tf` 작성**

`infra/terraform/gcp/main.tf`:

```hcl
locals {
  name_prefix = "${var.project_name}-${var.environment}"
  ssh_keys    = "${var.ssh_username}:${trimspace(file(pathexpand(var.ssh_public_key_path)))}"
}

resource "google_compute_network" "this" {
  name                    = "${local.name_prefix}-vpc"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "this" {
  name          = "${local.name_prefix}-subnet"
  ip_cidr_range = "10.10.0.0/24"
  region        = var.gcp_region
  network       = google_compute_network.this.id
}

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

resource "google_compute_firewall" "allow_monitoring" {
  name          = "${local.name_prefix}-allow-monitoring"
  network       = google_compute_network.this.id
  source_ranges = [var.allowed_admin_cidr]
  target_tags   = ["goexchange-server"]

  allow {
    protocol = "tcp"
    ports    = ["3000", "9090"]
  }
}

resource "google_compute_firewall" "allow_api" {
  name    = "${local.name_prefix}-allow-api"
  network = google_compute_network.this.id
  source_ranges = [
    var.allowed_admin_cidr,
    "${google_compute_instance.load_gen.network_interface[0].network_ip}/32",
  ]
  target_tags = ["goexchange-server"]

  allow {
    protocol = "tcp"
    ports    = ["8080"]
  }
}

resource "google_compute_instance" "server" {
  name         = "${local.name_prefix}-server"
  machine_type = var.server_machine_type
  zone         = var.gcp_zone
  tags         = ["goexchange-server"]

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

resource "google_compute_instance" "load_gen" {
  name         = "${local.name_prefix}-load-gen"
  machine_type = var.load_gen_machine_type
  zone         = var.gcp_zone
  tags         = ["goexchange-loadgen"]

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

- [ ] **Step 5: `outputs.tf` 작성**

`infra/terraform/gcp/outputs.tf`:

```hcl
output "server_external_ip" {
  description = "서버 인스턴스의 외부 IP (Go 앱 + Postgres + Prometheus + Grafana)."
  value       = google_compute_instance.server.network_interface[0].access_config[0].nat_ip
}

output "server_internal_ip" {
  description = "서버 인스턴스의 내부 IP."
  value       = google_compute_instance.server.network_interface[0].network_ip
}

output "load_gen_external_ip" {
  description = "부하생성 인스턴스의 외부 IP (k6)."
  value       = google_compute_instance.load_gen.network_interface[0].access_config[0].nat_ip
}

output "load_gen_internal_ip" {
  description = "부하생성 인스턴스의 내부 IP."
  value       = google_compute_instance.load_gen.network_interface[0].network_ip
}

output "server_ssh_command" {
  description = "서버 인스턴스 SSH 접속 명령."
  value       = "ssh -i ${replace(pathexpand(var.ssh_public_key_path), ".pub", "")} ${var.ssh_username}@${google_compute_instance.server.network_interface[0].access_config[0].nat_ip}"
}

output "load_gen_ssh_command" {
  description = "부하생성 인스턴스 SSH 접속 명령."
  value       = "ssh -i ${replace(pathexpand(var.ssh_public_key_path), ".pub", "")} ${var.ssh_username}@${google_compute_instance.load_gen.network_interface[0].access_config[0].nat_ip}"
}
```

- [ ] **Step 6: `terraform.tfvars.example` 작성**

`infra/terraform/gcp/terraform.tfvars.example`:

```hcl
gcp_project_id      = "my-gcp-project-id"
allowed_admin_cidr  = "203.0.113.10/32"
ssh_public_key_path = "~/.ssh/goexchange-gcp.pub"
```

- [ ] **Step 7: `README.md` 작성**

`infra/terraform/gcp/README.md`:

```markdown
# GoExchange GCP 스트레스 테스트 Terraform

이 폴더는 스트레스 테스트용 GCP 인스턴스 2대(서버, 부하생성)를 만들기 위한 Terraform 구성입니다.

## 생성하는 리소스

- VPC 네트워크 + 서브넷
- 방화벽 규칙 3개 (SSH, 모니터링 포트, API 포트)
- Compute Engine 인스턴스 2대 (server, load-gen)

## 1. gcloud 인증

```powershell
gcloud auth application-default login
gcloud config set project <내 GCP 프로젝트 ID>
```

## 2. SSH 키 만들기

```powershell
ssh-keygen -t ed25519 -C "goexchange-gcp" -f "$env:USERPROFILE\.ssh\goexchange-gcp"
```

## 3. 내 공인 IP 확인

```powershell
Invoke-RestMethod https://checkip.amazonaws.com
```

출력된 IP 뒤에 `/32`를 붙여서 `allowed_admin_cidr`에 넣습니다.

## 4. terraform.tfvars 만들기

```powershell
Copy-Item terraform.tfvars.example terraform.tfvars
```

`terraform.tfvars`를 열고 `gcp_project_id`, `allowed_admin_cidr`, `ssh_public_key_path`를 실제 값으로 수정합니다. 이 파일은 Git에 올리지 않습니다.

## 5. 초기화 및 검증

```powershell
terraform init
terraform fmt
terraform validate
```

## 6. 생성 예정 리소스 확인

```powershell
terraform plan -out tfplan
```

## 7. 실제 생성

```powershell
terraform apply tfplan
```

## 8. 삭제

```powershell
terraform destroy
```

## 주의

- `terraform apply` 이후 Compute Engine, 디스크, 외부 IP 등 GCP 비용이 발생할 수 있습니다.
- `terraform.tfstate`, `terraform.tfvars`, `.terraform` 폴더는 Git에 올리지 않습니다.
- 개인키 파일은 절대 공유하지 않습니다.
```

- [ ] **Step 8: 검증**

```bash
cd infra/terraform/gcp
terraform init
terraform validate
```

Expected: `terraform validate` prints `Success! The configuration is valid.` (실제 GCP 프로젝트/자격증명 없이도 `validate`는 문법·타입만 검사하므로 통과해야 한다. `gcp_project_id`가 없으면 init/validate 단계에서 변수 프롬프트가 뜰 수 있으니, 검증만 할 때는 `terraform validate` 실행 전에 `TF_VAR_gcp_project_id=dummy-project TF_VAR_allowed_admin_cidr=203.0.113.10/32` 환경변수를 임시로 지정한다.)

```bash
cd infra/terraform/gcp
TF_VAR_gcp_project_id=dummy-project TF_VAR_allowed_admin_cidr=203.0.113.10/32 terraform validate
```

- [ ] **Step 9: 커밋**

```bash
git add infra/terraform/gcp
git commit -m "feat: replace AWS EC2 terraform with GCP stress test infra"
```

---

### Task 2: Prometheus 메트릭 패키지 + HTTP 계측

**Files:**
- Create: `internal/metrics/metrics.go`
- Create: `internal/metrics/middleware.go`
- Create: `internal/metrics/middleware_test.go`
- Modify: `cmd/main.go` (router 설정 부분, 현재 130~148줄 부근)
- Modify: `go.mod`, `go.sum` (via `go get`/`go mod tidy`)

**Interfaces:**
- Produces: `metrics.HTTPRequestsTotal`, `metrics.HTTPRequestDuration` (내부용), `metrics.HTTPMiddleware() gin.HandlerFunc`, `metrics.OrderPipelineMatchLatency prometheus.Histogram`, `metrics.OrderSettlementDuration prometheus.Histogram` — Task 3이 뒤의 두 히스토그램을 사용한다.

- [ ] **Step 1: 의존성 추가**

```bash
go get github.com/prometheus/client_golang
```

- [ ] **Step 2: 메트릭 정의 작성**

`internal/metrics/metrics.go`:

```go
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total HTTP requests processed, labeled by method, path, and status code.",
	}, []string{"method", "path", "status"})

	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request duration in seconds, labeled by method, path, and status code.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path", "status"})

	OrderPipelineMatchLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "order_pipeline_match_latency_seconds",
		Help:    "Time from order enqueue into the matching engine to completion of matching for that order.",
		Buckets: prometheus.DefBuckets,
	})

	OrderSettlementDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "order_settlement_duration_seconds",
		Help:    "Time to persist trade settlement (wallet/ledger updates) after a match event.",
		Buckets: prometheus.DefBuckets,
	})
)
```

- [ ] **Step 3: 실패하는 미들웨어 테스트 작성**

`internal/metrics/middleware_test.go`:

```go
package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPMiddlewareRecordsRequestsTotalAndDuration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(metrics.HTTPMiddleware())
	router.GET("/ping", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	before := testutil.ToFloat64(metrics.HTTPRequestsTotal.WithLabelValues(http.MethodGet, "/ping", "200"))

	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	after := testutil.ToFloat64(metrics.HTTPRequestsTotal.WithLabelValues(http.MethodGet, "/ping", "200"))
	assert.Equal(t, before+1, after)
}

func TestHTTPMiddlewareUsesUnmatchedPathForUnknownRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(metrics.HTTPMiddleware())

	before := testutil.ToFloat64(metrics.HTTPRequestsTotal.WithLabelValues(http.MethodGet, "unmatched", "404"))

	req := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	after := testutil.ToFloat64(metrics.HTTPRequestsTotal.WithLabelValues(http.MethodGet, "unmatched", "404"))
	assert.Equal(t, before+1, after)
}
```

- [ ] **Step 4: 테스트가 실패하는지 확인**

Run: `go test ./internal/metrics/... -run TestHTTPMiddleware -v`
Expected: FAIL (`metrics.HTTPMiddleware`가 아직 존재하지 않아 컴파일 에러)

- [ ] **Step 5: 미들웨어 구현**

`internal/metrics/middleware.go`:

```go
package metrics

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

func HTTPMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		path := c.FullPath()
		if path == "" {
			path = "unmatched"
		}
		status := strconv.Itoa(c.Writer.Status())

		HTTPRequestsTotal.WithLabelValues(c.Request.Method, path, status).Inc()
		HTTPRequestDuration.WithLabelValues(c.Request.Method, path, status).Observe(time.Since(start).Seconds())
	}
}
```

- [ ] **Step 6: 테스트 통과 확인**

Run: `go test ./internal/metrics/... -v`
Expected: PASS

- [ ] **Step 7: `cmd/main.go`에 라우터 계측 연결**

`cmd/main.go` 상단 import 블록에 두 줄 추가:

```go
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
```

(`internal/matching` import 바로 다음 줄에 알파벳 순으로 추가)

```go
	"github.com/prometheus/client_golang/prometheus/promhttp"
```

(`github.com/gin-contrib/cors` 바로 앞줄에 추가)

`r := gin.Default()` 다음, 기존 `r.Use(cors.New(...))` 블록 뒤에 미들웨어와 `/metrics` 라우트를 추가한다. 현재:

```go
	r := gin.Default()

	r.Use(cors.New(cors.Config{
		AllowOrigins: config.CORSAllowedOriginsFromEnv(),
		AllowMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders: []string{"Content-Type", "Authorization", middleware.DevToolsTokenHeader},
	}))

	r.GET("/ping", func(c *gin.Context) {
```

다음과 같이 수정한다:

```go
	r := gin.Default()

	r.Use(cors.New(cors.Config{
		AllowOrigins: config.CORSAllowedOriginsFromEnv(),
		AllowMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders: []string{"Content-Type", "Authorization", middleware.DevToolsTokenHeader},
	}))
	r.Use(metrics.HTTPMiddleware())

	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	r.GET("/ping", func(c *gin.Context) {
```

- [ ] **Step 8: 빌드 및 전체 테스트 확인**

```bash
go mod tidy
go build ./...
go test ./... 2>&1 | tail -40
```

Expected: 빌드 성공, 모든 테스트 PASS.

- [ ] **Step 9: 커밋**

```bash
git add internal/metrics cmd/main.go go.mod go.sum
git commit -m "feat: add prometheus HTTP metrics and /metrics endpoint"
```

---

### Task 3: 매칭엔진 큐잉·정산 지연 지표

**Files:**
- Modify: `internal/matching/order.go` (Order 구조체)
- Modify: `internal/matching/engine.go` (MatchingEngine 구조체, `Start()`)
- Create: `internal/matching/engine_metrics_test.go`
- Modify: `internal/service/order_service.go` (`CreateOrder`)
- Modify: `internal/service/matching_bootstrap_service.go` (`matchingOrderFromModelOrder`)
- Modify: `cmd/main.go` (엔진 초기화, `processTradeSettlement`)
- Modify: `cmd/main_test.go`

**Interfaces:**
- Consumes: `metrics.OrderPipelineMatchLatency`, `metrics.OrderSettlementDuration` (Task 2에서 정의)
- Produces: `matching.Order.EnqueuedAt time.Time` 필드, `matching.MatchingEngine.MatchLatencyObserver func(time.Duration)` 필드 — 이후 다른 태스크는 이 필드를 직접 참조하지 않는다.

- [ ] **Step 1: `Order`에 `EnqueuedAt` 필드 추가**

`internal/matching/order.go`의 `Order` 구조체:

```go
type Order struct {
	ID                uint
	UserID            uint
	Amount            decimal.Decimal
	QuoteAmount       decimal.Decimal
	CoinSymbol        string
	Side              model.OrderSide
	FilledAmount      decimal.Decimal
	FilledQuoteAmount decimal.Decimal
	CreatedAt         time.Time
	EnqueuedAt        time.Time
	OrderType         model.OrderType
	Price             decimal.Decimal
}
```

- [ ] **Step 2: `MatchingEngine`에 `MatchLatencyObserver` 필드 추가**

`internal/matching/engine.go`의 `MatchingEngine` 구조체:

```go
type MatchingEngine struct {
	OrderBook   *OrderBook
	OrderBooks  map[string]*OrderBook
	OrderCh     chan *Order
	CancelCh    chan CancelOrderCommand
	SnapshotReq chan OrderBookSnapshotRequest
	TradeCh     chan *model.Trade
	ExecutionCh chan ExecutionEvent
	SnapshotCh  chan OrderBookSnapshot
	engineID    string
	tradeSeq    int64

	MatchLatencyObserver func(time.Duration)
}
```

- [ ] **Step 3: 실패하는 테스트 작성**

`internal/matching/engine_metrics_test.go`:

```go
package matching

import (
	"sync"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMatchingEngineReportsMatchLatencyAfterProcessingOrder(t *testing.T) {
	me := NewMatchingEngine()

	var mu sync.Mutex
	var observed time.Duration
	done := make(chan struct{}, 1)
	me.MatchLatencyObserver = func(d time.Duration) {
		mu.Lock()
		observed = d
		mu.Unlock()
		done <- struct{}{}
	}
	me.Start()

	me.OrderCh <- &Order{
		ID:         1,
		UserID:     1,
		CoinSymbol: "BTC",
		Side:       model.OrderSideBuy,
		Price:      decimal.NewFromInt(100),
		Amount:     decimal.NewFromInt(1),
		OrderType:  model.OrderTypeLimit,
		EnqueuedAt: time.Now(),
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for match latency observation")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.GreaterOrEqual(t, observed, time.Duration(0))
	require.Less(t, observed, 2*time.Second)
}

func TestMatchingEngineSkipsObserverWhenEnqueuedAtIsZero(t *testing.T) {
	me := NewMatchingEngine()

	called := make(chan struct{}, 1)
	me.MatchLatencyObserver = func(time.Duration) {
		called <- struct{}{}
	}
	me.Start()

	me.OrderCh <- &Order{
		ID:         2,
		UserID:     1,
		CoinSymbol: "BTC",
		Side:       model.OrderSideBuy,
		Price:      decimal.NewFromInt(100),
		Amount:     decimal.NewFromInt(1),
		OrderType:  model.OrderTypeLimit,
	}

	// Drain the snapshot channel so Match() completes without blocking, then
	// give the observer a short window to (not) fire.
	select {
	case <-me.SnapshotCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for snapshot")
	}

	select {
	case <-called:
		t.Fatal("observer should not be called when EnqueuedAt is zero")
	case <-time.After(200 * time.Millisecond):
	}
}
```

- [ ] **Step 4: 테스트가 실패하는지 확인**

Run: `go test ./internal/matching/... -run TestMatchingEngineReportsMatchLatency -v`
Expected: FAIL (observer가 호출되지 않아 타임아웃)

- [ ] **Step 5: `Start()`에서 observer 호출하도록 구현**

`internal/matching/engine.go`의 `Start()` 내부, 현재:

```go
			case order := <-me.OrderCh:
				if order == nil {
					continue
				}
				me.Match(order)
				me.SnapshotCh <- me.GetOrderBookSnapshot(order.CoinSymbol)
```

다음과 같이 수정한다:

```go
			case order := <-me.OrderCh:
				if order == nil {
					continue
				}
				me.Match(order)
				if me.MatchLatencyObserver != nil && !order.EnqueuedAt.IsZero() {
					me.MatchLatencyObserver(time.Since(order.EnqueuedAt))
				}
				me.SnapshotCh <- me.GetOrderBookSnapshot(order.CoinSymbol)
```

- [ ] **Step 6: 테스트 통과 확인**

Run: `go test ./internal/matching/... -v`
Expected: PASS (기존 `engine_test.go`, `engine_concurrency_test.go`, `engine_bench_test.go` 포함 전부)

- [ ] **Step 7: 주문 생성 경로에서 `EnqueuedAt` 설정**

`internal/service/order_service.go` 상단 import에 `"time"` 추가 (기존 `"errors"` 다음 줄, 알파벳 순).

`CreateOrder` 내부, 현재:

```go
	if s.MatchingEngine != nil {
		s.MatchingEngine.OrderCh <- &matching.Order{
			ID:                order.ID,
			UserID:            order.UserID,
			CoinSymbol:        order.CoinSymbol,
			Side:              order.Side,
			Price:             order.Price,
			Amount:            order.Amount,
			QuoteAmount:       matchingQuoteAmountForOrder(order),
			CreatedAt:         order.CreatedAt,
			OrderType:         order.OrderType,
			FilledAmount:      order.FilledAmount,
			FilledQuoteAmount: order.FilledQuoteAmount,
		}
	}
```

다음과 같이 수정한다:

```go
	if s.MatchingEngine != nil {
		s.MatchingEngine.OrderCh <- &matching.Order{
			ID:                order.ID,
			UserID:            order.UserID,
			CoinSymbol:        order.CoinSymbol,
			Side:              order.Side,
			Price:             order.Price,
			Amount:            order.Amount,
			QuoteAmount:       matchingQuoteAmountForOrder(order),
			CreatedAt:         order.CreatedAt,
			EnqueuedAt:        time.Now(),
			OrderType:         order.OrderType,
			FilledAmount:      order.FilledAmount,
			FilledQuoteAmount: order.FilledQuoteAmount,
		}
	}
```

- [ ] **Step 8: 부트스트랩 경로에서도 `EnqueuedAt` 설정**

`internal/service/matching_bootstrap_service.go` 상단 import에 `"time"` 추가 (기존 `"fmt"` 다음 줄).

`matchingOrderFromModelOrder`의 반환 구문, 현재:

```go
	return &matching.Order{
		ID:           order.ID,
		UserID:       order.UserID,
		CoinSymbol:   coinSymbol,
		Side:         order.Side,
		Price:        order.Price,
		Amount:       remaining,
		CreatedAt:    order.CreatedAt,
		OrderType:    order.OrderType,
		FilledAmount: order.FilledAmount,
	}, nil
```

다음과 같이 수정한다:

```go
	return &matching.Order{
		ID:           order.ID,
		UserID:       order.UserID,
		CoinSymbol:   coinSymbol,
		Side:         order.Side,
		Price:        order.Price,
		Amount:       remaining,
		CreatedAt:    order.CreatedAt,
		EnqueuedAt:   time.Now(),
		OrderType:    order.OrderType,
		FilledAmount: order.FilledAmount,
	}, nil
```

- [ ] **Step 9: 정산 지연 지표에 대한 실패하는 테스트 작성**

`cmd/main_test.go` 상단 import에 추가:

```go
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
```

파일 끝에 헬퍼와 테스트를 추가:

```go
func histogramSampleCount(t *testing.T, h prometheus.Histogram) uint64 {
	t.Helper()
	m := &dto.Metric{}
	require.NoError(t, h.Write(m))
	return m.GetHistogram().GetSampleCount()
}

func TestProcessTradeSettlementRecordsSettlementDuration(t *testing.T) {
	before := histogramSampleCount(t, metrics.OrderSettlementDuration)

	settler := &fakeTradeSettler{result: service.SettlementResult{Applied: true, TradeID: 1}}
	processTradeSettlement(testTrade(), settler, nil, func([]byte) {}, discardLogger())

	after := histogramSampleCount(t, metrics.OrderSettlementDuration)
	assert.Equal(t, before+1, after)
}
```

- [ ] **Step 10: 테스트가 실패하는지 확인**

Run: `go test ./cmd/... -run TestProcessTradeSettlementRecordsSettlementDuration -v`
Expected: FAIL (`processTradeSettlement`이 아직 `metrics.OrderSettlementDuration`을 관찰하지 않아 `before`와 `after`가 같음)

- [ ] **Step 11: `cmd/main.go`에 observer와 정산 지연 계측 연결**

import 블록에 추가 (`internal/matching` 다음 줄):

```go
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
```

(이미 Task 2에서 추가했다면 중복 추가하지 않는다 — `grep -n '"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"' cmd/main.go`로 먼저 확인한다.)

`me := matching.NewMatchingEngine()` 다음 줄에 observer 연결을 추가한다. 현재:

```go
	me := matching.NewMatchingEngine()
	me.Start()
```

다음과 같이 수정한다:

```go
	me := matching.NewMatchingEngine()
	me.MatchLatencyObserver = func(d time.Duration) {
		metrics.OrderPipelineMatchLatency.Observe(d.Seconds())
	}
	me.Start()
```

`processTradeSettlement` 함수 내부, 현재:

```go
	result, err := settler.SettleTrade(trade)
	if err != nil {
```

다음과 같이 수정한다:

```go
	settlementStart := time.Now()
	result, err := settler.SettleTrade(trade)
	metrics.OrderSettlementDuration.Observe(time.Since(settlementStart).Seconds())
	if err != nil {
```

- [ ] **Step 12: 테스트 통과 및 전체 빌드/테스트 확인**

```bash
go mod tidy
go build ./...
go test ./... 2>&1 | tail -60
```

Expected: 빌드 성공, 모든 테스트 PASS.

- [ ] **Step 13: 커밋**

```bash
git add internal/matching internal/service cmd/main.go cmd/main_test.go go.mod go.sum
git commit -m "feat: instrument order matching queue latency and settlement duration"
```

---

### Task 4: 서버 인스턴스 docker-compose + Prometheus + Grafana

**Files:**
- Create: `docker-compose.stress.yml`
- Create: `monitoring/prometheus.yml`
- Create: `monitoring/grafana/provisioning/datasources/prometheus.yml`
- Create: `monitoring/grafana/provisioning/dashboards/dashboards.yml`
- Create: `monitoring/grafana/provisioning/dashboards/json/goexchange-stress.json`
- Create: `.env.stress.example`

**Interfaces:**
- Consumes: Task 2/3에서 노출한 `/metrics` 엔드포인트, `http_requests_total`, `http_request_duration_seconds`, `order_pipeline_match_latency_seconds`, `order_settlement_duration_seconds` 메트릭 이름
- Produces: 없음 (배포 설정 파일)

- [ ] **Step 1: `.env.stress.example` 작성**

`.env.stress.example`:

```bash
POSTGRES_DB=goexchange
POSTGRES_USER=goexchange
POSTGRES_PASSWORD=change-me-postgres-password
GOEXCHANGE_JWT_SECRET=change-me-to-a-long-random-secret
GOEXCHANGE_DEV_TOOLS_TOKEN=change-me-dev-tools-token
GOEXCHANGE_CORS_ALLOWED_ORIGINS=http://localhost:3000
GOEXCHANGE_WS_ALLOWED_ORIGINS=http://localhost:3000
GRAFANA_ADMIN_PASSWORD=change-me-grafana-password
```

- [ ] **Step 2: `docker-compose.stress.yml` 작성**

`docker-compose.stress.yml`:

```yaml
name: goexchange-stress

services:
  postgres:
    image: postgres:18-alpine
    container_name: goexchange-stress-postgres
    environment:
      POSTGRES_DB: ${POSTGRES_DB:-goexchange}
      POSTGRES_USER: ${POSTGRES_USER:-goexchange}
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}
    volumes:
      - goexchange-stress-postgres-data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ${POSTGRES_USER:-goexchange} -d ${POSTGRES_DB:-goexchange}"]
      interval: 10s
      timeout: 5s
      retries: 10
    networks:
      - goexchange-stress

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
      GOEXCHANGE_CORS_ALLOWED_ORIGINS: ${GOEXCHANGE_CORS_ALLOWED_ORIGINS:-http://localhost:3000}
      GOEXCHANGE_WS_ALLOWED_ORIGINS: ${GOEXCHANGE_WS_ALLOWED_ORIGINS:-http://localhost:3000}
    ports:
      - "8080:8080"
    healthcheck:
      test: ["CMD-SHELL", "wget -qO- http://localhost:8080/ping >/dev/null || exit 1"]
      interval: 30s
      timeout: 3s
      start_period: 20s
      retries: 5
    networks:
      - goexchange-stress

  postgres-exporter:
    image: quay.io/prometheuscommunity/postgres-exporter:v0.15.0
    container_name: goexchange-stress-postgres-exporter
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      DATA_SOURCE_NAME: postgresql://${POSTGRES_USER:-goexchange}:${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}@postgres:5432/${POSTGRES_DB:-goexchange}?sslmode=disable
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
      - postgres-exporter
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
  goexchange-stress-postgres-data:
  goexchange-stress-grafana-data:
```

- [ ] **Step 3: Prometheus 스크레이핑 설정 작성**

`monitoring/prometheus.yml`:

```yaml
global:
  scrape_interval: 5s

scrape_configs:
  - job_name: goexchange-backend
    metrics_path: /metrics
    static_configs:
      - targets: ["backend:8080"]

  - job_name: node-exporter
    static_configs:
      - targets: ["node-exporter:9100"]

  - job_name: postgres-exporter
    static_configs:
      - targets: ["postgres-exporter:9187"]
```

- [ ] **Step 4: Grafana 데이터소스 프로비저닝 작성**

`monitoring/grafana/provisioning/datasources/prometheus.yml`:

```yaml
apiVersion: 1

datasources:
  - name: Prometheus
    uid: prometheus
    type: prometheus
    access: proxy
    url: http://prometheus:9090
    isDefault: true
```

- [ ] **Step 5: Grafana 대시보드 프로비저닝 설정 작성**

`monitoring/grafana/provisioning/dashboards/dashboards.yml`:

```yaml
apiVersion: 1

providers:
  - name: goexchange
    orgId: 1
    folder: ''
    type: file
    options:
      path: /etc/grafana/provisioning/dashboards/json
```

- [ ] **Step 6: Grafana 대시보드 JSON 작성**

`monitoring/grafana/provisioning/dashboards/json/goexchange-stress.json`:

```json
{
  "title": "GoExchange Stress Test",
  "uid": "goexchange-stress",
  "timezone": "browser",
  "schemaVersion": 39,
  "version": 1,
  "refresh": "5s",
  "time": { "from": "now-15m", "to": "now" },
  "panels": [
    {
      "id": 1,
      "title": "Go 런타임 (goroutine / GC / 힙 메모리)",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 0, "y": 0 },
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "targets": [
        { "refId": "A", "expr": "go_goroutines", "legendFormat": "goroutines" },
        { "refId": "B", "expr": "rate(go_gc_duration_seconds_sum[1m])", "legendFormat": "gc seconds/sec" },
        { "refId": "C", "expr": "go_memstats_heap_alloc_bytes", "legendFormat": "heap alloc bytes" }
      ]
    },
    {
      "id": 2,
      "title": "HTTP 엔드포인트 QPS / p95 지연",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 12, "y": 0 },
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "targets": [
        { "refId": "A", "expr": "sum(rate(http_requests_total[1m])) by (path)", "legendFormat": "{{path}} qps" },
        { "refId": "B", "expr": "histogram_quantile(0.95, sum(rate(http_request_duration_seconds_bucket[1m])) by (le, path))", "legendFormat": "{{path}} p95" }
      ]
    },
    {
      "id": 3,
      "title": "매칭/정산 파이프라인 지연 (p95)",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 0, "y": 8 },
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "targets": [
        { "refId": "A", "expr": "histogram_quantile(0.95, rate(order_pipeline_match_latency_seconds_bucket[1m]))", "legendFormat": "match latency p95" },
        { "refId": "B", "expr": "histogram_quantile(0.95, rate(order_settlement_duration_seconds_bucket[1m]))", "legendFormat": "settlement duration p95" }
      ]
    },
    {
      "id": 4,
      "title": "머신 자원 (CPU / 메모리)",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 12, "y": 8 },
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "targets": [
        { "refId": "A", "expr": "100 - (avg(rate(node_cpu_seconds_total{mode=\"idle\"}[1m])) * 100)", "legendFormat": "CPU 사용률(%)" },
        { "refId": "B", "expr": "(1 - (node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes)) * 100", "legendFormat": "메모리 사용률(%)" }
      ]
    },
    {
      "id": 5,
      "title": "Postgres 커넥션 / 커밋 통계",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 0, "y": 16 },
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "targets": [
        { "refId": "A", "expr": "pg_stat_activity_count", "legendFormat": "{{state}} 커넥션" },
        { "refId": "B", "expr": "rate(pg_stat_database_xact_commit{datname=\"goexchange\"}[1m])", "legendFormat": "commits/sec" }
      ]
    }
  ]
}
```

- [ ] **Step 7: 문법 검증**

```bash
docker compose -f docker-compose.stress.yml --env-file .env.stress.example config >/dev/null
python3 -c "import json; json.load(open('monitoring/grafana/provisioning/dashboards/json/goexchange-stress.json'))"
```

Expected: 둘 다 에러 없이 종료 (첫 번째 명령은 병합된 compose 설정을 출력만 하고 실제 컨테이너를 띄우지 않는다).

- [ ] **Step 8: 커밋**

```bash
git add docker-compose.stress.yml monitoring .env.stress.example
git commit -m "feat: add stress test docker-compose stack with prometheus and grafana"
```

---

### Task 5: k6 스트레스 시나리오

**Files:**
- Create: `loadtest/order-submission-stress.js`

**Interfaces:**
- Consumes: 없음 (기존 `loadtest/order-submission-baseline.js`의 구조를 참고하되 독립된 파일)
- Produces: 없음 (실행 스크립트)

- [ ] **Step 1: 스크립트 작성**

`loadtest/order-submission-stress.js`:

```javascript
import http from 'k6/http';
import { check, sleep } from 'k6';
import exec from 'k6/execution';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const DEV_TOOLS_TOKEN = __ENV.DEV_TOOLS_TOKEN;
const DEV_TOOLS_TOKEN_HEADER = 'X-GoExchange-Dev-Token';

const TOTAL_USERS = 800;
const COIN_SYMBOL = 'BTC';
const FIXED_PRICE = '50000000';
const ORDER_AMOUNT = '0.001';
const BUYER_KRW_FUNDING = '100000000';
const SELLER_BTC_FUNDING = '1000';

// 사전에 정의된 목표 수치나 임계값 없이, Grafana 대시보드를 보면서 에러율 급증이나
// p95 급격 저하가 관측되는 시점에 수동으로 실행을 중단한다. 800 VU까지도 시스템이
// 버틴다면 이 배열에 단계를 더 추가해서 계속 밀어붙일 수 있다.
const STRESS_STAGES = [
  { duration: '2m', target: 50 },
  { duration: '2m', target: 100 },
  { duration: '2m', target: 200 },
  { duration: '2m', target: 400 },
  { duration: '2m', target: 800 },
];

export const options = {
  scenarios: {
    order_submission_stress: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: STRESS_STAGES,
      exec: 'submitOrders',
    },
  },
};

export function setup() {
  if (!DEV_TOOLS_TOKEN) {
    throw new Error(
      'DEV_TOOLS_TOKEN environment variable is required (pass -e DEV_TOOLS_TOKEN=<value> matching the server\'s GOEXCHANGE_DEV_TOOLS_TOKEN)'
    );
  }

  const users = [];
  for (let i = 1; i <= TOTAL_USERS; i++) {
    // 홀/짝수로 역할을 배정해, VU ID가 낮은 구간(램프업 초반)에도 매수자/매도자가
    // 함께 활성화되도록 한다.
    const role = i % 2 === 1 ? 'buyer' : 'seller';
    const email = `stress-user-${i}@test.local`;

    // 가입-또는-로그인 폴백: 같은 테스트 DB에 스크립트를 여러 번 실행해도
    // 안전하게 재실행 가능하도록 한다.
    const registerRes = http.post(
      `${BASE_URL}/auth/register`,
      JSON.stringify({
        name: `Stress Test User ${i}`,
        email: email,
        password: 'loadtest-password-123',
      }),
      { headers: { 'Content-Type': 'application/json' }, tags: { name: 'setup' } }
    );

    let token;
    if (registerRes.status === 201) {
      token = registerRes.json('data.token');
    } else if (registerRes.status === 409) {
      const loginRes = http.post(
        `${BASE_URL}/auth/login`,
        JSON.stringify({ email: email, password: 'loadtest-password-123' }),
        { headers: { 'Content-Type': 'application/json' }, tags: { name: 'setup' } }
      );
      if (loginRes.status !== 200) {
        throw new Error(
          `setup: user ${i} (${email}) already registered but login failed: ${loginRes.status} ${loginRes.body}`
        );
      }
      token = loginRes.json('data.token');
    } else {
      throw new Error(
        `setup: failed to register user ${i} (${email}): ${registerRes.status} ${registerRes.body}`
      );
    }

    const fundBody =
      role === 'buyer'
        ? { coin_symbol: 'KRW', amount: BUYER_KRW_FUNDING }
        : { coin_symbol: COIN_SYMBOL, amount: SELLER_BTC_FUNDING };

    const fundRes = http.post(`${BASE_URL}/dev/wallets/fund`, JSON.stringify(fundBody), {
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${token}`,
        [DEV_TOOLS_TOKEN_HEADER]: DEV_TOOLS_TOKEN,
      },
      tags: { name: 'setup' },
    });

    if (fundRes.status !== 200) {
      throw new Error(
        `setup: failed to fund wallet for user ${i} (${email}): ${fundRes.status} ${fundRes.body}`
      );
    }

    users.push({ token, role });
  }

  return { users };
}

export function submitOrders(data) {
  const vuIndex = (exec.vu.idInTest - 1) % data.users.length;
  const user = data.users[vuIndex];

  const orderBody = {
    coin_symbol: COIN_SYMBOL,
    side: user.role === 'buyer' ? 'BUY' : 'SELL',
    order_type: 'LIMIT',
    price: FIXED_PRICE,
    amount: ORDER_AMOUNT,
  };

  const res = http.post(`${BASE_URL}/orders`, JSON.stringify(orderBody), {
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${user.token}`,
    },
    tags: { name: 'create_order' },
  });

  check(res, {
    'order accepted (status 200)': (r) => r.status === 200,
  });

  sleep(0.2 + Math.random() * 0.3);
}
```

- [ ] **Step 2: 문법 검증**

```bash
k6 inspect loadtest/order-submission-stress.js
```

Expected: JSON 형태로 시나리오 설정(`order_submission_stress`, stages 5개)이 출력됨. 에러 없이 종료.

- [ ] **Step 3: 커밋**

```bash
git add loadtest/order-submission-stress.js
git commit -m "feat: add k6 ramping stress scenario for order submission"
```

---

### Task 6: GCP 스트레스 테스트 실행 문서

**Files:**
- Create: `docs/gcp-stress-test-runbook.md`

**Interfaces:**
- Consumes: Task 1의 Terraform output 이름(`server_external_ip`, `server_internal_ip`, `load_gen_external_ip`, `server_ssh_command`, `load_gen_ssh_command`), Task 4의 `docker-compose.stress.yml`/`.env.stress.example`, Task 5의 `loadtest/order-submission-stress.js`, 기존 `docs/benchmarks/` 컨벤션
- Produces: 없음 (실행 문서)

- [ ] **Step 1: 런북 작성**

`docs/gcp-stress-test-runbook.md`:

```markdown
# GCP 분리 환경 스트레스 테스트 실행 런북

`docs/superpowers/specs/2026-07-06-gcp-stress-test-design.md`의 설계를 실제로 실행하는 순서입니다.
이 문서의 단계들은 실제 GCP 비용이 발생하므로, 각 단계를 실행하기 전에 내용을 이해하고 진행하세요.

## 1. 인프라 생성

`infra/terraform/gcp/README.md`를 따라 `terraform apply`를 실행합니다. 완료되면 아래 출력값을 기록해둡니다.

```bash
terraform output
```

- `server_external_ip`, `server_internal_ip`
- `load_gen_external_ip`
- `server_ssh_command`, `load_gen_ssh_command`

## 2. 서버 인스턴스에 코드 배포

로컬에서 저장소를 서버 인스턴스로 복사합니다 (git clone도 가능하지만, 사설 저장소라면 scp가 더 간단합니다).

```bash
scp -r -i ~/.ssh/goexchange-gcp . goexchange@<server_external_ip>:~/go-exchange-back
```

## 3. 환경변수 파일 준비

서버 인스턴스에 SSH 접속 후:

```bash
ssh -i ~/.ssh/goexchange-gcp goexchange@<server_external_ip>
cd ~/go-exchange-back
cp .env.stress.example .env
# .env를 열어 POSTGRES_PASSWORD, GOEXCHANGE_JWT_SECRET, GOEXCHANGE_DEV_TOOLS_TOKEN,
# GRAFANA_ADMIN_PASSWORD를 실제 값으로 채운다.
```

## 4. 스택 기동

```bash
docker compose -f docker-compose.stress.yml up -d --build
docker compose -f docker-compose.stress.yml ps
```

`backend`, `postgres`, `prometheus`, `grafana`, `node-exporter`, `postgres-exporter` 6개 컨테이너가 모두 `Up` (backend/postgres는 `healthy`)이어야 한다.

## 5. Grafana 확인

로컬 브라우저에서 `http://<server_external_ip>:3000`에 접속해, `admin` / `.env`에 설정한 `GRAFANA_ADMIN_PASSWORD`로 로그인한다. "GoExchange Stress Test" 대시보드가 보이는지 확인한다.

## 6. 부하생성 인스턴스에서 k6 실행

```bash
ssh -i ~/.ssh/goexchange-gcp goexchange@<load_gen_external_ip>
sudo snap install k6   # 또는 https://k6.io/docs/get-started/installation/ 의 우분투 설치 방법
```

로컬에서 `loadtest/order-submission-stress.js`를 부하생성 인스턴스로 복사한 뒤 실행한다.

```bash
scp -i ~/.ssh/goexchange-gcp loadtest/order-submission-stress.js goexchange@<load_gen_external_ip>:~/order-submission-stress.js
ssh -i ~/.ssh/goexchange-gcp goexchange@<load_gen_external_ip> \
  "k6 run -e BASE_URL=http://<server_internal_ip>:8080 -e DEV_TOOLS_TOKEN=<GOEXCHANGE_DEV_TOOLS_TOKEN 값> ~/order-submission-stress.js"
```

`server_internal_ip`를 쓰는 이유는 같은 VPC 안에서는 내부 IP가 더 빠르고, 외부 IP 대역폭/과금을 피할 수 있기 때문이다.

## 7. 실시간 관찰과 수동 종료

Grafana 대시보드(5번 단계에서 연 탭)를 계속 보면서, 다음 중 하나가 관측되면 k6 실행 터미널에서 `Ctrl+C`로 중단한다.

- `http_req_failed`(또는 `create_order` 태그의 에러율)이 급격히 증가
- HTTP p95 응답시간이 이전 단계 대비 몇 배 이상 급격히 저하
- CPU/메모리 패널이 포화(90%+)에 근접하고 다른 지표도 함께 무너짐
- Postgres 커넥션 수가 한계에 도달하는 신호

어느 패널이 가장 먼저 무너지는지, 몇 VU 근처에서 그랬는지를 기록해둔다.

## 8. 결과 기록

k6 종료 시 출력되는 요약과, Grafana 대시보드 스크린샷(문제가 시작된 시점 전후)을 캡처해서 기존 컨벤션대로 저장한다.

- `docs/benchmarks/03-YYYY-MM-DD-gcp-stress-test.md` 생성 (형식은 `docs/benchmarks/README.md` 참고)
- k6 요약, 병목이 관측된 VU 구간, 병목 원인(CPU/메모리/GC/DB 커넥션/매칭엔진 큐잉 중 무엇이었는지) 서술
- Grafana 스크린샷은 `docs/benchmarks/`에 이미지 파일로 함께 커밋하거나, 스크린샷 없이 관측한 수치(예: "CPU 92%, p95 1.2s, order_pipeline_match_latency_seconds p95 3.4s")를 텍스트로 남긴다
- `docs/benchmarks/README.md` 목록에 항목 추가

## 9. 인스턴스 정리

결과 기록 및 추가 분석이 끝나면(며칠 이내), 비용을 막기 위해 인스턴스를 삭제한다.

```bash
cd infra/terraform/gcp
terraform destroy
```
```

- [ ] **Step 2: 커밋**

```bash
git add docs/gcp-stress-test-runbook.md
git commit -m "docs: add GCP stress test execution runbook"
```
