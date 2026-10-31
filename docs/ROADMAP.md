# Roadmap

**Current: Milestone 4 — identity, secrets and ACR. Prompt: `docs/prompts/m04-identity-secrets-acr.md`.**

Milestones 1, 2 and 3 are done, on macOS 27 (arm64) with azurerm 4.81.0.

- `examples/terraform/azure-basic` applies, plans clean (`-detailed-exitcode`
  = 0) and destroys; survives a `--persist` restart; every resource imports
  into a fresh state with a clean plan.
- `examples/terraform/azure-storage` does the same for a storage account,
  container and blob, with real bytes behind it. `az storage blob
  upload/download/list` works, and `test/storage-sdk` drives blob, queue and
  table with Microsoft's Python SDKs. Storage accounts and containers are the
  first `Backing: "real"` resources.
- `examples/terraform/azure-aks` starts a real Kubernetes cluster: k3s in
  Docker, `kubectl get nodes` showing the nodes, `node_count` scaling by
  adding agent containers, and pods and services appearing live in the
  visualizer. Apply, clean plan, scale, clean plan, destroy all verified.
- CI is green on ubuntu, macos and windows, plus the SDK harness and a
  Terraform apply/plan/destroy job.

Each milestone has a session prompt in `docs/prompts/` (see its README).

Each milestone ends with a Terraform config that applies and destroys cleanly against Nimbus, and the visualizer showing it.

## Phase 1 — Azure

| # | Milestone | Done when |
|---|---|---|
| 1 | ~~**ARM core + visualizer** (scaffold)~~ **done** | `azure-basic` applies and destroys on Mac; graph and feed update live |
| 2 | ~~**Storage data plane** (Blob, Queue, Table)~~ **done** | `azurerm_storage_account` + container + blob apply; `az storage blob upload` works; files visible in the inspector |
| 3 | ~~**AKS on k3s** (Docker)~~ **done** | `azurerm_kubernetes_cluster` creates a k3s container; `kube_config` output works with `kubectl`; pods and services appear under the cluster in the graph |
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

- **CI does not run the AKS example.** It needs Docker, so it cannot join the
  default matrix unconditionally. The unit tests cover the whole hook through
  the Fake, and `NIMBUS_DOCKER_TESTS=1` covers the real engine, but no CI job
  provisions a cluster. A Linux job could: GitHub runners have Docker.
- **The Docker-stopped path is only unit-tested.** Stopping Docker Desktop
  mid-session would have disrupted the machine, so the ARM error and feed note
  are covered by a test against the Fake rather than by pulling the plug.

- **CI has never run.** The repository has no remote, so `.github/workflows/ci.yml`
  is unproven. The YAML parses, every shell step was reproduced locally on
  macOS, and all test packages cross-compile for linux and windows, but the
  Linux and Windows legs need a real run. Push to GitHub and check.
- **Windows has not been tested at all this session** — no Windows machine was
  available. The cert fix matters there too: Windows has its own certificate
  rules, and `nimbus cert` uses `Import-Certificate` into `CurrentUser\Root`.

## Later

- **A mid-apply restart fails the apply.** Terraform polls while the port is
  closed and azurerm reports `unimplemented polling status "Unknown"`. Nothing
  Nimbus returns can change that — the request never arrives. Surviving it
  would mean holding the listener open across a restart, which is a much
  bigger change than it is worth today.
- **Cluster watching polls every 3s per cluster.** Fine for a few clusters
  someone is looking at; the Kubernetes watch API would scale better but needs
  resourceVersion bookkeeping and re-sync handling.
- **Only the default namespace is mirrored** into the graph; kube-system and
  friends are hidden, so a workload deployed elsewhere will not appear.

- **Certificates expire yearly now** (397 days, forced by Apple's limit).
  `EnsureCerts` reissues the leaf automatically within 30 days of expiry and
  keeps the trusted CA, so no re-trust is needed — but this has only been
  tested by deleting the leaf, never by actually waiting a year.
- Only **block blobs** are emulated. Page and append blobs return
  `UnsupportedBlobType`. Nothing needs them yet; disks and log appends would.
- **SAS tokens are accepted on shape alone**, not verified — there is no
  signature check and no expiry enforcement. Fine for a local emulator, but it
  means a SAS cannot be used to *test* that permissions are wired correctly.
- Table `$filter` supports `eq` joined by `and`. Anything else matches
  everything rather than excluding on a guess.
- The **visualizer re-renders on `requestAnimationFrame`**, which browsers
  pause in a hidden tab, so a backgrounded visualizer can show a stale layout
  until it is brought forward. Harmless, but confusing once noticed.
