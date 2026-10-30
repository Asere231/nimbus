# Roadmap

**Current: Milestone 1 (finish) — prompt: `docs/prompts/m01-finish-azure-core.md`.** Prove `examples/terraform/azure-basic` applies, plans clean and destroys on macOS and Windows; add CI.

Each milestone has a session prompt in `docs/prompts/` (see its README).

Each milestone ends with a Terraform config that applies and destroys cleanly against Nimbus, and the visualizer showing it.

## Phase 1 — Azure

| # | Milestone | Done when |
|---|---|---|
| 1 | **ARM core + visualizer** (scaffold) | `azure-basic` applies and destroys on Mac and Windows; graph and feed update live |
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

## Later

Follow-ups noted during milestones go here.
