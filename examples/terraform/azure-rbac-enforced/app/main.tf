# The application side: a VNet, applied as a restricted principal.
#
# Apply ../platform first. Then this fails, is granted, and succeeds --
# with nothing in THIS file changing between the two. See ../platform for
# the sequence and for why the example is two modules.

terraform {
  required_version = ">= 1.5"
  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.0"
    }
  }
}

# This provider CLAIMS to be the application's principal.
#
# Identity in Nimbus is the bearer token's `oid`, and the token endpoint
# derives that from the client_id a token was requested for -- so setting
# client_id to the principal's GUID is how a client says who it is.
# Nothing is verified, which is the right trade for the question being
# asked: "would these permissions work", not "is this caller genuine".
# See internal/azure/principal.go.
provider "azurerm" {
  features {}

  subscription_id = var.subscription_id
  tenant_id       = "00000000-0000-0000-0000-000000000001"
  client_id       = var.app_principal_id
  client_secret   = "nimbus"

  resource_provider_registrations = "none"
  metadata_host                   = var.nimbus_host
}

variable "nimbus_host" {
  type    = string
  default = "localhost:4701"
}

variable "subscription_id" {
  type    = string
  default = "00000000-0000-0000-0000-000000000000"
}

variable "app_principal_id" {
  type    = string
  default = "22222222-2222-2222-2222-222222222222"
}

variable "resource_group" {
  type    = string
  default = "rg-nimbus-rbac"
}

# Reading works from the start, because ../platform granted Reader at the
# subscription. That it succeeds while the write below fails is the whole
# distinction RBAC exists to draw, visible in one apply.
data "azurerm_resource_group" "rg" {
  name = var.resource_group
}

# Writing works only once Network Contributor has been granted at the
# resource group. Nothing here mentions a role, a scope or an assignment:
# the resource is ordinary, and the permission is somewhere else entirely.
resource "azurerm_virtual_network" "vnet" {
  name                = "vnet-nimbus"
  location            = data.azurerm_resource_group.rg.location
  resource_group_name = data.azurerm_resource_group.rg.name
  address_space       = ["10.40.0.0/16"]
}

output "vnet_id" {
  value = azurerm_virtual_network.vnet.id
}
