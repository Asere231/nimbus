# IAM that can say no.
#
# Every other example here proves something WORKS. This one proves
# something is refused, and then that the same apply succeeds once the
# policy grants it -- both directions, because an emulator that refuses
# everything would pass the first half.
#
#   1. nimbus start --clouds aws --enforce-iam     (in another terminal)
#   2. terraform init
#   3. terraform apply                 -> FAILS with AccessDenied on
#                                         s3:PutObject, naming the role
#   4. terraform apply -var grant_write=true
#                                      -> succeeds
#   5. terraform destroy -var grant_write=true
#
# Watch http://localhost:4700 while step 3 runs: the refusal is a red line
# in the activity feed naming the principal, the action and the statement
# that decided.
#
# Without --enforce-iam every step succeeds, because nothing is ever
# refused -- which is how Nimbus behaved before Milestone 13 and is still
# the default. The other examples are unaffected by the flag either way:
# their credentials map to nothing, so Nimbus treats them as the account
# root.
#
# grant_read is the interesting one, and it is a TRAP on purpose.
#
# Set it to false after a successful apply and the role can no longer read
# the object Terraform is managing. The next plan then fails on the
# refresh -- `api error Forbidden: Forbidden`, because an HTTP HEAD reply
# has no body to put a reason in -- and you cannot plan your way out: the
# apply that would restore the permission has to refresh first, and the
# refresh is what is failing. The way out is to skip it:
#
#   terraform apply -refresh=false -var grant_write=true -var grant_read=true
#
# Real AWS behaves exactly the same way. Revoking a permission Terraform
# needs in order to READ its own resources is a mistake worth meeting here
# rather than on something that matters.
#
# What it is NOT is a denial being mistaken for a missing object. A denied
# read fails the plan; it does not make Terraform decide the object is
# gone and offer to create it again. That was measured before the rest of
# this milestone was built, because the other answer would have made
# enforcement actively dangerous.

terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

# The administrative provider. Its credentials map to nothing, so Nimbus
# treats it as the account ROOT -- which in real AWS carries no policies
# and can do anything. It is what builds the role and the bucket, because
# the restricted role certainly cannot.
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
    sts = var.nimbus_endpoint
    iam = var.nimbus_endpoint
    s3  = var.nimbus_endpoint
  }
}

# The restricted provider. Nimbus never verifies a signature, so an
# identity is whatever the client claims: an access key id that is a role's
# NAME is taken to be that role. (The other way in is the provider's own
# assume_role block, whose session key Nimbus derives from the role ARN and
# maps back -- see internal/aws/principal.go.)
provider "aws" {
  alias  = "app"
  region = var.region
  # A literal, not aws_iam_role.app.name: a provider configuration that
  # refers to a resource attribute is unknown at plan time, and Terraform
  # refuses to configure a provider from an unknown value. So the name is
  # a variable both the role and this provider read.
  access_key = var.role_name
  secret_key = "nimbus"

  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
  skip_region_validation      = true
  s3_use_path_style           = true

  endpoints {
    sts = var.nimbus_endpoint
    iam = var.nimbus_endpoint
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

variable "role_name" {
  type        = string
  default     = "nimbus-app"
  description = "The role the restricted provider claims to be."
}

variable "grant_read" {
  type        = bool
  default     = true
  description = <<-EOT
    Whether the role may read objects. Setting it to false AFTER a
    successful apply is how to see what a denied READ does to a refresh:
    either Terraform reports the denial, or it reads it as "the object is
    gone" and plans to create it again. Which one it is decides whether
    enforcement is safe to leave on.
  EOT
}

variable "grant_write" {
  type        = bool
  default     = false
  description = <<-EOT
    Whether the role may write objects. false is the point of the example:
    the apply fails with AccessDenied on s3:PutObject. Set it to true and
    the same apply succeeds, which is the other half of the proof.
  EOT
}

# --- the bucket and the role ------------------------------------------------

resource "aws_s3_bucket" "data" {
  bucket        = "nimbus-enforced"
  force_destroy = true
}

resource "aws_iam_role" "app" {
  name = var.role_name

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "ec2.amazonaws.com" }
    }]
  })
}

# The policy under test. Reading is granted unless grant_read is turned
# off; writing only when grant_write is turned on.
#
# Note what it does NOT say: there is no Deny anywhere. The refusal in step
# 3 is the IMPLICIT deny -- nothing grants s3:PutObject, so nothing allows
# it. That is the rule the whole engine rests on, and it is worth seeing
# work without an explicit Deny to lean on.
resource "aws_iam_role_policy" "app" {
  name = "nimbus-app"
  role = aws_iam_role.app.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat(var.grant_read ? [{
      Sid    = "ReadTheBucket"
      Effect = "Allow"
      Action = ["s3:ListBucket", "s3:GetObject"]
      Resource = [
        aws_s3_bucket.data.arn,
        "${aws_s3_bucket.data.arn}/*",
      ]
      }] : [], var.grant_write ? [{
      Sid      = "WriteTheBucket"
      Effect   = "Allow"
      Action   = ["s3:PutObject", "s3:DeleteObject"]
      Resource = ["${aws_s3_bucket.data.arn}/*"]
    }] : [])
  })
}

# --- what the role tries to do ----------------------------------------------

# Created by the RESTRICTED provider, so it is the role's permissions that
# decide. depends_on is needed because nothing in this resource references
# the policy, and without it Terraform may write the object before the
# policy exists -- at which point the role has no policies at all and the
# refusal is right for the wrong reason.
resource "aws_s3_object" "report" {
  provider = aws.app

  bucket       = aws_s3_bucket.data.bucket
  key          = "reports/today.txt"
  content      = "written by ${var.role_name}\n"
  content_type = "text/plain"

  depends_on = [aws_iam_role_policy.app]
}

output "role_arn" {
  value = aws_iam_role.app.arn
}

output "bucket" {
  value = aws_s3_bucket.data.bucket
}
