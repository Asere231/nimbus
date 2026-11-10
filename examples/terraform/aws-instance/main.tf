# An EC2 instance that really boots, in a VPC, with a key pair and a
# user-data script that really runs.
#
#   1. nimbus start --clouds aws           (in another terminal)
#   2. terraform init                      (needs internet once, or a
#                                           provider mirror: see docs/OFFLINE.md)
#   3. terraform apply                     and watch http://localhost:4700
#   4. nimbus ssh web                      a shell on the instance
#      nimbus ssh web -- cat /var/log/nimbus-boot.log
#   5. aws --endpoint-url http://localhost:4702 ec2 stop-instances  --instance-ids <id>
#      aws --endpoint-url http://localhost:4702 ec2 start-instances --instance-ids <id>
#   6. terraform destroy
#
# Docker must be running. The first apply pulls alpine:3.20.
#
# What is real: the instance is a container that boots, runs your user data,
# keeps its filesystem across a stop and start, and serves its own instance
# metadata. What is not: there is no kernel of its own, no EBS volume, and
# **no sshd** -- Nimbus is stdlib-only, so it cannot run an SSH server, and
# no official base image ships one. `nimbus ssh` is `docker exec`, and the
# key pair below is recorded and written to the instance's authorized_keys
# so the machine looks the way this file says it does. `ssh -i key.pem` does
# not work, and that is a limitation rather than a bug.

terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

# The aws CLI needs its own region and credentials -- a provider block does
# not reach it:
#
#   export AWS_ACCESS_KEY_ID=nimbus AWS_SECRET_ACCESS_KEY=nimbus
#   export AWS_DEFAULT_REGION=us-east-1
provider "aws" {
  region     = var.region
  access_key = "nimbus"
  secret_key = "nimbus"

  # Nimbus verifies no signature and has no account to look up, so the
  # provider must not try to validate either.
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
  skip_region_validation      = true
  s3_use_path_style           = true

  endpoints {
    ec2 = var.nimbus_endpoint
    iam = var.nimbus_endpoint
    sts = var.nimbus_endpoint
    s3  = var.nimbus_endpoint
  }
}

variable "region" {
  type    = string
  default = "us-east-1"
}

variable "nimbus_endpoint" {
  type        = string
  default     = "http://localhost:4702"
  description = "Nimbus's AWS port. One endpoint serves every service."
}

variable "instance_image" {
  type        = string
  default     = "alpine:3.20"
  description = <<-TEXT
    The container image behind the instance. An AMI id cannot choose it: the
    provider never asks what an AMI means -- it sends the id to RunInstances
    and nothing else -- so the id below is opaque and this tag is how a
    config says what to really run. Any image the Docker daemon can pull
    works, including one pushed to the emulated ACR in the azure-app example.
  TEXT
}

# --- the network --------------------------------------------------------------

resource "aws_vpc" "main" {
  cidr_block           = "10.0.0.0/16"
  enable_dns_hostnames = true

  tags = { Name = "nimbus-instance" }
}

resource "aws_subnet" "main" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.0.1.0/24"
  availability_zone = "${var.region}a"

  tags = { Name = "nimbus-instance-a" }
}

# Recorded and never enforced: no packet in Nimbus is filtered, the same
# honest limitation IAM and Azure RBAC have. It is here because a real
# config has one, and because the graph draws it.
resource "aws_security_group" "web" {
  name        = "nimbus-web"
  description = "ssh and http, for show"
  vpc_id      = aws_vpc.main.id

  ingress {
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = { Name = "nimbus-web" }
}

# aws_key_pair always IMPORTS: it has no mode where AWS generates a key, so
# the action on the wire is ImportKeyPair and the public key below is yours.
# Replace it with your own if you like; nothing verifies it, because there
# is no sshd to verify it against.
resource "aws_key_pair" "web" {
  key_name   = "nimbus-web"
  public_key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleKeyForNimbusLocalEmulatorOnly nimbus@local"
}

# --- the instance -------------------------------------------------------------

resource "aws_instance" "web" {
  ami           = "ami-0abcdef1234567890"
  instance_type = "t3.micro"
  subnet_id     = aws_subnet.main.id
  key_name      = aws_key_pair.web.key_name

  vpc_security_group_ids = [aws_security_group.web.id]

  # This really runs, inside the container, at boot. Its output goes to the
  # instance's console (`docker logs`) and to the log file below.
  user_data = <<-SH
    #!/bin/sh
    echo "booted at $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "I am $(hostname), address $(hostname -i)"

    # The instance can read its own metadata. Real IMDS is
    # 169.254.169.254, which a container cannot reach without root, so
    # Nimbus serves it on a port and puts the address in
    # AWS_EC2_METADATA_SERVICE_ENDPOINT -- which every current SDK reads.
    echo "instance-id: $(wget -q -O- "$AWS_EC2_METADATA_SERVICE_ENDPOINT/latest/meta-data/instance-id")"
  SH

  tags = {
    Name = "web"

    # Which image backs it. See the variable's description.
    "nimbus:image" = var.instance_image
  }
}

output "instance_id" {
  value = aws_instance.web.id
}

output "private_ip" {
  value       = aws_instance.web.private_ip
  description = "Derived from the instance id inside the subnet, so it is the same on every read."
}

output "shell" {
  value       = "nimbus ssh web"
  description = "A shell on the instance. docker exec, not SSH -- see the note at the top."
}

output "console" {
  value       = "nimbus ssh web -- cat /var/log/nimbus-boot.log"
  description = "What the user-data script printed."
}
