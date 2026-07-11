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
  target_tags   = ["goexchange-server", "goexchange-loadgen", "goexchange-db"]

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

  # 외부 IP 없음(C-1): 원장/지갑 데이터가 있는 DB는 공인망에 노출하지 않는다.
  # egress(도커 이미지 pull, apt)는 Cloud NAT로, SSH는 IAP 터널로 대체한다.
  network_interface {
    network    = google_compute_network.this.id
    subnetwork = google_compute_subnetwork.this.id
  }

  metadata = {
    ssh-keys = local.ssh_keys
  }
}

# C-1: 외부 IP가 없는 DB 인스턴스의 아웃바운드(도커 이미지 pull, apt 업데이트)용 NAT.
# 외부 IP를 가진 인스턴스(server, load_gen)는 NAT를 거치지 않으므로 영향 없다.
resource "google_compute_router" "this" {
  name    = "${local.name_prefix}-router"
  network = google_compute_network.this.id
  region  = var.gcp_region
}

resource "google_compute_router_nat" "this" {
  name                               = "${local.name_prefix}-nat"
  router                             = google_compute_router.this.name
  region                             = var.gcp_region
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "LIST_OF_SUBNETWORKS"

  subnetwork {
    name                    = google_compute_subnetwork.this.id
    source_ip_ranges_to_nat = ["ALL_IP_RANGES"]
  }
}

# C-1: 외부 IP가 없는 DB 인스턴스 SSH용 IAP 터널 허용.
# 35.235.240.0/20은 GCP IAP TCP 포워딩의 고정 소스 대역이다.
resource "google_compute_firewall" "allow_iap_ssh" {
  name          = "${local.name_prefix}-allow-iap-ssh"
  network       = google_compute_network.this.id
  source_ranges = ["35.235.240.0/20"]
  target_tags   = ["goexchange-db"]

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}

# C-2: DB 부트 디스크 일일 스냅샷. 원장/지갑/체결이 이 디스크 한 장에만 존재하므로
# 디스크 장애 = 전 고객 잔고 소실이다. 디스크 스냅샷은 crash-consistent지만
# Postgres는 전원 차단과 동일한 시나리오를 WAL 복구로 처리한다.
resource "google_compute_resource_policy" "db_daily_snapshot" {
  name   = "${local.name_prefix}-db-daily-snapshot"
  region = var.gcp_region

  snapshot_schedule_policy {
    schedule {
      daily_schedule {
        days_in_cycle = 1
        start_time    = "19:00" # UTC 19:00 = KST 04:00, 트래픽 최저 시간대
      }
    }

    retention_policy {
      max_retention_days    = var.db_snapshot_retention_days
      on_source_disk_delete = "KEEP_AUTO_SNAPSHOTS"
    }

    snapshot_properties {
      storage_locations = [var.gcp_region]
      labels = {
        app  = "goexchange"
        role = "db-backup"
      }
    }
  }
}

resource "google_compute_disk_resource_policy_attachment" "db_boot_snapshot" {
  # initialize_params로 만든 부트 디스크의 이름은 인스턴스 이름과 같다.
  name = google_compute_resource_policy.db_daily_snapshot.name
  disk = google_compute_instance.db.name
  zone = var.gcp_zone
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
    ports    = ["5432", "9187"]
  }
}
