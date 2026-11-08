# A GKE cluster that really runs, against Nimbus, offline.
#
#   1. nimbus start --clouds gcp          (in another terminal)
#   2. terraform init                     (needs internet once, or a provider
#                                          mirror: see docs/OFFLINE.md)
#   3. terraform apply                    and watch http://localhost:4700
#   4. nimbus gcp kubeconfig nimbus-gke --out kubeconfig
#      KUBECONFIG=./kubeconfig kubectl get nodes
#   5. terraform destroy
#
# The cluster is k3s in Docker -- a real Kubernetes API server, the same
# engine behind the azure-aks and aws-eks examples. `kubectl get nodes`
# talks to it, not to anything Nimbus wrote.
#
# Docker must be running. The first apply pulls the k3s image, which takes
# a few minutes; after that it is cached.
#
# `gcloud container clusters get-credentials` also works, but it needs
# `nimbus gcp install-auth-plugin` first -- see docs/GCLOUD.md for why.

terraform {
  required_version = ">= 1.5"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
  }
}

provider "google" {
  project = var.project
  region  = var.region
  zone    = var.zone

  access_token = "nimbus"

  compute_custom_endpoint             = "${var.nimbus_endpoint}/compute/v1/"
  iam_custom_endpoint                 = "${var.nimbus_endpoint}/"
  resource_manager_custom_endpoint    = "${var.nimbus_endpoint}/v1/"
  resource_manager_v3_custom_endpoint = "${var.nimbus_endpoint}/v3/"

  # The BARE ORIGIN, with no version. Measured: the provider appends `v1/`
  # to this itself, so an endpoint ending in /v1/ produces
  # /v1/v1/projects/... and every call 404s. Same trap as iam above.
  container_custom_endpoint = "${var.nimbus_endpoint}/"
}

variable "project" {
  type    = string
  default = "nimbus-local"
}

variable "region" {
  type    = string
  default = "us-central1"
}

variable "zone" {
  type    = string
  default = "us-central1-a"
}

variable "nimbus_endpoint" {
  type        = string
  default     = "http://localhost:4703"
  description = "Nimbus's GCP port. One endpoint serves every API."
}

# The network the cluster sits on. Giving it one explicitly rather than
# relying on `default` is both better practice and clearer in the graph:
# the cluster is drawn inside the network it belongs to.
resource "google_compute_network" "vpc" {
  name                    = "nimbus-gke-net"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "nodes" {
  name          = "nimbus-gke-nodes"
  network       = google_compute_network.vpc.id
  region        = var.region
  ip_cidr_range = "10.20.0.0/20"

  # The ranges GKE's pods and services come from. Nimbus records them; the
  # cluster inside the container uses k3s's own.
  secondary_ip_range {
    range_name    = "pods"
    ip_cidr_range = "10.21.0.0/16"
  }
  secondary_ip_range {
    range_name    = "services"
    ip_cidr_range = "10.22.0.0/20"
  }
}

resource "google_container_cluster" "main" {
  name     = "nimbus-gke"
  location = var.zone

  network    = google_compute_network.vpc.id
  subnetwork = google_compute_subnetwork.nodes.id

  # One node from the cluster itself, then a pool below. The default pool
  # is removed the way a real GKE config does it, so the node pool is the
  # only thing that owns nodes.
  initial_node_count       = 1
  remove_default_node_pool = true

  # Nimbus never deletes anything by accident, and this makes `terraform
  # destroy` work without a second apply.
  deletion_protection = false
}

resource "google_container_node_pool" "workers" {
  name     = "nimbus-workers"
  cluster  = google_container_cluster.main.name
  location = var.zone

  # Two nodes, so there is something to see: the k3s server container is
  # itself a node, so this adds one agent container beside it. Change it
  # and re-apply to watch a node appear or disappear.
  node_count = 2

  node_config {
    machine_type = "e2-medium"
    labels = {
      pool = "workers"
    }
  }
}

output "cluster_endpoint" {
  value       = google_container_cluster.main.endpoint
  description = "The cluster's API server, on localhost."
}

output "kubeconfig_command" {
  value = "nimbus gcp kubeconfig ${google_container_cluster.main.name} --out kubeconfig"
}
