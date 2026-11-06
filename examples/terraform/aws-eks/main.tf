# An EKS cluster that is a real Kubernetes cluster, fully offline.
#
# Nimbus runs it as k3s in Docker -- the same engine behind the azure-aks
# example -- and answers the token authentication webhook the API server
# asks, which is how `aws eks get-token` produces a kubeconfig that works.
#
#   1. nimbus start --clouds aws          (in another terminal)
#   2. terraform init && terraform apply  (a few minutes: it pulls k3s once)
#   3. aws --endpoint-url http://localhost:4702 eks update-kubeconfig \
#        --name nimbus --kubeconfig ./kubeconfig
#      kubectl --kubeconfig ./kubeconfig get nodes
#      kubectl --kubeconfig ./kubeconfig create deployment web --image nginx
#   4. terraform destroy
#
# Docker must be running, and the visualizer's Kubernetes tab draws whatever
# you create inside the cluster.

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
  region     = var.region
  access_key = "nimbus"
  secret_key = "nimbus"

  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
  skip_region_validation      = true

  endpoints {
    sts = var.nimbus_endpoint
    iam = var.nimbus_endpoint
    ec2 = var.nimbus_endpoint
    eks = var.nimbus_endpoint
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
#
# EKS records its subnets and never uses them: the cluster is a container on
# a Docker network. They are here because a real deployment has them, and
# because the visualizer then draws the cluster beside the VPC it belongs to.

resource "aws_vpc" "main" {
  cidr_block           = "10.0.0.0/16"
  enable_dns_hostnames = true
  tags                 = { Name = "nimbus-eks-vpc" }
}

resource "aws_subnet" "a" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.0.1.0/24"
  availability_zone = "${var.region}a"
  tags              = { Name = "nimbus-eks-a" }
}

resource "aws_subnet" "b" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.0.2.0/24"
  availability_zone = "${var.region}b"
  tags              = { Name = "nimbus-eks-b" }
}

# --- the roles --------------------------------------------------------------

resource "aws_iam_role" "cluster" {
  name = "nimbus-eks-cluster"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "eks.amazonaws.com" }
    }]
  })
}

resource "aws_iam_role" "nodes" {
  name = "nimbus-eks-nodes"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "ec2.amazonaws.com" }
    }]
  })
}

# --- the cluster ------------------------------------------------------------

resource "aws_eks_cluster" "main" {
  name     = "nimbus"
  role_arn = aws_iam_role.cluster.arn

  vpc_config {
    subnet_ids = [aws_subnet.a.id, aws_subnet.b.id]
  }
}

resource "aws_eks_node_group" "default" {
  cluster_name    = aws_eks_cluster.main.name
  node_group_name = "default"
  node_role_arn   = aws_iam_role.nodes.arn
  subnet_ids      = [aws_subnet.a.id, aws_subnet.b.id]

  # One agent beside the k3s server, so `kubectl get nodes` shows two.
  scaling_config {
    desired_size = 2
    min_size     = 1
    max_size     = 3
  }
}

output "kubeconfig" {
  value = "aws --endpoint-url ${var.nimbus_endpoint} eks update-kubeconfig --name ${aws_eks_cluster.main.name} --kubeconfig ./kubeconfig"
}
