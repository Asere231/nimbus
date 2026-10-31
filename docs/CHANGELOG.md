# Changelog

## 0.4.0 — 2026-10-31

Milestone 4: identity, secrets and a container registry. An app on the
emulated cluster now pulls its image from an emulated ACR and reads its
password out of an emulated Key Vault.

- **Key Vault**, both planes. The vault resource carries a `vaultUri` on
  `{vault}.vault.nimbus.localhost:<port>`, and that host serves secrets for
  real: set (every set makes a version), get by version, list, versions,
  attribute updates, delete, and the recycle bin — recover and purge. The
  vault itself soft-deletes too, so azurerm's destroy-and-purge and its
  recover-on-collision both work.
- **Managed identities and RBAC.** User-assigned identities with stable IDs,
  any resource's `identity` block completed generically, role assignments
  stored as extension resources at any scope, and Azure's built-in role
  definitions served with their real GUIDs so `role_definition_name` lookups
  resolve. Nothing is enforced: Nimbus records permissions, it does not check
  them.
- **ACR** backed by a `registry:2` container per registry, with
  `loginServer = localhost:<port>` so `docker push` works with no daemon
  configuration at all. Each k3s node gets a containerd mirror pointing that
  host at the registry container, written to `certs.d`, which k3s honours
  without a restart — so a registry created after a cluster is still pullable.
- **Log Analytics** as an honest stub: stable `customerId` and shared keys,
  nothing that ingests a log.
- **`examples/terraform/azure-app`** applies, plans clean and destroys: a
  registry, a vault with a secret, an identity, two role assignments, a
  cluster, and a `kubernetes_deployment` whose pod pulls the pushed image and
  reads the secret from its environment.
- **`nimbus az register`** teaches the `az` CLI about Nimbus as a custom
  cloud, which is what `az keyvault` needs — az never reads our metadata
  document.
- **Visualizer**: a vault's secrets are listed by name with a per-secret
  reveal button, and role assignments are drawn as dashed links from the
  identity that owns the principal to the scope they cover.

Five things the real provider found, each now a regression test: the Key
Vault challenge's authority must carry a tenant path or go-autorest panics
on it; an ARM read of a vault triggers a data-plane read of certificate
contacts, where 501 crashes the provider and 404 is correct; identity
deletes must be synchronous; a vault URI is mapped back to a resource ID
through `/subscriptions/{id}/resources`; and purging a vault is
long-running. Plus one of our own: a registry container outliving Nimbus
held the port its login server names.

## 0.3.0 — 2026-10-30

Milestone 3: AKS. `azurerm_kubernetes_cluster` starts a real Kubernetes
cluster — k3s in Docker — and `kubectl` talks to its actual API.

- **`internal/engines/docker`**: the `docker` CLI driven through
  `exec.Command`, with a `Fake` so the whole AKS hook is testable without a
  daemon. Daemon-down is recognised across runtimes and never mistaken for an
  operational failure; every call is timeout-bounded, because a hang tells you
  nothing while an error names Docker.
- **Real async operations.** The operation endpoint tracks per-operation
  state instead of always answering `Succeeded`, and `Hook` gained
  `Provision`/`Deprovision`, which run in the background while the write is
  answered immediately with `provisioningState: Creating`.
- **`examples/terraform/azure-aks`** applies, plans clean, scales
  `node_count` by adding agent containers, plans clean again, and destroys
  every container.
- **Credentials** are built from k3s's PEM files read out of the container,
  not from `k3s.yaml` — generating a kubeconfig is string formatting, parsing
  YAML is the one thing the stdlib cannot do. The same certificates give
  Nimbus an HTTPS client for the cluster API, so no `client-go` either.
- **The visualizer shows what is inside the cluster**: nodes, deployments,
  pods and services polled every few seconds and mirrored into the resource
  store, so the browser needs no Kubernetes knowledge.
- With `--persist`, clusters survive a restart and Nimbus reattaches to them,
  rebuilding the identical kubeconfig from the same container's certificates.

Bugs found by running the real provider: `kubeletidentity.resourceId` must be
a full ARM ID, not a name; and azurerm scales a node pool by writing the
`agentPools` child and reading the count back off the parent, so the two must
be mirrored.

## 0.2.0 — 2026-10-30

Milestone 2: the storage data plane. Storage accounts and containers are the
first resources Nimbus genuinely backs rather than just remembering.

- **Blob, Queue and Table services**, stdlib-only, speaking the XML and OData
  wire formats the real clients expect. Containers, blobs (including Put Block
  / Put Block List for large uploads, byte ranges and metadata), queues with
  real visibility-timeout semantics, and tables with an OData `eq`/`and`
  filter subset.
- **Shared Key authentication is verified**, not waved through: a wrong key
  fails exactly as it would against Azure. Test vectors are signed by
  Microsoft's own SDKs rather than a second reading of the documentation.
- **`examples/terraform/azure-storage`** applies, plans clean and destroys.
- **`az storage blob upload/download/list`** works against a connection string
  from `nimbus env azure --storage-account NAME`.
- **`test/storage-sdk`** drives all three services with the Azure SDKs for
  Python, including a 2 MB block upload compared byte for byte.
- **Data survives `--persist`**: each account gets a directory under
  `~/.nimbus/data/storage`, with blobs as plain files you can `cat`.
- **Visualizer**: containers appear under their account, the inspector lists
  blobs with sizes and download links, and the four ARM `*/Services/default`
  plumbing resources are hidden from the graph.

Addressing, which was the milestone's real design problem: azurerm builds
data-plane URLs from the metadata suffix, not from `primaryEndpoints`, so a
bare hostname would send it to port 443 — which needs root. The suffix now
carries our port. Nested `*.localhost` names resolve to loopback on both macOS
and Windows with nothing installed, so no DNS tooling was needed.

Bugs found by running real clients, each with a regression test: the Table
service signs differently from blob and queue; storage accounts need four ARM
child resources or azurerm polls forever; containers created through ARM must
exist in the data plane; `Content-MD5` is a content setting and computing one
causes a perpetual blob replacement; metadata names must come back lower-cased
and un-canonicalised.

## 0.1.1 — 2026-10-30

First run against the real Terraform azurerm provider (4.81.0), on macOS 27
arm64. `examples/terraform/azure-basic` applies, plans clean and destroys;
state survives a `--persist` restart between apply and destroy; every resource
imports into a fresh state with a clean plan.

- **Fixed:** macOS rejected the server certificate Nimbus generates
  (`x509: "localhost" certificate is not standards compliant`). Apple caps the
  validity *span*, and backdating `NotBefore` put ours one hour over. Now 397
  days, computed from `NotBefore` as a UTC duration.
- **Fixed:** `EnsureCerts` reissued the CA whenever it reissued the server
  certificate, which would have forced a keychain re-trust. It now reuses an
  existing CA and reissues only the leaf, including when the leaf is near
  expiry or has a span macOS would reject.
- **Fixed:** a resource's `id` and `name` were taken from the request path on
  every write, so a client addressing an existing resource with different
  casing renamed it — a perpetual Terraform diff waiting to happen.
- Added tests for PATCH on a missing resource, malformed PUT bodies, cascade
  delete ordering, case-insensitive IDs across every verb, and the visualizer
  API, which had none.
- Added CI: `go vet` / `go test` / build on Linux, macOS and Windows, plus
  Linux jobs for the SDK harness and a real Terraform apply → plan → destroy.
- Investigated and did not change: azurerm's 20 s resource-group create and
  10 s-per-delete are client-side constants that nothing we return affects.

## 0.1.0 — 2026-10-30

- Azure Resource Manager emulator: metadata, client-secret tokens, OpenID discovery, generic CRUD for any resource type, resource groups, virtual networks and subnets with VNet/subnet sync, fast async-operation polling.
- Core store with cascade delete, optional persistence, event bus.
- Visualizer: live topology graph, activity feed, inspector; embedded, offline.
- Verified with `hashicorp/go-azure-sdk` via `test/sdkcompat`.
