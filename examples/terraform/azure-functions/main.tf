# An Azure Function that really runs, deployed by Terraform.
#
# Until Milestone 20 this was the last thing Azure could not do here. AWS
# Lambda was comparatively cheap because every
# public.ecr.aws/lambda/<runtime> image ships the Runtime Interface
# Emulator, the runtime client and the SDK inside it, so Nimbus mounts a
# handler into a container that already knows how to run it. Microsoft
# publishes a host image too -- the roadmap said for several milestones
# that there was "no Azure equivalent to lean on", and that was wrong --
# but it expects a DEPLOY rather than a mount, and that single difference
# shaped all of this.
#
# Three things were measured before any of it was built:
#
#   1. The host runs offline. It binds :80 and logs "Job host started"
#      with no network at all.
#   2. A BIND MOUNT makes it find nothing. With the code mounted at
#      /home/site/wwwroot it reports "0 functions found" for BOTH
#      programming models, with nothing in the logs saying why. The
#      identical tree baked in with a COPY loads and serves it. So Nimbus
#      BUILDS an image per deployment -- still offline, since a local base
#      image plus a COPY touches no network.
#   3. The image ships the host and the Python worker but NOT the
#      azure-functions package. Real Azure installs requirements.txt for
#      you with a remote build, which needs the network. Here the
#      dependencies have to be VENDORED, which is step 2 below.
#
# Running it:
#
#   1. nimbus start --clouds azure          (in another terminal)
#   2. Vendor the dependencies. This is the one step real Azure does for
#      you, and Nimbus refuses the deploy without it rather than letting
#      every invocation fail on `import azure.functions`:
#
#        cd handler
#        pip install --target .python_packages/lib/site-packages -r requirements.txt
#        cd ..
#
#   3. docker pull mcr.microsoft.com/azure-functions/python:4-python3.11
#      (once, while online -- about 1.5 GB, and the apply is much faster
#      with it already local)
#   4. terraform init && terraform apply
#   5. curl -k "https://$(terraform output -raw hostname)/api/hello?name=nimbus"
#        -> hello nimbus from azure functions
#   6. docker logs $(docker ps -qf label=nimbus.role=functionapp)
#        -> the handler's own print, beside the host's
#           "Executed 'Functions.hello' (Succeeded, ...)"
#   7. terraform destroy
#
# Docker must be running: a function app is a container here, built from
# an image of its own.

terraform {
  required_version = ">= 1.5"
  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.0"
    }
    archive = {
      source  = "hashicorp/archive"
      version = "~> 2.4"
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

resource "azurerm_resource_group" "rg" {
  name     = "rg-nimbus-functions"
  location = "eastus"
}

# A function app REQUIRES a storage account -- azurerm will not create one
# without it, and it builds AzureWebJobsStorage from it automatically.
#
# Worth noticing in the applied state: the connection string it writes
# carries `EndpointSuffix=storage.nimbus.localhost:4701`, which is
# Nimbus's own storage data plane. The host reports that one component
# unhealthy ("Unable to create client for AzureWebJobsStorage") and serves
# HTTP triggers perfectly without it, so nothing here depends on it -- but
# the wiring is genuinely pointed at Nimbus rather than at Azure.
resource "azurerm_storage_account" "fn" {
  name                     = "stnimbusfunctions"
  resource_group_name      = azurerm_resource_group.rg.name
  location                 = azurerm_resource_group.rg.location
  account_tier             = "Standard"
  account_replication_type = "LRS"
}

# The plan. Y1 is the consumption tier, which is what a function app
# usually runs on; Nimbus reports the tier, size, family and kind that
# follow from the sku name, because azurerm reads them back when it
# updates the app.
resource "azurerm_service_plan" "fn" {
  name                = "asp-nimbus-functions"
  resource_group_name = azurerm_resource_group.rg.name
  location            = azurerm_resource_group.rg.location
  os_type             = "Linux"
  sku_name            = "Y1"
}

# The deployment package: the handler directory, including the
# .python_packages you vendored in step 2.
data "archive_file" "handler" {
  type        = "zip"
  source_dir  = "${path.module}/handler"
  output_path = "${path.module}/handler.zip"
}

resource "azurerm_linux_function_app" "fn" {
  name                = "fn-nimbus"
  resource_group_name = azurerm_resource_group.rg.name
  location            = azurerm_resource_group.rg.location
  service_plan_id     = azurerm_service_plan.fn.id

  storage_account_name       = azurerm_storage_account.fn.name
  storage_account_access_key = azurerm_storage_account.fn.primary_access_key

  site_config {
    application_stack {
      # This becomes `linuxFxVersion: "PYTHON|3.11"` in the request, which
      # is where the runtime really lives -- a better signal than
      # FUNCTIONS_WORKER_RUNTIME, which carries no version at all. Nimbus
      # picks the host image from it, and REFUSES a stack it has no image
      # for rather than running the nearest one.
      python_version = "3.11"
    }
  }

  app_settings = {
    GREETING = "hello from nimbus"
  }

  # The zip deploy. azurerm publishes it through the deployment (Kudu)
  # site at {app}.scm.{suffix} -- a SECOND hostname, two labels deep,
  # which is why the certificate needs its own wildcard for it.
  #
  # Nimbus unpacks the archive, refuses it if the dependencies are not
  # vendored, rebuilds the app's image and restarts the container. That
  # rebuild is why a deploy takes a few seconds rather than being instant.
  zip_deploy_file = data.archive_file.handler.output_path
}

output "hostname" {
  value       = azurerm_linux_function_app.fn.default_hostname
  description = "curl -k \"https://<this>/api/hello?name=nimbus\""
}

output "call_it" {
  value = "curl -k \"https://${azurerm_linux_function_app.fn.default_hostname}/api/hello?name=nimbus\""
}
