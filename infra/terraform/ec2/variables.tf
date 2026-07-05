variable "aws_region" {
  description = "AWS region where the EC2 instance will be created."
  type        = string
  default     = "ap-northeast-2"
}

variable "aws_profile" {
  description = "Local AWS CLI profile name used by Terraform."
  type        = string
  default     = "goexchange"
}

variable "project_name" {
  description = "Project name used for AWS resource names and tags."
  type        = string
  default     = "goexchange"
}

variable "environment" {
  description = "Deployment environment name."
  type        = string
  default     = "dev"
}

variable "instance_type" {
  description = "EC2 instance type."
  type        = string
  default     = "t3.micro"
}

variable "root_volume_size_gb" {
  description = "Root EBS volume size in GiB."
  type        = number
  default     = 20

  validation {
    condition     = var.root_volume_size_gb >= 8
    error_message = "root_volume_size_gb must be at least 8."
  }
}

variable "allowed_ssh_cidr" {
  description = "CIDR block allowed to connect to SSH. Use your public IP with /32."
  type        = string

  validation {
    condition     = can(cidrhost(var.allowed_ssh_cidr, 0))
    error_message = "allowed_ssh_cidr must be a valid CIDR block, for example 203.0.113.10/32."
  }
}

variable "key_pair_name" {
  description = "AWS EC2 key pair name to create from the local public key."
  type        = string
  default     = "goexchange-ec2"
}

variable "public_key_path" {
  description = "Path to the local SSH public key that will be registered as an EC2 key pair."
  type        = string
  default     = "~/.ssh/goexchange-ec2.pub"
}
