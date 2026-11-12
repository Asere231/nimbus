# IAM that can say no, on GCP.
#
#   1. nimbus start --clouds gcp --enforce-iam     (in another terminal)
#   2. terraform init
#   3. terraform apply              -> FAILS with PERMISSION_DENIED on
#                                      storage.objects.create
#   4. terraform apply -var grant=true
#                                   -> succeeds
#   5. terraform destroy -var grant=true
#
# ONE module, where the Azure example needs two -- and the reason is a
# fact measured back in Milestone 10: **the google provider makes no
# startup calls.** It goes straight to the resource it was told to create,
# so a provider claiming a restricted identity configures fine with no
# permissions at all. azurerm reads the subscription when it configures,
# which means an Azure principal cannot be granted, in the same apply, the
# access its own provider needs in order to start.
#
# Watch http://localhost:4700 while step 3 runs: the refusal is a red line
# naming the member, the permission and the binding that was consulted.

terraform {
  required_version = ">= 1.5"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
  }
}

# The administrative provider. Its token is not member-shaped, so Nimbus
# treats it as the default identity, which holds no bindings and is
# unrestricted -- the role AWS's account root and Azure's default
# client_id play.
provider "google" {
  project = var.project
  region  = var.region

  access_token = "nimbus"

  storage_custom_endpoint             = "${var.nimbus_endpoint}/storage/v1/"
  resource_manager_custom_endpoint    = "${var.nimbus_endpoint}/v1/"
  resource_manager_v3_custom_endpoint = "${var.nimbus_endpoint}/v3/"
}

# The restricted provider.
#
# GCP writes its policy members as `serviceAccount:...`, `user:...` and so
# on -- and since there is no OAuth here and the token is never checked,
# the token can simply BE that member string. No mapping, no derivation,
# nothing stored: what is in this configuration is character-for-character
# what appears in the binding below. See internal/gcp/principal.go.
provider "google" {
  alias   = "app"
  project = var.project
  region  = var.region

  access_token = var.app_member

  storage_custom_endpoint             = "${var.nimbus_endpoint}/storage/v1/"
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
  type    = string
  default = "http://localhost:4703"
}

variable "app_member" {
  type        = string
  default     = "serviceAccount:app@nimbus-local.iam.gserviceaccount.com"
  description = "The member the restricted provider claims to be. Also its bearer token, verbatim."
}

variable "grant" {
  type        = bool
  default     = false
  description = <<-EOT
    Which role the member holds. false grants roles/storage.objectViewer,
    which can read objects and not write them: the apply fails with
    PERMISSION_DENIED. true grants roles/storage.objectAdmin, and the same
    apply succeeds.
  EOT
}

# --- what the administrator builds ------------------------------------------

resource "google_storage_bucket" "data" {
  name                        = "nimbus-iam-enforced"
  location                    = "US"
  force_destroy               = true
  uniform_bucket_level_access = true
}

# The binding under test, at the PROJECT. Every resource here belongs to
# the one project, so a project-level binding grants everywhere -- which
# makes GCP's inheritance trivial where Azure's needed segment-aligned
# prefix matching.
#
# google_project_iam_member is the additive form: it reads the policy,
# adds one member and writes it back, rather than replacing the whole
# document.
resource "google_project_iam_member" "app" {
  project = var.project
  role    = var.grant ? "roles/storage.objectAdmin" : "roles/storage.objectViewer"
  member  = var.app_member
}

# --- what the restricted member tries to do ---------------------------------

# Written by the RESTRICTED provider, so the binding decides. depends_on is
# needed because nothing here references the binding, and without it
# Terraform may write the object before the binding exists -- at which
# point the member holds nothing and the refusal is right for the wrong
# reason.
resource "google_storage_bucket_object" "report" {
  provider = google.app

  name    = "reports/today.txt"
  bucket  = google_storage_bucket.data.name
  content = "written by ${var.app_member}\n"

  depends_on = [google_project_iam_member.app]
}

# --- a binding on the RESOURCE, not on the project -------------------------
#
# Milestone 17. Everything above grants at the PROJECT, which reaches
# every resource in it. A bucket's own IAM policy grants on that bucket
# alone -- and on its objects, which is what everybody means by giving a
# service account access to a bucket.
#
# `second_member` holds NO project binding at all. It can write into
# nimbus-iam-enforced and nowhere else, and the only thing that grants it
# is google_storage_bucket_iam_member below. Turn `resource_binding` off
# and the same apply fails with PERMISSION_DENIED.
#
# Note which API this uses. Pub/Sub and Cloud Run set a resource policy
# with a colon verb (`topics/t:setIamPolicy`); Cloud Storage uses a
# SUB-RESOURCE whose HTTP method carries the verb (`PUT /b/{bucket}/iam`),
# with the policy as the body rather than wrapped in {"policy": ...}.
# Four conventions for IAM in one cloud.

variable "second_member" {
  type        = string
  default     = "serviceAccount:reports@nimbus-local.iam.gserviceaccount.com"
  description = "A member with no project binding, granted only by the bucket's own policy."
}

variable "resource_binding" {
  type        = bool
  default     = true
  description = <<-EOT
    Whether the bucket grants second_member anything. Set it to false and
    the second object fails with PERMISSION_DENIED on
    storage.objects.create, naming a member that holds nothing anywhere
    else -- which is the point: the bucket was the only thing granting it.
  EOT
}

provider "google" {
  alias   = "reports"
  project = var.project
  region  = var.region

  access_token = var.second_member

  storage_custom_endpoint             = "${var.nimbus_endpoint}/storage/v1/"
  resource_manager_custom_endpoint    = "${var.nimbus_endpoint}/v1/"
  resource_manager_v3_custom_endpoint = "${var.nimbus_endpoint}/v3/"
}

resource "google_storage_bucket_iam_member" "reports" {
  count = var.resource_binding ? 1 : 0

  bucket = google_storage_bucket.data.name
  role   = "roles/storage.objectAdmin"
  member = var.second_member
}

resource "google_storage_bucket_object" "from_resource_binding" {
  provider = google.reports

  name    = "reports/from-the-bucket-policy.txt"
  bucket  = google_storage_bucket.data.name
  content = "written by ${var.second_member}, allowed only by the bucket's own policy\n"

  depends_on = [google_storage_bucket_iam_member.reports]
}

output "member" {
  value = var.app_member
}

output "role" {
  value = google_project_iam_member.app.role
}
