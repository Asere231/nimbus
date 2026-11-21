# Roadmap

**Current: Milestone 21 is done on macOS; Linux by hand is what is
left** (`docs/prompts/m21-cross-platform.md`). Everything Milestones 18
to 20 added on one Windows machine has now been exercised on Apple
Silicon: all 19 Terraform examples apply, plan clean and destroy with no
containers left, `examples/multicloud` brings up three clusters in 33
seconds, a Lambda redeploys with changed code, and a container reaches
all three clouds -- Azure over its real data-plane hostname with TLS
verified. Nine of the ten Windows-driven changes cost macOS **nothing**,
which is recorded as plainly as a bug would be.

The tenth was an architecture nobody had pulled on:
**`mcr.microsoft.com/azure-functions/python` has no arm64 build**, and it
is the only image Nimbus pulls that does not. A pull or build with no
native manifest now retries under `linux/amd64`. Azure Functions still
does not RUN under emulation -- the host faults and sits at 503 -- so an
amd64 host is the requirement, proven not to be image drift because CI
passed the identical digest on amd64 two days earlier. **Rosetta was
tried and does not fix it**, only changes the failure; the decision is to
accept the limitation rather than abandon "real clients are the spec" by
serving the trigger ourselves. See CLAUDE.md for the alternatives weighed. Two bugs found
alongside it were not about architecture at all: a failed build reported
`Dockerfile:3` and nothing else on every platform, and the readiness
probe accepted a 503 that means "not ready", which is how an emulated
function app reported a successful apply and served nothing.

`nimbus ssh` interactively is **done on macOS**, and it had to be a
person: the interactive form allocates a TTY, so no tool can drive it.
Tab completion works, which is the assertion worth making -- a completion
listing cannot be produced without a TTY. It also turned up the only bug
a test could never have found: Docker Desktop printed
`What's next: Try Docker Debug ... -> docker debug nimbus-ec2-web-...`
on exit, ending a session about an INSTANCE by naming the container, in
a command that exists so nobody has to think about one.

**DELIBERATELY UNMET: Linux, by a person.** This is a gap, not an
oversight, and it is recorded here rather than left looking finished.

CI runs Linux on every push -- unit tests, `go test -race`, and every
Docker-backed example, on Docker ENGINE rather than Desktop, which is the
discriminator that caught the `host.docker.internal` bug. So the
*mechanisms* are covered. What is NOT covered is the half that found
three of this milestone's bugs on the other two platforms:

- the interactive `nimbus ssh`, which no tool can drive;
- `nimbus cert` followed as written, with a real `update-ca-certificates`
  and a Terraform apply afterwards -- CI sets `SSL_CERT_FILE` instead,
  which is a different mechanism and Milestone 18 proved the two are not
  interchangeable;
- the exploratory parts: running an example, reading the visualizer in a
  browser, noticing something that was never asserted about.

**What it would cost to close:** a session on a Linux machine with Docker
and a browser, working through the same list
(`docs/prompts/m21-cross-platform.md`) that macOS was held to. Anyone
picking it up should start from the "THE CHANGES MADE FOR WINDOWS"
section of that prompt, since those are the decisions whose cost on Linux
is still unmeasured by a person.

**What to distrust until then:** nothing specific is suspected -- which
is exactly what was true of macOS before this milestone, and it had three
bugs waiting. The honest statement is that Linux is proven to the depth
CI reaches and no further.

**Milestone 20 is done.** Azure runs a
FUNCTION now, which was the last thing it could not do: an
`azurerm_linux_function_app` applies, `curl` of its hostname returns what
the handler returned, the plan afterwards is clean, it survives a restart,
and the destroy leaves no container and no built image.

The feasibility study in `docs/prompts/m20-functions.md` corrected this
file's own reasoning before a line was written -- Microsoft's Functions
host image DOES run offline, and what makes it hard is that it needs a
DEPLOY rather than a mount, so Nimbus builds an image per deployment. Of
the dozen things azurerm then insisted on, the one worth remembering is
that a **501 from the deployment site makes it retry for ever** while a
404 makes it fail fast: Cloud Storage's `anywhereCaches` lesson one step
worse, because a polite failure that never terminates beats one that
merely misleads.

**Milestone 19 is done.** CI now restarts
Nimbus and proves the resource still WORKS — `restart-control-plane`
requires a clean plan afterwards, `restart-docker` requires the Lambda to
answer and leaves no container behind. Until those existed, every Terraform
job was apply, verify and destroy inside ONE process, which is why four of
Milestone 18's six bugs lived in the reload path and all four were found by
hand.

Six suspicions were checked and **five were disproved**: two restarts are
no different from one, a restart invents no runs in the journal, a stray
file in the data directory is ignored rather than fatal, a container
removed behind Nimbus's back is correctly not adopted, and an apply
SURVIVES the process being killed mid-flight as long as it comes back. What
the exercise actually found was one wrong error message — a function with
no container claimed to be "still starting. State: Active" — and one
divergence sharper than documented: a port change makes GCP accumulate a
duplicate resource, because an id is the self link.

Two milestones running, the pattern has held: **most suspicions are wrong,
and the bugs you find are rarely the ones you went looking for.**

**Milestone 18 is done.** Nimbus was run on
Windows for the first time, and everything on the list was run rather than
reasoned about: every example applies, plans clean and destroys there;
Terraform, `az`, `aws`, `gcloud` and `kubectl` all drive it; Lambda,
Container Apps and all three k3s clouds work; `az deployment` compiles real
Bicep; Microsoft's Python SDKs drive the storage data plane; and interactive
`nimbus ssh` was checked by hand at a TTY. What differs about the platform
is in CLAUDE.md under "Windows, actually run", including the suspicions that
were checked and **disproved** — among them the one the prompt called
sharpest.

The lesson worth keeping is what the bugs turned out to be. Six were found,
and only two were about Windows: a case-insensitive filesystem merging two
object keys into one file (also true on macOS, silently, for ten
milestones), every S3 key ending in `.json` lost on restart, `--persist`
leaking a container on every delete, a blob name escaping its container,
only `aggregated/instances` implemented, and a `local-exec` truncated by
`cmd /C`. **Running real clients on an untried platform is what found them;
most were not platform bugs at all.**

The next milestone is unchosen; "Later" below is the list to pick from.

**Phase 4 is complete.** Milestone 11 gave all three clouds instances that
really boot, Milestone 12 cleared the defects that made Nimbus quietly
wrong, Milestone 13 built the decision engine, and Milestone 14 finished
it: Azure RBAC, GCP IAM and AWS IAM are all enforced behind
`--enforce-iam`, the inspector explains why, and `nimbus can` answers the
question directly. "Permissions are recorded but never enforced" is no
longer true of any cloud.

Milestones 1 to 14 are done, on macOS 27 (arm64) with azurerm 4.81.0,
aws 5.100.0, google 6.50.0, az 2.90.0, aws-cli 1.40.33, gcloud 592.0.0 and
Terraform 1.16.4.

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
  by `node --test 'test/ui/*.test.js'`. All of it verified on the owner's
  machine against the `azure-app` apply: the run replays in dependency
  order and ends where the live graph is, the cluster's pods appear behind
  the service they are selected by, and the exported SVG opens standalone.
- `examples/terraform/aws-basic` is the second cloud: a VPC with two
  subnets, an internet gateway, a route table, a security group, an S3
  bucket with versioning and an object, and an IAM role with a policy and
  an instance profile. Fourteen resources, applying against the same
  process that serves Azure, with a clean plan and a clean destroy.
  `aws sts get-caller-identity`, `aws s3 cp/ls` and `aws ec2 describe-vpcs`
  all verified with aws-cli 1.40.33.
- `examples/terraform/aws-serverless` runs a Python Lambda for real: AWS's
  own base image, the Runtime Interface Emulator it ships, and a handler
  that writes to the emulated DynamoDB through `AWS_ENDPOINT_URL` with no
  endpoint in its code. Apply, clean plan, invoke, scan, read the log group
  and destroy all verified.
- `examples/terraform/aws-eks` brings up a cluster `kubectl` can reach:
  `aws eks update-kubeconfig` then `kubectl get nodes` shows two nodes, and
  a deployment created in it appears in the visualizer's Kubernetes view —
  the same view AKS uses, with no AWS-specific code. Apply, clean plan,
  destroy verified.
- The k3s engine lives in `internal/engines/k3s` and serves both clouds; the
  function runner is `internal/engines/funcs`, equally cloud-neutral.
- `examples/terraform/gcp-basic` is the third cloud: a custom VPC, two
  subnetworks with a secondary range, a firewall rule, a service account
  and a versioned bucket holding an object. Seven resources, applying
  against the same process that serves Azure and AWS, with a clean plan
  and a clean destroy. The GCP tab came from a type table and one line in
  `types.js`, which was the point of Milestone 7's refactor.
- `gcloud` works against the same server, with endpoint overrides and
  `auth/disable_credentials`: `compute networks list`, `networks subnets
  list`, `firewall-rules list`, `iam service-accounts list`,
  `projects describe`, `storage ls/cp/cat`, all verified on macOS with
  gcloud 592.0.0 and written up in `docs/GCLOUD.md`. The CLI found a bug
  Terraform never would: it quotes its multipart boundary in a way RFC
  2045 forbids and Go refuses.
- `examples/terraform/gcp-gke` runs a real GKE cluster: k3s in Docker,
  `kubectl get nodes` showing two nodes through a token authentication
  webhook, a node pool whose count really adds and removes agent
  containers, and a clean plan and destroy. `nimbus gcp kubeconfig`
  writes the kubeconfig; `gcloud container clusters get-credentials`
  works too, after `nimbus gcp install-auth-plugin`.
- `examples/terraform/gcp-run-pubsub` ties Pub/Sub to Cloud Run: a Python
  container that really runs, a push subscription, and a published
  message arriving in the handler's own log. Verified with Terraform, a
  plain curl and `gcloud pubsub topics publish`.
- `examples/multicloud` creates a network and a managed Kubernetes
  cluster on all three clouds in one apply, against one Nimbus process:
  twelve resources, three k3s containers, three tabs, `kubectl get nodes`
  against each. Applies, plans clean and destroys.
- The GKE cluster needed one entry in `types-gcp.js` and one name in
  `render.js`'s clouds list to appear in the Kubernetes view — no new
  drawing code at all, which is what Milestones 7 and 8 were for.
- CI is green on ubuntu, macos and windows, plus the SDK harness, the
  generator's tests, the visualizer's node tests, the Terraform
  apply/plan/destroy jobs (Azure AND AWS against one server), the two
  Docker-backed AWS jobs (Lambda + DynamoDB, EKS + kubectl) and an az +
  Bicep deployment job.

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
| 7 | ~~Core: SigV4-tolerant auth, STS, IAM, S3, VPC/EC2 networking~~ **done** | The `aws` provider creates a VPC, subnets and an S3 bucket via `endpoints {}` overrides |
| 8 | ~~EKS on k3s, Lambda in Docker, DynamoDB on a local store~~ **done** | An EKS cluster is reachable with `kubectl` |

## Phase 3 — GCP (port 4703)

| # | Milestone | Done when |
|---|---|---|
| 9 | ~~Core: OAuth, Resource Manager projects, Compute networks, GCS~~ **done** | The `google` provider creates a VPC and a bucket via `*_custom_endpoint` settings |
| 10 | ~~GKE on k3s, Pub/Sub, Cloud Run in Docker~~ **done** | A GKE cluster is reachable with `kubectl` |

## Phase 4 — making it functional

| # | Milestone | Done when |
|---|---|---|
| 11 | ~~**Compute instances** on a new `vm` engine, all three clouds~~ **done** | `nimbus ssh` into an instance Terraform created, and its boot-script output is in the console log |
| 12 | ~~**Correctness pass**: AKS node resource group, namespaces, S3 multipart, the feed after a restart~~ **done** | A >8 MB `aws s3 cp` round-trips byte for byte, a destroy leaves no `MC_*` group, and a pod outside `default` is drawn |
| 13 | ~~**Authorization core + AWS IAM**, opt-in via `--enforce-iam`~~ **done** | An apply fails with a legible `AccessDenied` and succeeds once the policy grants the action |
| 14 | ~~**Azure RBAC, GCP IAM, and explaining a decision**~~ **done** | The inspector says why the current principal is allowed or denied, on all three clouds |

Milestone 11 is first because it is the clearest gap and the `vm` engine
follows a pattern that has now worked twice (`k3s`, `run`). Authorization
comes last because it is the largest, and because refusing access is easier
to demonstrate once there are instances to refuse it to.

- `examples/terraform/aws-instance`, `gcp-instance` and `azure-vm` each boot a
  real container: an EC2 instance on the lifecycle states the provider waits
  for, a GCE instance whose image comes from `debian-cloud/debian-12` and
  really runs Debian, and an Azure VM that provisions asynchronously so the
  apply does not finish before the machine is up. All three apply, plan clean,
  stop and start keeping their filesystem, and destroy leaving no containers.
  `nimbus ssh NAME` gives a shell on any of them without knowing which cloud.

- Milestone 12 was the correctness pass, and one of its four defects turned
  out not to exist: the k3s watcher had never filtered to `default`, so a
  custom namespace already appeared — proven on all three clouds and now
  covered by tests, with the stale claim corrected here and in CLAUDE.md.
  The other three were real. The AKS node resource group now dies with its
  cluster; S3 multipart upload works, and implementing it uncovered a worse
  bug on the DOWNLOAD side, where ignoring `Range` while advertising
  `Accept-Ranges` silently corrupted every large file; and the activity feed
  is seeded from the journal, so it no longer claims nothing has happened
  after a restart.

- Milestone 13 turned the project's most-repeated limitation into a
  feature: `internal/authz` decides whether a request is allowed and knows
  no cloud's policy language, and AWS IAM compiles its documents down to
  it. Behind `--enforce-iam`, an apply now fails with a legible
  `AccessDenied` naming the action, the principal and the statement that
  decided, and succeeds once the policy grants it — both directions proven
  in `examples/terraform/aws-iam-enforced` and in CI. The flag costs
  nothing when off, and nothing when on either: an unmapped caller is the
  account root, so `aws-basic` applies, plans clean and destroys against an
  enforcing server unchanged. Measured along the way: a denied read fails a
  plan rather than being mistaken for a missing object, which is what made
  enforcement safe to build at all.

- Milestone 14 finished authorization. Azure RBAC and GCP IAM now run on
  the engine Milestone 13 built — which needed **no change** for either,
  the question M13 left open. `examples/terraform/azure-rbac-enforced`
  proves scope inheritance: a role assignment made at a resource group
  governs a VNet created inside it, and the app module's second apply
  succeeds with no change to its own configuration, because what changed
  was the permission. `examples/terraform/gcp-iam-enforced` does the same
  for a bucket through a project-level binding. The inspector explains, for
  any selected resource, which of that type's actions the principal may
  perform and which rule decided; `nimbus can ACTION RESOURCE` asks the
  same endpoint, so the answer cannot drift from the behaviour. All three
  clouds enforce in ONE process, verified against `examples/multicloud`
  with the flag on.

## Phase 5 — finishing what is started

| # | Milestone | Done when |
|---|---|---|
| 15 | ~~**Finishing Azure**: Container Apps on the `run` engine, Log Analytics that ingests and queries~~ **done** | `curl` reaches a container Terraform created, and its stdout comes back out of a Log Analytics query |
| 16 | ~~**A network view for AWS and GCP**~~ **done** | An EC2 and a GCE instance each appear in the subnet they belong to |
| 17 | ~~**Resource-level policies**~~ **done** | A principal reads a bucket ONLY because of the bucket policy, and an explicit deny in it wins |
| 18 | ~~**Windows, properly**~~ **done** | Terraform, a data plane and a container all work on Windows, and what is different is written down |
| 19 | ~~**Surviving a restart**~~ **done** | CI applies, restarts Nimbus, and proves the resource still WORKS — then destroys it with nothing left behind |
| 20 | ~~**Azure Functions**~~ **done** | `curl` of a function app's hostname returns what the handler returned, and the host's own log records the invocation |

Milestone 15 was first because Azure was the one cloud that could not run
code, and it was indeed the cheapest: `internal/engines/run` already did
the work and Azure simply was not using it, and
`microsoft.app/containerapps` was already in the spec table with
`ingress.fqdn` declared and unfilled.

- `examples/terraform/azure-containerapp` runs a real container on Azure:
  an image pulled, a container started, and `curl` against the FQDN from
  the output returning what it serves — through Nimbus, so the request is
  in the activity feed. Applies, plans clean, destroys, leaves nothing.
  Its console lands in the Log Analytics workspace its environment names,
  and `az monitor log-analytics query` returns the rows: a workspace is no
  longer a shell whose comment said queries "would find nothing".
  Measured along the way: azurerm polls the RESOURCE for a container app
  rather than an operation, which is GKE's behaviour and the opposite of
  Cloud Run's.

- The network view draws all three clouds. One drawing, one layout, and a
  `NETWORK` descriptor per cloud saying which type is a network, which is
  a subnet, how a subnet names its network, what hangs off it and how to
  reach an instance's subnet — the same dispatch `identityLinks` already
  used. Verified against real applies rather than the fixture:
  `aws-basic` draws a VPC with its CIDR and region, both subnets and the
  route table associated with the public one; `gcp-basic` draws a
  custom-mode network with no CIDR and no per-subnet chips, because a GCP
  firewall attaches to the network and matches on tags; `aws-instance`
  draws the EC2 instance inside `nimbus-instance-a`, which is the thing
  the milestone existed for. `network.js` is off the Azure exemption list
  in `test/ui/clouds.test.js`, so a single cloud's type name left in the
  shared drawing now fails the build — and that guard grew a GCP arm,
  which it had never had.

- `examples/terraform/aws-bucket-policy-enforced` proves the half
  identity-based evaluation could not express: a role whose own policy
  grants nothing but `sts:GetCallerIdentity` reads an object, and the
  only reason is the bucket's policy. An explicit `Deny` in that policy
  beats an `Allow` in the caller's own; `Principal: "*"` makes the bucket
  readable with no credentials at all (`aws --no-sign-request`), and
  `restrict_public_buckets` takes that away while the policy still says
  `*` — which the visualizer's "Public" badge follows, because the badge
  is derived by the evaluator rather than read off the document.
  `examples/terraform/gcp-iam-enforced` grows a member granted only by a
  bucket's own IAM policy, and `azure-rbac-enforced` a secret readable
  only with "Key Vault Secrets User". All measured against aws 5.100.0,
  google 6.50.0 and azurerm 4.x.

**Still unscheduled, and the gap to trust least:**

- **Everything still unscheduled is listed under "Later".** Windows was
  Milestone 18 and is done; see the Windows entry above and CLAUDE.md's
  "Windows, actually run" for what is now proven there and what is not.

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

- **`examples/multicloud` is not in CI**, and was not before this
  milestone either: it wants Docker for three k3s clusters at once, which
  is a heavy new job to add in a close-out and exactly the kind nobody
  then looks at. It is verified on the owner's machine instead — twelve
  resources across three clouds in one process with `--enforce-iam` ON,
  applying, planning clean, destroying and leaving no containers, which is
  the strongest evidence that turning enforcement on costs nothing. The
  flag-off path for four examples across the same three clouds IS in CI.

- **The Docker-stopped path is only unit-tested.** Stopping Docker Desktop
  mid-session would have disrupted the machine, so the ARM error and feed note
  are covered by a test against the Fake rather than by pulling the plug.

- **Windows is no longer the gap to trust least** — Milestone 18 ran it. What
  was exercised there, on Windows 11 Home with Terraform 1.17.1, azurerm 4.x,
  aws 5.100.0, az 2.92.0, aws-cli **2**.40.0 and Docker Desktop 29.9.3:
  `azure-basic`, `aws-basic`, `gcp-basic`, `azure-storage`, `aws-serverless`,
  `azure-containerapp`, `aws-eks` and `aws-instance` all apply, plan clean
  (`-detailed-exitcode` = 0) and destroy; `az` registers through
  `nimbus az register --run` and drives the blob data plane; `kubectl` reaches
  an EKS cluster through the token webhook; `nimbus ssh` works from Git Bash,
  PowerShell and cmd.exe. `nimbus cert`'s `CurrentUser\Root` advice is
  verified rather than assumed.
  `gcloud` works too, after `nimbus gcp register --run`, and the
  visualizer's 109 `node --test` cases pass there. gcloud found a real gap
  nothing else had: `compute networks subnets list` — without the
  `--regions` that `docs/GCLOUD.md` documents — reads the AGGREGATED
  collection, which was implemented for instances only.
  Since then, also verified on Windows: **all four `--enforce-iam`
  examples**, including `azure-rbac-enforced`'s full five-step sequence —
  the app module denied with `AuthorizationFailed`, granted from the
  platform module, then succeeding with no change to its own configuration;
  **`az deployment`** with a real local Bicep compile (az 2.92.0, Bicep
  0.49.2), outputs resolving and a second what-if reporting `NoChange` for
  every resource; and **`gcp-gke`**, where `nimbus gcp kubeconfig` wrote a
  config `kubectl` used to see both nodes Ready.
  Also verified: **`azure-aks`** — applies, the kubeconfig Terraform writes
  drives `kubectl get nodes` to two Ready nodes, destroys clean; and the
  **`test/storage-sdk` Python harness**, which passes in full against
  Python 3.15 and the pinned Azure SDKs — blob including a 2 MB block
  upload, a ranged read and metadata casing, queue including visibility
  timeouts and pop receipts, and table including an OData filter and a
  merge. Note `run.sh` is **Unix-only** (`$VENV/bin/python`); on Windows a
  venv puts the interpreter in `Scripts/`, so run `check.py` directly:
  `.venv/Scripts/python.exe check.py --account stnimbusdata`.
  **Interactive `nimbus ssh` works on Windows**, run by hand from
  PowerShell because a TTY cannot be driven by a tool: the shell opens on
  the container, Ctrl+C interrupts both at the bare prompt and during a
  `sleep` without killing the terminal, and `exit` returns cleanly. Arrow
  keys were not checked and are not a Nimbus property anyway — alpine's
  busybox `sh` has no line editing compiled in.
  **`az aks get-credentials` works** — the earlier failures were a wrong
  resource group guessed by hand, not a bug; `az aks list` names it.
  **`azure-app` works too, and found a real Windows bug**: its push
  provisioner was a two-line `local-exec`, whose default interpreter is
  `/bin/sh -c` on Unix but `cmd /C` on Windows, where a NEWLINE ends the
  command. So `docker tag` ran, `docker push` silently did not, the
  provisioner reported success, and the failure surfaced minutes later and
  somewhere else as a pod in `ImagePullBackOff` with `localhost:5001/app:v1:
  not found`. Joined with `&&` on one line, which both interpreters
  understand, the whole chain works: image pushed to the emulated ACR, k3s
  pulling it through the containerd mirror, pod `Running`, and
  `DB_PASSWORD=hunter2` in its environment read from the emulated Key Vault.
  **Every item on the Windows list has now been run.**

## Later

- **A persisted store written before Milestone 18 may hold a stale file.**
  `fsname.Encode` is the identity for ordinary lowercase keys, so existing
  data loads unchanged, but a key that previously collided or failed to write
  can leave one orphan file from the broken era. Clearing `~/.nimbus/data` is
  the simple answer. Startup now SAYS how many such files it is ignoring, so
  the orphan no longer sits there unmentioned — what it still does not do is
  reconcile a legacy name with its encoded one, so a bucket whose directory
  predates `fsname` can end up with a stale copy beside a current one and the
  one that wins depends on walk order.
- **A container app runs EVERY container in its template, and `minReplicas`
  copies of the group.** They share one network namespace per replica, so a
  sidecar reaches the main container on localhost exactly as it does in
  Azure; only the first container's port is published and only it is
  reached by the FQDN, which is also what Azure does. Requests are spread
  across the replicas round robin.

  `maxReplicas` is recorded and inert, and `minReplicas: 0` runs one copy.
  Both are honest rather than lazy: scaling OUT means reacting to load and
  nothing in a local emulator measures load, and scale-to-zero needs a
  request to wake a container up when the proxy needs something to forward
  to. The count is capped at `run.MaxReplicas` (8) and a larger one is
  REFUSED naming the cap, rather than silently clamped -- a configuration
  asking for 50 and getting 8 is a divergence with nothing to read.

  There are still no revisions: one app, one template,
  `latestRevisionName` derived from the id and stable. Running every
  revision at once is what traffic splitting would cost, and nothing a
  local apply does needs it -- the same decision Cloud Run made here.
- **An app's FQDN has one label, so app names must be unique across
  environments.** Real Azure's is
  `{app}.{envdomain}.{region}.azurecontainerapps.io`; a TLS wildcard
  matches exactly one label, so the environment cannot be in the name
  without a certificate per environment. Two environments holding apps of
  the same name is legal in Azure and collides here.
- **A workspace's logs are in memory and bounded** at 10,000 lines, and
  are NOT persisted even with `--persist` — like CloudWatch Logs' and
  Pub/Sub's. The workspace survives a restart; its rows do not. Ingestion
  polls each running app's console every three seconds, so a line can be
  up to three seconds late, and only a CONTAINER APP's output is ingested:
  nothing else writes to a workspace, so diagnostic settings, the AKS
  monitoring add-on and the agent ingestion API still record nothing.
- **The KQL subset is `where`, `project`, `summarize count()`, `order by`,
  `sort by`, `top`, `distinct`, `take`, `limit` and `count`.** `where`
  takes several comparisons joined by `and` and `or`, with `and` binding
  tighter, and understands `ago(10m)`, `ago(7d)` and
  `datetime(...)` — so a time range can be written, where before it could
  not be expressed at all. `contains` is case-insensitive, as KQL's is.

  Everything else is refused and named rather than guessed at:
  parentheses in a `where`, any aggregation other than `count()`, grouping
  or sorting on more than one column, `extend`, `join`, `mv-expand`,
  `parse`, `render` and every other function. An aggregation is also
  TERMINAL here — after `summarize`, `distinct` or `count` only `take` and
  `limit` still apply, because carrying groups and numbers on through the
  rest of a pipeline would mean inventing semantics.
- **A workspace's rows survive a restart** with `--persist`, unlike
  CloudWatch Logs' and Pub/Sub's. They used not to, and the failure was
  quiet rather than loud: a query against a reloaded workspace simply
  returned nothing, which is indistinguishable from a workspace nobody has
  used. They are still bounded at 10,000 lines per workspace.
- **Azure Functions WORK**, since Milestone 20: an
  `azurerm_linux_function_app` with `zip_deploy_file` applies, `curl` of
  its hostname returns what the handler returned, the plan afterwards is
  clean, it survives a restart, and the destroy leaves no container and no
  built image. See `examples/terraform/azure-functions`.

  The host is Microsoft's own image, so the programming model is the real
  one -- both the classic `function.json` model and the v2 decorators were
  measured. Nimbus BUILDS an image per deployment, because the host finds
  zero functions in a bind mount and loads them from an identical tree
  baked in with a `COPY`; that stays offline, since a local base image
  plus a `COPY` touches no network.

  Two limits worth knowing. **Dependencies must be vendored** into
  `.python_packages`: the image ships the host and the Python worker but
  not `azure-functions`, and real Azure installs `requirements.txt` with a
  remote build that needs the network. Nimbus REFUSES a deploy without
  them rather than letting every invocation fail on an import. And
  **Python 3.11 is the only runtime**: each is its own image of about
  1.5 GB, the table is keyed by `linuxFxVersion`, and an unknown stack is
  refused by name rather than run on the nearest image.

  A function app's logs are in its container console, NOT in Log
  Analytics. Real Azure routes them through Application Insights, which
  Nimbus does not model -- so there is no workspace for them to reach, and
  the console carries both the handler's own output and the host's
  `Executed 'Functions.hello' (Succeeded...)` record.

  There are no slots, no durable functions and no non-HTTP triggers -- a
  timer or a queue trigger needs the host's own scheduler and a working
  `AzureWebJobsStorage`, which is the obvious next thing. The deployment
  (Kudu) site implements the zip deploy and the deployment history and
  nothing else of its large surface.

  Written up with the measurements in `docs/prompts/m20-functions.md`.

- **Resource policies ARE evaluated since Milestone 17**, on all three
  clouds: an S3 bucket policy (with its `Principal` element, and
  `Principal: "*"` making a bucket genuinely public to unsigned
  requests), a Pub/Sub topic's and a Cloud Run service's own bindings, a
  GCS bucket's — which reaches its objects. Azure `dataActions` are
  enforced on Key Vault's data plane.

  What is still NOT evaluated: permission boundaries, SCPs, session
  policies, Azure deny assignments (Terraform cannot create one — only
  Blueprints and managed applications can, so there is nothing to drive
  them with), a role's trust policy when it is assumed, a Lambda's
  resource policy (stored so `GetPolicy` can answer), S3 ACLs, and
  cross-account combination — AWS requires BOTH sides to allow across
  accounts, and Nimbus has one account, so an allow from either side is
  enough here. An account-level `Principal` (`...:root`) refuses rather
  than approximating that rule.

  **Azure STORAGE's data plane cannot be enforced**, and the reason is
  structural rather than unfinished: Shared Key carries no principal at
  all, so there is nothing to evaluate a blob data action against. Doing
  it would mean requiring OAuth on that data plane, which would break
  `examples/terraform/azure-storage` and `test/storage-sdk`. So a
  "Storage Blob Data Reader" assignment still governs nothing, while a
  "Key Vault Secrets User" one decides.

  Nothing authenticates either: the credential IS the claim of identity,
  so enforcement answers "would these permissions work", never "is this
  caller genuine".
- **The role catalogues are curations and the basic roles are
  approximations.** Azure's built-ins carry the real GUIDs but only the
  actions an example needs are exact; GCP's `roles/viewer`, `editor` and
  `owner` are expressed as the wildcard patterns they amount to, where the
  real documents are enormous explicit lists. Any role not in a catalogue
  REFUSES and names itself. They are transcriptions rather than fetched,
  so they drift as the clouds revise a role, and nothing detects that.
- **An approximation written with `.*` grants more than the real role.**
  `roles/compute.networkAdmin` was granting `*.setIamPolicy` until a test
  caught it. The exclusions are added case by case as they are noticed,
  which means the ones nobody has noticed are still there.
- **Ten condition keys and twenty-six operators are supported**, and the
  rule for what is in the list is "can the server answer it from the
  request in front of it". So the caller's ARN, name, derived id, type and
  account; the requested region; the source address; whether the transport
  is encrypted; and the clock, in both spellings. The operators are the
  String, Arn, Numeric and Date families, `IpAddress`/`NotIpAddress`,
  `Bool` and `Null`, with the `IfExists` suffix on all of them but `Null`.

  Everything else REFUSES by name rather than passing. The notable
  exclusions are `aws:ResourceTag/*`, `aws:PrincipalOrgID`, `aws:TagKeys`
  and `aws:MultiFactorAuthAge` — Nimbus has no tags in the authorization
  path, no organisation and no MFA, and inventing an answer is how a policy
  comes to look as though it protects something it does not — and the
  `ForAllValues:`/`ForAnyValue:` prefixes, which quantify over a
  multivalued key when every key here is a single string.

  Worth knowing: `aws:SecureTransport` is answered truthfully, and because
  AWS is served over plain HTTP the truth is **false**. The standard
  hardening policy, a `Deny` on `aws:SecureTransport: false`, therefore
  denies everything here. That is what the policy says, and it is pinned by
  a test rather than smoothed over.
- **Six AWS managed policies are bundled**, chosen because their real
  documents are short enough to transcribe exactly. They are
  transcriptions, not fetched, so they can drift as AWS revises a version,
  and nothing detects that. Any other managed ARN refuses and names itself.
- **Most EC2 and IAM actions authorize against `"*"`.** AWS itself requires
  that for nearly every Describe, but where real AWS *does* support
  resource-level permissions on an EC2 action, Nimbus does not — so a
  policy restricting `ec2:TerminateInstances` to one instance id grants it
  on all of them here. Too much rather than too little, which is the
  direction a client cannot be hurt by, but it means a narrow EC2 policy
  cannot be *proven* narrow.
- **EKS, Lambda and the k3s clusters authorize the API call, not what is
  behind it.** `eks:DescribeCluster` can be refused; nothing inside the
  cluster is affected, because the token webhook still answers
  `system:masters` for everyone. A Lambda's execution role is recorded and
  the handler's own calls arrive as root, since Nimbus mints no instance or
  function credentials.
- **All three clouds enforce**, since Milestones 14 and 17: AWS IAM, Azure
  RBAC and GCP IAM, plus resource policies and Azure `dataActions`. This
  entry used to say the opposite — that only AWS was enforced — and stayed
  wrong for four milestones after that stopped being true. It was caught by
  running the `--enforce-iam` examples rather than by reading, which is the
  same lesson the rest of Milestone 18 kept teaching.

- **An instance is a container, with everything that implies.** No kernel of
  its own, no block device, no systemd, no cloud-init and no guest agent, so
  anything needing one does not work. Instance types are recorded and nothing
  limits CPU or memory to match: a `t3.micro` has whatever the machine has.
  There are no EBS volumes, managed disks or persistent disks — the root
  filesystem is the container's and it survives a stop/start, which is as far
  as it goes.
- **There is no `sshd` in an instance, on any cloud.** Nimbus is stdlib-only
  and cannot implement an SSH server, and no vendor-official base image ships
  one. `nimbus ssh` is `docker exec`. Key pairs round-trip and the public key
  is written to `authorized_keys`, so a configuration is honest, but
  `ssh -i key.pem` does not work. Running a real `sshd` would mean depending
  on a community image for the headline feature, which was weighed and
  declined; it is the obvious thing to revisit.
- **The metadata service is not at 169.254.169.254.** It is reachable through
  `AWS_EC2_METADATA_SERVICE_ENDPOINT` and `GCE_METADATA_HOST`, which the SDKs
  read, so an SDK inside an instance can find out which instance it is. Code
  written against the literal address cannot. Azure has no such override at
  all, so a VM there gets `NIMBUS_VM_*` environment variables instead. No
  cloud mints instance credentials, so the IAM-role paths answer nothing.
- **GCP still has no Cloud Functions**, and here the old reasoning holds:
  Google publishes no functions host image. Gen 2 IS Cloud Run underneath
  and the runtime contract is the Functions Framework -- an ordinary
  library the user's code imports, whose `functions-framework` command is
  just an HTTP server, so the `run` engine's contract already fits.
  Deploying from SOURCE is what needs a builder and the network. The open
  question is whether to build an image from a plain `python:3.11-slim`
  with the framework vendored, or to require a container image and admit
  it is Cloud Run under another name; `docs/prompts/m20-functions.md`
  states both and asks.
- **Cloud Routers and their NAT configuration round-trip as state and
  nothing routes.** The same is true of every other GCP networking object
  that implies traffic handling.
- **GCP IAM is enforced**, behind `--enforce-iam`: the project policy since
  Milestone 14 and a resource's own bindings — a bucket's, a topic's, a
  Cloud Run service's — since Milestone 17. This entry used to end by
  saying resource-level policies were "recorded and not evaluated", which
  had been false for two milestones, while the AWS entry fifty lines above
  said the opposite.
- **GKE is k3s, so most of its surface is not modelled.** No Autopilot, no
  workload identity, no private clusters, no release channels, no
  autoscaling: those fields round-trip and nothing acts on them. Every
  request to the cluster is `system:masters`, because the token webhook
  says yes to everything — the same limitation EKS has. A node pool's
  nodes are agent containers, not VM instances, so nothing answers
  `instances.list`; the instance group manager behind a pool is derived
  on demand purely so the provider can compute a node count.
- **Cloud Run runs every container in a service, and the latest revision
  only.** Sidecars share a network namespace as they do on Azure, since
  both clouds sit on the same `run` engine. What it does NOT have is
  Azure's replica count: `scaling.minInstanceCount` round-trips and one
  copy runs, because no Cloud Run example here needs more and the engine
  side is already done if one ever does. `traffic`
  round-trips without splitting anything: running every revision at once
  is what that would cost, and nothing a local apply does needs it. There
  is no scale-to-zero, no concurrency limit, no CPU or memory limit, and
  no Cloud Run jobs. `gcloud run deploy` is not implemented — the Knative
  API is served read-only, for `list` and `describe`.
- **Pub/Sub messages live in memory and are bounded**, like CloudWatch
  Logs' events: they do not survive a restart even with `--persist`. Log
  Analytics rows DO, so the three are no longer alike — if a restart loses
  something it should lose, it is one of these two. There is no
  ordering guarantee, no dead-letter topic, no `seek` and no snapshots;
  an `oidcToken` on a push subscription is recorded and never minted or
  verified. Push only reaches endpoints on this machine, by design.
- **An AKS cluster's node resource group outlives its cluster.** Deleting
  the cluster leaves `MC_<rg>_<cluster>_<location>` in the store, where
  real Azure removes it. Noticed during Milestone 10's multicloud
  destroy; it predates that work and affects only the graph.
- **GCS has no object versioning history.** `versioning` round-trips as a
  bucket setting and every write bumps the generation, but the previous
  generation is gone: `?versions=true` lists one entry per name. No
  lifecycle rule ever runs, and there are no signed URLs, no composite
  objects and no customer-managed keys.
- **No OAuth.** The provider is given an `access_token` directly and Nimbus
  never looks at it. There is no token exchange, no metadata server and no
  `gcloud auth login`.
- **EKS node groups are still k3s agent containers, not instances.** They do
  not appear in `DescribeInstances`, so a cluster's nodes and an account's
  instances are two separate worlds. NAT gateways are recorded but nothing
  routes. IAM itself IS enforced now, behind `--enforce-iam`; what is not is
  any of EKS's own authorization — see the entry below.
- **Lambda has only `$LATEST`.** No published versions, aliases, layers,
  event source mappings, function URLs or concurrency settings: publishing a
  version would mean keeping a copy of the code and a second container per
  version, and nothing a local apply does needs it. An `Event` invocation
  runs immediately rather than being queued, so there is no retry behaviour
  to observe.
- **DynamoDB has no streams, no TTL expiry and no index projections.** A
  secondary index works because its keys are ordinary attributes of the same
  items, so a query on it needs no separate storage — but a `KEYS_ONLY`
  index returns the whole item, which is too much rather than too little.
  `UpdateTimeToLive` round-trips as a setting; nothing ever expires. Nested
  attribute paths (`a.b`, `a[0]`), parentheses and `size()` in an expression
  are refused rather than guessed at.
- **CloudWatch Logs keeps 10,000 events per stream, in memory.** They do not
  survive a restart even with `--persist`, a retention policy is recorded and
  never applied, and `filterPattern` is matched as a substring rather than as
  CloudWatch's own small language — which can only ever return too much.
- **EKS has no Fargate, no add-ons, no OIDC provider and no access entries.**
  Every request to the cluster is authenticated as `system:masters`, because
  the token webhook says yes to everything; the aws-auth ConfigMap and IAM
  role mapping are not modelled.
- **S3 has multipart upload but no object versioning HISTORY.** `?versioning`
  round-trips as a setting, but every PUT overwrites: there is one version
  of an object. `?versions` lists each object once, as version `"null"` —
  real S3's id for an object written before versioning was enabled — which
  is what lets the provider empty a versioned bucket for `force_destroy`.
  Until Nimbus answered that call at all, such a bucket could never be
  destroyed: the destroy simply retried for ever. An upload in flight is
  held in memory and bounded at 512 MB,
  so it does not survive a restart — real S3 keeps an incomplete upload
  until a lifecycle rule reaps it, and `ListMultipartUploads` here only ever
  shows what this process started. A ranged GET honours one range; a
  multi-range request is answered with the whole object rather than a
  `multipart/byteranges` body, because no client Nimbus serves sends one.
- **No presigned-URL verification.** A presigned request is routed by its
  `X-Amz-Credential` and then served like any other; the signature and the
  expiry are not checked, like every other signature here.
- **Runs are inferred from gaps, not announced.** `core.RunGap` is 30 s,
  measured against azurerm's silent ~20 s resource-group wait. Two things
  done within half a minute of each other merge into one run, and a pause
  longer than 30 s inside one apply would split it. A `nimbus run --
  terraform apply` wrapper that marked the boundaries explicitly would remove
  the guess; the heuristic needs no new CLI surface, so it went first.
- **A run from before this was built replays without its baseline.** The
  journal is told what already exists only from the first restart that
  writes a snapshot, so runs recorded before that one still begin from an
  empty world and end with fewer resources than are live. Nothing can
  recover what was never recorded; new runs are complete.
- **The activity feed still starts empty after a restart**, because the SSE
  stream replays the bus's in-memory ring and the journal is deliberately not
  streamed. The timeline is how history is reached now; seeding the feed from
  the journal on connect is a small change if it ever grates.
- **The plan overlay's azurerm → ARM type table is partial** (`armTypes` in
  `internal/ui/plan.go`). An unmapped type still draws, labelled with its
  Terraform address; add a line when an example needs one.
- **Plan overlays are topology-only.** The network and Kubernetes views draw
  the live graph and ignore a loaded plan.
- **The Kubernetes view has been checked against the fixture and a real
  cluster on each of the three clouds**, including a workload in a namespace
  of its own. What it has not been checked against is a big cluster: the
  layout is untested beyond a handful of objects, and nothing paginates.
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
- **Azure role assignments ARE enforced** since Milestone 14, behind
  `--enforce-iam`, including scope inheritance from a subscription or
  resource group — and since Milestone 17 that includes `dataActions` on
  **Key Vault's data plane**, so a "Key Vault Secrets User" assignment
  decides and a Contributor can delete a vault without reading a secret
  out of it. Storage's data plane is the exception: Shared Key carries no
  principal, so a "Storage Blob Data Reader" assignment still governs
  nothing.

- **A mid-apply restart fails the apply.** Terraform polls while the port is
  closed and azurerm reports `unimplemented polling status "Unknown"`. Nothing
  Nimbus returns can change that — the request never arrives. Surviving it
  would mean holding the listener open across a restart, which is a much
  bigger change than it is worth today.
- **Cluster watching polls every 3s per cluster.** Fine for a few clusters
  someone is looking at; the Kubernetes watch API would scale better but needs
  resourceVersion bookkeeping and re-sync handling.
- **The SYSTEM namespaces are hidden** from the graph — `kube-system`,
  `kube-public` and `kube-node-lease` — and every other namespace is
  mirrored. That is deliberate: k3s runs about ten pods of its own, and
  drawing them would bury the one deployment somebody made. (This entry used
  to say only `default` was mirrored, which was never true; Milestone 12
  proved a custom namespace appears on all three clouds and added the tests
  that were missing.)

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
