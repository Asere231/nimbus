# A policy attached to the BUCKET, not to the caller.
#
# `aws-iam-enforced` next door proves the other half: a role allowed or
# refused by its own policy. This one proves the half that identity-based
# evaluation cannot express -- a principal whose own policies grant it
# NOTHING, reading an object because the bucket says it may.
#
#   1. nimbus start --clouds aws --enforce-iam     (in another terminal)
#   2. terraform init
#   3. terraform apply
#   4. terraform output check                      -> run what it prints
#   5. terraform destroy
#
# The role here has one inline policy and it grants exactly
# sts:GetCallerIdentity. Nothing it holds mentions S3 at all. It can still
# read the open bucket, and it cannot read the closed one, and the only
# difference between them is the bucket policy.
#
# Watch http://localhost:4700 while it runs. The refusal on the closed
# bucket is a red line in the feed; select either bucket and the access
# panel names the policy that decided, because the inspector and the
# server are the same evaluator.
#
# --- three things worth understanding here ---------------------------------
#
# An ALLOW from either side is enough. The role's own policy and the
# bucket's are evaluated as one set, so either can grant. That is AWS's
# rule within a single account, and it is why `deny_everyone = true` below
# is so blunt: an explicit Deny beats every Allow anywhere, including the
# role's own.
#
# `Principal` is what makes a bucket policy a bucket policy. An identity
# policy is already attached to the principal it governs and may not carry
# one; a resource policy must. Delete the Principal line from the open
# bucket's policy and Nimbus refuses it by name rather than reading the
# gap as "everybody" -- which is the dangerous default and the reason it
# refuses.
#
# `public = true` is the one people meet by accident. It sets
# `Principal: "*"`, and then anyone can read the bucket with NO
# CREDENTIALS AT ALL -- `aws --no-sign-request`, or a browser. The bucket
# also gains a "Public" badge in the visualizer. Then set
# `block_public_access = true`: the policy is untouched and still says
# `*`, and the bucket is no longer public. That gap between what the
# policy says and what the bucket does is the single most instructive
# thing in this example.

terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

# The administrative provider: credentials that map to nothing, which
# Nimbus treats as the account root, as real AWS does.
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
  default     = "nimbus-reader"
  description = "The role the bucket policy names. Its own policy grants no S3 at all."
}

variable "public" {
  type        = bool
  default     = false
  description = <<-EOT
    Whether the open bucket also grants `Principal: "*"`, which is how a
    bucket is made public and how it is accidentally made public. With it
    on, `aws --no-sign-request s3 cp` works and the visualizer badges the
    bucket.
  EOT
}

variable "block_public_access" {
  type        = bool
  default     = false
  description = <<-EOT
    Turn this on WITH public = true. The policy still says `*` and the
    bucket stops being public: RestrictPublicBuckets is evaluated after
    the policy, so an anonymous read is refused and the badge goes away,
    while the named role is unaffected.
  EOT
}

variable "deny_everyone" {
  type        = bool
  default     = false
  description = <<-EOT
    Add an explicit Deny to the open bucket's policy. It beats every
    Allow, including one in the caller's own identity policy -- which is
    what makes a bucket policy a control rather than a convenience.
  EOT
}

# --- the two buckets --------------------------------------------------------

resource "aws_s3_bucket" "open" {
  bucket        = "nimbus-open"
  force_destroy = true
}

resource "aws_s3_bucket" "closed" {
  bucket        = "nimbus-closed"
  force_destroy = true
}

# Written by the ROOT provider, so the objects exist whatever the role may
# or may not do.
resource "aws_s3_object" "open" {
  bucket  = aws_s3_bucket.open.id
  key     = "hello.txt"
  content = "Readable because the bucket says so.\n"
}

resource "aws_s3_object" "closed" {
  bucket  = aws_s3_bucket.closed.id
  key     = "hello.txt"
  content = "Not readable: this bucket has no policy.\n"
}

# --- a role that is granted nothing ----------------------------------------

resource "aws_iam_role" "reader" {
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

# Deliberately useless. If this granted any S3 action the example would
# prove nothing: an allow could have come from either side, and the whole
# point is that it comes from the bucket.
resource "aws_iam_role_policy" "reader" {
  name = "says-nothing-about-s3"
  role = aws_iam_role.reader.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid      = "WhoAmI"
      Effect   = "Allow"
      Action   = "sts:GetCallerIdentity"
      Resource = "*"
    }]
  })
}

# --- the policy that actually grants ---------------------------------------

resource "aws_s3_bucket_policy" "open" {
  bucket = aws_s3_bucket.open.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([{
      Sid       = "LetTheReaderRead"
      Effect    = "Allow"
      Principal = { AWS = aws_iam_role.reader.arn }
      Action    = ["s3:GetObject", "s3:ListBucket"]
      Resource  = [aws_s3_bucket.open.arn, "${aws_s3_bucket.open.arn}/*"]
      }], var.public ? [{
      Sid       = "AnyoneMayRead"
      Effect    = "Allow"
      Principal = "*"
      Action    = "s3:GetObject"
      Resource  = "${aws_s3_bucket.open.arn}/*"
      }] : [], var.deny_everyone ? [{
      Sid       = "NobodyMayRead"
      Effect    = "Deny"
      Principal = "*"
      Action    = "s3:GetObject"
      Resource  = "${aws_s3_bucket.open.arn}/*"
    }] : [])
  })
}

# RestrictPublicBuckets is the one evaluated when a request arrives: the
# policy is left exactly as written and a public grant in it stops
# applying. BlockPublicPolicy is about the WRITE instead, so it is left
# off here -- it would refuse the policy above rather than neutralise it.
resource "aws_s3_bucket_public_access_block" "open" {
  count  = var.block_public_access ? 1 : 0
  bucket = aws_s3_bucket.open.id

  restrict_public_buckets = true
}

# --- what to run ------------------------------------------------------------

output "check" {
  value = <<-EOT
    # The role's OWN policy grants no S3. These two differ only in the
    # bucket policy:
    AWS_ACCESS_KEY_ID=${var.role_name} AWS_SECRET_ACCESS_KEY=nimbus \
      aws --endpoint-url ${var.nimbus_endpoint} s3 cp s3://${aws_s3_bucket.open.id}/hello.txt -
    AWS_ACCESS_KEY_ID=${var.role_name} AWS_SECRET_ACCESS_KEY=nimbus \
      aws --endpoint-url ${var.nimbus_endpoint} s3 cp s3://${aws_s3_bucket.closed.id}/hello.txt -

    # Which policy decided, from the same evaluator the server uses:
    nimbus can --principal ${var.role_name} s3:GetObject '${aws_s3_bucket.open.arn}/hello.txt'
    nimbus can --principal ${var.role_name} s3:GetObject '${aws_s3_bucket.closed.arn}/hello.txt'

    # No credentials at all. Refused until -var public=true, and refused
    # again once -var block_public_access=true is added on top:
    aws --endpoint-url ${var.nimbus_endpoint} --no-sign-request \
      s3 cp s3://${aws_s3_bucket.open.id}/hello.txt -
  EOT
}

output "open_bucket" {
  value = aws_s3_bucket.open.arn
}

output "reader_role" {
  value = aws_iam_role.reader.arn
}
