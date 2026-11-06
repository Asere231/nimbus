# Changelog

## 0.7.0 — 2026-11-06

Milestone 7: AWS core. A second cloud on the same engine, which is also the
first real test of whether six milestones of "cloud-agnostic" meant anything.

- **AWS on port 4702**, plain HTTP. `nimbus start --clouds azure,aws` runs
  both clouds in one process, sharing the store, the event journal and the
  visualizer.
- **Routed by signature, not by path.** Every Query service posts to exactly
  `/`; STS, EC2 and IAM differ only in the fourth field of the SigV4
  credential scope. Signatures are never verified — a local emulator
  rejecting one could only ever be reporting a clock skew.
- **STS**: `GetCallerIdentity`, `AssumeRole`.
- **EC2 networking**: VPCs, subnets, internet and NAT gateways, route tables
  and routes, security groups and rules, tags, availability zones, account
  attributes, with the describe filters the provider actually sends.
- **S3**, path-style: buckets with the whole sub-configuration matrix, and
  objects with real bytes under `{DataDir}/s3`.
- **IAM**: roles, users, managed and inline policies, attachments, instance
  profiles. Recorded, never enforced, exactly as Azure's RBAC is.
- **`examples/terraform/aws-basic`** applies 14 resources across all four
  services, plans clean and destroys, and CI runs it beside the Azure
  examples against one server.
- **The visualizer is multi-cloud.** Badges, sibling order, the root
  container, the hidden types and any non-containment links all come from
  the cloud's own table; tabs are built from what `--clouds` started; views
  declare which clouds they mean anything for. A test fails the build if a
  shared module names a cloud's types.
- **`core.Store.Update`** fixes a lost update that a real apply could hit:
  two read-modify-writes on one resource used to lose one of the changes.
  It was filed as a flaky AKS test and was not one.

## 0.6.0 — 2026-10-31

Milestone 6: visualizer v2. The visualizer stops being only a live picture
and becomes something you can replay, predict with, and publish from.

- **Replay an apply.** Events are kept in a journal (`events.jsonl`, with
  `--persist`) and grouped into runs; the bar under the canvas steps through
  one, resources appearing in the order the provider created them, with the
  API call behind each step. Replay needs no snapshots: an event already
  names the resource completely, which is what lets a resource that has since
  been destroyed still be drawn.
- **Network view.** VNets as containers with their subnets and address
  ranges, the NSG and route table attached to each subnet, peerings as arcs,
  and a column of network resources attached to nothing — which the tree
  cannot show at all.
- **Kubernetes view.** Namespaces holding deployments, replica sets and pods,
  nested by ownerReferences rather than by name, with each service joined to
  the pods its selector matches, pod phase and restarts. The watcher now
  mirrors replica sets, service selectors and restart counts.
- **Plan view.** `nimbus plan-view plan.out` draws a `terraform plan` on the
  graph before it is applied: `+`, `~`, `-` and `-/+` in Terraform's own
  notation, with a ghost card for each resource that does not exist yet.
- **Export.** Any view as a standalone SVG or a 2× PNG, light-themed.
- **ES modules.** `app.js` is now eleven modules with no build step and no
  CDN, and the pure ones have unit tests run by `node --test 'test/ui/*.test.js'` — the
  visualizer's first. A Go test walks the import graph so a typo in a module
  path fails the build rather than the browser.
- **`?fixture`** renders a sample stack with no server state, which is how the
  browser, theme and phone-width checks are done.
- Phone width: the header wraps instead of pushing the resource counts and
  Reset off the right-hand edge.

Found by running the whole thing against a real `azure-app` apply, and
fixed before the milestone closed:

- A replay opened on its **last** step, which is pixel-identical to the live
  graph, so picking a run appeared to do nothing. It starts at the first step
  and plays.
- A replay began from an **empty world**, so it ended holding only what that
  run had touched — the subscription was missing at 24/24 although it was
  there live. Each run now carries what existed when it began. That needed a
  snapshot written to the journal at startup, because the journal only ever
  hears about changes and a restart is not one.
- An empty view drew a blank canvas. `examples/terraform/azure-app` has no
  virtual networks at all, so the network view was correct and looked broken;
  each view now says what it would draw.
- `node --test` is given a glob, not a directory: node 24 tries to load a
  directory argument as a module, so CI failed where the same command passed
  locally on node 26.

## 0.5.0 — 2026-10-31

Milestone 5: breadth. Most Azure types now have the right shape without
anyone hand-writing a hook, and ARM/Bicep templates deploy.

- **Spec-driven types.** `tools/specgen` (its own module; the main module
  stays stdlib-only) reads Microsoft's published OpenAPI specs and emits the
  table of fields the server owns — 53 types, 444 fields, from the AZ-104/305
  scope. `put()` applies it to any type with no hook, so a NAT gateway comes
  back with `etag`, `resourceGuid`, `provisioningState` and an empty `subnets`
  array that nobody wrote code for. The specs are fetched and cached; only the
  generated table is committed.
- **Three rules the table obeys**, each tested: never overwrite what the
  client sent, never invent a nested object the client did not ask for, and
  derive every generated value from the resource ID so two identical writes
  read back identically.
- **Template deployments.** `Microsoft.Resources/deployments` expands a
  template and creates each resource by PUTting it back through the same
  server, so hooks run and the visualizer draws each one appearing. Includes
  the expression language Bicep emits, resource *and* property copy loops,
  dependency ordering from `dependsOn` and from any `resourceId()` in a body,
  outputs with `reference()`, `validate`, `what-if` and deployment operations.
- **`examples/bicep`** deploys a VNet, its subnets, an NSG and a storage
  account through the real `az` CLI, and redeploying it changes nothing.
- **A third tier in the visualizer.** Resources are now "accepted (state
  only)", "modelled from the API spec" or "backed by a local engine", and the
  header counts how many are still accepted-only.
- Etags are derived from the resource ID rather than regenerated per write,
  which is what made a redeploy a true no-op.
- Three things running the real `az` CLI found, each fixed with a test: it
  sends a deployment body that is not JSON (the template is spliced in under
  an unquoted key); parameter defaults are expressions and have to be
  evaluated; and a freshly started Nimbus listed no subscription, so `az
  login` fell back to using the tenant id as the subscription id.

## 0.4.1 — 2026-10-31

Fixes found while proving `az keyvault` against Nimbus on macOS, which is now
the last of Milestone 4's done-criteria to be verified with a real client.

- `nimbus az register` trusts the CA before it calls `az`, not after. The very
  first command, `az cloud register`, fetches `/metadata/endpoints` over TLS,
  so the old order failed with an SSL error blaming the network. `--run` now
  sets `REQUESTS_CA_BUNDLE` on each child process, so it works whatever the
  shell has exported.
- It also turns off MSAL's instance discovery
  (`core.instance_discovery=false`), which otherwise validates our authority
  by calling the real `login.microsoftonline.com` — reaching the internet and
  then failing, since the CA bundle no longer trusts public certificates.
- Re-registering an existing cloud falls back to `az cloud update` instead of
  exiting, so running the command again after a port change works.

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
