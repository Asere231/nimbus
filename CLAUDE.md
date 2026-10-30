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
