# Changelog

## Unreleased

Picking off deferred items from the roadmap's "Later", a tier at a time.

- **A presigned URL's expiry is enforced, and its identity is honoured.**
  Both were missing and either one alone made presigning useless locally:
  a URL minted with `--expires-in 1` still served its object minutes
  later, and `AWSAccessKeyId` — SigV2's spelling, which is what
  `aws s3 presign` emits under aws-cli 1.x — was not read at all, so a
  presigned request resolved to **anonymous**, throwing away the authority
  of whoever signed it. An expired URL is now refused with S3's own
  `AccessDenied` / "Request has expired", quoting both times, in the SigV2
  and the SigV4 form; the key names the principal whose permissions are
  checked. Enforced whether or not `--enforce-iam` is on, as a storage SAS
  is on the Azure side, because a presigned URL needs no principal and so
  has no root fallback to land in. The signature itself is still not
  verified — Nimbus holds no secret for a base credential — and
  `docs/ROADMAP.md` says so, with the one case that could be.
- **A pending subnet is drawn inside its pending network.** The plan does
  not say so the obvious way: a subnet's `vpc_id` is unknown exactly as its
  own id is, because the network does not exist yet either. It keeps the
  relationship in its `configuration` section as a Terraform address
  instead, which the overlay now carries — addresses rather than
  per-provider field names, so it works for every provider without a table.
- **The plan overlay works for GCP, which it never had.** Every `google_*`
  type was missing from its tables, so a loaded plan drew each pending change
  as a bare Terraform address with no badge and no place on the graph — and
  nothing failed, because an unmapped change still draws. Eight AWS and six
  Azure types the examples use were missing too. A test now reads the
  examples' own `.tf` files, so a type nobody considered fails rather than
  drifting. A pending create in AWS or GCP is also parented to the cloud's
  root — an account, a project — instead of floating beside the graph, since
  neither cloud has a resource group to sit under.
- **ARM deployments work at subscription scope.** `az deployment sub create`
  reached Nimbus already — the path parsed and the hook fired — and failed on
  `ParentResourceNotFound: Parent resource '/subscriptions/.../resourceGroups/'
  not found`, because every template resource's ID was built on a resource
  group that was not there. A deployment's scope now comes from its own path
  (not from the template's `$schema`, which says what the author intended
  rather than what ARM was asked to do): at subscription scope IDs hang off
  the subscription, `deployment().location` comes from the request's
  top-level `location`, and `resourceGroup()` is refused by name at submit
  time. A resource group's ID has no `providers` segment, which is the one
  place the generic builder is wrong and the type a subscription deployment
  most often creates. `az deployment sub list` no longer reports
  resource-group deployments.
- **A failed Azure provision records its reason on the RESOURCE**, not only
  on the operation. `az deployment ... show` reads the resource, so a
  failure it could see said nothing but "Failed"; real ARM puts it in
  `properties.error`. Found by a test whose own failure message read
  "the deployment failed: &lt;nil&gt;".
- **A nested ARM deployment's outputs reach the outer template.** The
  resources always worked — a nested deployment is an ordinary
  `Microsoft.Resources/deployments` resource going through the same PUT — so
  what was missing was the half that makes nesting worth doing:
  `reference('inner').outputs.x.value` needed `reference()` to accept a bare
  NAME (ARM resolves one within the template, and it is the documented way to
  read nested outputs), and the outer deployment to WAIT, since a nested
  deployment's PUT answers while it is still provisioning.
- **DynamoDB TTL expires items.** `UpdateTimeToLive` round-tripped as a
  setting and nothing ever expired. A background sweeper takes them now, and
  the removal reaches the table's stream as an ordinary REMOVE stamped with
  the service principal — which is the only thing distinguishing an expiry
  from a client's own delete, and the whole basis of the archive-what-expired
  pattern. Reads are deliberately not filtered: that is DynamoDB's own model,
  and filtering would make an item vanish with no record of it going.
  **The Streams API and the Lambda event spell that identity differently** —
  `PrincipalId`/`Type` against `principalId`/`type` — and each client finds
  nothing in the other's: with lowercase keys `aws dynamodbstreams
  get-records` reported an empty object.
- **A DynamoDB stream invokes a Lambda.** `aws_lambda_event_source_mapping`
  is the poller between the two halves this project had built separately —
  and it is a poller, which is the part people get wrong: a function does
  not subscribe to anything, so batch size and starting position configure
  the MAPPING and a function with no mapping is never called however much
  is written to the table. An event source Nimbus cannot poll is refused by
  name rather than accepted and never fired. A handler that threw is a
  FAILED batch, retried and then skipped, where real Lambda blocks the
  shard until the records expire — stated in `LastProcessingResult` rather
  than silent.
- **DynamoDB has streams.** `StreamSpecification` round-tripped as a setting
  and nothing recorded a thing. Every write to a stream-enabled table is now
  appended as an INSERT, MODIFY or REMOVE carrying the item's keys and
  whichever images `StreamViewType` asks for, `LatestStreamArn` and
  `LatestStreamLabel` are on the table description (so `stream_arn` reaches
  Terraform state), and `aws dynamodbstreams` reads it all back. One shard
  per table — which gives a total order, more than real DynamoDB promises.
  An iterator type nobody implemented is refused rather than defaulted,
  because a cursor that silently started somewhere else makes a consumer
  miss records or replay them and neither looks like an error. There is no
  event source mapping, so nothing invokes a function from a stream: you
  read it yourself.
- **Lambda publishes versions and has aliases.** `publish = true` and
  `aws_lambda_alias` are ordinary provider resources and neither could
  apply. A published version is an immutable snapshot of the configuration
  AND the code: proved by changing the handler, re-applying, and watching
  version 1 answer with the old code while `$LATEST` answered with the new.
  An alias carries a qualified ARN of its own, which is what makes "may
  invoke prod but not `$LATEST`" a policy somebody can write — so a
  qualified invoke is classified against that ARN. A version's container
  starts on the first invoke rather than at publish time, and a weighted
  alias really splits — ignoring the weights would send every request one
  way while the configuration said otherwise, which a client reads as an
  answer about its canary. **`publish = true`
  does not call PublishVersion**: the provider sends `"Publish": true` in
  the CreateFunction body and only uses the endpoint on an update, so
  implementing the endpoint alone left it reporting version `$LATEST` with
  nothing erroring.
- **Object versioning is a history in BOTH clouds, and the two models are
  genuinely different.** `?versioning` and `versioning.enabled` each
  round-tripped as a setting and did nothing: every write overwrote. Now an
  overwrite keeps what it replaced, a deleted object's history survives, an
  old version is readable by id, and a bucket is not empty while any of it
  is left — which is what the providers' `force_destroy` depends on. S3
  lays a delete MARKER over the history as a new version, so removing the
  marker undeletes; Cloud Storage has no marker and ARCHIVES the live
  generation with `timeDeleted` instead, so there is nothing to remove and
  nothing to undelete. A bucket that never had versioning is unchanged in
  both, down to S3's absent `x-amz-version-id`, and a data directory
  written before this loads unchanged. `s3:GetObjectVersion` and
  `s3:DeleteObjectVersion` are classified separately, as real AWS requires.
- **A GCP role may not grant `setIamPolicy` by accident.** Four bundled
  roles were expressed as wildcards broad enough to hand out the single
  worst permission to be loose about. `roles/cloudfunctions.developer` and
  `roles/cloudfunctions.viewer` are corrected against Google's published
  lists, and `TestNoBundledRoleGrantsIAMByAccident` asks through the
  production evaluator so the catalogue cannot drift back.

## 0.24.0 — 2026-11-25

Milestone 24: GCP runs code, so **all three clouds do** — and none of them
the same way.

- **`google_cloudfunctions2_function` did nothing at all.** Gen 2 now
  applies, `curl` of its URL returns what the handler returned, the plan
  afterwards is clean, it survives a restart still answering, and the
  destroy leaves no container and no image. Built from the source archive
  on `internal/engines/run`, which needed **no change**: the Functions
  Framework is an ordinary HTTP server reading `$PORT`, and that is the
  whole of that engine's contract.
- **The three clouds' three answers**, which is most of why this project
  exists. Lambda mounts the code into a vendor image that already ships
  the Runtime Interface Emulator. Azure Functions needs a .NET host that
  negotiates with a language worker over gRPC, and finds nothing in a
  bind mount — the code must be baked in. Cloud Functions' contract is a
  library the code brings. The third asks the least of the platform, and
  is the only one that runs on arm64.
- **`gcloud functions deploy --gen2` works too**, and needed four
  endpoints the Terraform provider never asks for — a runtime catalogue
  built from the same table the deploy uses, `functions:generateUploadUrl`
  for gcloud's own source route, and **two pre-flight APIs that were
  silently reaching the real Google**. Service Usage and Cloud Build are
  not emulated; `nimbus gcp register` points them at Nimbus so nothing
  leaves the machine, and anything beyond the pre-flight is refused by
  name. Verified: 15 requests, every one to localhost.
- **Gen 1 is not emulated**, and says so. Its list answers empty, because
  `gcloud functions list` reads the v1 and v2 lists and merges them and a
  404 there printed an error beside a listing that had worked. A gen 1
  deploy is refused naming Gen 2.
- **The source archive carries its dependencies**, installed by one
  documented `pip install --target` step, because real Cloud Functions
  installs `requirements.txt` during a remote build and a remote build is
  the one thing an offline emulator cannot have.
- **An image leaked after a restart.** A reattached service cannot know
  its image was built, so the plugin removes it by its derived tag — which
  Azure's function app already did. Found by running the restart
  criterion, not by reading.
- **New example** `examples/terraform/gcp-functions`, a badge in
  `types-gcp.js`, and a CI job driving the example with Terraform and then
  with gcloud.

## 0.23.0 — 2026-11-24

Three milestones in one release. The minor version tracks the milestone
number here — 0.20.0 was Milestone 20 — so 0.21.0 and 0.22.0 do not
exist as releases: Milestone 21 verified the Windows-driven work on
Apple Silicon and Milestone 22 added SAS enforcement, and neither was
cut at the time.

### Milestone 23: a Key Vault that does cryptography, not just storage

- **`azurerm_key_vault_key` and `azurerm_key_vault_certificate` could not
  be applied at all.** Keys, certificates, `storage` and `rng` all
  answered 501, so the half of Key Vault that is about *cryptography*
  rather than *storage* was absent. Both resources now apply, plan clean
  and destroy, and `az keyvault key create/show/list/sign/verify` and
  `az keyvault certificate create/show` work.
- **The private half never leaves, and that is asserted rather than
  hoped for.** A test walks every response body a key can produce —
  bundle, list, versions, crypto result, patch, delete, recycle bin — at
  any depth, for the private JWK members RFC 7518 names. A signature the
  vault makes is verified with `crypto` against the public key the vault
  *published*, and the vault decrypts what that same published key
  encrypted. All of it with `crypto/rsa`, `crypto/ecdsa` and
  `crypto/x509` — no dependency, no network.
- **The full RSA operation set**, not just sign/verify: sign, verify,
  encrypt, decrypt, wrapKey and unwrapKey. azurerm's `key_opts` asks for
  all six and Nimbus returns them in `key_ops`, so refusing four would
  have been a vault lying about itself. EC keys sign and verify and
  cannot encrypt — arithmetic, not a gap, and the refusal says so.
- **A certificate is three objects under one name**: the certificate, a
  key whose private half never leaves, and a secret carrying the whole
  thing for a TLS stack. Creating one writes all three, deleting takes
  all three, recovering restores all three.
- **Two clients were needed and they found different bugs.** Three faults
  were invisible to Terraform: a key *list* entry keeps `kid` flat where
  a bundle nests the JWK under `key` (azurerm never lists keys), a
  certificate create must answer **202** and not 200 (azurerm tolerates
  200), and the sign/verify round trip, which azurerm never performs.
- **An authorization bug this milestone activated is fixed.**
  `DataActionFor` returned `certificatecontacts/read` for *every* path
  under `/certificates`, so a certificate create would have been
  authorized by whatever could read a contact list. Contacts keeps its
  permissive action deliberately — a 403 there crashes azurerm's Key
  Vault client.
- **Three built-in roles**, transcribed from Microsoft's published
  definitions: Key Vault Crypto Officer, Crypto User and Certificates
  Officer. Crypto User may *use* a key and not manage one, which is the
  least-privilege case the data-action axis exists to express.
- **Refused by name rather than approximated**: key import, HSM key
  types, the P-256K curve, rotation policies, backup, and any certificate
  issuer other than `Self`. A certificate's secret reports
  `application/x-pem-file` rather than claiming the PKCS#12 its policy
  asks for, because the standard library has no PKCS#12 encoder — a
  stated divergence that azurerm never sees, since it does not read the
  secret.
- **New example** `examples/terraform/azure-keyvault`, and a CI job
  driving it with both Terraform and `az`.

### Milestone 22: an Azure Shared Access Signature now decides

- **A SAS was accepted on shape alone.** `Authenticate` checked that
  `sig`, `se` and `sp` were *present* and let the request through —
  nothing verified the signature, enforced the expiry, or compared `sp`
  against the operation being attempted. Shared Key, the one scheme that
  carries no identity at all, was the only one verified properly. All of
  it decides now: the signature against both account keys, the `se`/`st`
  window, `sp` against the operation, `sr`/`ss`/`srt` against what the
  request addresses, and `sip`/`spr` against the caller's address and
  transport. Every refusal names what was wrong — a bad signature quotes
  the string-to-sign and the layout it used, an expired token says when it
  expired, and a missing permission names the letter.
- **Enforced unconditionally**, not behind `--enforce-iam`, unlike the
  three identity-policy languages: a SAS needs no principal so there is no
  "unmapped caller is unrestricted" fallback for it to land in, real Azure
  has no mode in which a SAS does not decide, and nothing else in the
  repository generates one — so the blast radius was measured at zero.
- **Three string-to-sign layouts, measured rather than read.** Microsoft
  documents none for the `sv` that `az` 2.90.0 sends, so a token signed
  with a key we chose became the oracle: each layout was confirmed offline
  by reproducing the client's own signature. The three tokens are test
  fixtures. It mattered — `az` sends `sv=2026-04-06` (16 fields), while
  azurerm sends `2022-11-02` for an account SAS and **`2018-11-09`, which
  is 15 fields, for a container SAS**. Supporting only what `az` emits
  would have refused a token the Terraform provider generates by default.
- **A container's public access level decides, for the first time.**
  `PublicAccess` had been stored, persisted, listed and echoed back as
  `x-ms-blob-public-access` since the blob service was built and read in
  no decision, so every anonymous request to a *private* container was
  served. `blob` now permits reading a known name and not listing;
  `container` permits both; anonymous is read-only either way. A private
  container and an absent one answer identically — 404, naming neither —
  because a distinguishable refusal is enough to enumerate container
  names.
- **A SAS whose signature cannot be decoded is no longer treated as
  anonymous.** `URL.Query()` discards its parse error, so a malformed
  percent-escape in `sig` made the parameter vanish and the request
  present as having no credentials — answering 404 "the specified resource
  does not exist" for what was really a corrupt token. Presence is read
  from the raw query now.
- **What is not implemented is refused by name**, not attempted: a user
  delegation SAS, a stored access policy (`si`), `sr=d`, a service version
  older than `2015-04-05`, and a *service* SAS for queue, table or file.
  An *account* SAS covers every service.
- **New example** `examples/terraform/azure-storage-sas`, with both kinds
  of token generated client-side by the provider, and ten CI probes that
  assert the status *and* the reason.
- Corrects a Milestone 17 note that was right about Shared Key and had
  been read as a claim about the whole storage data plane. Both halves are
  kept in `CLAUDE.md`, because the way it went wrong is the lesson.

### Milestone 21: everything Milestones 18 to 20 built on one Windows machine, run on Apple Silicon

- **Nimbus could not build an image on arm64.**
  `mcr.microsoft.com/azure-functions/python` has no arm64 build, and it is
  the only image Nimbus pulls that does not. A pull or build with no native
  manifest now retries under `linux/amd64`. Azure Functions still does not
  run under emulation — the host faults and stays at 503 — so an amd64
  host remains the requirement, and `docs/TROUBLESHOOTING.md` says so.
  Rosetta was tried and does not help — it changes the failure without
  fixing it — so that is recorded too, to save the next person the
  afternoon.
- **A failed `docker build` reported `Dockerfile:3` and nothing else**, on
  every platform: the helper kept only the first line of the output while
  BuildKit prints the cause last.
- **The readiness probe accepted a 503 that means "not ready"**, which is
  how an emulated function app reported a successful apply and then served
  nothing. Opt-in per service, so an ordinary one is unchanged.
- Verified on macOS: all 19 Terraform examples apply, plan clean and
  destroy leaving no containers; `examples/multicloud` brings up three
  clusters; a Lambda redeploys with changed code; a container reaches all
  three clouds with Azure over its real data-plane hostname, TLS verified.

**A Lambda's code is mounted read-only, as it is in real AWS**, which fixes
a bug that made redeploying a function impossible on Linux. The container
runs as root, so a Python handler's first import left `__pycache__` owned
by root in the code directory, and the ordinary user Nimbus runs as could
not then remove it: `terraform apply` after editing a handler failed with
`unlinkat .../app.cpython-312.pyc: permission denied`, and the `destroy`
before it had already failed silently, because that error was discarded.

Real Lambda's `/var/task` is read-only — only `/tmp` is writable — so this
removes the cause rather than cleaning up after it. `RemoveCode` reports
what it could not delete instead of discarding it, and a directory
poisoned before this change now produces an error that says how to clear
it.

Windows and macOS never saw this, because Docker Desktop presents a bind
mount permissively. CI never saw it because no job had ever deployed the
same function twice: every one applies, invokes and destroys once.
`TestRedeployingAFunctionThatHasRunSucceeds` is that sequence, and it
fails on Linux without the fix.

**Code in one cloud can reach a service in another, and now the real SDKs
can too.** `docs/CROSS-CLOUD.md` is the measured write-up: five vantage
points — a Lambda, an Azure Function App, a Cloud Run service, an EC2
instance and an EKS pod — against ten targets in all three clouds, 49 of
49, with the Azure legs using the real data-plane hostname and a verified
certificate. Proven with `boto3`, `azure-storage-blob`,
`google-cloud-storage` and, in a second language, `@aws-sdk/client-dynamodb`
and `@azure/storage-blob`.

Two things had to be fixed to get there.

**Every container Nimbus starts now resolves Azure's addressable data
planes and trusts the CA in front of them.** A blob URL is
`{account}.blob.storage.nimbus.localhost`, built by the SDK from the
account name, and nested `*.localhost` resolves to `127.0.0.1` — which
inside a container is the container. So the real Azure SDK could not reach
blob storage from a Lambda or a Container App at all; a pod could be fixed
by hand with `hostAliases`, and nothing else could, because Nimbus builds
the `docker run` command line. `docker.SetHostExtras` is a process-wide
provider consulted before every run, rather than a field each cloud would
have to remember, and `azure.DataPlaneHosts` supplies the concrete names.
The CA is mounted at `/etc/nimbus/nimbus-ca.pem` in the same change,
because fixing only the hostname moves the failure from "cannot resolve"
to "certificate verify failed".

**A GCP URL that a client FOLLOWS is built from the host the request came
in on.** An endpoint override could not help here: `google-cloud-storage`
uploaded fine, because it composes that URL itself, and then followed
`mediaLink` to `localhost:4703` and was refused. Identity links are left
alone — a Compute self link is a store id, and a per-request value would
make one resource into two — so Terraform keeps reading the configured
address and sees no diff.

Also fixed: a PATCH may now create a site config (`Hook.PatchCreates`),
which azurerm needs when it updates a function app.
`internal/azure/functionapp_arm_test.go` had **never compiled** — Go reads
a trailing `_arm` as the GOARCH suffix — so eight tests were silently
excluded on every machine and all three CI legs;
`TestNoGoFileIsExcludedFromTheBuild` now asks `go list` directly.

## 0.20.0 — 2026-11-18

Milestone 20: Azure Functions. Azure runs a function now, which was the
last thing it could not do.

    curl -k "https://fn-nimbus.azurewebsites.nimbus.localhost:4701/api/hello?name=nimbus"
    hello nimbus from azure functions

The host is Microsoft's own image, so the programming model is the real
one. `terraform apply` of an `azurerm_linux_function_app` with
`zip_deploy_file` completes, the plan after it is clean, the app survives
a restart, and the destroy leaves no container and no built image while
keeping the shared base image. `examples/terraform/azure-functions` is the
worked example and CI drives all of it on Linux.

**Nimbus builds an image per deployment**, which is the whole shape of
this milestone. The feasibility study found that the Functions host
reports "0 functions found" when the code is bind-mounted and loads them
from an identical tree baked in with a `COPY` — so `docker.Client` gained
`Build` and `RemoveImage`, and the `run` engine can now be asked to build
before it runs. That stays offline: a local base image plus a `COPY`
touches no network.

Two limits, both deliberate and both stated where you meet them.
**Dependencies must be vendored** into `.python_packages`, because the
image ships the host and the Python worker but not `azure-functions` and
real Azure installs `requirements.txt` with a remote build; Nimbus refuses
a deploy without them rather than letting every invocation fail on an
import. And **Python 3.11 is the only runtime**, because each is its own
1.5 GB image; an unknown stack is refused by name.

A function app's logs are in its container console, not in Log Analytics —
real Azure routes them through Application Insights, which Nimbus does not
model. The console carries both the handler's own `print` and the host's
`Executed 'Functions.hello' (Succeeded...)`.

Of everything azurerm insisted on, four are worth repeating:

- **A 501 from the deployment site makes it retry for ever.** Ten minutes
  of "Deployment service came back with a 501 status code, retrying..."
  with the apply never finishing and nothing naming the refused path. A
  404 makes it fail fast. Cloud Storage's `anywhereCaches` lesson one step
  worse: a polite failure that never terminates beats one that merely
  misleads.
- **An empty object is not a safe default.** The generic "unknown action
  returns `{}`" made the provider PANIC with a nil pointer dereference,
  because it dereferences the publishing credentials without checking
  them.
- **`backup` must 404, not answer an empty document.** An empty one reads
  as "this exists and is disabled", and azurerm flattened it into a block
  the configuration never had — a permanent diff.
- **It publishes through `/api/publish?type=zip`**, not the
  `/api/zipdeploy` every tutorial shows, and it checks readiness with
  `GET /deployments` — with no `/api` prefix.

`Hook.SkipSpecDefaults` is new: the generated spec table was inventing
`siteConfig.http20Enabled` as Microsoft's documented `true` where
azurerm's schema defaults it to `false`, and `properties.backup` as an
explicit null. Those defaults exist for the types nobody wrote a hook for,
and for a hand-written type they do harm.

Three bugs of this project's own, all of them old lessons by new roads:
two instances of one engine sharing a role label adopted each other's
containers on a restart; `useDocker` missed the new engine, exactly as
AWS's once missed `s.vms` — so a reflection-based test now counts the
engines and fails if the list is shorter; and a destroy after a restart
left a 3 GB built image behind, because the engine remembers nothing about
having built it once the process has gone.

### Also in this release

Work that landed before the milestone and had not been released yet.

### Functions: a feasibility study that corrects what we believed

Azure and GCP still cannot run a function, but the reason recorded in the
roadmap turned out to be wrong, and wrong in the way that survives: nobody
had tried it. Microsoft DOES publish a Functions host image, and it runs
offline -- a Python function answered through it in a probe.

What is actually hard is that the host expects a deploy, not a mount.
Bind-mounting the code made it find zero functions in both programming
models with nothing in the logs saying why; the identical tree baked in
with a `COPY` loaded and served it. So a Functions deployment needs an
image build -- still offline, since a local base image plus a `COPY`
touches no network, but `internal/engines/docker` has no `Build` verb yet.

The measurements and the resulting design questions are in
`docs/prompts/m20-functions.md`. Nothing was implemented.

### Container Apps: sidecars and replicas that actually run

A template's second container used to be stored and not started, and
`minReplicas` round-tripped as state with nothing acting on it — so an app
with a cache beside it and two replicas asked for applied cleanly, reported
Succeeded, and ran a single container. Worse than an error, because nothing
said so.

Now every container in the template runs, as a group sharing one network
namespace per replica, and `minReplicas` is the number of groups. Requests
are spread across them round robin, and each replica's console reaches the
Log Analytics workspace tagged with its container name.

Measured on a real two-replica app with a redis sidecar: four containers,
the two sidecars in two different namespaces, `nc -z 127.0.0.1 6379`
answering from inside the app container, seven requests split 3/4 across
the replicas, 34 and 32 log lines from each in the workspace, a clean plan,
and a destroy that took all four.

Cloud Run shares the engine, so its sidecars work too. It has no replica
count — nothing here needs one, and the engine side is done if it ever
does.

`maxReplicas` stays inert and `minReplicas: 0` runs one copy: scaling out
means reacting to load and nothing in a local emulator measures load, and
scale-to-zero needs a request to wake a container when the proxy needs
something to forward to. A count above 8 is refused naming the cap rather
than silently clamped. Revisions are still declined, as on Cloud Run.

### Log Analytics: rows that survive a restart, and more KQL

A workspace's rows were memory-only, and losing them did not fail loudly —
a query against a reloaded workspace simply returned nothing, which is
indistinguishable from a workspace nobody has used. They are persisted with
`--persist` now, per workspace, written atomically because the ingestion
goroutine rewrites them every three seconds.

The query subset gained `summarize count()` with an optional `by` column,
`order by`, `sort by`, `top`, `distinct`, `and`/`or` in a `where`, and
`ago()`. That last one is the important one: without it a query's time
range could not be *written*, not merely not honoured, and it appears in
virtually every real KQL query.

The discipline is unchanged — what is not implemented is refused and named,
because a no-op returns every row and an empty result reads as "nothing
logged yet". So parentheses, any aggregation other than `count()`, grouping
or sorting on several columns, and `extend`/`join`/`mv-expand`/`parse` all
still refuse. An aggregation is terminal: after it, only `take` and `limit`
apply, since carrying groups through a pipeline would mean inventing
semantics.

Two details worth knowing: `order by` defaults to **descending**, as KQL
does and no SQL dialect does; and `and` binds tighter than `or`, found by
word boundary so that `contains "android"` is not split in half.

### Condition keys and operators: four to ten, eight to twenty-six

The rule for what belongs in the list is "can the server answer it from the
request in front of it". Nimbus has the request, so it knows the caller's
address, whether the connection is encrypted, what kind of principal it is,
its derived id and the clock — so those are answered now instead of
refused. Tags, organisations and MFA are things it would have to invent, so
they still refuse by name.

The operators gained the Numeric and Date families, `IpAddress`,
`NotIpAddress`, `Null`, the IgnoreCase pair, the `IfExists` suffix, and the
`ArnNotLike` that was simply missing while its three siblings were there.
`ForAllValues:`/`ForAnyValue:` stay unsupported on purpose: they quantify
over a multivalued key, and every key here is a single string.

Two things worth calling out:

- **`aws:SecureTransport` is false, truthfully**, because AWS is served over
  plain HTTP. The standard hardening policy — a `Deny` on
  `aws:SecureTransport: false` — therefore denies everything here. That is
  what the policy says, and a test pins it so nobody smooths it over.
- **The explainer disagreed with the enforcement.** `authz.Explain`
  evaluated with an empty context, so `nimbus can` reported that hardening
  policy as *allowed* while every real request was denied. It takes the
  context now, built from a stand-in request that is honest about what
  Nimbus is, and a test asserts the two paths agree rather than trusting
  that they do.

### A real application, and the three bugs it found

`examples/apps/url-shortener` is the first example here that is an
APPLICATION rather than a tour of one service: a DynamoDB table with a
secondary index, a Lambda doing conditional writes and atomic counters
against it, an S3 bucket it publishes a page to, a log group you read to
see what happened, and the IAM tying them together. It was built to find
out what breaks when services are used together, and it found three
things, none of which any single-service example could have.

- **A versioned bucket could never be destroyed.** `GET /{bucket}?versions`
  fell through to the generic empty document for an unimplemented
  sub-resource, and the aws provider empties a versioned bucket through
  exactly that call — so `force_destroy` deleted nothing, the bucket
  refused to go while it held objects, and the destroy retried past ten
  minutes. `ListObjectVersions` now lists every object as one version with
  id `"null"`, which is all Nimbus keeps.
- **CloudWatch Logs tags were discarded**, under a comment saying they were
  "never read back by anything that matters". The provider reads them on
  every refresh, so a log group with tags applied cleanly and then wanted
  the same change on every plan, for ever.
- **A log group did not survive a restart.** Its existence lived both in the
  store, which persists, and in an in-memory index, which is what
  `DescribeLogGroups` reads — so Terraform said `has been deleted` and
  recreated a resource that was right there.

The second and third are the same lesson as Milestone 18's engine
reattachment and Milestone 11's `Content-MD5`, one service further on; the
first is Cloud Storage's `anywhereCaches` 404 almost exactly.

### Two small things from Milestones 18 and 19

Both about a first contact with the binary that looked like a failure.

- **`nimbus --help` prints usage to stdout and exits 0.** It used to fall
  through to the unknown-command path, which writes usage to stderr and
  exits 2 — rendered in PowerShell as a red `NativeCommandError` block,
  an alarming answer to a command that worked. An unknown command still
  exits 2, where it belongs.
- **Startup says how many files in the data directory it is ignoring.**
  Every store that reads its state off disk skips a file it did not write,
  which is right — refusing to start over a stray file nobody asked about
  would be far worse. But it did that in silence, so an object that was
  genuinely not being served read exactly like one that had never been
  uploaded. `internal/datadir` now prints one counted line per store,
  naming the directory, and nothing at all on a clean start.

  The first version of this counted a pre-`fsname` uppercase directory as
  an orphan. It is not one: `Decode` is a percent-decoder, so a name with
  nothing to decode comes back as itself and a store written before
  Milestone 18 loads exactly as it did. Reporting those would have told
  everyone upgrading that their objects were gone.

## 0.19.0 — 2026-11-17

Milestone 19: surviving a restart. `--persist` promised that state
outlives stopping and starting Nimbus, and nothing in CI had ever checked
it — every Terraform job was apply, verify and destroy inside one process.
That is where four of Milestone 18's six bugs lived.

- **CI restarts Nimbus now.** `restart-control-plane` applies two
  examples, restarts, and requires a **clean plan** — which means every
  field round-tripped through disk exactly as sent, where "the resource
  is still listed" would have passed while all four of those bugs were
  present. `restart-docker` restarts and then **invokes the Lambda**,
  requiring the handler's own output, and fails if a container is left
  behind.
- **Round-trip tests for the two stores that had none**, Cloud Storage
  and DynamoDB. Both assert the wire shape: a 30-digit DynamoDB number
  must survive byte for byte, `{"N":"5"}` and `{"S":"5"}` must stay
  different items, and GCS objects must keep `md5Hash`, `crc32c` and
  `generation`.
- **A function with no running container says so.** It used to answer
  `The function X is still starting. State: Active` — self-contradictory,
  and advice to wait for something that would never happen. Found by
  removing a container with `docker rm` and restarting.
- **Five suspicions disproved and written down**: two restarts behave like
  one, a restart invents no journal runs, a stray file is ignored rather
  than fatal, a container removed externally is correctly not adopted, and
  an apply survives the process being killed mid-flight as long as it
  comes back.
- **A port change is worse than documented on GCP**: an id is the self
  link, so the store accumulates a duplicate of the same resource per port
  used. Kept as a documented divergence; fixing it means rewriting ids on
  load.

## 0.18.2 — 2026-11-17

Milestone 18 closed. Every item on the Windows list has been run, and the
last of them found the sixth bug of the milestone.

- **`local-exec` is `cmd /C` on Windows, where a newline ends the
  command.** `azure-app`'s push provisioner was a two-line heredoc, so
  `docker tag` ran and `docker push` silently did not — the provisioner
  reported success, the apply went green, and the failure surfaced
  minutes later in another system as a pod in `ImagePullBackOff` saying
  `localhost:5001/app:v1: not found`. Joined with `&&` on one line, which
  both interpreters understand, so no per-OS `interpreter` is needed.
- **Every Compute collection has an aggregated form**, not just
  instances. `gcloud compute networks subnets list` — without the
  `--regions` the docs happen to use — answered "Nimbus does not
  implement the Compute collection `aggregated`". Found by running
  gcloud on Windows; not a platform bug.
- **Documentation for people who did not build it**: `INSTALL.md`,
  `CLIENTS.md`, `CLI.md`, `IAM.md`, `TROUBLESHOOTING.md` and
  `ARCHITECTURE.md`, plus a README whose status line had been announcing
  "v0.1.0, Milestone 1" on the front page of a public repository.
- **The rest of the Windows list run**: all four `--enforce-iam`
  examples including Azure RBAC's five-step grant sequence, `az
  deployment` with a real local Bicep compile, `gcp-gke`, `azure-aks`,
  `az aks get-credentials`, the Python storage SDK harness, and
  interactive `nimbus ssh` by hand at a TTY.

## 0.18.1 — 2026-11-12

`--persist` now restores the running things, not just the store.

- **Every engine is reattached on startup.** Only k3s clusters were, so
  after a restart with `--persist` a Lambda read back `Active` and
  answered `ResourceNotReadyException` to every invoke; a container app
  and a Cloud Run service kept their addresses and served nothing; and an
  instance could not be terminated, because the delete never reached a
  container the registry had forgotten. In every case the container was
  plainly `Up` in `docker ps`, which is the least helpful place for the
  evidence to be.
- `funcs` and `run` gained a `Reattach`; `vm` already had one, written,
  unit-tested and called by nobody. The assertion now lives at the cloud,
  where the wiring is, rather than at the engine, where it passed while
  nothing invoked it.
- **`UseDocker` on the AWS server missed the vm engine**, so it kept the
  real Docker client through every test that called it — what the test
  harness's own comment says must never happen.

## 0.18.0 — 2026-11-12

Milestone 18: Windows, actually run. The code had been built and
unit-tested there on every push and never once executed — no Windows
machine had run Terraform, `az` or `kubectl` against Nimbus. Most of the
prompt's suspicions turned out to be wrong, including the one it called
sharpest; the three real bugs were none of them Windows-only.

- **Eight examples apply, plan clean and destroy on Windows**:
  `azure-basic`, `aws-basic`, `gcp-basic`, `azure-storage`,
  `aws-serverless`, `azure-containerapp`, `aws-eks` and `aws-instance`.
  `az` registers through `nimbus az register --run` and drives the blob
  data plane; `kubectl` reaches an EKS cluster through the token
  webhook; `nimbus ssh` works from Git Bash, PowerShell and cmd.exe.
- **An object name is not a filename.** NTFS and APFS are both
  case-insensitive by default, so two keys differing only in case became
  one file — S3 then listed a key that 404'd while the key that worked
  was not listed, and Azure returned the same content under both names.
  `:` opened an NTFS alternate data stream; `?`, `*` and `|` failed the
  write with the error swallowed in both stores. `internal/fsname`
  percent-encodes what a filesystem cannot hold, and is the identity for
  every key in every example here. Only the Linux CI leg was ever immune,
  which is why ten milestones passed without anyone noticing.
- **Every S3 key ending in `.json` was lost on restart, on every
  platform.** `load` skipped all `.json` paths as metadata sidecars,
  including the object's own data file.
- **`--persist` is about shutdown, not about delete.** A destroyed
  Lambda, Container App or Cloud Run service left its container running
  for ever, because two engines checked the keep-running flag themselves
  one level below the only caller that should.
- **A blob name could escape its container.** `../../x` was written two
  levels up, beside the account manifest a blob of that name would
  overwrite. S3 had refused this since Milestone 8; the blob store never
  learned it.
- **`.gitattributes` pins the working tree to LF**, so `gofmt` runs on
  all three CI legs instead of Linux only — it had been skipped there
  with a comment explaining the CRLF problem, which is what stopped
  anyone asking what else CRLF reached. The answer, measured: nothing.
- **A Windows Terraform job joins CI**, applying `azure-basic` and
  `aws-basic`. No Docker: `windows-latest` has no daemon for Linux
  containers, which is a limit of the runner rather than an oversight.

## 0.17.0 — 2026-11-11

Milestone 17: policies attached to the thing, not to the caller.
Milestones 13 and 14 could refuse a request by evaluating what the
CALLER holds. The other half of every cloud's model round-tripped and
decided nothing.

- **An S3 bucket policy decides.** A role whose own policy grants
  nothing but `sts:GetCallerIdentity` now reads an object because the
  bucket says it may, and an explicit `Deny` in a bucket policy beats an
  `Allow` in the caller's own. The `Principal` element was not modelled
  at all — without it a bucket policy grants to everyone — and is
  required in a resource policy and refused in an identity one.
- **An unsigned request is no longer the account root.** It was, which
  meant that with `--enforce-iam` on, sending no credentials was the
  most powerful thing you could do. Anonymous is its own principal now,
  scoped to S3, and `Principal: "*"` is what grants it.
- **A public bucket is badged in the visualizer**, derived by the
  evaluator rather than read off the policy — so a policy granting `*`
  behind `restrict_public_buckets` is correctly NOT public.
- **GCP resource bindings are evaluated**: a Pub/Sub topic's, a Cloud
  Run service's and a GCS bucket's, the last reaching its objects.
  Pub/Sub's `setIamPolicy` was discarding the policy while claiming to
  record it, and Cloud Storage had no bucket IAM endpoint at all.
- **Azure `dataActions` are enforced on Key Vault's data plane.** A
  Contributor can delete a vault and cannot read a secret out of it,
  which is Azure's most surprising true rule. Storage's data plane
  cannot be enforced — Shared Key carries no principal — and the
  roadmap says so.
- **The explanation follows.** `/api/authz` names the bucket policy or
  the binding that decided, answers about objects the store does not
  hold by finding their nearest ancestor, and evaluates a data action on
  the data-plane axis — it would otherwise have told a Contributor it
  may read a secret.
- `internal/authz` was not changed for any of it. Concatenating the
  caller's rules with the resource's IS AWS's same-account rule, a GCP
  resource binding is just another binding, and which Azure axis applies
  rides in the request's context.
- New: `examples/terraform/aws-bucket-policy-enforced`.

## 0.16.0 — 2026-11-11

Milestone 16: one network view, three clouds. It drew a VNet, its
subnets, their security groups and route tables — and it was Azure-only,
declared that way in `render.js`. An EC2 or GCE instance appeared in no
network picture anywhere.

- **The network view serves AWS and GCP**, with the layout and the
  drawing untouched. Only the MODEL became per-cloud: each cloud's table
  exports a `NETWORK` descriptor naming its network and subnet types, how
  a subnet names its network, what hangs off a subnet and how to reach an
  instance's subnet. `networkShape(cloud)` dispatches it the way
  `identityLinks` already did.
- **An instance is drawn inside the subnet it belongs to**, on all three
  clouds — the thing the milestone existed for. Azure goes VM → NIC →
  subnet, AWS reads `subnetId`, GCP reads `networkInterfaces[].subnetwork`.
- **Each cloud's differences are expressed, not flattened.** A GCP
  custom-mode network has no CIDR and its subnets have no chips, because
  a firewall attaches to the network and matches on tags; an AWS subnet's
  route table is found through the route tables' own `associationSet`.
  There is no "public subnet" badge: it means something only on AWS, and
  Nimbus routes nothing.
- **`network.js` is off the Azure exemption list** in
  `test/ui/clouds.test.js`, so a single cloud's type name left in the
  shared drawing fails the build. That guard had never covered GCP at
  all; it does now, verified by injecting a `compute.googleapis.com/`
  type and watching it fail.
- **An export test that passed for the wrong reason, fixed.** It asserted
  a class is styled in the exported SVG by searching the whole of
  `export.js` — which matches a `querySelectorAll` argument. It was green
  while two classes had no rule at all.
- Verified against real applies rather than the fixture: `aws-basic`,
  `gcp-basic` and `aws-instance` each draw, and the instance one shows the
  EC2 instance running inside its subnet.

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
