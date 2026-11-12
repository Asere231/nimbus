# Pointing real clients at Nimbus

Nimbus is only useful if the tools you already use talk to it. Nothing here
is a Nimbus-specific SDK — these are the ordinary endpoint-override settings
every cloud client has.

- [Azure](#azure) — Terraform, `az`, the SDKs
- [AWS](#aws) — Terraform, `aws`, the SDKs
- [Google Cloud](#google-cloud) — Terraform, `gcloud`
- [Kubernetes](#kubernetes)
- [What the identifiers are](#what-the-identifiers-are)

Start Nimbus first: `nimbus start --clouds azure,aws,gcp`.

---

## Azure

Azure is the one cloud that requires **HTTPS**, so trust the CA first
([INSTALL.md](INSTALL.md#trust-the-certificate)).

### Terraform

```hcl
provider "azurerm" {
  features {}

  metadata_host   = "localhost:4701"
  subscription_id = "00000000-0000-0000-0000-000000000000"
  tenant_id       = "00000000-0000-0000-0000-000000000001"
  client_id       = "00000000-0000-0000-0000-000000000002"
  client_secret   = "nimbus"

  resource_provider_registrations = "none"
}
```

`metadata_host` is the whole mechanism: the provider fetches a metadata
document from it and learns every other endpoint — ARM, the login authority,
the storage and Key Vault DNS suffixes — from that one setting.

### The `az` CLI

```bash
nimbus az register --run
az login --service-principal -u 00000000-0000-0000-0000-000000000002 \
  -p nimbus --tenant 00000000-0000-0000-0000-000000000001 --allow-no-subscriptions
```

Then `az group list`, `az storage blob upload`, `az keyvault secret show` and
the rest work normally. `az cloud set -n AzureCloud` switches back.

### The SDKs

```bash
eval "$(nimbus env azure)"                    # macOS / Linux
nimbus env azure | Invoke-Expression          # PowerShell
```

For the storage data plane, a connection string spells out every endpoint so
no client has to guess the port:

```bash
eval "$(nimbus env azure --storage-account mystorageaccount)"
# exports AZURE_STORAGE_CONNECTION_STRING
```

### Storage and Key Vault addresses

Data planes are addressed by hostname, as in real Azure, and those names
resolve to loopback with nothing installed — no hosts file, no DNS server:

```
https://{account}.blob.storage.nimbus.localhost:4701/
https://{vault}.vault.nimbus.localhost:4701/
https://{app}.containerapps.nimbus.localhost:4701/
```

The suffix carries the port, which real Azure's never does — binding 443
would need root.

## AWS

Plain HTTP, no certificate. Credentials are accepted on shape alone;
signatures are never verified, so any values will do.

### Terraform

```hcl
provider "aws" {
  region     = "us-east-1"
  access_key = "nimbus"
  secret_key = "nimbus"

  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
  skip_region_validation      = true

  s3_use_path_style = true

  endpoints {
    sts = "http://localhost:4702"
    iam = "http://localhost:4702"
    ec2 = "http://localhost:4702"
    s3  = "http://localhost:4702"
    # ...and any other service you use
  }
}
```

`s3_use_path_style` matters: it is why AWS needs no TLS here. Path-style
means no `bucket.host` name, so no certificate and no DNS.

### The `aws` CLI

```bash
export AWS_ACCESS_KEY_ID=nimbus
export AWS_SECRET_ACCESS_KEY=nimbus
export AWS_DEFAULT_REGION=us-east-1
export AWS_ENDPOINT_URL=http://localhost:4702

aws sts get-caller-identity
aws s3 ls s3://my-bucket
aws ec2 describe-vpcs
```

`AWS_ENDPOINT_URL` is read by every current SDK and CLI version. For older
ones, pass `--endpoint-url http://localhost:4702` per command. For `aws s3`,
path-style addressing needs `addressing_style = path` in your config.

**aws-cli v1 and v2 differ** in two places that matter. `--payload` has no
common inline spelling — use `--payload fileb://file.json`, which means "these
bytes, verbatim" to both. And v2 wraps request bodies in `aws-chunked`
framing, which Nimbus decodes.

### Your own code

Nothing special. Set `AWS_ENDPOINT_URL` and an SDK written against real AWS
works unchanged — including code running **inside** a Nimbus Lambda, where
the variable is set for you (pointing at `host.docker.internal`, because
inside a container `localhost` is the container).

## Google Cloud

Plain HTTP, no certificate, no credentials.

### Terraform

```hcl
provider "google" {
  project = "nimbus-local"
  region  = "us-central1"
  zone    = "us-central1-a"

  compute_custom_endpoint              = "http://localhost:4703/compute/v1/"
  storage_custom_endpoint              = "http://localhost:4703/storage/v1/"
  iam_custom_endpoint                  = "http://localhost:4703/"
  container_custom_endpoint            = "http://localhost:4703/"
  cloud_resource_manager_custom_endpoint = "http://localhost:4703/"
}
```

**The version belongs to the client, not the service.** `compute` and
`storage` carry `/v1/` because the provider does not append it; `iam`,
`container` and `cloudresourcemanager` are the bare origin because it does.
Getting this wrong produces `/v1/v1/projects/...`.

### `gcloud`

```bash
nimbus gcp register --run
```

Full command list and caveats in [GCLOUD.md](GCLOUD.md). On Windows in Git
Bash, run `gcloud.cmd` rather than `gcloud`.

## Kubernetes

Clusters are real k3s in Docker, so `kubectl` works against them normally.

```bash
# EKS
aws eks update-kubeconfig --name mycluster --kubeconfig ./kubeconfig
KUBECONFIG=./kubeconfig kubectl get nodes

# GKE
nimbus gcp kubeconfig mycluster --out ./kubeconfig
KUBECONFIG=./kubeconfig kubectl get nodes

# AKS
az aks get-credentials --resource-group rg --name mycluster --file ./kubeconfig
```

EKS authenticates through a **token authentication webhook**, the same
mechanism real EKS uses — `aws eks get-token` builds its bearer token
entirely client-side, so nothing else could work. The `aws` CLI must be on
your PATH and its credentials set in the shell running `kubectl`, because
the kubeconfig shells out to it on every call.

## What the identifiers are

Fixed, so examples and documentation can name them.

| Cloud | Thing | Value |
|---|---|---|
| Azure | subscription | `00000000-0000-0000-0000-000000000000` |
| Azure | tenant | `00000000-0000-0000-0000-000000000001` |
| Azure | client / app | `00000000-0000-0000-0000-000000000002` |
| Azure | client secret | `nimbus` |
| AWS | account | `000000000000` |
| AWS | credentials | anything |
| GCP | project | `nimbus-local` |

Nimbus **verifies no signature and no token**. It is an emulator on your
machine with nothing to protect, and a local emulator rejecting a signature
could only ever be reporting that your clock is wrong. Identity is still
read from the credential, because `--enforce-iam` needs to know who you are
claiming to be — see [IAM.md](IAM.md).
