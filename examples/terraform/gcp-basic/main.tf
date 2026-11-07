# A Google Cloud stack that runs fully offline against Nimbus.
#
#   1. nimbus start --clouds gcp          (in another terminal)
#   2. terraform init                     (needs internet once, or a provider
#                                          mirror: see docs/OFFLINE.md)
#   3. terraform apply                    and watch http://localhost:4700
#   4. terraform destroy
#
# Plain HTTP, like the AWS examples. Cloud Storage is addressed by path
# (/storage/v1/b/{bucket}), so no bucket name becomes a hostname and nothing
# has to be trusted on your machine.
#
# For the gcloud CLI against the same server, see docs/GCLOUD.md.

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
  zone    = "${var.region}-a"

  # A bearer token Nimbus never checks. Giving one directly is what stops
  # the provider going looking for application-default credentials -- and
  # reaching Google to do it.
  access_token = "nimbus"

  # One endpoint per service. The trailing slash matters: the provider
  # joins paths onto these, and some of them already carry their version
  # while others do not -- iam_custom_endpoint includes it, which is why
  # this one has no "v1".
  compute_custom_endpoint             = "${var.nimbus_endpoint}/compute/v1/"
  storage_custom_endpoint             = "${var.nimbus_endpoint}/storage/v1/"
  iam_custom_endpoint                 = "${var.nimbus_endpoint}/"
  resource_manager_custom_endpoint    = "${var.nimbus_endpoint}/v1/"
  resource_manager_v3_custom_endpoint = "${var.nimbus_endpoint}/v3/"
}

variable "project" {
  type    = string
  default = "nimbus-local"
}

variable "region" {
  type    = string
  default = "us-central1"
}

variable "nimbus_endpoint" {
  type        = string
  default     = "http://localhost:4703"
  description = "Nimbus's GCP port. One endpoint serves every API."
}

# --- networking -------------------------------------------------------------

resource "google_compute_network" "main" {
  name = "nimbus-net"
  # Custom mode: no subnetwork per region appears on its own, so the two
  # below are the only ones.
  auto_create_subnetworks = false
  description             = "The example network"
}

resource "google_compute_subnetwork" "public" {
  name          = "nimbus-public"
  network       = google_compute_network.main.id
  ip_cidr_range = "10.0.1.0/24"
  region        = var.region
}

resource "google_compute_subnetwork" "private" {
  name                     = "nimbus-private"
  network                  = google_compute_network.main.id
  ip_cidr_range            = "10.0.2.0/24"
  region                   = var.region
  private_ip_google_access = true

  secondary_ip_range {
    range_name    = "pods"
    ip_cidr_range = "10.1.0.0/16"
  }
}

resource "google_compute_firewall" "ssh" {
  name    = "nimbus-allow-ssh"
  network = google_compute_network.main.name

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }

  source_ranges = ["10.0.0.0/8"]
  target_tags   = ["ssh"]
}

# --- identity ---------------------------------------------------------------
#
# Recorded, never enforced: no request to Nimbus is refused for lacking a
# role, so this proves the shape of a deployment rather than its
# permissions -- the same honest limitation Azure RBAC and AWS IAM have.

resource "google_service_account" "app" {
  account_id   = "nimbus-app"
  display_name = "Nimbus example app"
  description  = "Used by the gcp-basic example"
}

# --- storage ----------------------------------------------------------------

resource "google_storage_bucket" "data" {
  name     = "nimbus-data"
  location = "US"

  # The bucket refuses to be deleted while it holds anything, as Cloud
  # Storage does; force_destroy is the provider emptying it first.
  force_destroy = true

  versioning {
    enabled = true
  }

  labels = {
    project = "nimbus"
  }
}

resource "google_storage_bucket_object" "readme" {
  name    = "docs/hello.txt"
  bucket  = google_storage_bucket.data.name
  content = "Written by Terraform into an emulated Cloud Storage bucket.\n"
}

output "network" {
  value = google_compute_network.main.self_link
}

output "download" {
  value = "curl ${var.nimbus_endpoint}/storage/v1/b/${google_storage_bucket.data.name}/o/docs%2Fhello.txt?alt=media"
}
