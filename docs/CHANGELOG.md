# Changelog

## 0.15.0 — 2026-11-11

Milestone 15: finishing Azure. It was the only one of the three clouds
that could not run your code, and the only one with nowhere to keep what
anything printed.

- **Azure Container Apps that really run**, on `internal/engines/run` —
  the same engine behind GCP's Cloud Run, unchanged for its second
  caller. An image pulled, a container started, and `curl` against the
  FQDN in the Terraform output returning what it serves. Requests are
  proxied rather than sent to the container's published port, so they
  appear in the activity feed and the FQDN stays stable across restarts.
- **Log Analytics that holds logs and answers queries.** A workspace was a
  shell whose own comment said queries "would find nothing because nothing
  was ever sent to it". A container app's console now lands in the
  workspace its environment names, and `az monitor log-analytics query`
  returns the rows.
- **A KQL pipeline subset** — `where`, `project`, `take`, `limit`,
  `count` — with everything else refused AND named. Both other answers are
  worse: a no-op returns every row, which is the `FilterExpression` trap,
  and an empty result is indistinguishable from "nothing has been logged
  yet". For the same reason an unknown table is empty while an unknown
  workspace is a 404.
- **The app in the visualizer**, linked to its environment, with its log
  lines in the inspector beside it — and in `fixture.json`, so `?fixture`
  shows all of it with no server.
- `examples/terraform/azure-containerapp`, and a Docker-backed CI job that
  applies it, curls the app, waits for its console to reach the workspace,
  queries it, and checks that an unsupported operator is refused by name.

Measured, and three of these changed the work:

- **azurerm polls the RESOURCE for a container app, not an operation** —
  not one `Azure-AsyncOperation` request in a whole debug log. GKE's
  behaviour, the opposite of Cloud Run's. The first run proved nothing,
  because the image was already local; with a real pull it took 27 seconds
  and the container was answering the instant the apply returned.
- **`az monitor log-analytics query` needed no new CLI plumbing**: the
  extension takes its base URL from the metadata document's
  `logAnalyticsResourceId`, which has pointed at Nimbus since Milestone 5
  and gone unused. The command itself is not in az 2.90.0 — it is the
  `log-analytics` extension, which installs only in a shell without
  `REQUESTS_CA_BUNDLE` set.
- **azurerm calls `listSecrets` right after creating an app**, and
  answering it with the generic `{}` put a red "unimplemented action" line
  in the feed for something that was working.

Two bugs came from tests that already existed: the hook invented
`properties.configuration` when a client sent none, and a container app
cannot reach Succeeded against the Docker `Fake` — deliberately, because
`run`'s readiness probe is a real HTTP GET.

## 0.14.0 — 2026-11-11

Milestone 14: Azure RBAC, GCP IAM, and showing why. "Permissions are
recorded but never enforced" is no longer true of any cloud, and Phase 4
is complete.

- **The decision engine needed no change for any of the three policy
  languages**, which was Milestone 13's open question — it had served
  exactly one. The strongest evidence is not that Azure and GCP fitted but
  how: both are pure grants with no Deny, and each has exactly one role
  needing an exclusion rather than an allow-list (Azure's Contributor,
  GCP's `roles/editor`), so both compile a grant to one rule and the
  deny-wins ordering never fires. Two clouds arriving at the same shape
  independently is what says the interface is right.
- **Azure RBAC**, including the scope inheritance that makes it make
  sense: an assignment at a subscription or resource group governs
  everything beneath it. Action names are derived from the ARM type rather
  than tabulated; `notActions` are subtracted, not denied; and matching is
  segment-aligned, because `strings.HasPrefix` would make an assignment at
  `.../rg` silently govern `.../rg-two`.
- **GCP IAM**: project bindings through the curated role catalogue, with
  permissions derived by walking a path's structure. `testIamPermissions`
  now answers truthfully and needs no permission itself, like
  `sts:GetCallerIdentity`.
- **Identity costs no new state on either cloud, because the credential is
  the claim.** Azure derives the token's `oid` from the `client_id` it was
  asked for; GCP takes the `access_token` as the policy member verbatim.
- **The inspector explains a decision** — which of a resource type's
  actions the principal may perform, and which rule decided — from one
  `/api/authz` endpoint backed by each cloud's own evaluator. **`nimbus
  can ACTION RESOURCE`** asks that same endpoint, so the answer cannot
  drift from the behaviour; one table per cloud drives both the request
  path and the CLI and asserts they never disagree.
- **Two examples**: `azure-rbac-enforced`, two root modules where the
  app's second apply succeeds with no change to its own configuration
  because what changed was the permission; and `gcp-iam-enforced`, one
  module, because the google provider makes no startup calls.

Measured, and three of these reshaped the work:

- **azurerm populates a Resource Provider cache when the provider
  configures**, and `resource_provider_registrations = "none"` does not
  skip it — so a principal with no roles fails before reaching the
  resource under test. **And a provider is configured before any resource
  exists**, even with `count = 0` on its only resource, so an Azure
  principal cannot be granted in one apply the access its own provider
  needs to start. That is why that example is two modules.
- **Every Azure example sets `client_id` to the default**, so the derived
  `oid` is not the default object id — and checking only the latter
  refused all of `azure-basic` the moment the flag was on.
- **A path cannot be read by guessing which segments are plural.** A Cloud
  Run service named `s` came out as `run.s.list`.

Verified with all three clouds enforcing in one process, against
`examples/multicloud`: twelve resources, three k3s clusters, clean plan,
clean destroy, no containers left.

## 0.13.0 — 2026-11-10

Milestone 13: authorization that can say no. The project's most-repeated
honest limitation — "recorded, never enforced" — becomes a feature for
AWS, behind an opt-in `--enforce-iam`.

- **A cloud-neutral decision engine** in `internal/authz`: a principal, an
  action, a resource, a context, and the order rules settle in. Explicit
  deny wins, nothing is allowed unless something says so, and a rule that
  *cannot be evaluated* refuses rather than reading as "no". It knows no
  cloud's policy syntax; each cloud compiles its own documents down to a
  `Rule`, which is what Milestone 14 will do for Azure and GCP.
- **AWS IAM evaluation**: real documents, with `Statement`, `Action` and
  `Resource` each a string or a list, `NotAction`/`NotResource`,
  case-insensitive action matching against case-sensitive ARNs, and a
  stated set of condition operators and keys with everything outside it
  refused by name. Six managed policies ship — only those whose real
  document is short enough to transcribe exactly — and any other managed
  ARN refuses and names itself.
- **Identity with no new state**: the SigV4 access key id. `AssumeRole`
  already derives its key from the role's ARN, so a key maps back by
  deriving each role's and comparing; a key that is a role or user *name*
  maps to that entity. Anything unmapped is the account root, which is
  what real root is — no policies, full access — so turning the flag on
  costs nothing: `aws-basic` applies, plans clean and destroys against an
  enforcing server unchanged. `GetCallerIdentity` now reports whoever the
  request resolves to.
- **Refusals that look right**: 403 with each service's own code and
  envelope, the message naming the action, the principal and the statement
  that decided. Every denial reaches the activity feed.
- **`examples/terraform/aws-iam-enforced`**, proving both directions: an
  apply that fails with a legible `AccessDenied`, and the same apply
  succeeding once the policy grants the action. In CI, along with the
  assertion that the flag changes nothing when off.

Measured against aws provider 5.100.0, and the reason the rest of it was
safe to build:

- A denial fails the operation at once and the whole message survives into
  the terraform error. No retry storm.
- **A denied read fails a plan rather than being mistaken for a missing
  object.** The other answer would have made Terraform plan to recreate
  resources that exist, which would be worse than no enforcement. CI pins
  it on `1 to add` specifically.
- A denied HEAD loses its reason entirely — HTTP gives a HEAD reply no
  body, so the provider reports only `Forbidden: Forbidden`. Real S3 is no
  better, which is what makes the feed load-bearing; `X-Nimbus-Denied`
  carries it for anyone reading the response.
- Revoking a permission Terraform needs to *read* its own resources leaves
  a state no plan can escape, because the apply that would restore it
  refreshes first. `-refresh=false` is the way out. Kept as a documented
  trap, since real AWS does the same.

Two bugs found by tests rather than by use: root was being refused for an
unmapped action (whether an action is mapped is a policy question, and
root has none), and buffering the request body once was not enough — the
classifier reads it to EOF, so it has to be put back after the check as
well as before.

## 0.12.0 — 2026-11-10

Milestone 12: the correctness pass. Defects that made Nimbus quietly
**wrong** rather than merely incomplete — the kind that teach a reader
something false.

Four were planned. One of them did not exist, and fixing another uncovered
something worse.

- **S3 multipart upload.** `aws s3 cp` switches protocol by itself above
  8 MB and never attempts a plain PUT, so a large file failed outright.
  Five operations, all hanging off the object's path and told apart by
  query parameters rather than by verb. A multipart object's ETag is the
  digest of the PART digests with a `-N` suffix, not the MD5 of the bytes.
- **...and the ranged GET that found.** The upload worked and the download
  was still wrong: `aws s3 cp` downloads a large object with concurrent
  ranged GETs, and Nimbus advertised `Accept-Ranges: bytes` while ignoring
  the header — so every request returned the whole object and the client
  assembled overlapping copies. A 20,000,000-byte file came back as
  36,777,216 bytes with nothing erroring. `Range` is honoured now: 206 with
  `Content-Range`, 416 for a range past the end, suffix ranges, clamping,
  and the whole object for a header nothing can parse.
- **An AKS cluster's node resource group dies with it.** The `MC_*` group
  survived every destroy, and the cause was structural: a resource group is
  parented to the subscription, so neither of `Store.Delete`'s cascade
  routes could reach it. An audit of every side-effect `store.Put` found no
  others; a Lambda's log group deliberately outlives its function, as in
  real AWS, and is now pinned by a test saying so.
- **The activity feed survives a restart.** It was seeded from the bus's
  in-memory ring, which a new process starts empty, while the timeline
  beside it replayed the journal — two panels, one store, opposite answers.
  It reads `Bus.RecentEvents` now, which excludes the `snapshot` records a
  restart writes: those say something already existed, not that something
  happened.
- **The namespace defect did not exist.** Both the prompt and the roadmap
  said the k3s watcher mirrored only `default`. It never did — it hides the
  system namespaces and mirrors the rest. Proven on all three clouds, and
  the real gap was that nothing tested it: every UI test and the fixture
  used a single namespace, so a false claim stood for three milestones. The
  tests exist now, and the claim is corrected.

## 0.11.0 — 2026-11-10

Milestone 11: compute instances. All three clouds now boot a machine — the
primitive every cloud course opens with, and the one thing Nimbus could not
do despite running Kubernetes, functions and container services.

- **An instance really boots**, on a new cloud-neutral `internal/engines/vm`:
  a container that runs the script it was given, keeps its filesystem across
  a stop and a start, and serves its own metadata. `aws_instance`,
  `google_compute_instance` and `azurerm_linux_virtual_machine` all reach the
  same engine; the plugins supply the image, the script, the keys and the
  metadata, and the engine knows about no cloud.
- **The base image comes from the configuration**, so it really is the
  distribution you asked for. GCE and Azure name theirs semantically —
  `debian-cloud/debian-12` and `Canonical/ubuntu-24_04-lts` give you Debian
  and Ubuntu with `apt` in them — while an AMI id is opaque, so EC2 takes a
  `nimbus:image` tag instead.
- **`nimbus ssh NAME`** gives a shell on an instance on any cloud, without a
  per-cloud table: it asks the running Nimbus which resources record a
  backing container. It is `docker exec`, and says so — Nimbus is stdlib-only
  and cannot run an SSH server, so `ssh -i key.pem` does not work.
- **A metadata service per instance**, reached through the overrides the SDKs
  read (`AWS_EC2_METADATA_SERVICE_ENDPOINT`, `GCE_METADATA_HOST`), because
  169.254.169.254 needs a route only root can add. An SDK inside an instance
  can ask which instance it is; code written against the literal address
  still cannot, and Azure has no such override at all.
- **Azure VMs appear inside their subnet in the network view**, found through
  their NIC — the only view where that relationship is visible, since ARM
  parents a VM to its resource group.
- **CI runs `go test -race` on Linux.** It found three real data races, all
  the same shape and one of them shipped in 0.10.0: a long-running operation
  finished by a background goroutine while a poll serialises it.

### Also in this release: four red CI jobs, fixed

Three causes, none of them visible on a Mac. The two AWS Docker jobs had
never passed since Milestone 8 and the GKE one failed on its first run — all
of it one missing Docker flag.

- **Every container now gets `--add-host host.docker.internal:host-gateway`.**
  That name is a Docker Desktop convenience; on Docker Engine, which is what
  Linux CI runs, it does not resolve at all. A Lambda's `AWS_ENDPOINT_URL`
  and the k3s token-webhook kubeconfig both use it, so EKS and GKE answered
  `kubectl` with a 401 — the API server was up and could not reach the
  webhook to review the token. Passed unconditionally in `docker.runArgs`,
  because a per-cloud opt-in is what was forgotten in the first place.
- **A zip entry and an S3 key are judged as names, not paths.** `funcs.safeJoin`
  deferred to `filepath.IsAbs`, which is false for `/absolute.py` on Windows
  — so an absolute entry was accepted there. `cleanKey` split only on `/`, so
  `a\..\..\x` reached `filepath.Join`, where Windows reads the backslashes
  as separators and the key leaves the data directory. Both now refuse the
  same name on every platform.
- **The Lambda example and its CI job send `--payload fileb://`.** There is no
  inline payload both aws-cli versions accept: v2 reads one as base64, v1
  rejects the flag that fixes that as an unknown option.
- The executable-bit assertion is skipped on Windows, which cannot express one.
- **An AKS test stopped racing the thing it was testing.** It asked for
  credentials straight after the PUT and expected "not ready", which means
  beating the provisioning goroutine: 200 passes in a row locally, and one
  red macOS build. It now holds the cluster un-ready by making the container
  fail to start, which nothing can race.
- **The EKS and GKE jobs wait for the node COUNT before waiting on Ready.**
  `kubectl wait --all` matches only the nodes that exist when it runs, so it
  was returning satisfied with the control plane alone and never waiting for
  the pool's agent to register.

## 0.10.0 — 2026-11-07

Milestone 10: GCP compute. GKE, Pub/Sub and Cloud Run, which finishes the
third cloud — and one apply that stands up a cluster on all three.

- **GKE is real Kubernetes**, on the same k3s engine behind AKS and EKS.
  Clusters and node pools, a node count that really adds and removes agent
  containers, and a token authentication webhook so `kubectl` can reach
  the API server. `nimbus gcp kubeconfig CLUSTER` writes a kubeconfig;
  `nimbus gcp install-auth-plugin` makes `gcloud container clusters
  get-credentials` work too, by shimming the plugin gcloud insists on.
- **Pub/Sub** topics and subscriptions, publish, pull, acknowledge and ack
  deadlines with redelivery — plus **push delivery**, the first thing
  Nimbus does outbound. It POSTs the real envelope, only to endpoints on
  this machine, with bounded retries, and puts every delivery in the
  activity feed.
- **Cloud Run services really run**: any image the Docker daemon can pull,
  started with `PORT` set and every request proxied through, so the
  response is the one the container produced. Revisions, IAM recorded, and
  a URL that is stable across redeploys because it points at Nimbus.
- **`internal/engines/run`** is a new cloud-neutral engine for HTTP
  services, beside `k3s` and `funcs`. A service is not a function: it
  answers any method on any path, it is its own image, and its status code
  is its answer.
- **`examples/terraform/gcp-gke`** and **`examples/terraform/gcp-run-pubsub`**,
  both applying, planning clean and destroying.
- **`examples/multicloud`**: a network and a managed Kubernetes cluster on
  Azure, AWS and GCP in one apply, against one process. Twelve resources,
  three k3s containers, three tabs.
- **The visualizer needed one type-table entry** to draw GKE in the
  Kubernetes view, and one name in a clouds list. No new drawing code,
  which is what Milestones 7 and 8 were for.
- **A `nimbus gcp` command group**: `register`, `kubeconfig`,
  `install-auth-plugin`.
- New protocol facts in CLAUDE.md, including the two that cost the most:
  a GKE node pool's count is computed from the Compute API's instance
  group managers rather than read, and GKE and Cloud Run poll opposite
  things — the cluster's status versus the operation's `done`.
- Two bugs of our own: a count written as an `int` was invisible to a
  reader expecting `float64`, and a node pool's background rescale could
  start a container after the cluster's delete had stopped everything,
  leaving a "successful" destroy with k3s still running.

## 0.9.0 — 2026-11-06

Milestone 9: GCP core. A third cloud, and the one that tested whether
"multi-cloud" survived being built twice.

- **GCP on port 4703**, plain HTTP. `nimbus start --clouds azure,aws,gcp`
  runs all three in one process, sharing the store, the event journal and
  the visualizer.
- **Measured before it was built.** The google provider was pointed at a
  logging server first, and what came back shaped everything: it makes no
  startup calls at all, it never polls an operation that already says DONE,
  and its `*_custom_endpoint` settings carry their API version for some
  services and not others.
- **Resource Manager**: projects over v1 and v3 from one stored project, and
  the project IAM policy — recorded, never enforced.
- **Compute**: networks, subnetworks with secondary ranges, firewalls,
  routes, Cloud Routers, regions and zones, and operations. Routed
  generically on `/projects/{p}/{scope}/{collection}/{name}` with a table of
  per-type fill functions rather than a switch. A PATCH applies only the
  fields its `updateMask` names.
- **IAM**: service accounts, with the email derived from the account id and
  the project so it never drifts between reads.
- **Cloud Storage**: buckets and objects with real bytes on disk, all three
  upload protocols (`media`, `multipart/related` and resumable with its 308s
  and `Content-Range` arithmetic), `md5Hash` and `crc32c` computed from the
  content, delimiter listings, and a bucket that refuses to be deleted while
  it holds anything.
- **The visualizer gains a GCP tab** from a `types-gcp.js` table and one
  line in `types.js` — which was the whole point of the Milestone 7
  refactor. The lint test that forbids a shared module naming a cloud's
  types now covers three clouds, and `gcp` could no longer play the unknown
  cloud in its own assertions.
- **`examples/terraform/gcp-basic`**: a custom VPC, two subnetworks, a
  firewall rule, a service account and a bucket with an object. Applies,
  plans clean and destroys, and runs in CI beside the Azure and AWS examples
  against one server.
- **`docs/GCLOUD.md`**: the endpoint overrides and `auth/disable_credentials`
  that point the `gcloud` CLI at Nimbus.


## 0.8.0 — 2026-11-06

Milestone 8: AWS compute. Three services that run rather than record, and the
k3s engine lifted out of Azure so both clouds share it.

- **The k3s engine is cloud-neutral** (`internal/engines/k3s`). One `Engine`
  per cloud with its own container prefix and registry; Azure keeps its hooks
  and ARM shapes and becomes a caller. The AKS tests and the `azure-aks`
  example passed untouched, which was the point of doing it first.
- **EKS**: clusters and node groups over REST-JSON, backed by real k3s
  containers. `aws eks update-kubeconfig` produces a kubeconfig that works,
  because Nimbus answers the **token authentication webhook** real EKS uses —
  `aws eks get-token` builds its bearer token entirely client-side, so no
  static token can ever match it.
- **Lambda**: handlers that really execute, in `public.ecr.aws/lambda/*`
  images through the Runtime Interface Emulator they ship. Zip or S3 code,
  environment variables, timeouts, `RequestResponse` and `Event` invocations,
  `LogType=Tail`, tags and resource policies. A function reaches Nimbus's own
  services through `AWS_ENDPOINT_URL`, so a handler written for real AWS runs
  here unchanged.
- **DynamoDB**: tables and items, with the expression language the SDKs send —
  `KeyConditionExpression` with BETWEEN and `begins_with`, filters,
  `UpdateExpression` with SET/REMOVE/ADD/DELETE, `if_not_exists`,
  `list_append`, conditions, projections, paging by key, batches. Attribute
  values keep their wire shape end to end, so a 38-digit number survives, and
  an expression Nimbus cannot parse is refused rather than quietly matching
  everything.
- **CloudWatch Logs**: groups, streams and events, with a function's output
  written to `/aws/lambda/<name>` — the emulator's own duplicate START/END/
  REPORT lines filtered out.
- **The Kubernetes view is multi-cloud.** It had one Azure type name in it;
  with that replaced by each cloud's own, an EKS cluster draws with no AWS
  code at all. The AWS inspector gains a summary panel (runtime, invocations,
  item count, cluster endpoint).
- **`examples/terraform/aws-serverless`** and **`examples/terraform/aws-eks`**,
  both applying, planning clean and destroying, both in CI where Docker is.

Fixed along the way: the readiness probe used to be a real invocation, which
ran the handler once at deploy time — a function with side effects performed
them before anybody asked, and the serverless example wrote a phantom item on
every apply. A GET to a path the emulator does not serve answers as soon as
the port is open and runs nothing.


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
