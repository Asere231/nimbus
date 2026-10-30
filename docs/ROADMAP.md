# Roadmap

**Current: Milestone 2 — Storage data plane. Prompt: `docs/prompts/m02-storage.md`.**

Milestone 1 is done. `examples/terraform/azure-basic` applies, plans clean
(`-detailed-exitcode` = 0) and destroys against Nimbus with azurerm 4.81.0 on
macOS 27 (arm64), survives a `--persist` restart between apply and destroy, and
every resource in it imports into a fresh state with a clean plan. CI is
written but unproven until this is pushed to GitHub — see "Left to confirm".

Each milestone has a session prompt in `docs/prompts/` (see its README).

Each milestone ends with a Terraform config that applies and destroys cleanly against Nimbus, and the visualizer showing it.

## Phase 1 — Azure

| # | Milestone | Done when |
|---|---|---|
| 1 | ~~**ARM core + visualizer** (scaffold)~~ **done** | `azure-basic` applies and destroys on Mac; graph and feed update live |
| 2 | **Storage data plane** (Blob, Queue, Table, Azurite-compatible) | `azurerm_storage_account` + container + blob apply; `az storage blob upload` works; files visible in the inspector |
| 3 | **AKS on k3s** (Docker) | `azurerm_kubernetes_cluster` creates a k3s container; `kube_config` output works with `kubectl`; pods and services appear under the cluster in the graph |
| 4 | **Identity and secrets**: Key Vault secrets, managed identities, ACR on `registry:2`, Log Analytics stub | An app on AKS pulls an image from the emulated ACR and reads a Key Vault secret |
| 5 | **Breadth**: spec-driven defaults from `azure-rest-api-specs`, ARM/Bicep deployments, `az` CLI as a custom cloud | `az group create` and a Bicep deployment work; 50+ resource types round-trip in Terraform |
| 6 | **Visualizer v2**: apply timeline, plan/diff view, Kubernetes view, export diagram as SVG/PNG | A `terraform apply` can be replayed step by step in the UI |

## Phase 2 — AWS (port 4702)

| # | Milestone | Done when |
|---|---|---|
| 7 | Core: SigV4-tolerant auth, STS, IAM, S3, VPC/EC2 networking | The `aws` provider creates a VPC, subnets and an S3 bucket via `endpoints {}` overrides |
| 8 | EKS on k3s, Lambda in Docker, DynamoDB on a local store | An EKS cluster is reachable with `kubectl` |

## Phase 3 — GCP (port 4703)

| # | Milestone | Done when |
|---|---|---|
| 9 | Core: OAuth, Resource Manager projects, Compute networks, GCS | The `google` provider creates a VPC and a bucket via `*_custom_endpoint` settings |
| 10 | GKE on k3s, Pub/Sub, Cloud Run in Docker | A GKE cluster is reachable with `kubectl` |

## Design decisions still open

- **Data-plane DNS (M2):** clients address storage as `{account}.blob.{suffix}`. Options: hosts-file entries written by `nimbus dns`, a tiny local DNS server, or path-style endpoints. Must work on both macOS and Windows without admin where possible.
- **Engine lifecycle (M3):** whether k3s containers survive `nimbus stop` (faster restarts) or are always torn down (cleaner).

## Left to confirm

- **CI has never run.** The repository has no remote, so `.github/workflows/ci.yml`
  is unproven. The YAML parses, every shell step was reproduced locally on
  macOS, and all test packages cross-compile for linux and windows, but the
  Linux and Windows legs need a real run. Push to GitHub and check.
- **Windows has not been tested at all this session** — no Windows machine was
  available. The cert fix matters there too: Windows has its own certificate
  rules, and `nimbus cert` uses `Import-Certificate` into `CurrentUser\Root`.

## Later

- **Cascade delete does not run child hooks.** `handleResource` calls
  `AfterDelete` only for the type being deleted; `store.Delete` removes
  descendants without firing theirs. Harmless while every hook is pure state,
  but it must be fixed before M3, or deleting a resource group will leave an
  AKS k3s container running. Probably: have the store return what it deleted
  and let the ARM layer run each type's hook, leaves first.
- **Certificates expire yearly now** (397 days, forced by Apple's limit).
  `EnsureCerts` reissues the leaf automatically within 30 days of expiry and
  keeps the trusted CA, so no re-trust is needed — but this has only been
  tested by deleting the leaf, never by actually waiting a year.
- The visualizer scales a wide graph down to 70% and then scrolls
  horizontally. Correct and reachable, but on a narrow window a deep tree looks
  truncated at first glance. Worth revisiting in M6 (Visualizer v2).
