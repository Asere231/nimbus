# One apply, three clouds, three real Kubernetes clusters.
#
# This is the example the whole project has been building towards: a
# network and a managed Kubernetes cluster on Azure, AWS and GCP, applied
# in one run against ONE Nimbus process, with all three drawn side by side
# in the visualizer.
#
#   1. nimbus cert                        once, to trust the CA (Azure needs TLS)
#   2. nimbus start --clouds azure,aws,gcp   (in another terminal)
#   3. terraform init && terraform apply
#   4. http://localhost:4700 -- three tabs, each with a cluster in it
#   5. terraform destroy
#
# Expect this to take a while and to be the heaviest thing here: three k3s
# clusters means three privileged containers, and the first run pulls the
# image. Single-node clusters everywhere, deliberately -- the point is
# that three clouds share one store, not how many nodes each has.
#
# Docker must be running. The three providers never agree on anything --
# different auth, different endpoint settings, different ideas of what a
# network is -- which is exactly why doing it once is worth something.

terraform {
  required_version = ">= 1.5"
  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.0"
    }
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
  }
}

# --- Azure ----------------------------------------------------------------
#
# The only one that needs TLS: the Azure SDKs refuse plain HTTP. It finds
# every endpoint through the metadata document on metadata_host, so there
# is one setting rather than one per service.

provider "azurerm" {
  features {}

  metadata_host   = "localhost:4701"
  subscription_id = "00000000-0000-0000-0000-000000000000"
  tenant_id       = "00000000-0000-0000-0000-000000000001"
  client_id       = "00000000-0000-0000-0000-000000000002"
  client_secret   = "nimbus"

  resource_provider_registrations = "none"
}

resource "azurerm_resource_group" "main" {
  name     = "rg-multicloud"
  location = "eastus"
}

resource "azurerm_virtual_network" "main" {
  name                = "vnet-multicloud"
  location            = azurerm_resource_group.main.location
  resource_group_name = azurerm_resource_group.main.name
  address_space       = ["10.10.0.0/16"]
}

resource "azurerm_subnet" "nodes" {
  name                 = "snet-nodes"
  resource_group_name  = azurerm_resource_group.main.name
  virtual_network_name = azurerm_virtual_network.main.name
  address_prefixes     = ["10.10.1.0/24"]
}

resource "azurerm_kubernetes_cluster" "main" {
  name                = "aks-multicloud"
  location            = azurerm_resource_group.main.location
  resource_group_name = azurerm_resource_group.main.name
  dns_prefix          = "aks-multicloud"

  default_node_pool {
    name           = "default"
    node_count     = 1
    vm_size        = "Standard_D2s_v3"
    vnet_subnet_id = azurerm_subnet.nodes.id
  }

  identity {
    type = "SystemAssigned"
  }
}

# --- AWS ------------------------------------------------------------------
#
# Plain HTTP, and one endpoint per service rather than a metadata document.
# S3 is addressed path-style, which is why none of this needs a
# certificate.

provider "aws" {
  region     = "us-east-1"
  access_key = "nimbus"
  secret_key = "nimbus"

  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
  skip_region_validation      = true

  endpoints {
    sts = "http://localhost:4702"
    iam = "http://localhost:4702"
    ec2 = "http://localhost:4702"
    eks = "http://localhost:4702"
  }
}

resource "aws_vpc" "main" {
  cidr_block = "10.20.0.0/16"
  tags       = { Name = "vpc-multicloud" }
}

# EKS insists on subnets in two availability zones, as real EKS does.
resource "aws_subnet" "a" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.20.1.0/24"
  availability_zone = "us-east-1a"
  tags              = { Name = "subnet-multicloud-a" }
}

resource "aws_subnet" "b" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.20.2.0/24"
  availability_zone = "us-east-1b"
  tags              = { Name = "subnet-multicloud-b" }
}

resource "aws_iam_role" "cluster" {
  name = "eks-multicloud-cluster"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "eks.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_eks_cluster" "main" {
  name     = "eks-multicloud"
  role_arn = aws_iam_role.cluster.arn

  vpc_config {
    subnet_ids = [aws_subnet.a.id, aws_subnet.b.id]
  }
}

# --- Google ---------------------------------------------------------------
#
# Plain HTTP again, but one endpoint setting per service and no agreement
# on whether the version belongs to the endpoint or to the client --
# container wants the bare origin, the others carry their version.

provider "google" {
  project = "nimbus-local"
  region  = "us-central1"
  zone    = "us-central1-a"

  access_token = "nimbus"

  compute_custom_endpoint             = "http://localhost:4703/compute/v1/"
  iam_custom_endpoint                 = "http://localhost:4703/"
  resource_manager_custom_endpoint    = "http://localhost:4703/v1/"
  resource_manager_v3_custom_endpoint = "http://localhost:4703/v3/"
  container_custom_endpoint           = "http://localhost:4703/"
}

resource "google_compute_network" "main" {
  name                    = "vpc-multicloud"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "nodes" {
  name          = "subnet-multicloud"
  network       = google_compute_network.main.id
  region        = "us-central1"
  ip_cidr_range = "10.30.1.0/24"
}

resource "google_container_cluster" "main" {
  name     = "gke-multicloud"
  location = "us-central1-a"

  network    = google_compute_network.main.id
  subnetwork = google_compute_subnetwork.nodes.id

  initial_node_count  = 1
  deletion_protection = false
}

# --- what you get ---------------------------------------------------------

output "kubeconfigs" {
  value = <<-TXT
    Three clusters, three ways of getting at them -- which is the point:

      Azure  terraform output -raw aks_kubeconfig > aks.kubeconfig
      AWS    aws --endpoint-url http://localhost:4702 eks update-kubeconfig \
               --name eks-multicloud --kubeconfig eks.kubeconfig
      GCP    nimbus gcp kubeconfig gke-multicloud --out gke.kubeconfig

    Then: kubectl --kubeconfig <file> get nodes
  TXT
}

output "aks_kubeconfig" {
  value     = azurerm_kubernetes_cluster.main.kube_config_raw
  sensitive = true
}
