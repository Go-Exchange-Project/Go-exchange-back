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
