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
