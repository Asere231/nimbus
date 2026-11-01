# Roadmap

**Current: Milestone 7 — AWS core. Prompt: `docs/prompts/` (not written yet).**

Milestones 1 to 6 are done, on macOS 27 (arm64) with azurerm 4.81.0,
az 2.90.0 and Terraform 1.16.4.

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
- `examples/terraform/azure-app` ties them together: an image pushed to an
  emulated ACR, pulled by the cluster through a containerd mirror, running a
  pod that reads a secret from the emulated Key Vault data plane. Apply, clean
  plan, destroy, and a re-apply of the purged vault name all verified.
- `nimbus az register` makes the `az` CLI talk to Nimbus: `az login`,
  `az account list`, and `az keyvault secret show`/`set` against the vault
  Terraform created, all verified on macOS with az 2.90.0.
- 53 resource types are modelled from Microsoft's own OpenAPI specs rather
  than hand-written hooks: `tools/specgen` emits the table, `put()` applies it,
  and a test matrix round-trips every one of them through PUT, GET, list and
  DELETE. The visualizer counts what is still accepted-only.
- ARM template deployments work at resource-group scope, including the Bicep
  expression subset, resource and property copy loops, dependency ordering,
  outputs, `validate` and `what-if`. `az deployment group create
  --template-file main.bicep` was run on macOS with az 2.90.0 and Bicep
  0.48.1: it deploys, the outputs resolve, and a second what-if reports
  NoChange for every resource.
- The visualizer has three views (topology, network, Kubernetes), replays a
  `terraform apply` step by step from a persisted event journal, overlays a
  `terraform plan` before it is applied, and exports any view as SVG or PNG.
  It is ES modules with no build step, and its pure parts have unit tests run
  by `node --test test/ui/`.
- CI is green on ubuntu, macos and windows, plus the SDK harness, the
  generator's tests, the visualizer's node tests, the Terraform
  apply/plan/destroy jobs and an az + Bicep deployment job.

Each milestone has a session prompt in `docs/prompts/` (see its README).

Each milestone ends with a Terraform config that applies and destroys cleanly against Nimbus, and the visualizer showing it.

## Phase 1 — Azure

| # | Milestone | Done when |
|---|---|---|
| 1 | ~~**ARM core + visualizer** (scaffold)~~ **done** | `azure-basic` applies and destroys on Mac; graph and feed update live |
| 2 | ~~**Storage data plane** (Blob, Queue, Table)~~ **done** | `azurerm_storage_account` + container + blob apply; `az storage blob upload` works; files visible in the inspector |
| 3 | ~~**AKS on k3s** (Docker)~~ **done** | `azurerm_kubernetes_cluster` creates a k3s container; `kube_config` output works with `kubectl`; pods and services appear under the cluster in the graph |
| 4 | ~~**Identity and secrets**: Key Vault secrets, managed identities, ACR on `registry:2`, Log Analytics stub~~ **done** | An app on AKS pulls an image from the emulated ACR and reads a Key Vault secret |
| 5 | ~~**Breadth**: spec-driven defaults from `azure-rest-api-specs`, ARM/Bicep deployments, `az` CLI as a custom cloud~~ **done** | `az group create` and a Bicep deployment work; 50+ resource types round-trip in Terraform |
| 6 | ~~**Visualizer v2**: apply timeline, plan/diff view, Kubernetes view, export diagram as SVG/PNG~~ **done** | A `terraform apply` can be replayed step by step in the UI |

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

- **`az bicep install` needs a shell without `REQUESTS_CA_BUNDLE`** set, since
  that variable replaces the trust store rather than adding to it. Verified on
  macOS with Bicep 0.48.1; `main.json` is now the compiler's own output.
- **No Key Vault SDK harness.** `test/storage-sdk` drives blobs, queues and
  tables with Microsoft's Python SDKs; the vault has no equivalent, so the
  challenge flow is only proven against azurerm and the az CLI.

- **The Docker-stopped path is only unit-tested.** Stopping Docker Desktop
  mid-session would have disrupted the machine, so the ARM error and feed note
  are covered by a test against the Fake rather than by pulling the plug.

- **Windows has only been DNS-tested.** No Windows machine has run Terraform,
  `az` or `kubectl` against Nimbus; CI's Windows leg runs the Go tests only.
  The certificate path matters there too: Windows has its own rules, and
  `nimbus cert` uses `Import-Certificate` into `CurrentUser\Root`.

## Later

- **Runs are inferred from gaps, not announced.** `core.RunGap` is 30 s,
  measured against azurerm's silent ~20 s resource-group wait. Two things
  done within half a minute of each other merge into one run, and a pause
  longer than 30 s inside one apply would split it. A `nimbus run --
  terraform apply` wrapper that marked the boundaries explicitly would remove
  the guess; the heuristic needs no new CLI surface, so it went first.
- **The activity feed still starts empty after a restart**, because the SSE
  stream replays the bus's in-memory ring and the journal is deliberately not
  streamed. The timeline is how history is reached now; seeding the feed from
  the journal on connect is a small change if it ever grates.
- **The plan overlay's azurerm → ARM type table is partial** (`armTypes` in
  `internal/ui/plan.go`). An unmapped type still draws, labelled with its
  Terraform address; add a line when an example needs one.
- **Plan overlays are topology-only.** The network and Kubernetes views draw
  the live graph and ignore a loaded plan.
- **The Kubernetes view has only been checked against the fixture and one
  real cluster shape.** Namespaces other than `default` are still filtered
  out by the watcher, so a workload deployed elsewhere appears in neither
  view.
- **`REQUESTS_CA_BUNDLE` replaces the trust store rather than adding to it**,
  so while it points at the Nimbus CA, `az` cannot reach any real host — which
  breaks `az bicep install`, `az upgrade` and anything else that downloads.
  `nimbus az register` could write a combined bundle (the CAs az already
  trusts, plus ours) and point at that instead. Until then the workaround is a
  shell without the variable set.
- **Deployments are resource-group scope only.** `az deployment sub create`,
  management-group and tenant scope, nested deployments, `templateLink` (which
  would fetch over the network) and Complete mode are all unimplemented.
- **Two spec types have nothing to fill**: `Microsoft.Compute/sshPublicKeys`
  and `Microsoft.Sql/servers/firewallRules` genuinely declare no read-only
  field and no default, so the table skips them.

- **Only secrets are emulated in Key Vault.** Keys and certificates answer 501
  NotSupported, except the certificate CONTACTS list, which exists because
  azurerm reads it on every vault read.
- **The registry has no authentication.** `registry:2` runs open, and
  `listCredentials` returns decorative values; an imagePullSecret built from
  them works because nothing checks it.
- **Role assignments are never enforced.** See CLAUDE.md: they round-trip and
  are drawn, but no request is refused.

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
