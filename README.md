# Nimbus

A from-scratch, offline, multi-cloud emulator with a live visualizer. Azure, AWS and GCP run side by side on the same engine.

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
| Real | Storage accounts and containers, AKS clusters and node pools, Key Vaults, container registries, Linux virtual machines, **Container Apps** | Something actually runs: bytes on disk, k3s in Docker, a `registry:2` container, a container serving on its own FQDN, secrets you can read back with a client |
| Modelled by hand | Resource groups, virtual networks, subnets, managed identities, role assignments and definitions, Log Analytics workspaces | Validation, server-generated fields, parent/child sync, cascade delete. A workspace **holds logs**: a container app's console lands in it and `az monitor log-analytics query` returns the rows |
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
| S3 | Buckets path-style with their whole sub-configuration matrix (versioning, tagging, policy, encryption, public access block, ownership, …) and objects with real bytes on disk. Large files work both ways: multipart upload above the CLI's 8 MB threshold, with the `-N` part-count ETag, and ranged GETs so a parallel download reassembles correctly |
| IAM | Roles, users, managed and inline policies, attachments, instance profiles — and **enforced**, with `--enforce-iam`: a request the caller's policies do not allow is refused with a legible `AccessDenied`. Identity-based policies, wildcards, explicit deny, a stated subset of conditions |
| EKS | Clusters and node groups as **real Kubernetes** (k3s in Docker). `aws eks update-kubeconfig` then `kubectl get nodes` works, through the same token authentication webhook real EKS uses |
| EC2 instances | Machines that **really boot**: a container per instance, running your user data, keeping its filesystem across a stop and start, and serving its own metadata. Key pairs, instance types, lifecycle states and the attribute read-backs the provider makes |
| Lambda | Functions that **really run**, in AWS's own base images through the Runtime Interface Emulator they ship. Zip or S3 code, environment, timeout, `RequestResponse` and `Event` invocations, tags and resource policies |
| DynamoDB | Tables and items with the expression language the SDKs send: `Query`, `Scan`, `UpdateItem` with SET/REMOVE/ADD/DELETE, conditions, batches. Attribute values keep their wire shape, so a 38-digit number survives |
| CloudWatch Logs | Log groups and streams, `PutLogEvents`, `GetLogEvents`, `FilterLogEvents`. A function's output lands in `/aws/lambda/<name>` |

```bash
# The CLI needs its own region and credentials; any values will do.
export AWS_ACCESS_KEY_ID=nimbus AWS_SECRET_ACCESS_KEY=nimbus AWS_DEFAULT_REGION=us-east-1

aws --endpoint-url http://localhost:4702 sts get-caller-identity
aws --endpoint-url http://localhost:4702 s3 cp README.md s3://demo/
aws --endpoint-url http://localhost:4702 lambda invoke --function-name writer /dev/stdout
```

Four examples: `aws-basic` applies 14 resources across STS, EC2, S3 and IAM;
`aws-serverless` deploys a Python handler that writes to DynamoDB, with
nothing but `AWS_ENDPOINT_URL` telling it where Nimbus is; `aws-eks` brings up
a cluster `kubectl` can reach; and `aws-iam-enforced` proves a permission is
genuinely refused and then granted. All plan clean and destroy.

### A container that really runs, on Azure

```bash
cd examples/terraform/azure-containerapp && terraform apply
curl -k "https://$(terraform output -raw fqdn)/"
```

An `azurerm_container_app` starts a real container from any image the
Docker daemon can pull, on the same engine behind GCP's Cloud Run. The
FQDN in the output is Nimbus, which proxies through — so the request shows
up in the activity feed, where a request straight to a container port
would be invisible.

It has **one label** of app name before a shared suffix, where real Azure
also puts the environment and the region in: a TLS wildcard matches
exactly one label, so an `{app}.{env}.` name would need a certificate per
environment. Two environments therefore cannot hold apps of the same name
here, which real Azure allows.

Whatever the container prints lands in the Log Analytics workspace its
environment names, and can be read back:

```bash
az monitor log-analytics query \
  --workspace "$(terraform output -raw workspace_customer_id)" \
  --analytics-query "ContainerAppConsoleLogs | where Log_s contains 'GET' | take 10"
```

`where`, `project`, `take`, `limit` and `count` work. Anything else is
**refused and named** rather than guessed at — `summarize` answers "not
supported by Nimbus, so this query is refused rather than answered with a
guess". The query command lives in az's `log-analytics` extension, which
needs installing in a shell without `REQUESTS_CA_BUNDLE` set.

### Permissions that are actually checked

All three clouds, one engine. `internal/authz` holds the decision and the
order rules settle in and knows no cloud's policy language; each cloud
compiles its own documents down to it — IAM policies, Azure role
assignments, GCP bindings.

```bash
nimbus start --clouds azure,aws,gcp --enforce-iam
```

Nimbus verifies no signatures, so identity is whatever the client claims —
which is the right trade for the question being asked: *would these
permissions work?*, not *is this caller genuine?* The access key id is the
claim. A session key from `sts:AssumeRole` maps back to its role, a key that
is a role or user **name** maps to that entity, and anything else is the
account root, which — as in real AWS — has no policies and can do anything.
So turning the flag on breaks nothing that was working.

```
Error: uploading S3 Object (reports/today.txt) to Bucket (nimbus-enforced):
api error AccessDenied: role arn:aws:iam::000000000000:role/nimbus-app is not
authorized to perform s3:PutObject: nothing grants s3:PutObject on
arn:aws:s3:::nimbus-enforced/reports/today.txt to role
arn:aws:iam::000000000000:role/nimbus-app
```

Every refusal also appears in the activity feed with the rule that decided
it, which matters more than it sounds: a denied `HeadObject` reaches the
provider as nothing but `Forbidden: Forbidden`, because an HTTP HEAD reply
has no body to carry a reason.

**The same on the other two clouds.** Azure's identity is the bearer
token's `oid`, which the token endpoint derives from the `client_id` a
token was requested for — so a provider sets `client_id` to the principal's
GUID. A role assignment made at a subscription or a resource group governs
everything beneath it, which `examples/terraform/azure-rbac-enforced` is
built to show. GCP's identity is simpler still: there is no OAuth and the
`access_token` was never checked, and GCP writes policy members as
`serviceAccount:…` — so the token can simply *be* the member string.

**And you can ask instead of finding out.** Select a resource in the
visualizer and the inspector says which of that type's actions the
principal may perform and which rule decided. The same question from a
shell:

```bash
nimbus can --principal serviceAccount:app@nimbus-local.iam.gserviceaccount.com \
    storage.objects.create http://localhost:4703/storage/v1/b/nimbus-data
```

```
ALLOWED  storage.objects.create
  principal  serviceAccount:app@nimbus-local.iam.gserviceaccount.com
  rule       project IAM binding #1 (roles/storage.objectAdmin)
```

Both read one endpoint backed by each cloud's own evaluator — the thing
that answers is the thing that decides, so a yes here cannot disagree with
what a request gets. The exit status is the answer, so it works in a
script: 0 allowed, 1 denied, 2 cannot tell.

A request is routed to a service by its **SigV4 credential scope**, not by its
path — every Query service posts to `/` — except DynamoDB and CloudWatch Logs,
which name the action in `X-Amz-Target`, and Lambda and EKS, whose paths do
say what they want.

## GCP

`nimbus start --clouds azure,aws,gcp` adds a Google Cloud endpoint on port
4703. Plain HTTP, like AWS: Cloud Storage is addressed by path, so no bucket
name becomes a hostname and there is nothing to trust.

| API | What works |
|---|---|
| Resource Manager | Projects over v1 and v3, project IAM policy get/set — and **enforced** with `--enforce-iam`: bindings decide, and `testIamPermissions` answers truthfully |
| Compute | VPC networks (auto and custom mode), subnetworks with secondary ranges, firewalls, routes, Cloud Routers, regions and zones, and operations |
| IAM | Service accounts, with a derived email that never drifts between reads |
| Cloud Storage | Buckets and objects with real bytes, all three upload protocols — media, multipart/related and resumable — plus `md5Hash`, `crc32c` and delimiter listings |
| Compute instances | Machines that **really boot**, with the image chosen from the GCE image name — `debian-cloud/debian-12` runs real Debian. Startup scripts from metadata, stop/start/reset, and GCE-shaped metadata including a nested recursive query |
| GKE | Clusters and node pools as **real Kubernetes** (k3s in Docker), scaled by node count. `nimbus gcp kubeconfig CLUSTER` — or `gcloud container clusters get-credentials` after `nimbus gcp install-auth-plugin` — then `kubectl get nodes` |
| Pub/Sub | Topics and subscriptions, publish, pull, acknowledge, ack deadlines with redelivery, and **push delivery** that really POSTs to a local endpoint |
| Cloud Run | Services from any image the Docker daemon can pull, **running for real** with `PORT` set and every request proxied through. Revisions, a stable URL, and IAM recorded |

```bash
curl http://localhost:4703/compute/v1/projects/nimbus-local/global/networks
curl "$(cd examples/terraform/gcp-run-pubsub && terraform output -raw service_url)"
```

Three examples: `gcp-basic` applies seven resources across Compute, IAM and
Cloud Storage; `gcp-gke` brings up a cluster `kubectl` can reach; and
`gcp-run-pubsub` wires a Pub/Sub topic to a Cloud Run service, so publishing a
message invokes a container that really runs. All plan clean and destroy. For
the `gcloud` CLI against the same server, see [docs/GCLOUD.md](docs/GCLOUD.md).

## All three at once

`examples/multicloud` is one `terraform apply` that creates a network and a
managed Kubernetes cluster on **each** cloud, against one Nimbus process:

| | Network | Cluster | Reached with |
|---|---|---|---|
| Azure | VNet + subnet | AKS | `kube_config_raw` from the state |
| AWS | VPC + two subnets | EKS | `aws eks update-kubeconfig` |
| GCP | VPC + subnetwork | GKE | `nimbus gcp kubeconfig` |

Twelve resources, three k3s containers, three tabs in the visualizer, one
store and one activity feed behind all of it. It is the heaviest thing here —
expect minutes and a few gigabytes of containers.

| | Azure | AWS | GCP |
|---|---|---|---|
| Transport | HTTPS (the SDKs insist) | HTTP | HTTP |
| Endpoint config | one `metadata_host` | one per service | one per service |
| Networking | VNets, subnets, NSGs, route tables | VPCs, subnets, gateways, security groups | VPC networks, subnetworks, firewalls, routes |
| Object storage | Blob (+ Queue, Table) | S3 | Cloud Storage |
| Managed Kubernetes | AKS | EKS | GKE |
| Instances | Linux VMs | EC2 | Compute Engine |
| Serverless compute | — | Lambda | Cloud Run |
| Messaging | — | — | Pub/Sub |
| NoSQL | Table storage | DynamoDB | — |
| Secrets | Key Vault | — | — |
| Registry | ACR | — | — |
| Templates | ARM + Bicep | — | — |
| Identity | Managed identities, RBAC | IAM | Service accounts, IAM |
| CLI | `nimbus az register` | `aws --endpoint-url` | `nimbus gcp register` |

Everything in the Identity row is **enforced**, on all three clouds, behind
`--enforce-iam`. A request the caller's policies, role assignments or
bindings do not allow is refused with that cloud's own code and a message
naming the action, the principal and the rule that decided — so a local
apply can show that a deployment's permissions are wrong, which is most of
what the Identity row is for.

Policies attached to the **resource** decide too: an S3 bucket policy, a
Pub/Sub topic's or a Cloud Run service's own bindings, and a GCS bucket's,
which reaches the bucket's objects. A bucket policy granting
`Principal: "*"` really is readable with no credentials at all, and the
graph badges it — unless a public access block takes it back, in which
case the policy still says `*` and the bucket is not public. Azure's
`dataActions` are enforced on Key Vault's data plane, so a Contributor
can delete a vault and cannot read a secret out of it.

What is still only recorded: permission boundaries, SCPs, a role's trust
policy, Azure deny assignments, S3 ACLs, and Azure **storage's** data
plane — Shared Key carries no principal, so there is nothing to evaluate
a blob data action against. And nothing is *authenticated* — the
credential is the claim of identity, so this answers "would these
permissions work", never "is this caller genuine". See
`docs/ROADMAP.md` for the full list.

### What an instance is, and what it is not

An instance on any of the three clouds is a **container**. It boots, runs the
script you gave it — user data, a startup script or custom data, depending on
the cloud — keeps its filesystem across a stop and a start, and answers its
own metadata service. The base image comes from what the configuration asked
for, so `debian-cloud/debian-12` and `Canonical/ubuntu-24_04-lts` really do
give you Debian and Ubuntu, with `apt` in them.

It is not a virtual machine. There is no kernel of its own, no block device,
no systemd, no cloud-init and no guest agent, so anything that needs one will
not work. Two limits are worth naming outright:

- **There is no `sshd`.** Nimbus is stdlib-only and cannot implement an SSH
  server, and no vendor-official base image ships one. `nimbus ssh NAME`
  gives you a shell — it is `docker exec` underneath, and the help text says
  so. Key pairs round-trip and the public key is written to the instance's
  `authorized_keys`, so a configuration is honest, but `ssh -i key.pem` does
  not work.
- **The metadata service is not at `169.254.169.254`.** That address is
  link-local and a container cannot reach it without a route only root can
  add. Each instance gets its own metadata endpoint instead, and is told
  where it is through `AWS_EC2_METADATA_SERVICE_ENDPOINT` or
  `GCE_METADATA_HOST`, which the SDKs read. Code written against the literal
  address still fails, and Azure has no such override at all — a VM there is
  given its details as environment variables.

## The visualizer

`http://localhost:4700`, served from inside the binary: no build step, no CDN,
works with the network off.

- **Three views.** *Topology* is the containment tree. *Network* draws a VNet,
  a VPC or a GCP network as a container holding its subnets with their address
  ranges, the instances running in each one, whatever that cloud attaches to a
  subnet — an NSG and route table on Azure, the associated route table on AWS,
  nothing on GCP, where firewalls attach to the network and match on tags —
  and anything attached to nothing. *Kubernetes* draws a cluster as namespaces
  holding deployments, replica sets and pods, with each service joined to the
  pods its selector matches — for AKS, EKS and GKE alike, since all three are
  the same engine underneath.
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

Not yet: GKE and the rest of GCP's compute, EC2 instances, and Azure
deployments at subscription scope or above — see `docs/ROADMAP.md`.

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
