# A Cloud Function that really runs, which completes the set: all three
# clouds run code.
#
#   1. ./bin/nimbus start --clouds gcp          (in another terminal)
#   2. pip install --target src -r src/requirements.txt
#                                               (needs the internet ONCE)
#   3. terraform init && terraform apply        and watch http://localhost:4700
#   4. curl "$(terraform output -raw function_url)?name=nimbus"
#   5. terraform destroy
#
# Step 2 is the interesting one. A Cloud Function's runtime contract is the
# FUNCTIONS FRAMEWORK -- an ordinary pip package your code imports, whose
# `functions-framework` command is just an HTTP server reading $PORT. Real
# Cloud Functions installs your requirements.txt during a remote build; a
# remote build is the one thing an offline emulator cannot have, so the
# source archive has to carry its dependencies. The azure-functions example
# asks the same of you, for the same reason.
#
# What Nimbus then does is what `gcloud functions deploy --source` does: read
# the archive, write a Dockerfile around it, build an image and run it. The
# code is BAKED IN rather than mounted, and the server is the real Functions
# Framework out of your own archive -- not Nimbus pretending to be Google.
#
# Three clouds, three completely different answers:
#
#   Lambda            the base image ships the Runtime Interface Emulator,
#                     so the code is MOUNTED into somebody else's runtime
#   Azure Functions   a .NET host negotiates with a language worker over
#                     gRPC, and finds nothing in a bind mount
#   Cloud Functions   a LIBRARY the code brings, served over $PORT
#
# Docker must be running. The first apply pulls python:3.11-slim.

terraform {
  required_version = ">= 1.5"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
    archive = {
      source  = "hashicorp/archive"
      version = "~> 2.0"
    }
  }
}

provider "google" {
  project = var.project
  region  = var.region

  access_token = "nimbus"

  storage_custom_endpoint          = "${var.nimbus_endpoint}/storage/v1/"
  resource_manager_custom_endpoint = "${var.nimbus_endpoint}/v1/"

  # Cloud Functions Gen 2 answers under the SAME /v2 prefix Cloud Run does.
  # Nimbus tells them apart by the collection segment -- `functions` or
  # `services` -- because there is nothing in the prefix to go on, which is
  # exactly the situation /v1 is already in with four services under it.
  cloudfunctions2_custom_endpoint = "${var.nimbus_endpoint}/v2/"
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

# The source archive. A function's code reaches the platform through Cloud
# STORAGE -- the provider uploads a zip and the function names the bucket and
# object -- so this example exercises two of Nimbus's services at once, and
# the build reads the archive out of Nimbus's own object store.
data "archive_file" "src" {
  type        = "zip"
  source_dir  = "${path.module}/src"
  output_path = "${path.module}/src.zip"
}

resource "google_storage_bucket" "code" {
  name                        = "nimbus-function-source"
  location                    = "US"
  uniform_bucket_level_access = true
  force_destroy               = true
}

resource "google_storage_bucket_object" "src" {
  name   = "hello-${data.archive_file.src.output_md5}.zip"
  bucket = google_storage_bucket.code.name
  source = data.archive_file.src.output_path
}

resource "google_cloudfunctions2_function" "hello" {
  name     = "nimbus-hello"
  location = var.region

  build_config {
    # python310 through python313 are implemented. A runtime Nimbus has not
    # been measured against is refused BY NAME rather than served by
    # whichever interpreter looks closest -- a python312 function quietly
    # running on 3.11 would behave almost right, which is worse than not
    # running at all.
    runtime = "python311"
    # Which function in main.py to call. It reaches the container as
    # FUNCTION_TARGET, which is how the framework learns it.
    entry_point = "hello"

    source {
      storage_source {
        bucket = google_storage_bucket.code.name
        object = google_storage_bucket_object.src.name
      }
    }
  }

  service_config {
    max_instance_count = 1
    available_memory   = "256M"
    timeout_seconds    = 60
  }
}

# The function's address. Stable on purpose: the container's host port
# changes every time it starts, and the provider stores this in state -- so a
# URL carrying the port would be a permanent diff. Nimbus proxies instead,
# which is what Cloud Run does here too.
output "function_url" {
  value = google_cloudfunctions2_function.hello.url
}

# The Cloud Run service a Gen 2 function really runs on. Reported, because
# the provider reads it; not created as a separate resource, because no
# configuration asked for one.
output "backing_service" {
  value = google_cloudfunctions2_function.hello.service_config[0].service
}

output "try_it" {
  value = <<-EOT
    URL=${google_cloudfunctions2_function.hello.url}

    # 1. the handler's own output
    curl "$URL?name=nimbus"

    # 2. its console, which is where print() goes
    docker logs "$(docker ps --filter label=nimbus.resource=${google_cloudfunctions2_function.hello.name} -q)"

    # 3. the image Nimbus built for it, removed again by `terraform destroy`
    docker images --filter 'reference=nimbus-gcf-*'
  EOT
}
