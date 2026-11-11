# The platform side: who may do what.
#
# Run this first, as an administrator would. It creates the resource group
# and grants the application's principal access to it. The application
# itself lives in ../app and is applied by a provider that CLAIMS to be
# that principal.
#
# Two root modules rather than one, and the reason is a measured constraint
# rather than taste: Terraform configures every provider declared in a
# configuration before any resource exists, and azurerm's configuration
# reads the subscription (see azurerm_role_assignment.read). So a principal
# cannot be granted, in the same apply, the access its own provider needs
# in order to start. Real deployments have exactly this ordering -- access
# is granted before the thing that uses it runs -- so splitting the two is
# more faithful than working around it.
#
#   1. nimbus start --clouds azure --enforce-iam     (in another terminal)
#   2. cd platform && terraform init && terraform apply
#   3. cd ../app   && terraform init && terraform apply
#        -> FAILS: AuthorizationFailed on
#           Microsoft.Network/virtualNetworks/write
#   4. cd ../platform && terraform apply -var grant=true
#   5. cd ../app && terraform apply
#        -> succeeds, with NO change to the application's own configuration
#
# Step 5 is the point of the whole example: what changed was the
# permission, not the thing asking for it.
#
# Two things measured here, both worth knowing before meeting them on
# something real:
#
#   * azurerm fails a denial AT ONCE and the whole AuthorizationFailed
#     message survives into the terraform error -- the principal, the
#     action and the scope. No retry storm.
#
#   * Revoking the grant leaves a resource you cannot DESTROY: the delete
#     needs the same permission the create did. Re-apply with
#     -var grant=true and then destroy. (AWS has the same trap and a
#     nastier recovery -- there the refresh itself fails, so it needs
#     -refresh=false. Here reads still work, because Reader is separate.)
#
# Finally: `terraform destroy` here has to run in ../app FIRST, then in
# this directory. The platform module owns the resource group, and
# destroying it would take the VNet inside it with it.

terraform {
  required_version = ">= 1.5"
  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.0"
    }
  }
}

# The administrative provider. Its client_id is Nimbus's default, which is
# the unrestricted identity -- the role AWS's account root plays.
provider "azurerm" {
  features {}

  subscription_id = var.subscription_id
  tenant_id       = "00000000-0000-0000-0000-000000000001"
  client_id       = "00000000-0000-0000-0000-000000000002"
  client_secret   = "nimbus"

  resource_provider_registrations = "none"
  metadata_host                   = var.nimbus_host
}

variable "nimbus_host" {
  type        = string
  default     = "localhost:4701"
  description = "Nimbus's Azure port, which serves the cloud metadata document."
}

variable "subscription_id" {
  type    = string
  default = "00000000-0000-0000-0000-000000000000"
}

variable "app_principal_id" {
  type        = string
  default     = "22222222-2222-2222-2222-222222222222"
  description = "The principal ../app claims to be. Identity in Nimbus is the bearer token's oid, which is derived from the client_id a token was requested for -- so ../app sets its client_id to this."
}

variable "grant" {
  type        = bool
  default     = false
  description = <<-EOT
    Whether the principal also gets Network Contributor at the resource
    group. false leaves it with Reader at the subscription: it can read
    everything and write nothing, so ../app's apply fails. true adds the
    assignment and that same apply succeeds -- without the assignment ever
    naming the VNet, which is what scope inheritance means.
  EOT
}

resource "azurerm_resource_group" "rg" {
  name     = "rg-nimbus-rbac"
  location = "eastus"
}

# Reader at the SUBSCRIPTION, which the principal needs before it can do
# anything at all.
#
# Measured, not guessed: azurerm populates a Resource Provider cache when
# the provider initialises, calling GET /subscriptions/{id}/providers --
# and `resource_provider_registrations = "none"` does NOT skip it. Without
# this, ../app fails during provider startup rather than on the resource
# under test, and the error names the provider block instead of anything
# you wrote.
resource "azurerm_role_assignment" "read" {
  scope                = "/subscriptions/${var.subscription_id}"
  role_definition_name = "Reader"
  principal_id         = var.app_principal_id

  # Nimbus has no Microsoft Graph, so the provider must not try to look the
  # principal up in a directory first.
  skip_service_principal_aad_check = true
}

# The assignment under test, made at the RESOURCE GROUP rather than at the
# VNet. Scope inheritance is what carries it to every resource inside the
# group, now and later.
resource "azurerm_role_assignment" "write" {
  count = var.grant ? 1 : 0

  scope                = azurerm_resource_group.rg.id
  role_definition_name = "Network Contributor"
  principal_id         = var.app_principal_id

  skip_service_principal_aad_check = true
}

output "resource_group" {
  value = azurerm_resource_group.rg.name
}

output "roles" {
  value = var.grant ? "Reader (subscription) + Network Contributor (resource group)" : "Reader (subscription) only"
}
