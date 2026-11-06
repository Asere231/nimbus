# Nimbus

A from-scratch, offline, multi-cloud emulator with a live visualizer. Azure and AWS run side by side on the same engine; GCP plugs into it later.

Real tools talk to it unchanged: the Terraform `azurerm` provider and the Azure SDKs see an Azure Resource Manager endpoint, and the visualizer at `http://localhost:4700` draws every resource as it is created.

```
terraform apply  ──HTTPS──▶  Nimbus Azure (ARM) :4701 ──┐
terraform apply  ──HTTP───▶  Nimbus AWS        :4702 ──┴▶ store ──▶ event bus ──SSE──▶  visualizer :4700
```

**Status (v0.1.0, Milestone 1):** ARM control plane with resource groups, virtual networks, subnets, and an "accepted" tier for every other resource type; token and metadata endpoints; live visualizer. Verified with `hashicorp/go-azure-sdk` (the SDK inside the azurerm provider). See [docs/ROADMAP.md](docs/ROADMAP.md).

## Quick start

Needs Go 1.22+ (and later, Docker for AKS). Works on macOS, Windows and Linux.

```bash
# build
go build -o bin/nimbus ./cmd/nimbus          # Windows: go build -o bin\nimbus.exe .\cmd\nimbus

# trust the local CA once (prints the exact command for your OS)
./bin/nimbus cert

# run
./bin/nimbus start                            # add --persist to keep state between runs
```

Open http://localhost:4700, then in another terminal:

```bash
cd examples/terraform/azure-basic
terraform init
terraform apply
```

## Why a certificate?

The azurerm provider only talks HTTPS to the metadata and ARM endpoints. On first start Nimbus creates its own CA in `~/.nimbus/certs` and signs a `localhost` certificate with it. `nimbus cert` prints the one command that trusts that CA on your OS:

| OS | Command (also printed by `nimbus cert`) |
|---|---|
| macOS | `sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ~/.nimbus/certs/nimbus-ca.pem` |
| Windows (PowerShell) | `Import-Certificate -FilePath $HOME\.nimbus\certs\nimbus-ca.pem -CertStoreLocation Cert:\CurrentUser\Root` |
| Linux | copy to `/usr/local/share/ca-certificates/` and run `update-ca-certificates`, or set `SSL_CERT_FILE` |

The CA key never leaves your machine. Remove "Nimbus Local CA" from the same store to undo it.

## Using it from Terraform

Either put the settings in the provider block (see the example) or export them:

```bash
eval "$(./bin/nimbus env azure)"          # PowerShell: ./bin/nimbus.exe env azure | Invoke-Expression
```

| Setting | Value |
|---|---|
| `metadata_host` / `ARM_METADATA_HOSTNAME` | `localhost:4701` |
| `subscription_id` | `00000000-0000-0000-0000-000000000000` (any GUID works) |
| `tenant_id` / `client_id` / `client_secret` | any values; Nimbus issues a token for anything |
| `resource_provider_registrations` | `"none"` |

## What works today

| Tier | Resource types | Behaviour |
|---|---|---|
| Real | Storage accounts and containers, AKS clusters and node pools, Key Vaults, container registries | Something actually runs: bytes on disk, k3s in Docker, a `registry:2` container, secrets you can read back with a client |
| Modelled by hand | Resource groups, virtual networks, subnets, managed identities, role assignments and definitions, Log Analytics workspaces | Validation, server-generated fields, parent/child sync, cascade delete |
| Modelled from the specs | 53 more types across networking, compute, web, data and integration | Read-only fields and defaults taken from Microsoft's own OpenAPI specs, applied generically — no hook per type |
| Accepted | Any other ARM type | Stored and returned with `provisioningState: Succeeded`; nothing runs behind it |
| Actions | `listKeys`, `listClusterUserCredential`, `listCredentials`, `sharedKeys`, … | Answered for the types above; anything else returns `{}` and shows as "unimplemented" in the activity feed |

Data planes: Blob, Queue and Table (Shared Key verified, real bytes), and Key
Vault secrets (challenge auth, versions, soft delete).

**ARM and Bicep templates deploy**: `az deployment group create --template-file
main.bicep` expands the template and creates each resource through the normal
path, so it appears in the visualizer as it goes. See `examples/bicep`.

**The `az` CLI works** as a custom cloud — `nimbus az register --run`, then
`az group create`, `az network vnet create`, `az resource list`, `az keyvault
secret set/show` and `az storage blob`.

## AWS

`nimbus start --clouds azure,aws` adds an AWS endpoint on port 4702. Plain
HTTP, because no AWS client insists on TLS and S3 is addressed path-style —
so there is nothing to trust on your machine.

| Service | What works |
|---|---|
| STS | `GetCallerIdentity`, `AssumeRole` |
| EC2 | VPCs, subnets, internet and NAT gateways, route tables and routes, security groups and rules, tags, availability zones, account attributes, with the describe filters the provider sends |
| S3 | Buckets path-style with their whole sub-configuration matrix (versioning, tagging, policy, encryption, public access block, ownership, …) and objects with real bytes on disk |
| IAM | Roles, users, managed and inline policies, attachments, instance profiles — recorded, never enforced |

```bash
aws --endpoint-url http://localhost:4702 sts get-caller-identity
aws --endpoint-url http://localhost:4702 s3 cp README.md s3://demo/
```

`examples/terraform/aws-basic` applies 14 resources across all four services,
plans clean and destroys. One request is routed to a service by its **SigV4
credential scope**, not by its path: every Query service posts to `/`.

## The visualizer

`http://localhost:4700`, served from inside the binary: no build step, no CDN,
works with the network off.

- **Three views.** *Topology* is the containment tree. *Network* draws VNets as
  containers with their subnets, address ranges, the NSG and route table on
  each subnet, peerings between them, and anything attached to nothing.
  *Kubernetes* draws a cluster as namespaces holding deployments, replica sets
  and pods, with each service joined to the pods its selector matches.
- **Replay an apply.** Events are grouped into runs; the bar under the canvas
  steps through one, resources appearing in the order the provider created
  them, with the API call behind each step. With `--persist`, runs survive a
  restart.
- **See a plan before you apply it.** `nimbus plan-view plan.out` draws
  `terraform plan` on the graph: `+` on what will be created, `~` on what will
  change, `-` on what goes.
- **Export.** SVG or PNG of whichever view is showing, in a light theme, for a
  README or a slide.
- `?fixture` renders a sample stack with no server state, which is how the
  browser and layout checks are done.

Not yet: GCP, AWS compute (EC2 instances, Lambda, EKS), and Azure deployments
at subscription scope or above — see `docs/ROADMAP.md`.

## Repo layout

```
cmd/nimbus/            CLI: start (--clouds), cert, env, az register, plan-view
internal/core/         cloud-agnostic engine: store, event bus, certs, persistence
internal/azure/        ARM emulator: routing, identity/metadata, generic CRUD, per-type hooks
internal/aws/          AWS emulator: SigV4 routing, EC2 Query/XML, S3 REST, IAM
internal/azure/deployments/  ARM template expressions, copy loops, dependency ordering
internal/ui/           visualizer API + SSE, the plan overlay, and the embedded web app (static/, ES modules, one type table per cloud)
tools/specgen/         generates the spec-derived type table (separate Go module; the only thing that fetches anything)
examples/terraform/    configs to apply against Nimbus
examples/bicep/        an ARM/Bicep template to deploy with az
test/sdkcompat/        drives Nimbus with hashicorp/go-azure-sdk (separate Go module)
test/ui/               the visualizer's own unit tests, run by node (no npm, no package.json)
docs/                  roadmap, offline setup
```

## Tests

```bash
go test ./...                              # unit + HTTP tests, no network
node --test 'test/ui/*.test.js'            # the visualizer's pure modules
cd test/sdkcompat && go run . -endpoint https://localhost:4701   # real Azure SDK against a running Nimbus
```
