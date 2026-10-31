# Working on Nimbus

Nimbus is a from-scratch, offline cloud emulator in Go with a live web visualizer. Azure first, then AWS and GCP on the same core. The owner is learning cloud engineering through it: explain non-obvious protocol details in comments, keep the code readable.

## Ground rules

- **Main module is stdlib-only.** No third-party Go deps in `github.com/Asere231/nimbus`. Test harnesses that need SDKs live in their own module under `test/`.
- **The visualizer has no build step and no CDN.** Plain HTML/CSS/JS in `internal/ui/static`, embedded with `go:embed`. It must work offline.
- **Real clients are the spec.** Match what the azurerm provider / `hashicorp/go-azure-sdk` actually send and expect, not what the docs say. When unsure, read the SDK source (shallow-clone `github.com/hashicorp/go-azure-sdk`, look in `sdk/client/resourcemanager` and `sdk/environments`).
- **Never call real cloud endpoints.** DNS suffixes in the metadata stay on `*.nimbus.localhost`.
- Run `gofmt -l .`, `go vet ./...`, `go test ./...` before finishing any change.

## How a request flows

1. `internal/azure/server.go` `ServeHTTP` routes metadata, token, OpenID, async-operation paths, else ARM.
2. `internal/azure/ids.go` `parseTarget` turns an ARM path into a `target` (kind, type, ID, parent).
3. `internal/azure/arm.go` handles it generically: PUT/PATCH/GET/HEAD/DELETE, lists, actions.
4. Per-type behaviour lives in **hooks** (`Hook{BeforePut, AfterPut, AfterDelete, Backing}`), registered in files like `network.go`.
5. `internal/core/store.go` stores the ARM JSON and publishes events; `internal/ui` streams them to the browser.

## Protocol facts already learned (do not regress)

- Metadata (`/metadata/endpoints?api-version=2022-09-01`) is a **single JSON object**, not an array. `name`, `resourceManager` and `microsoftGraphResourceId` are required.
- `resourceManager` must have **no trailing slash**; the SDK joins `base + "/" + path`, and a `//` makes its pollers compute the wrong resource path (delete polling never ends).
- Writes return `Azure-AsyncOperation` + `Retry-After: 1`; the operation URL always says `Succeeded`. Without it the SDK waits 10 s per resource. Older SDK versions always use the 10 s delete poller for DELETE.
- Responses must be `Content-Type: application/json` or the SDK skips provisioning-state checks.
- Deleting something absent returns 204. A missing resource group is `ResourceGroupNotFound`; other misses are `ResourceNotFound`.
- Tokens are unsigned JWTs; clients only decode the claims (`oid`, `tid`, `appid`).
- A resource's `id` and `name` are fixed when it is created. ARM resolves IDs case-insensitively but always answers with the casing used at creation, and the name is immutable. Take them from the path only on create, or a client using different casing silently renames the resource and Terraform sees a perpetual diff.
- **macOS rejects long-lived server certificates** with `x509: "localhost" certificate is not standards compliant`, which never mentions dates. The limit is on the validity *span* (`NotAfter - NotBefore`): measured on macOS 27, 824 days passes and 825 fails. Backdating `NotBefore` for clock skew spends part of that budget. Compute `NotAfter` from `NotBefore` as a plain duration in UTC — `AddDate` works in the local zone, so crossing a daylight-saving boundary shifts the span by an hour. Nimbus uses 397 days, under Apple's documented 398.
- **Nested `*.localhost` names resolve to loopback on macOS and Windows, with nothing installed.** `st1.blob.storage.nimbus.localhost` returns `127.0.0.1`/`::1` with no `/etc/hosts` entry, no `/etc/resolver`, and no hosts-file edit on Windows — verified on macOS 27 (system resolver, and Go's with both the cgo and pure-Go resolvers) and on Windows via `[System.Net.Dns]::GetHostAddresses`, which is the `getaddrinfo` path the SDKs use. So the storage data plane needs no DNS tooling. Do not build `nimbus dns`.
- **The storage data-plane suffix carries our port**, which real Azure's never does. azurerm builds data-plane URLs as `https://%s.blob.%s/` from `suffixes.storage` (NOT from `primaryEndpoints`), so a bare hostname would send it to port 443, which needs root to bind. `storage.nimbus.localhost:4701` substitutes into that format string as a valid URL on our own port, and SNI still sees the bare hostname so the certificate matches.
- **A few types' create must answer 202, not 201.** Their SDK clients are generated as long-running and reject anything else outright: `performing Create: unexpected status 201 (201 Created)`. Storage accounts are the first; network types still expect 201. `Hook.CreateStatus` is how a type opts in.
- **A TLS wildcard matches exactly one label.** `*.localhost` does not cover `a.b.localhost`, so each storage service level needs its own SAN (`*.blob.storage.nimbus.localhost`, `*.queue.…`). Adding a name to `serverDNSNames` reissues certificates that predate it, reusing the CA so no re-trust.
- **azurerm polls the blob service for data-plane readiness** right after creating an account (`GET /?comp=properties&restype=service`), and fails the apply if it does not answer. The account cannot finish applying without a working blob endpoint.
- **The Table service signs differently from blob and queue.** A shorter header list, `x-ms-date` instead of `Date`, no `x-ms-*` block at all, and a canonicalised resource carrying only the `comp` parameter. Reading `Date` gives an empty field and a 403 that explains nothing.
- **`Content-MD5` is a content setting, not an automatic property.** A blob uploaded without `x-ms-blob-content-md5` has none; do not compute one. azurerm stores whatever is returned on first read, after which the config (silent about `content_md5`) disagrees with state and every plan wants to replace the blob, since `content_md5` is ForceNew.
- **azurerm 4.x creates containers through ARM** when `azurerm_storage_container` is given `storage_account_id`, but `azurerm_storage_blob`, `az` and every SDK use the data plane. A container made one way must exist the other, or an apply fails with `ContainerNotFound` on a container Terraform created seconds earlier. Both directions are mirrored in `storage.go`.
- **Metadata names must come back lower-cased and written verbatim.** Clients compare them literally — the Python SDK strips `x-ms-meta-` and uses the rest as a dictionary key, so `X-Ms-Meta-Owner` yields `Owner` and a lookup of `owner` misses. Go canonicalises header names on *both* sides, so the read path lower-cases and the write path assigns into `w.Header()` directly instead of calling `Set`.
- **A few types' create is long-running for real**, and the operation endpoint has to track state (`internal/azure/operations.go`). An unknown operation ID 404s, as ARM does — measured: azurerm does NOT mistake that for success. Restarting Nimbus mid-apply still fails the apply with `unimplemented polling status "Unknown"`, but that is the poll hitting a **closed port**, not the operation being lost; nothing we return can improve it.
- **`identityProfile.kubeletidentity.resourceId` must be a full UserAssignedIdentity ARM ID**, not a name. A name fails the apply with `parsing the UserAssignedIdentity ID: the number of segments didn't match`, naming neither the field nor the resource. Azure puts it in the node resource group as `{cluster}-agentpool`.
- **azurerm scales a node pool by PUTting the `agentPools` CHILD**, then reads the count back off the **parent** cluster's `agentPoolProfiles` on the next refresh. Without mirroring child into parent, the apply reports success, no node appears, and the plan is dirty forever. Same parent/child sync the VNet and its subnets need — treat it as a shape, not a one-off.
- **Anything azurerm re-reads must be derived, not generated.** The AKS kubeconfig, `fqdn`, `nodeResourceGroup` and identity GUIDs are hashes of the resource ID or built once and returned byte-for-byte. A value that differs between reads is a permanent diff; this is the `Content-MD5` lesson generalised.
- **A Key Vault's data-plane address comes from `properties.vaultUri`**, which azurerm reads through ARM ("looking up base uri for secret %q in %s") and then uses for every secret call. The metadata `keyVaultDns` suffix carries our port too, for the `az` CLI, which never reads the metadata document.
- **The Key Vault `401` challenge's `authorization` URL must carry a tenant path segment.** go-autorest, still used by the provider for Key Vault, parses the challenge with `bc.values[tenantID] = asURL.Path[1:]`, so a bare origin panics it: `slice bounds out of range [1:0]`, with nothing in the trace mentioning Key Vault. Send `authorization`, `authorization_uri`, `resource` and `scope`, and give the authority a tenant.
- **Reading a vault through ARM makes azurerm read CERTIFICATE CONTACTS from the data plane.** Returning 501 there crashes its Key Vault client. Real Key Vault answers `404 ContactsNotFound` when none are set, which azurerm tolerates deliberately — so answer that.
- **Purging a vault is long-running** (202 + `Azure-AsyncOperation`). A plain 200 sends the poller to the deleted-vault URL, where our own 404 reads as a failed purge and fails the destroy.
- **Deleting a user-assigned identity is synchronous** (200). A 202 fails with `unexpected status 202 (202 Accepted) received with no body`, like storage account and container deletes.
- **azurerm maps a vault URI back to a resource ID through `/subscriptions/{id}/resources`** with a `resourceType eq '...'` filter, not through anything Key Vault-shaped. Without that path the destroy fails while "populating the Key Vaults cache".
- **RBAC is recorded, never enforced.** Role assignments and the built-in role definitions round-trip (with the real GUIDs), but no request is ever refused for lacking a role. A local apply therefore cannot prove a deployment's permissions are right.
- **ACR's `loginServer` is `localhost:<port>`.** Docker treats a registry on localhost as insecure by default, so `docker push` needs no daemon configuration — verified through Docker Desktop's VM on macOS. A pretty hostname would need TLS the daemon trusts (its trust store is inside that VM) or an insecure-registries edit.
- **k3s v1.34.12 points containerd at `certs.d`** (`config_path` in its generated `config.toml`), so a mirror written to `.../certs.d/<host>/hosts.toml` takes effect on the next pull with no restart — which is what lets a registry created *after* a cluster still be pullable. The registry container must also join the cluster's Docker network to be resolvable by name. `registries.yaml` is written as well, for builds that only read registry configuration at startup.
- **Soft-deleted vaults are not persisted** across a restart, like operations: a name that was taken becomes free again. The harmless direction to diverge in.
- **`az` compiles Bicep on your machine**, so Nimbus never sees a `.bicep` file and needs no Bicep parser. `az deployment group create --template-file x.bicep` shells out to `bicep build --stdout` before its first HTTP call (measured in az 2.90.0, `command_modules/resource/custom.py`). Templates arrive as ordinary ARM JSON.
- **A Bicep `for` inside a property is NOT a resource copy.** It compiles to a property-level copy — an object carrying `{"copy":[{"name","count","input"}]}` that expands into a sibling array — which looks nothing like the resource-level copy that repeats a whole resource. A template engine that implements only the latter silently produces a resource with no subnets.
- **`validate` and `whatIf` are POSTed at a deployment name that does not exist yet**, which is the entire point of them, so they have to be answered before the usual "does this resource exist" check.
- **`uniqueString()` is not reproducible from outside Azure**: the algorithm is unpublished. Nimbus returns a deterministic 13-character value that is stable and unique but differs from Azure's. Templates that only need a unique repeatable name are unaffected; one that compares against a value computed in real Azure is not.
- **Etags are derived from the resource ID, not generated.** Azure changes an etag on every write; doing the same here made redeploying an unchanged template rewrite the resource. Nothing Nimbus serves does optimistic concurrency, so there is nothing to lose and a stable read to gain. Same rule as the AKS kubeconfig and `Content-MD5`.
- **When reading Microsoft's OpenAPI specs, every property must keep the file it was declared in.** A resource's `allOf` almost always reaches into `common-types`; if that file becomes "the" current document, the resource's own `$ref`s resolve against it, find nothing, and the type yields zero fields **with no error at all**. `tools/specgen` pins this with a test, and reports unresolvable refs rather than swallowing them.
- **`az cloud register` reads `/metadata/endpoints` at once**, so the Nimbus CA has to be trusted *before* that command runs, not before the first `az keyvault` call. It fails with an SSL error that blames the network. So the metadata document is not only a Terraform/SDK mechanism after all — az fetches it too. `nimbus az register --run` sets `REQUESTS_CA_BUNDLE` on each child process rather than relying on the shell.
- **MSAL validates an unknown authority against the real `login.microsoftonline.com`** before it will talk to ours, which both reaches the internet and then fails, because `REQUESTS_CA_BUNDLE` has replaced the system trust store and a genuine Microsoft certificate no longer verifies. The error says "behind a proxy" and names no authority. `az config set core.instance_discovery=false` turns it off — the switch Azure Stack uses (`azure/cli/core/_profile.py`). An `/adfs`-suffixed authority also disables it, but rewrites the tenant to `adfs`.
- **`az cloud register` is not idempotent** (`The cloud 'nimbus' is already registered`), which re-registering after a port change always hits. The same flags work as `az cloud update`.

- **Some azurerm delays cannot be fixed from this side.** A resource group takes ~20 s to create and every DELETE a flat 10 s, no matter what we return. `azurerm_resource_group` ignores `Azure-AsyncOperation` and runs its own client-side wait that re-reads the group several times; the delete poller waits, then GETs until the resource 404s. Adding async headers to resource-group writes or `Content-Type` to the empty 202 changes neither. Measured against azurerm 4.81.0 — don't re-chase it.

## Adding a resource type (the usual task)

1. Write a failing test in `internal/azure/*_test.go` that replays the client's calls (copy the shape from the SDK or a Terraform debug log: `TF_LOG=DEBUG terraform apply`).
2. Add a hook file (e.g. `storage.go`) and register it in `New()`.
3. Fill server-generated fields in `BeforePut`; start or stop a real engine in `AfterPut`/`AfterDelete` and set `Backing: "real"` if one runs.
4. Add a badge/colour for the type in `internal/ui/static/app.js` (`TYPES`).
5. Prove it with Terraform: add it to an example under `examples/terraform/` and apply + destroy.

## Debugging loop with Terraform

```bash
./bin/nimbus start                      # terminal 1, watch http://localhost:4700 with "show all API calls" on
TF_LOG=DEBUG terraform apply 2> tf.log  # terminal 2
```

The activity feed flags 4xx/5xx responses and "unimplemented action" calls: those are the next things to build.

## Milestone workflow (every session)

Each milestone has a prompt in `docs/prompts/`. When asked to work on one:

1. **Plan first.** Read the prompt, this file and `docs/ROADMAP.md`. Inspect the code it touches. Present a short plan (files, tests, open questions) and wait for approval before editing.
2. **Ask, don't guess, on decisions marked "ask the owner".** Give 2–3 options with a recommendation.
3. **Test first.** For every client call you support, add a test that replays it before implementing.
4. **Prove it with the real client** (Terraform, `az`, `kubectl`, `aws`, `gcloud`) on the owner's machine. The owner runs commands that need their machine (Docker, trusting certs, installing tools); tell them exactly what to run and what output to paste back.
5. **Stop at the done-criteria.** Do not start the next milestone. Note follow-ups in `docs/ROADMAP.md` under "Later".
6. **Close out:** all checks pass (`gofmt -l .`, `go vet ./...`, `go test ./...`); add any new protocol facts to the list above; mark the milestone done in `docs/ROADMAP.md` and move "Current" to the next one; write a short entry in `docs/CHANGELOG.md`; commit with a clear message.

If a session runs long, stop at a green, committed checkpoint and write what is left at the top of `docs/ROADMAP.md`, so the next session can resume from it.

## Where things are going

See `docs/ROADMAP.md`. Current milestone is listed at the top of that file.
