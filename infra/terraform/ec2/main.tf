locals {
  name_prefix = "${var.project_name}-${var.environment}"

  common_tags = {
    Project     = var.project_name
    Environment = var.environment
    ManagedBy   = "terraform"
  }
}

data "aws_vpc" "default" {
  default = true
}

data "aws_subnets" "default" {
  filter {
    name   = "vpc-id"
    values = [data.aws_vpc.default.id]
  }
}

data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["099720109477"]

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd*/ubuntu-noble-24.04-amd64-server-*"]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

resource "aws_key_pair" "this" {
  key_name   = var.key_pair_name
  public_key = trimspace(file(pathexpand(var.public_key_path)))

  tags = merge(local.common_tags, {
    Name = "${local.name_prefix}-key"
  })
}

resource "aws_security_group" "this" {
  name        = "${local.name_prefix}-sg"
  description = "Security group for GoExchange EC2 deployment"
  vpc_id      = data.aws_vpc.default.id

  ingress {
    description = "SSH from local operator IP"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [var.allowed_ssh_cidr]
  }

  ingress {
    description = "HTTP frontend"
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  ingress {
    description = "Backend API for initial MVP deployment"
    from_port   = 8080
    to_port     = 8080
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    description = "Allow all outbound traffic"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.common_tags, {
    Name = "${local.name_prefix}-sg"
  })
}

resource "aws_instance" "this" {
  ami                         = data.aws_ami.ubuntu.id
  instance_type               = var.instance_type
  subnet_id                   = element(sort(data.aws_subnets.default.ids), 0)
  vpc_security_group_ids      = [aws_security_group.this.id]
  key_name                    = aws_key_pair.this.key_name
  associate_public_ip_address = true

  root_block_device {
    volume_type = "gp3"
    volume_size = var.root_volume_size_gb
    encrypted   = true
  }

  tags = merge(local.common_tags, {
    Name = "${local.name_prefix}-ec2"
  })
}

resource "aws_eip" "this" {
  domain   = "vpc"
  instance = aws_instance.this.id

  tags = merge(local.common_tags, {
    Name = "${local.name_prefix}-eip"
  })
}
