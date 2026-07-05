output "public_ip" {
  description = "Elastic public IP address attached to the EC2 instance."
  value       = aws_eip.this.public_ip
}

output "ssh_command" {
  description = "SSH command for connecting to the EC2 instance."
  value       = "ssh -i ${replace(pathexpand(var.public_key_path), ".pub", "")} ubuntu@${aws_eip.this.public_ip}"
}

output "frontend_url" {
  description = "Frontend HTTP URL."
  value       = "http://${aws_eip.this.public_ip}"
}

output "backend_ping_url" {
  description = "Backend health check URL."
  value       = "http://${aws_eip.this.public_ip}:8080/ping"
}
