# IRSA: a pod gets AWS credentials from its Kubernetes service account.
#
# This is the one AWS mechanism where a signature genuinely decides
# something. The cluster's API server signs the pod's projected
# service-account token with a key Nimbus holds; Nimbus publishes the
# public half at the cluster's OIDC issuer; and
# `sts:AssumeRoleWithWebIdentity` verifies the token against it before
# handing out credentials for the role. Nothing is taken on faith, so a
# forged token is refused here exactly as it would be in real AWS.
#
# What this example demonstrates, in order:
#
#   1. The cluster has an OIDC issuer, served over HTTPS.
#   2. `data.tls_certificate` reads its thumbprint -- the step every real
#      IRSA module performs, and the reason the issuer cannot live on
#      Nimbus's plain-HTTP AWS port: that data source refuses `http://`.
#   3. An `aws_iam_openid_connect_provider` registers the issuer.
#   4. A role trusts ONE service account, by `sub`, and grants access to
#      ONE bucket. The second bucket is there to be refused.
#
#   nimbus cert                                 (once, and trust it)
#   nimbus start --clouds aws --enforce-iam     (in another terminal)
#   terraform init && terraform apply           (pulls k3s once)
#   ./verify.sh                                 (the pod, and the refusal)
#   terraform destroy
#
# Docker must be running. `--enforce-iam` is what makes the second bucket
# a refusal rather than a formality -- without it nothing is ever denied,
# and the example still applies cleanly but proves half as much.

terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.0"
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

  # S3 is addressed path-style, which is why AWS here needs no TLS and no
  # DNS: no `bucket.host` name means no certificate to trust.
  s3_use_path_style = true

  endpoints {
    sts = var.nimbus_endpoint
    iam = var.nimbus_endpoint
    ec2 = var.nimbus_endpoint
    eks = var.nimbus_endpoint
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

# --- the network the cluster claims to be in --------------------------------

resource "aws_vpc" "main" {
  cidr_block = "10.0.0.0/16"
  tags       = { Name = "nimbus-irsa-vpc" }
}

resource "aws_subnet" "a" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.0.1.0/24"
  availability_zone = "${var.region}a"
  tags              = { Name = "nimbus-irsa-a" }
}

resource "aws_subnet" "b" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.0.2.0/24"
  availability_zone = "${var.region}b"
  tags              = { Name = "nimbus-irsa-b" }
}

resource "aws_iam_role" "cluster" {
  name = "nimbus-irsa-cluster"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "eks.amazonaws.com" }
    }]
  })
}

# --- the cluster, and its issuer --------------------------------------------

resource "aws_eks_cluster" "main" {
  name     = "nimbus"
  role_arn = aws_iam_role.cluster.arn

  vpc_config {
    subnet_ids = [aws_subnet.a.id, aws_subnet.b.id]
  }
}

# The step that decides where the issuer can live. This data source
# REFUSES an `http://` url outright ("Invalid URL scheme"), so the issuer
# is served on Nimbus's second, TLS listener -- `--aws-tls-port`, 4704 by
# default -- while everything else about AWS stays on plain HTTP.
#
# It VERIFIES the chain, so `nimbus cert` has to have been run once on
# this machine -- without it the apply fails with "certificate signed by
# unknown authority" naming the issuer URL. The chain comes back as
# [CA, leaf] and the
# canonical pattern reads certificates[0], which is the CA -- matching
# real EKS, where the thumbprint is of the root.
data "tls_certificate" "oidc" {
  url = aws_eks_cluster.main.identity[0].oidc[0].issuer
}

resource "aws_iam_openid_connect_provider" "main" {
  url             = aws_eks_cluster.main.identity[0].oidc[0].issuer
  client_id_list  = ["sts.amazonaws.com"]
  thumbprint_list = [data.tls_certificate.oidc.certificates[0].sha1_fingerprint]
}

# --- what the pod may and may not touch -------------------------------------

resource "aws_s3_bucket" "allowed" {
  bucket        = "nimbus-irsa-allowed"
  force_destroy = true
}

resource "aws_s3_bucket" "denied" {
  bucket        = "nimbus-irsa-denied"
  force_destroy = true
}

resource "aws_s3_object" "hello" {
  bucket  = aws_s3_bucket.allowed.id
  key     = "hello.txt"
  content = "a pod read this with credentials it got from its service account\n"
}

resource "aws_s3_object" "secret" {
  bucket  = aws_s3_bucket.denied.id
  key     = "secret.txt"
  content = "the pod's role does not grant this bucket\n"
}

# --- the role the service account assumes -----------------------------------
#
# The two conditions are the whole of IRSA's security. `:sub` pins the
# exact service account -- namespace AND name -- so another pod in the
# same cluster, holding a token signed by the same key, cannot assume this
# role. `:aud` pins what the token was minted for. The condition KEY is
# named after the issuer without its scheme, which is how AWS stores it in
# the provider ARN.

locals {
  issuer_bare = replace(aws_eks_cluster.main.identity[0].oidc[0].issuer, "https://", "")
  namespace   = "demo"
  sa_name     = "reader"
}

resource "aws_iam_role" "pod" {
  name = "nimbus-irsa-reader"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRoleWithWebIdentity"
      Principal = { Federated = aws_iam_openid_connect_provider.main.arn }
      Condition = {
        StringEquals = {
          "${local.issuer_bare}:sub" = "system:serviceaccount:${local.namespace}:${local.sa_name}"
          "${local.issuer_bare}:aud" = "sts.amazonaws.com"
        }
      }
    }]
  })
}

resource "aws_iam_role_policy" "pod" {
  name = "read-one-bucket"
  role = aws_iam_role.pod.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      # ListBucket is about the BUCKET; GetObject is about what is inside
      # it. Two resources, because they are two different permissions --
      # the distinction a bucket-level grant alone always gets wrong.
      Action   = ["s3:GetObject", "s3:ListBucket"]
      Resource = [aws_s3_bucket.allowed.arn, "${aws_s3_bucket.allowed.arn}/*"]
    }]
  })
}

output "issuer" {
  value = aws_eks_cluster.main.identity[0].oidc[0].issuer
}

output "role_arn" {
  value = aws_iam_role.pod.arn
}

output "service_account" {
  value = "${local.namespace}/${local.sa_name}"
}

output "next" {
  value = "./verify.sh"
}
