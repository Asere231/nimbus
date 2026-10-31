# Storage, with a real data plane: the account, a container, and a blob whose
# bytes Nimbus actually stores.
#
#   1. ./bin/nimbus start      (in another terminal)
#   2. terraform apply         and watch http://localhost:4700
#   3. terraform destroy
#
# Unlike the control-plane-only example, azurerm talks to a second API here:
# https://{account}.blob.storage.nimbus.localhost:4701. That name resolves to
# loopback on macOS and Windows with nothing installed, and the port is carried
# in the metadata suffix because binding 443 would need root.

terraform {
  required_version = ">= 1.5"
  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.0"
    }
  }
}

provider "azurerm" {
  features {}

  metadata_host   = "localhost:4701"
  subscription_id = "00000000-0000-0000-0000-000000000000"
  tenant_id       = "00000000-0000-0000-0000-000000000001"
  client_id       = "00000000-0000-0000-0000-000000000002"
  client_secret   = "nimbus"

  resource_provider_registrations = "none"
  storage_use_azuread             = false
}

variable "location" {
  type    = string
  default = "eastus"
}

resource "azurerm_resource_group" "storage" {
  name     = "rg-storage-dev"
  location = var.location
}

resource "azurerm_storage_account" "data" {
  name                     = "stnimbusdata"
  resource_group_name      = azurerm_resource_group.storage.name
  location                 = azurerm_resource_group.storage.location
  account_tier             = "Standard"
  account_replication_type = "LRS"

  tags = {
    env = "dev"
  }
}

resource "azurerm_storage_container" "uploads" {
  name                  = "uploads"
  storage_account_id    = azurerm_storage_account.data.id
  container_access_type = "private"
}

resource "azurerm_storage_blob" "readme" {
  name                   = "readme.txt"
  storage_account_name   = azurerm_storage_account.data.name
  storage_container_name = azurerm_storage_container.uploads.name
  type                   = "Block"
  source                 = "${path.module}/readme.txt"
}

output "blob_url" {
  value = azurerm_storage_blob.readme.url
}

output "primary_blob_endpoint" {
  value = azurerm_storage_account.data.primary_blob_endpoint
}
