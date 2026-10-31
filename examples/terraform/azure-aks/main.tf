# A Kubernetes cluster that actually runs.
#
#   1. ./bin/nimbus start --persist     (in another terminal; Docker must be running)
#   2. terraform apply                  and watch http://localhost:4700
#   3. kubectl --kubeconfig ./kubeconfig get nodes
#   4. terraform destroy
#
# The cluster is k3s in a Docker container. `kubectl` talks to the real
# Kubernetes API inside it, so anything you deploy genuinely runs -- this is
# the first example where Nimbus is not just remembering JSON.
#
# node_count is real too: a cluster of N nodes is the k3s server container
# plus N-1 agent containers, and they show up in `docker ps`.

terraform {
  required_version = ">= 1.5"
  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.0"
    }
    local = {
      source  = "hashicorp/local"
      version = "~> 2.4"
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
}

variable "location" {
  type    = string
  default = "eastus"
}

variable "node_count" {
  type        = number
  default     = 2
  description = "Nodes in the default pool. Change it and re-apply to see containers appear or go."
}

resource "azurerm_resource_group" "aks" {
  name     = "rg-aks-dev"
  location = var.location
}

resource "azurerm_virtual_network" "aks" {
  name                = "vnet-aks-dev"
  location            = azurerm_resource_group.aks.location
  resource_group_name = azurerm_resource_group.aks.name
  address_space       = ["10.10.0.0/16"]
}

resource "azurerm_subnet" "nodes" {
  name                 = "snet-nodes"
  resource_group_name  = azurerm_resource_group.aks.name
  virtual_network_name = azurerm_virtual_network.aks.name
  address_prefixes     = ["10.10.1.0/24"]
}

resource "azurerm_kubernetes_cluster" "demo" {
  name                = "aks-nimbus-dev"
  location            = azurerm_resource_group.aks.location
  resource_group_name = azurerm_resource_group.aks.name
  dns_prefix          = "nimbusdev"

  default_node_pool {
    name           = "default"
    node_count     = var.node_count
    vm_size        = "Standard_DS2_v2"
    vnet_subnet_id = azurerm_subnet.nodes.id
  }

  identity {
    type = "SystemAssigned"
  }
}

# The kubeconfig, written where kubectl can find it. Terraform calls the
# credential action on every refresh, so this file has to come back identical
# each time or the plan would never settle.
resource "local_file" "kubeconfig" {
  content         = azurerm_kubernetes_cluster.demo.kube_config_raw
  filename        = "${path.module}/kubeconfig"
  file_permission = "0600"
}

output "kubectl" {
  value = "kubectl --kubeconfig ${path.module}/kubeconfig get nodes"
}

output "cluster_fqdn" {
  value = azurerm_kubernetes_cluster.demo.fqdn
}
