# A Compute Engine instance that really boots, with a startup script that
# really runs.
#
#   1. nimbus start --clouds gcp           (in another terminal)
#   2. terraform init                      (needs internet once, or a
#                                           provider mirror: see docs/OFFLINE.md)
#   3. terraform apply                     and watch http://localhost:4700
#   4. nimbus ssh web                      a shell on the instance
#      nimbus ssh web -- cat /var/log/nimbus-boot.log
#   5. gcloud compute instances list       (after nimbus gcp register --run)
#   6. terraform destroy
#
# Docker must be running. The first apply pulls debian:12 -- a real Debian,
# so apt works inside the instance.
#
# What is real: the instance is a container that boots, runs its
# startup-script, keeps its filesystem across a stop and start, and serves
# GCE-shaped metadata. What is not: there is no kernel of its own, no
# persistent disk, no guest environment, no OS Login and **no sshd** --
# Nimbus is stdlib-only, so it cannot run an SSH server. `nimbus ssh` is
# `docker exec`. The ssh-keys metadata below is recorded and written to the
# instance's authorized_keys so the machine looks the way this file says it
# does; `ssh -i` does not work, and that is a limitation rather than a bug.

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

  # Nimbus has no OAuth: the token is accepted and never looked at.
  access_token = "nimbus"

  # compute and storage carry their version in the endpoint; iam and
  # container do not, because the client appends it. There is no rule --
  # see the protocol facts in CLAUDE.md.
  compute_custom_endpoint             = "${var.nimbus_endpoint}/compute/v1/"
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

# --- the network --------------------------------------------------------------

resource "google_compute_network" "vpc" {
  name                    = "nimbus-instance-net"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "nodes" {
  name          = "nimbus-instance-subnet"
  ip_cidr_range = "10.20.0.0/24"
  region        = var.region
  network       = google_compute_network.vpc.id
}

# Recorded and never enforced: nothing in Nimbus filters a packet. It is
# here because a real config has one, and because the graph draws it.
resource "google_compute_firewall" "ssh" {
  name    = "nimbus-allow-ssh"
  network = google_compute_network.vpc.id

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }

  source_ranges = ["0.0.0.0/0"]
  target_tags   = ["ssh"]
}

# --- the instance -------------------------------------------------------------

resource "google_compute_instance" "web" {
  name         = "web"
  machine_type = "e2-micro"
  zone         = "${var.region}-a"

  # The image name is what picks the container image -- `debian-12` says
  # what it is, so nothing else has to. That is the opposite of an EC2
  # instance, where an AMI id is opaque and a tag has to say.
  boot_disk {
    initialize_params {
      image = "debian-cloud/debian-12"
    }
  }

  network_interface {
    subnetwork = google_compute_subnetwork.nodes.id
  }

  # In GCE a startup script is METADATA, where EC2 has a separate user-data
  # field. Both end up as the boot script of the container.
  metadata_startup_script = <<-SH
    #!/bin/sh
    echo "booted at $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "I am $(hostname), address $(hostname -i)"
    echo "apt is present: $(command -v apt-get)"
  SH

  metadata = {
    # "user:key" per line, because one instance can carry keys for several
    # users. Nimbus keeps the key and drops the user prefix.
    ssh-keys = "nimbus:ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleKeyForNimbusLocalOnly nimbus@local"
  }

  # Network tags -- strings a firewall rule matches on -- not the key/value
  # pairs EC2 calls tags. Those are `labels`, below.
  tags = ["web", "ssh"]

  labels = {
    example = "gcp-instance"
  }
}

output "instance_name" {
  value = google_compute_instance.web.name
}

output "internal_ip" {
  value       = google_compute_instance.web.network_interface[0].network_ip
  description = "Derived from the instance's self link, so it is the same on every read."
}

output "shell" {
  value       = "nimbus ssh web"
  description = "A shell on the instance. docker exec, not SSH -- see the note at the top."
}

output "startup_log" {
  value       = "nimbus ssh web -- cat /var/log/nimbus-boot.log"
  description = "What the startup script printed."
}
