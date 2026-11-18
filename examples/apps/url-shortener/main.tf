# A link shortener: a small APPLICATION, not a demonstration of one service.
#
# Every other example here exercises one service family. This one is a thing
# you could plausibly ship -- a table with a secondary index, a function that
# does conditional writes and atomic counters against it, a bucket it
# publishes a page to, a log group you read to see what happened, and the IAM
# that ties them together -- because combinations are where an emulator
# actually gets tested.
#
# Running it:
#
#   1. nimbus start --clouds aws --persist          (another terminal)
#   2. docker pull public.ecr.aws/lambda/python:3.12   (once, while online)
#   3. terraform init && terraform apply
#   4. The `aws` CLI needs its own region and credentials; Terraform's
#      provider block does not reach it, and any values will do because
#      Nimbus verifies no signature:
#
#        $env:AWS_ACCESS_KEY_ID="nimbus"; $env:AWS_SECRET_ACCESS_KEY="nimbus"
#        $env:AWS_DEFAULT_REGION="us-east-1"
#
#   5. terraform output -raw walkthrough   # every call, in order
#   6. terraform destroy
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

variable "name" {
  type        = string
  default     = "shortener"
  description = "Prefix for every resource, so two copies can coexist."
}

# --- storage ----------------------------------------------------------------

# The table is keyed by the short code, and carries a secondary index so one
# owner's links can be listed newest-first without scanning everything.
resource "aws_dynamodb_table" "links" {
  name         = "${var.name}-links"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "code"

  attribute {
    name = "code"
    type = "S"
  }
  attribute {
    name = "owner"
    type = "S"
  }
  attribute {
    name = "created"
    type = "S"
  }

  global_secondary_index {
    name            = "by-owner"
    hash_key        = "owner"
    range_key       = "created"
    projection_type = "ALL"
  }

  tags = { Project = "nimbus", App = var.name }
}

# Where the generated index page goes. Versioning is on because a page is
# republished every time a link is added, and keeping the old ones is the
# cheapest way to see that the publish really ran.
resource "aws_s3_bucket" "assets" {
  bucket        = "${var.name}-assets"
  force_destroy = true
  tags          = { Project = "nimbus", App = var.name }
}

resource "aws_s3_bucket_versioning" "assets" {
  bucket = aws_s3_bucket.assets.id
  versioning_configuration {
    status = "Enabled"
  }
}

# --- permissions ------------------------------------------------------------
#
# These are real policies, not decoration: with `nimbus start --enforce-iam`
# they decide, and the function's role is scoped to exactly the table, index,
# bucket prefix and log group it uses. Without the flag nothing is refused,
# so the same configuration documents the intent either way.

resource "aws_iam_role" "fn" {
  name = "${var.name}-fn"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "lambda.amazonaws.com" }
    }]
  })

  tags = { Project = "nimbus", App = var.name }
}

resource "aws_iam_role_policy" "fn" {
  name = "${var.name}-access"
  role = aws_iam_role.fn.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "Links"
        Effect = "Allow"
        Action = [
          "dynamodb:PutItem",
          "dynamodb:GetItem",
          "dynamodb:UpdateItem",
          "dynamodb:Query",
        ]
        # The index is a separate resource from the table it belongs to,
        # which is the detail everyone's first DynamoDB policy misses.
        Resource = [
          aws_dynamodb_table.links.arn,
          "${aws_dynamodb_table.links.arn}/index/by-owner",
        ]
      },
      {
        Sid      = "Pages"
        Effect   = "Allow"
        Action   = ["s3:PutObject", "s3:GetObject"]
        Resource = "${aws_s3_bucket.assets.arn}/pages/*"
      },
      {
        Sid      = "Logs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.fn.arn}:*"
      },
    ]
  })
}

# --- the function -----------------------------------------------------------

# Declared rather than left to the function to create, so retention is ours
# and a destroy takes it. (In real AWS a Lambda's log group outlives its
# function, which Nimbus copies deliberately -- so without this resource the
# group would survive the destroy.)
resource "aws_cloudwatch_log_group" "fn" {
  name              = "/aws/lambda/${var.name}"
  retention_in_days = 7
  tags              = { Project = "nimbus", App = var.name }
}

data "archive_file" "handler" {
  type        = "zip"
  source_dir  = "${path.module}/handler"
  output_path = "${path.module}/handler.zip"
}

resource "aws_lambda_function" "fn" {
  function_name = var.name
  role          = aws_iam_role.fn.arn
  handler       = "app.handler"
  runtime       = "python3.12"
  timeout       = 15
  memory_size   = 256

  filename         = data.archive_file.handler.output_path
  source_code_hash = data.archive_file.handler.output_base64sha256

  environment {
    variables = {
      TABLE    = aws_dynamodb_table.links.name
      BUCKET   = aws_s3_bucket.assets.bucket
      BASE_URL = "https://nimb.us"
    }
  }

  depends_on = [aws_cloudwatch_log_group.fn]

  tags = { Project = "nimbus", App = var.name }
}

# --- how to drive it --------------------------------------------------------

locals {
  aws = "aws --endpoint-url ${var.nimbus_endpoint}"
  # fileb:// rather than an inline payload: it is the only spelling both
  # aws-cli v1 and v2 read as raw bytes. v2 reads an inline --payload as
  # base64 and fails; v1 rejects the flag that would fix it.
  invoke = "${local.aws} lambda invoke --function-name ${aws_lambda_function.fn.function_name} --payload fileb://payload.json /dev/stdout"
}

output "walkthrough" {
  description = "Every call, in order. Paste one line at a time."
  value       = <<-EOT
    # 1. shorten two links, then the first one again (idempotent)
    printf '{"action":"create","url":"https://go.dev/doc/effective_go","owner":"bryan"}' > payload.json
    ${local.invoke}
    printf '{"action":"create","url":"https://www.terraform.io/docs","owner":"bryan"}' > payload.json
    ${local.invoke}
    printf '{"action":"create","url":"https://go.dev/doc/effective_go","owner":"bryan"}' > payload.json
    ${local.invoke}

    # 2. resolve one twice and watch the counter
    printf '{"action":"resolve","code":"CODE"}' > payload.json
    ${local.invoke}

    # 3. list one owner's links, newest first, off the secondary index
    printf '{"action":"list","owner":"bryan"}' > payload.json
    ${local.invoke}

    # 4. publish the page, then read it back out of S3
    printf '{"action":"publish","owner":"bryan"}' > payload.json
    ${local.invoke}
    ${local.aws} s3 cp s3://${aws_s3_bucket.assets.bucket}/pages/bryan/index.html -

    # 5. what the handler printed
    ${local.aws} logs filter-log-events --log-group-name ${aws_cloudwatch_log_group.fn.name} --query 'events[].message' --output text

    # 6. the table itself
    ${local.aws} dynamodb scan --table-name ${aws_dynamodb_table.links.name} --output table
  EOT
}

output "table" {
  value = aws_dynamodb_table.links.name
}

output "bucket" {
  value = aws_s3_bucket.assets.bucket
}

output "function" {
  value = aws_lambda_function.fn.function_name
}
