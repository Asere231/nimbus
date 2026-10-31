# The whole point of Milestone 4: an app that pulls its image from a local
# container registry and reads its password out of a local Key Vault.
#
#   1. docker pull nginx:alpine          (once, while online)
#   2. ./bin/nimbus start --persist      (in another terminal; Docker must be running)
#   3. terraform apply                   and watch http://localhost:4700
#   4. kubectl --kubeconfig ./kubeconfig get pods
#   5. terraform destroy
#
# Four things here are real rather than remembered: the registry is a
# registry:2 container you can `docker push` to, the cluster is k3s, the
# secret is stored by and read back from Nimbus's Key Vault data plane, and
# the pod genuinely pulls the image through the cluster's containerd mirror.
#
# The identity and the role assignments are the part that is NOT real: Nimbus
# records them and draws them, but enforces nothing. Every request it serves is
# served regardless of what was granted.

terraform {
  required_version = ">= 1.5"
  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.30"
    }
    local = {
      source  = "hashicorp/local"
      version = "~> 2.4"
    }
  }
}

provider "azurerm" {
  features {
    key_vault {
      # Nimbus implements both halves of soft delete, so leave the provider's
      # defaults on: destroy purges the vault, and a create that collides with
      # a soft-deleted name recovers it.
      purge_soft_delete_on_destroy    = true
      recover_soft_deleted_key_vaults = true
    }
  }

  metadata_host   = "localhost:4701"
  subscription_id = "00000000-0000-0000-0000-000000000000"
  tenant_id       = "00000000-0000-0000-0000-000000000001"
  client_id       = "00000000-0000-0000-0000-000000000002"
  client_secret   = "nimbus"

  resource_provider_registrations = "none"
}

provider "kubernetes" {
  host                   = azurerm_kubernetes_cluster.app.kube_config[0].host
  client_certificate     = base64decode(azurerm_kubernetes_cluster.app.kube_config[0].client_certificate)
  client_key             = base64decode(azurerm_kubernetes_cluster.app.kube_config[0].client_key)
  cluster_ca_certificate = base64decode(azurerm_kubernetes_cluster.app.kube_config[0].cluster_ca_certificate)
}

variable "location" {
  type    = string
  default = "eastus"
}

variable "source_image" {
  type        = string
  default     = "nginx:alpine"
  description = "An image already on this machine, retagged and pushed into the emulated registry."
}

resource "azurerm_resource_group" "app" {
  name     = "rg-app-dev"
  location = var.location
}

# --- the registry ------------------------------------------------------------

resource "azurerm_container_registry" "app" {
  name                = "acrnimbusdev"
  resource_group_name = azurerm_resource_group.app.name
  location            = azurerm_resource_group.app.location
  sku                 = "Basic"
  admin_enabled       = true
}

# Pushing happens during the apply so the example is one command. The login
# server is localhost:<port>, which Docker treats as insecure by default, so
# this needs no daemon configuration.
resource "terraform_data" "push" {
  triggers_replace = [azurerm_container_registry.app.login_server, var.source_image]

  provisioner "local-exec" {
    command = <<-EOT
      docker tag ${var.source_image} ${azurerm_container_registry.app.login_server}/app:v1
      docker push ${azurerm_container_registry.app.login_server}/app:v1
    EOT
  }
}

# --- the secret --------------------------------------------------------------

resource "azurerm_key_vault" "app" {
  name                       = "kv-nimbus-dev"
  resource_group_name        = azurerm_resource_group.app.name
  location                   = azurerm_resource_group.app.location
  tenant_id                  = "00000000-0000-0000-0000-000000000001"
  sku_name                   = "standard"
  soft_delete_retention_days = 7
  enable_rbac_authorization  = true
}

# Created through the Key Vault DATA plane, not the Resource Manager: this is
# the call that first gets a 401 with a WWW-Authenticate challenge, fetches a
# token for the resource it names, and retries.
resource "azurerm_key_vault_secret" "db" {
  name         = "db-password"
  value        = "hunter2"
  key_vault_id = azurerm_key_vault.app.id
}

# --- identity and access -----------------------------------------------------

resource "azurerm_user_assigned_identity" "app" {
  name                = "id-app"
  resource_group_name = azurerm_resource_group.app.name
  location            = azurerm_resource_group.app.location
}

resource "azurerm_role_assignment" "secrets" {
  scope                = azurerm_key_vault.app.id
  role_definition_name = "Key Vault Secrets User"
  principal_id         = azurerm_user_assigned_identity.app.principal_id
}

resource "azurerm_role_assignment" "pull" {
  scope                = azurerm_container_registry.app.id
  role_definition_name = "AcrPull"
  principal_id         = azurerm_kubernetes_cluster.app.kubelet_identity[0].object_id
}

# --- the cluster -------------------------------------------------------------

resource "azurerm_log_analytics_workspace" "app" {
  name                = "law-app-dev"
  resource_group_name = azurerm_resource_group.app.name
  location            = azurerm_resource_group.app.location
  sku                 = "PerGB2018"
  retention_in_days   = 30
}

resource "azurerm_kubernetes_cluster" "app" {
  name                = "aks-app-dev"
  location            = azurerm_resource_group.app.location
  resource_group_name = azurerm_resource_group.app.name
  dns_prefix          = "nimbusapp"

  default_node_pool {
    name       = "default"
    node_count = 1
    vm_size    = "Standard_DS2_v2"
  }

  identity {
    type = "SystemAssigned"
  }
}

resource "local_file" "kubeconfig" {
  content         = azurerm_kubernetes_cluster.app.kube_config_raw
  filename        = "${path.module}/kubeconfig"
  file_permission = "0600"
}

# --- the app ------------------------------------------------------------------

# Read back through the data plane, the way an application would. In Azure
# this is where the role assignment above would matter.
data "azurerm_key_vault_secret" "db" {
  name         = azurerm_key_vault_secret.db.name
  key_vault_id = azurerm_key_vault.app.id
}

resource "kubernetes_secret" "db" {
  metadata {
    name = "db-password"
  }
  data = {
    password = data.azurerm_key_vault_secret.db.value
  }
}

resource "kubernetes_deployment" "app" {
  depends_on = [terraform_data.push]

  metadata {
    name   = "app"
    labels = { app = "app" }
  }

  spec {
    replicas = 1
    selector {
      match_labels = { app = "app" }
    }
    template {
      metadata {
        labels = { app = "app" }
      }
      spec {
        container {
          name  = "app"
          image = "${azurerm_container_registry.app.login_server}/app:v1"

          env {
            name = "DB_PASSWORD"
            value_from {
              secret_key_ref {
                name = kubernetes_secret.db.metadata[0].name
                key  = "password"
              }
            }
          }
        }
      }
    }
  }
}

output "login_server" {
  value = azurerm_container_registry.app.login_server
}

output "vault_uri" {
  value = azurerm_key_vault.app.vault_uri
}

output "check" {
  value = <<-EOT
    kubectl --kubeconfig ${path.module}/kubeconfig get pods
    kubectl --kubeconfig ${path.module}/kubeconfig exec deploy/app -- printenv DB_PASSWORD
  EOT
}
