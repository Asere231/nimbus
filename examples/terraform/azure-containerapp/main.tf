# A container that really runs, on Azure.
#
# Until Milestone 15 this was the one thing Azure could not do here: AWS
# runs a Lambda in AWS's own base image and GCP runs a Cloud Run service in
# any image the daemon can pull, while Azure ran a Kubernetes cluster and a
# VM and nothing in between.
#
#   1. nimbus start --clouds azure          (in another terminal)
#   2. terraform init && terraform apply
#   3. curl -k "https://$(terraform output -raw fqdn)/"
#        -> whatever the image serves, proxied through Nimbus, so the
#           request shows up in the activity feed at localhost:4700
#   4. terraform destroy
#
# The FQDN has ONE label for the app and then a shared suffix, where real
# Azure puts the environment and the region in as well. A TLS wildcard
# matches exactly one label, so an {app}.{env}. name would need a
# certificate per environment and could not be issued ahead of time. The
# practical consequence: two environments cannot hold apps of the same
# name here, which real Azure allows.
#
# The suffix also carries Nimbus's port, as the storage and Key Vault ones
# do, because binding 443 needs root.
#
# Two things measured here against azurerm 4.81.0:
#
#   * **azurerm polls the RESOURCE, not an operation.** There is not one
#     Azure-AsyncOperation request in a debug log of this apply: it PUTs,
#     then re-reads the app until provisioningState is Succeeded. Same as
#     GKE, and the opposite of Cloud Run, which ignores its service and
#     polls the operation. So the create is long-running through
#     Hook.Provision and the apply genuinely waits -- proven with an image
#     that had to be pulled: 27 seconds, and the container was answering
#     the instant the apply returned.
#   * **azurerm calls `listSecrets` right after creating an app.** It
#     tolerates an empty answer, so this never failed -- but Nimbus
#     answered the generic `{}` and put a red "unimplemented action" line
#     in the activity feed for something that was working, which is how
#     people learn to ignore the colour. It answers an empty secrets list
#     now.

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

  subscription_id = "00000000-0000-0000-0000-000000000000"
  tenant_id       = "00000000-0000-0000-0000-000000000001"
  client_id       = "00000000-0000-0000-0000-000000000002"
  client_secret   = "nimbus"

  resource_provider_registrations = "none"
  metadata_host                   = var.nimbus_host
}

variable "nimbus_host" {
  type    = string
  default = "localhost:4701"
}

variable "image" {
  type        = string
  default     = "nginx:alpine"
  description = <<-EOT
    The image to run. Anything the Docker daemon can pull works, because
    the engine behind this is the same one Cloud Run uses. Pre-pull it
    while online if you want the apply to be fast: docker pull nginx:alpine
  EOT
}

resource "azurerm_resource_group" "rg" {
  name     = "rg-nimbus-app"
  location = "eastus"
}

# A workspace, because a Container Apps environment takes one and because
# it is where the app's output goes once Log Analytics can hold it. With no
# workspace the output stays in the container's console only, which is what
# real Azure does too.
resource "azurerm_log_analytics_workspace" "logs" {
  name                = "law-nimbus"
  location            = azurerm_resource_group.rg.location
  resource_group_name = azurerm_resource_group.rg.name
  sku                 = "PerGB2018"
  retention_in_days   = 30
}

resource "azurerm_container_app_environment" "env" {
  name                       = "cae-nimbus"
  location                   = azurerm_resource_group.rg.location
  resource_group_name        = azurerm_resource_group.rg.name
  log_analytics_workspace_id = azurerm_log_analytics_workspace.logs.id
}

resource "azurerm_container_app" "web" {
  name                         = "web"
  container_app_environment_id = azurerm_container_app_environment.env.id
  resource_group_name          = azurerm_resource_group.rg.name
  revision_mode                = "Single"

  template {
    container {
      name   = "main"
      image  = var.image
      cpu    = 0.25
      memory = "0.5Gi"

      env {
        name  = "GREETING"
        value = "hello from nimbus"
      }
    }
  }

  # targetPort is what the image listens on INSIDE the container. The
  # engine defaults to 8080, which is Cloud Run's convention and wrong for
  # most Azure images, so this matters.
  ingress {
    external_enabled = true
    target_port      = 80

    traffic_weight {
      percentage      = 100
      latest_revision = true
    }
  }
}

output "fqdn" {
  value       = azurerm_container_app.web.ingress[0].fqdn
  description = "curl -k https://<this>/"
}

output "workspace_customer_id" {
  value = azurerm_log_analytics_workspace.logs.workspace_id
}
