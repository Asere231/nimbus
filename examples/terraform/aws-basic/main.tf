# A small AWS stack that runs fully offline against Nimbus.
#
#   1. nimbus start --clouds azure,aws   (in another terminal)
#   2. terraform init                    (needs internet once, or a provider
#                                         mirror: see docs/OFFLINE.md)
#   3. terraform apply                   and watch http://localhost:4700
#   4. terraform destroy
#
# Plain HTTP, unlike the Azure examples. No AWS client insists on TLS, and S3
# is addressed path-style, so there is no bucket hostname needing a
# certificate -- which means nothing has to be trusted on your machine.

terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = var.region

  # Credentials are accepted on shape alone: Nimbus never verifies a
  # signature, so these can be anything.
  access_key = "nimbus"
  secret_key = "nimbus"

  # Everything below stops the provider reaching for the real AWS. Without
  # them it tries to validate the credentials, look up the account id and
  # read EC2 instance metadata, none of which exist here.
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
  skip_region_validation      = true

  # Path-style addressing: /bucket/key rather than bucket.host/key.
  s3_use_path_style = true

  endpoints {
    sts = var.nimbus_endpoint
    iam = var.nimbus_endpoint
    ec2 = var.nimbus_endpoint
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

# --- networking -------------------------------------------------------------

resource "aws_vpc" "main" {
  cidr_block           = "10.0.0.0/16"
  enable_dns_hostnames = true
  tags                 = { Name = "nimbus-vpc" }
}

resource "aws_subnet" "public" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.0.1.0/24"
  availability_zone = "${var.region}a"
  tags              = { Name = "nimbus-public" }
}

resource "aws_subnet" "private" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.0.2.0/24"
  availability_zone = "${var.region}b"
  tags              = { Name = "nimbus-private" }
}

resource "aws_internet_gateway" "igw" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = "nimbus-igw" }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.igw.id
  }

  tags = { Name = "nimbus-public" }
}

resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}

resource "aws_security_group" "web" {
  name        = "nimbus-web"
  description = "HTTPS in, everything out"
  vpc_id      = aws_vpc.main.id

  ingress {
    description = "HTTPS"
    from_port   = 443
    to_port     = 443
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

# --- storage ----------------------------------------------------------------

resource "aws_s3_bucket" "data" {
  bucket = "nimbus-data"

  # Lets destroy empty the bucket first. S3 refuses to delete one that is
  # not empty, and Nimbus refuses in the same way.
  force_destroy = true

  tags = { Name = "nimbus-data" }
}

resource "aws_s3_bucket_versioning" "data" {
  bucket = aws_s3_bucket.data.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_object" "readme" {
  bucket       = aws_s3_bucket.data.id
  key          = "readme.txt"
  content      = "Written by Terraform, stored by Nimbus.\n"
  content_type = "text/plain"
}

# --- identity ---------------------------------------------------------------
#
# Recorded, never enforced: the role and its policy round-trip so Terraform
# can manage them and the graph can draw them, but no request is ever refused
# for lacking a permission.

data "aws_iam_policy_document" "assume" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "app" {
  name               = "nimbus-app"
  assume_role_policy = data.aws_iam_policy_document.assume.json
  tags               = { Name = "nimbus-app" }
}

resource "aws_iam_policy" "read_data" {
  name        = "nimbus-read-data"
  description = "Read the objects in the data bucket"

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["s3:GetObject", "s3:ListBucket"]
      Resource = [aws_s3_bucket.data.arn, "${aws_s3_bucket.data.arn}/*"]
    }]
  })
}

resource "aws_iam_role_policy_attachment" "app" {
  role       = aws_iam_role.app.name
  policy_arn = aws_iam_policy.read_data.arn
}

resource "aws_iam_instance_profile" "app" {
  name = "nimbus-app"
  role = aws_iam_role.app.name
}

# --- outputs ----------------------------------------------------------------

output "vpc_id" {
  value = aws_vpc.main.id
}

output "bucket" {
  value = aws_s3_bucket.data.bucket
}

output "role_arn" {
  value = aws_iam_role.app.arn
}

output "check" {
  description = "Prove it with the aws CLI once this has applied."
  value       = <<-EOT
    aws --endpoint-url ${var.nimbus_endpoint} sts get-caller-identity
    aws --endpoint-url ${var.nimbus_endpoint} s3 ls s3://${aws_s3_bucket.data.bucket}
    aws --endpoint-url ${var.nimbus_endpoint} ec2 describe-vpcs --query 'Vpcs[].VpcId'
  EOT
}
