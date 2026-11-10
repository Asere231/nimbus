# A Linux virtual machine that really boots, with custom data that really
# runs.
#
#   1. nimbus start --clouds azure         (in another terminal)
#   2. terraform init                      (needs internet once, or a
#                                           provider mirror: see docs/OFFLINE.md)
#   3. terraform apply                     and watch http://localhost:4700
#   4. nimbus ssh vm-nimbus                a shell on the machine
#      nimbus ssh vm-nimbus -- cat /var/log/nimbus-boot.log
#   5. terraform destroy
#
# Docker must be running, and the Nimbus CA has to be trusted (`nimbus
# cert`) or SSL_CERT_FILE pointed at it. The first apply pulls ubuntu:24.04
# -- a real Ubuntu, so apt works inside the machine.
#
# Azure started from the opposite place to the other two clouds here: the
# VM type was already in Nimbus's spec table, so this example APPLIED
# before any of the code behind it existed -- it just did not run anything.
#
# What is real: the machine is a container that boots, runs its custom
# data, keeps its filesystem, and is waited for by Terraform the way a real
# VM is. What is not: there is no kernel of its own, no managed disk, no
# waagent, no VM extensions, no IMDS at 169.254.169.254 and **no sshd** --
# Nimbus is stdlib-only, so it cannot run an SSH server. `nimbus ssh` is
# `docker exec`. The key below is a real throwaway public key, because
# azurerm decodes it client-side and refuses an invalid one; it is written
# to the machine's authorized_keys so the configuration is honest, but
# `ssh -i` does not work.

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
}

variable "location" {
  type    = string
  default = "eastus"
}

variable "admin_public_key" {
  type        = string
  default     = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFOZQv8spJwsKhE202zjRHC/h/0m10n5m9B4Yg2jaGS7 nimbus@local"
  description = <<-TEXT
    A throwaway public key. azurerm decodes this client-side and fails with
    `decoding "admin_ssh_key.0.public_key" for public key data` if it is not
    a real key -- the aws provider accepts an obviously fake one, this does
    not. Replace it with your own if you like; nothing verifies it, because
    there is no sshd to verify it against.
  TEXT
}

resource "azurerm_resource_group" "rg" {
  name     = "rg-nimbus-vm"
  location = var.location
}

resource "azurerm_virtual_network" "vnet" {
  name                = "vnet-nimbus"
  address_space       = ["10.30.0.0/16"]
  location            = azurerm_resource_group.rg.location
  resource_group_name = azurerm_resource_group.rg.name
}

resource "azurerm_subnet" "internal" {
  name                 = "internal"
  resource_group_name  = azurerm_resource_group.rg.name
  virtual_network_name = azurerm_virtual_network.vnet.name
  address_prefixes     = ["10.30.1.0/24"]
}

resource "azurerm_network_interface" "nic" {
  name                = "nic-nimbus"
  location            = azurerm_resource_group.rg.location
  resource_group_name = azurerm_resource_group.rg.name

  ip_configuration {
    name                          = "internal"
    subnet_id                     = azurerm_subnet.internal.id
    private_ip_address_allocation = "Dynamic"
  }
}

resource "azurerm_linux_virtual_machine" "vm" {
  name                = "vm-nimbus"
  location            = azurerm_resource_group.rg.location
  resource_group_name = azurerm_resource_group.rg.name
  size                = "Standard_B1s"
  admin_username      = "nimbus"

  network_interface_ids = [azurerm_network_interface.nic.id]

  admin_ssh_key {
    username   = "nimbus"
    public_key = var.admin_public_key
  }

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = "Standard_LRS"
  }

  # The image is a publisher/offer/sku TRIPLE, not an id. Like a GCE image
  # name and unlike an opaque AMI, it says what it is -- so Nimbus derives
  # the container image from it and nothing here has to name one.
  source_image_reference {
    publisher = "Canonical"
    offer     = "ubuntu-24_04-lts"
    sku       = "server"
    version   = "latest"
  }

  # Base64 in the request, the same shape EC2's user data arrives in, where
  # GCE puts a startup script in metadata. This really runs at boot.
  #
  # Note what it does NOT do: redirect into /var/log/nimbus-boot.log. That
  # is the file Nimbus captures the console into, and a script writing
  # there fights the capture and leaves it empty. Print to stdout and let
  # the capture do the writing.
  custom_data = base64encode(<<-SH
    #!/bin/sh
    echo "booted at $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "I am $(hostname), address $(hostname -i)"
    echo "size from the environment: $NIMBUS_VM_SIZE"
    echo "apt is present: $(command -v apt-get)"
  SH
  )

  tags = {
    example = "azure-vm"
  }
}

output "vm_id" {
  value       = azurerm_linux_virtual_machine.vm.virtual_machine_id
  description = "Derived from the resource ID, so it is the same on every read."
}

output "private_ip" {
  value = azurerm_network_interface.nic.private_ip_address
}

output "shell" {
  value       = "nimbus ssh vm-nimbus"
  description = "A shell on the machine. docker exec, not SSH -- see the note at the top."
}

output "boot_log" {
  value       = "nimbus ssh vm-nimbus -- cat /var/log/nimbus-boot.log"
  description = "What the custom-data script printed."
}
