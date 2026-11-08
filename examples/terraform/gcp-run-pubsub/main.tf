# A Cloud Run service invoked by a Pub/Sub push subscription, for real.
#
#   1. nimbus start --clouds gcp          (in another terminal)
#   2. terraform init                     (needs internet once, or a provider
#                                          mirror: see docs/OFFLINE.md)
#   3. terraform apply                    and watch http://localhost:4700
#   4. curl "$(terraform output -raw service_url)"
#   5. gcloud pubsub topics publish nimbus-events --message '{"hello":"world"}'
#      (or see the publish command in the outputs for a plain curl)
#   6. docker logs "$(docker ps --filter label=nimbus.service=nimbus-echo -q)"
#   7. terraform destroy
#
# The service is a real container: Nimbus pulls the image, starts it with
# PORT set and proxies every request through to it. The handler below is
# the container's own code, so what you see in its logs is what Python
# really printed.
#
# Docker must be running. The first apply pulls python:3.12-alpine.

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

  access_token = "nimbus"

  compute_custom_endpoint             = "${var.nimbus_endpoint}/compute/v1/"
  iam_custom_endpoint                 = "${var.nimbus_endpoint}/"
  resource_manager_custom_endpoint    = "${var.nimbus_endpoint}/v1/"
  resource_manager_v3_custom_endpoint = "${var.nimbus_endpoint}/v3/"

  # Both of these carry their version, unlike iam and container above.
  # There is no rule: the provider appends the version for some services
  # and expects it in the endpoint for others, and nothing announces which
  # -- so Nimbus accepts a path either way rather than 404ing one of them.
  pubsub_custom_endpoint       = "${var.nimbus_endpoint}/v1/"
  cloud_run_v2_custom_endpoint = "${var.nimbus_endpoint}/v2/"
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

# The service. Its code is inline rather than in an image of its own, so
# there is nothing to build and nothing to push: the image is stock Python
# and the handler is an argument to it.
#
# A real deployment would use its own image. Nimbus pulls whatever the
# Docker daemon can reach, including a registry on localhost -- so an image
# pushed to the emulated ACR from the azure-app example works here too.
resource "google_cloud_run_v2_service" "echo" {
  name     = "nimbus-echo"
  location = var.region

  deletion_protection = false

  template {
    containers {
      image = "python:3.12-alpine"

      # -u so prints reach the log immediately rather than sitting in a
      # buffer, which is the difference between seeing a message arrive
      # and wondering whether it did.
      command = ["python", "-u", "-c"]

      args = [<<-PY
        import base64, json, os
        from http.server import BaseHTTPRequestHandler, HTTPServer

        class Handler(BaseHTTPRequestHandler):
            # Pub/Sub pushes a POST carrying its own envelope: the payload
            # is base64 under message.data, with the subscription beside it.
            def do_POST(self):
                length = int(self.headers.get("Content-Length") or 0)
                raw = self.rfile.read(length)
                try:
                    envelope = json.loads(raw)
                    message = envelope["message"]
                    data = base64.b64decode(message.get("data", "")).decode()
                    print("pubsub %s from %s: %s" % (
                        message.get("messageId"), envelope.get("subscription"), data))
                except Exception as err:
                    print("could not read the push envelope: %s" % err)
                # 204 means delivered. Pub/Sub treats any 2xx as an ack and
                # anything else as something to retry.
                self.send_response(204)
                self.end_headers()

            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Type", "text/plain")
                self.end_headers()
                self.wfile.write(b"nimbus cloud run: ok\n")

            def log_message(self, *args):
                pass

        # PORT is the contract: Cloud Run tells the container where to listen.
        port = int(os.environ.get("PORT", "8080"))
        print("listening on %d as %s" % (port, os.environ.get("K_SERVICE")))
        HTTPServer(("", port), Handler).serve_forever()
      PY
      ]
    }
  }
}

# Anyone may invoke it. Nimbus records this and never enforces it -- no
# request here is refused for lacking a role, the same honest limitation
# Azure RBAC and AWS IAM have.
resource "google_cloud_run_v2_service_iam_member" "public" {
  location = google_cloud_run_v2_service.echo.location
  name     = google_cloud_run_v2_service.echo.name
  role     = "roles/run.invoker"
  member   = "allUsers"
}

resource "google_pubsub_topic" "events" {
  name = "nimbus-events"

  labels = {
    example = "gcp-run-pubsub"
  }
}

# The push subscription that ties the two together. Publishing to the topic
# POSTs to the service's URL, which is why that URL has to be stable: it
# lives in this resource, and a container's port does not survive a
# redeploy.
resource "google_pubsub_subscription" "to_run" {
  name  = "nimbus-events-to-run"
  topic = google_pubsub_topic.events.id

  ack_deadline_seconds = 20

  push_config {
    push_endpoint = google_cloud_run_v2_service.echo.uri

    # Real Pub/Sub signs the push with this identity so the service can
    # verify it. Nimbus records it and mints nothing -- like every other
    # credential here.
    oidc_token {
      service_account_email = google_service_account.pusher.email
    }
  }
}

resource "google_service_account" "pusher" {
  account_id   = "nimbus-pubsub-push"
  display_name = "Pushes Pub/Sub messages to Cloud Run"
}

output "service_url" {
  value       = google_cloud_run_v2_service.echo.uri
  description = "Stable, and proxied by Nimbus so invocations show in the activity feed."
}

output "publish_with_curl" {
  value = join(" ", [
    "curl -s -X POST",
    "'${var.nimbus_endpoint}/v1/${google_pubsub_topic.events.id}:publish'",
    "-H 'Content-Type: application/json'",
    "-d '{\"messages\":[{\"data\":\"eyJoZWxsbyI6IndvcmxkIn0=\"}]}'",
  ])
  description = "Publishes {\"hello\":\"world\"} without gcloud."
}
