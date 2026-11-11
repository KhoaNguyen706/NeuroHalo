# NeuroHalo on AWS, for a short run: one EC2 machine that clones the
# repository and starts the same Docker Compose stack that runs on a laptop.
#
#   terraform init
#   terraform apply -var "allowed_cidr=$(curl -s https://checkip.amazonaws.com)/32"
#   terraform destroy          # when the run is over; the machine costs money while it exists
#
# Everything is reachable only from allowed_cidr. Grafana opens without a
# login, so never set allowed_cidr to 0.0.0.0/0.

terraform {
  required_version = ">= 1.6"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}

variable "region" {
  description = "AWS region to run in."
  type        = string
  default     = "us-east-1"
}

variable "allowed_cidr" {
  description = "The only address range that may reach the machine, for example 203.0.113.7/32 (your own IP)."
  type        = string

  validation {
    condition     = can(cidrhost(var.allowed_cidr, 0)) && var.allowed_cidr != "0.0.0.0/0"
    error_message = "allowed_cidr must be a CIDR range and must not be 0.0.0.0/0."
  }
}

variable "instance_type" {
  description = "Machine size. The 1,000-session load test needs about 8 CPUs and 16 GB."
  type        = string
  default     = "c7i.2xlarge"
}

variable "key_name" {
  description = "Name of an existing EC2 key pair for SSH. Leave empty for no SSH access."
  type        = string
  default     = ""
}

variable "repo_url" {
  description = "Git repository the machine clones."
  type        = string
  default     = "https://github.com/KhoaNguyen706/NeuroHalo.git"
}

provider "aws" {
  region = var.region
}

# The token clients send to the gateway and the api, instead of "dev-token".
resource "random_password" "token" {
  length  = 32
  special = false
}

# The newest Ubuntu 24.04 image, published by Canonical.
data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["099720109477"]

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"]
  }
}

# The account's default network; a short run does not need its own.
data "aws_vpc" "default" {
  default = true
}

locals {
  # What is opened to allowed_cidr.
  ports = {
    web     = 3000
    api     = 8080
    grafana = 3001
    gateway = 50051
  }
}

resource "aws_security_group" "neurohalo" {
  name        = "neurohalo"
  description = "NeuroHalo: web, api, Grafana and gateway, from one address range only"
  vpc_id      = data.aws_vpc.default.id

  dynamic "ingress" {
    for_each = local.ports
    content {
      description = ingress.key
      from_port   = ingress.value
      to_port     = ingress.value
      protocol    = "tcp"
      cidr_blocks = [var.allowed_cidr]
    }
  }

  dynamic "ingress" {
    for_each = var.key_name == "" ? [] : [22]
    content {
      description = "ssh"
      from_port   = 22
      to_port     = 22
      protocol    = "tcp"
      cidr_blocks = [var.allowed_cidr]
    }
  }

  egress {
    description = "all outbound: image pulls, git clone, sample downloads"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

# A fixed public address, known before the machine starts, so the web app
# can be built with the address browsers will reach the api at.
resource "aws_eip" "neurohalo" {
  domain = "vpc"
}

resource "aws_instance" "neurohalo" {
  ami                    = data.aws_ami.ubuntu.id
  instance_type          = var.instance_type
  key_name               = var.key_name == "" ? null : var.key_name
  vpc_security_group_ids = [aws_security_group.neurohalo.id]

  root_block_device {
    volume_type = "gp3"
    volume_size = 40
  }

  metadata_options {
    http_tokens = "required"
  }

  # Runs once on first boot: install Docker, clone the repository, write the
  # settings Docker Compose reads from .env, and start the stack.
  user_data = <<-EOT
    #!/bin/bash
    set -euxo pipefail
    curl -fsSL https://get.docker.com | sh
    git clone --depth 1 ${var.repo_url} /opt/neurohalo
    cd /opt/neurohalo
    cat > .env <<EOF
    INGEST_TOKEN=${random_password.token.result}
    PUBLIC_API_URL=http://${aws_eip.neurohalo.public_ip}:8080
    WEB_ORIGIN=http://${aws_eip.neurohalo.public_ip}:3000
    EOF
    docker compose up -d --build
  EOT

  tags = {
    Name = "neurohalo"
  }
}

resource "aws_eip_association" "neurohalo" {
  instance_id   = aws_instance.neurohalo.id
  allocation_id = aws_eip.neurohalo.id
}

output "app_url" {
  description = "The web app. The first start takes about 10 minutes (image builds)."
  value       = "http://${aws_eip.neurohalo.public_ip}:3000"
}

output "grafana_url" {
  description = "The pipeline dashboard."
  value       = "http://${aws_eip.neurohalo.public_ip}:3001"
}

output "api_token" {
  description = "The bearer token for the api and the gateway: terraform output -raw api_token"
  value       = random_password.token.result
  sensitive   = true
}
