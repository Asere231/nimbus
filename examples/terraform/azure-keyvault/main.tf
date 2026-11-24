# A Key Vault that does cryptography, not just storage.
#
#   1. ./bin/nimbus start      (in another terminal)
#   2. terraform apply         and watch http://localhost:4700
#   3. use the key             (see the commands in the outputs)
#   4. terraform destroy
#
# A secret is a string you put in and get back. A KEY is the opposite: the
# private half never leaves, and the vault performs operations with it on your
# behalf. That distinction is the entire reason Key Vault and KMS exist, and
# it is a property Nimbus really has -- no response from this service carries
# private key material, and a test walks every response body to prove it.
#
# A CERTIFICATE is three objects under one name:
#
#   certificates/web   the public x509
#   keys/web           the private half, usable for signing, never exported
#   secrets/web        the whole thing including the private key, which is how
#                      most clients load one into a TLS stack
#
# That indirection is the surprising part of the service, and it is why
# deleting the certificate takes all three with it.
#
# Everything here is signed by Nimbus with Go's standard library -- crypto/rsa,
# crypto/ecdsa and crypto/x509 -- so it needs no dependency and no network.

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

resource "azurerm_resource_group" "kv" {
  name     = "rg-keyvault"
  location = var.location
}

resource "azurerm_key_vault" "v" {
  name                       = "kv-nimbus-demo"
  location                   = azurerm_resource_group.kv.location
  resource_group_name        = azurerm_resource_group.kv.name
  tenant_id                  = "00000000-0000-0000-0000-000000000001"
  sku_name                   = "standard"
  soft_delete_retention_days = 7
}

# An RSA key declaring all six operations. Nimbus implements all six rather
# than just sign and verify, because key_opts is stored and returned in the
# JWK's key_ops -- a vault that advertised six capabilities and refused four
# would lie about itself.
resource "azurerm_key_vault_key" "signing" {
  name         = "signing-key"
  key_vault_id = azurerm_key_vault.v.id
  key_type     = "RSA"
  key_size     = 2048

  key_opts = [
    "sign",
    "verify",
    "wrapKey",
    "unwrapKey",
    "encrypt",
    "decrypt",
  ]
}

# An elliptic-curve key, which signs and verifies and CANNOT encrypt. That is
# arithmetic rather than a gap, and asking it to encrypt is refused with a
# message saying so.
resource "azurerm_key_vault_key" "ec" {
  name         = "ec-key"
  key_vault_id = azurerm_key_vault.v.id
  key_type     = "EC"
  curve        = "P-256"

  key_opts = ["sign", "verify"]
}

# A self-signed certificate, which Nimbus signs itself. `Self` is the only
# issuer that can work offline: DigiCert and GlobalSign are refused by name,
# because they need a network call and the offline rule is a ground rule.
resource "azurerm_key_vault_certificate" "web" {
  name         = "web"
  key_vault_id = azurerm_key_vault.v.id

  certificate_policy {
    issuer_parameters {
      name = "Self"
    }

    key_properties {
      exportable = true
      key_size   = 2048
      key_type   = "RSA"
      reuse_key  = false
    }

    secret_properties {
      content_type = "application/x-pkcs12"
    }

    x509_certificate_properties {
      subject            = "CN=web.nimbus.localhost"
      validity_in_months = 12
      key_usage          = ["digitalSignature", "keyEncipherment"]

      subject_alternative_names {
        dns_names = ["web.nimbus.localhost", "www.nimbus.localhost"]
      }
    }
  }
}

output "vault_uri" {
  value = azurerm_key_vault.v.vault_uri
}

# The key's PUBLIC half, which is all a client ever gets. There is no
# attribute here for the private one, in azurerm or in Azure.
output "signing_key_id" {
  value = azurerm_key_vault_key.signing.id
}

output "certificate_thumbprint" {
  value = azurerm_key_vault_certificate.web.thumbprint
}

# Two of the certificate's three addresses. azurerm exposes the certificate
# and the secret and has no attribute for the KEY -- the key is simply at the
# same name, which is the indirection this example is about. `az keyvault key
# show --name web` finds it.
output "certificate_addresses" {
  value = {
    certificate = azurerm_key_vault_certificate.web.id
    secret      = azurerm_key_vault_certificate.web.secret_id
  }
}

output "try_it" {
  value = <<-EOT
    # az needs the Nimbus cloud registered and the CA trusted:
    ./bin/nimbus az register --run
    export REQUESTS_CA_BUNDLE=$HOME/.nimbus/certs/nimbus-ca.pem
    az login --service-principal -u 00000000-0000-0000-0000-000000000002 -p nimbus --tenant 00000000-0000-0000-0000-000000000001 --allow-no-subscriptions

    V=${azurerm_key_vault.v.name}

    # 1. the key's public half -- and NO private half, anywhere
    az keyvault key show --vault-name $V --name signing-key

    # 2. sign a digest, then verify it. The private key never moves.
    #
    #    Note the digest is STANDARD base64 WITH its padding: az decodes it
    #    client-side and re-encodes it as base64url for the wire, so stripping
    #    the padding or pre-converting to base64url fails inside az with
    #    "Incorrect padding" before a request is ever sent. And the fields are
    #    `signature` and `isValid`, not `result` and `value`.
    D=$(printf 'hello' | openssl dgst -sha256 -binary | openssl base64 -A)
    S=$(az keyvault key sign --vault-name $V --name signing-key --algorithm RS256 --digest "$D" --query signature -o tsv)
    az keyvault key verify --vault-name $V --name signing-key --algorithm RS256 --digest "$D" --signature "$S" --query isValid

    # 3. an EC key cannot encrypt, and the refusal says why rather than just no
    az keyvault key encrypt --vault-name $V --name ec-key --algorithm RSA-OAEP-256 --value "aGk="

    # 4. the certificate, and the three names it lives under
    az keyvault certificate show --vault-name $V --name web --query "{cert:id, key:kid, secret:sid}"
    az keyvault key show         --vault-name $V --name web --query key.kid
    az keyvault secret show      --vault-name $V --name web --query contentType

    # 5. deleting the certificate takes its key and its secret with it
    az keyvault certificate delete --vault-name $V --name web
    az keyvault key show --vault-name $V --name web
  EOT
}
