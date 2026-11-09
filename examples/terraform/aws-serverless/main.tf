# A serverless stack that really runs, fully offline against Nimbus.
#
# The Lambda is executed by AWS's own base image through the Runtime
# Interface Emulator it ships with, so the handler below is run by the same
# runtime client that would run it in production -- and it reaches this
# Nimbus's DynamoDB through AWS_ENDPOINT_URL, with no endpoint in its code.
#
#   1. nimbus start --clouds aws          (in another terminal)
#   2. docker pull public.ecr.aws/lambda/python:3.12   (once, while online)
#   3. terraform init && terraform apply
#   The `aws` CLI needs a region and credentials of its own -- Terraform's
#   provider block does not reach it. Any values will do, since Nimbus
#   verifies no signature:
#
#     export AWS_ACCESS_KEY_ID=nimbus AWS_SECRET_ACCESS_KEY=nimbus
#     export AWS_DEFAULT_REGION=us-east-1
#
#   4. printf '{"id":"1"}' > payload.json
#      aws --endpoint-url http://localhost:4702 lambda invoke \
#        --function-name nimbus-writer --payload fileb://payload.json /dev/stdout
#
#      The payload comes from a file because no INLINE --payload works on
#      both aws-cli versions: v2 reads one as base64 and fails with
#      `Invalid base64: "{"id":"1"}"` unless given
#      --cli-binary-format raw-in-base64-out, and v1 rejects that flag as an
#      unknown option. fileb:// means "these bytes, verbatim" to both.
#
#      aws --endpoint-url http://localhost:4702 dynamodb scan --table-name nimbus-items
#      aws --endpoint-url http://localhost:4702 logs filter-log-events \
#        --log-group-name /aws/lambda/nimbus-writer
#   5. terraform destroy
#
# Docker must be running: a function is a container here.

terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
    archive = {
      source  = "hashicorp/archive"
      version = "~> 2.4"
    }
  }
}

provider "aws" {
  region     = var.region
  access_key = "nimbus"
  secret_key = "nimbus"

  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
  skip_region_validation      = true
  s3_use_path_style           = true

  endpoints {
    sts      = var.nimbus_endpoint
    iam      = var.nimbus_endpoint
    lambda   = var.nimbus_endpoint
    dynamodb = var.nimbus_endpoint
    logs     = var.nimbus_endpoint
    s3       = var.nimbus_endpoint
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

# --- the table --------------------------------------------------------------

resource "aws_dynamodb_table" "items" {
  name         = "nimbus-items"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "id"

  attribute {
    name = "id"
    type = "S"
  }

  tags = { Project = "nimbus" }
}

# --- the function's role ----------------------------------------------------
#
# Recorded, never enforced: Nimbus refuses nothing for lacking a role, so
# this proves the shape of a deployment rather than its permissions.

resource "aws_iam_role" "writer" {
  name = "nimbus-writer"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "lambda.amazonaws.com" }
    }]
  })
}

resource "aws_iam_role_policy" "writer" {
  name = "write-items"
  role = aws_iam_role.writer.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["dynamodb:PutItem", "logs:PutLogEvents", "logs:CreateLogStream"]
      Resource = "*"
    }]
  })
}

# --- the function -----------------------------------------------------------

data "archive_file" "handler" {
  type        = "zip"
  source_dir  = "${path.module}/handler"
  output_path = "${path.module}/handler.zip"
}

resource "aws_lambda_function" "writer" {
  function_name = "nimbus-writer"
  role          = aws_iam_role.writer.arn
  handler       = "app.handler"
  runtime       = "python3.12"
  timeout       = 15
  memory_size   = 256

  filename         = data.archive_file.handler.output_path
  source_code_hash = data.archive_file.handler.output_base64sha256

  environment {
    variables = {
      TABLE = aws_dynamodb_table.items.name
    }
  }

  tags = { Project = "nimbus" }
}

output "invoke" {
  # fileb:// rather than an inline payload: it is the only spelling both
  # aws-cli v1 and v2 read as raw bytes. See the note at the top.
  value = "printf '{\"id\":\"1\"}' > payload.json && aws --endpoint-url ${var.nimbus_endpoint} lambda invoke --function-name ${aws_lambda_function.writer.function_name} --payload fileb://payload.json /dev/stdout"
}

output "scan" {
  value = "aws --endpoint-url ${var.nimbus_endpoint} dynamodb scan --table-name ${aws_dynamodb_table.items.name}"
}
