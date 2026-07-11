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

output "db_internal_ip" {
  description = "DB 인스턴스의 내부 IP (외부 IP 없음)."
  value       = google_compute_instance.db.network_interface[0].network_ip
}

output "db_ssh_command" {
  description = "DB 인스턴스 SSH 접속 명령 (IAP 터널, roles/iap.tunnelResourceAccessor 필요)."
  value       = "gcloud compute ssh ${google_compute_instance.db.name} --zone ${var.gcp_zone} --tunnel-through-iap"
}
