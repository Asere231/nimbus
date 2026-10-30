# A small landing zone that runs fully offline against Nimbus.
#
#   1. nimbus start            (in another terminal)
#   2. terraform init          (needs internet once, or a provider mirror: see docs/OFFLINE.md)
#   3. terraform apply         and watch http://localhost:4700
#   4. terraform destroy
#
# Only control-plane resources are used here. Storage accounts, Key Vault and
# AKS also call data-plane endpoints, which arrive in Milestones 2 and 3.

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

  # Everything below points the real azurerm provider at Nimbus.
  metadata_host   = "localhost:4701"
  subscription_id = "00000000-0000-0000-0000-000000000000"
  tenant_id       = "00000000-0000-0000-0000-000000000001"
  client_id       = "00000000-0000-0000-0000-000000000002"
  client_secret   = "nimbus"

  # Nimbus reports every provider namespace as registered; skip the checks.
  resource_provider_registrations = "none"
}

variable "location" {
  type    = string
  default = "eastus"
}

resource "azurerm_resource_group" "platform" {
  name     = "rg-platform-dev"
  location = var.location
  tags = {
    env   = "dev"
    owner = "nimbus"
  }
}

resource "azurerm_virtual_network" "hub" {
  name                = "vnet-hub-dev"
  location            = azurerm_resource_group.platform.location
  resource_group_name = azurerm_resource_group.platform.name
  address_space       = ["10.0.0.0/16"]
}

resource "azurerm_subnet" "aks" {
  name                 = "snet-aks"
  resource_group_name  = azurerm_resource_group.platform.name
  virtual_network_name = azurerm_virtual_network.hub.name
  address_prefixes     = ["10.0.1.0/24"]
}

resource "azurerm_subnet" "app" {
  name                 = "snet-app"
  resource_group_name  = azurerm_resource_group.platform.name
  virtual_network_name = azurerm_virtual_network.hub.name
  address_prefixes     = ["10.0.2.0/24"]
}

resource "azurerm_network_security_group" "app" {
  name                = "nsg-app"
  location            = azurerm_resource_group.platform.location
  resource_group_name = azurerm_resource_group.platform.name

  security_rule {
    name                       = "allow-https"
    priority                   = 100
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "443"
    source_address_prefix      = "*"
    destination_address_prefix = "*"
  }
}

resource "azurerm_subnet_network_security_group_association" "app" {
  subnet_id                 = azurerm_subnet.app.id
  network_security_group_id = azurerm_network_security_group.app.id
}

resource "azurerm_route_table" "app" {
  name                = "rt-app"
  location            = azurerm_resource_group.platform.location
  resource_group_name = azurerm_resource_group.platform.name
}

output "vnet_id" {
  value = azurerm_virtual_network.hub.id
}

output "subnet_ids" {
  value = [azurerm_subnet.aks.id, azurerm_subnet.app.id]
}
