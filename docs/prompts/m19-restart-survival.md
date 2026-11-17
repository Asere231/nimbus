# Milestone 19: surviving a restart

## Goal

`--persist` promises that state survives stopping and starting Nimbus.
Nothing in CI has ever checked it.

Every Terraform job in `.github/workflows/ci.yml` is apply → verify →
destroy **inside one process**. So the entire persistence path — writing
state, reading it back, adopting the containers that outlived the process —
is exercised only by hand, when somebody happens to restart at the right
moment.

That is not a theory about where bugs might be. **Four of the six bugs in
Milestone 18 were in exactly this path**, and all four were found by hand:

- Two keys differing only in case merged into one file. Only visible on
  disk, after a reload.
- Every S3 key ending in `.json` was lost on restart, on every platform,
  because `load` skipped it as a metadata sidecar.
- `--persist` leaked a container on every delete, because two engines
  conflated "Nimbus is shutting down" with "a client deleted this".
- `vm.Engine.Reattach` was written, unit-tested, and called by nobody, so a
  restart forgot every running thing — a Lambda read back `Active` and
  answered `ResourceNotReadyException` to every invoke.

The last of those is the shape to keep in mind: **the store survives a
restart and the registry does not**, so a resource reads back perfectly and
does not work. Nothing in that picture points at the cause.

This milestone makes a restart something CI does, so the next bug of this
family is found by a machine rather than by somebody noticing.

## What is already known — do not re-measure

- **Every engine is reattached on startup** since 0.18.1. Each cloud's
  `reattach` is synchronous and called from the startup goroutine, so a test
  can assert a state it created rather than racing a background adoption.
  `TestARestartAdoptsEveryEnginesContainers` covers the AWS wiring.
- **`--persist` sets `KeepClusters`**, which makes `Shutdown` return early
  so containers outlive the process. It does NOT mean a delete should leave
  a container; `funcs` and `run` used to think it did.
- **The port is the one thing about a running container that cannot be
  derived.** `docker.HostPort` reads it back off the published mapping.
- **Object names go through `internal/fsname`** before becoming filenames.
  It is the identity for ordinary lowercase keys, so existing stores load
  unchanged.
- **GCP resource ids are self links and embed the port**, so a persisted
  store is only valid for the port that wrote it. That is a known
  divergence, not a bug to fix here.

## Suspicions worth testing first

Predictions, not known bugs. Each is cheap, and **a suspicion checked and
disproved is worth as much as one confirmed** — record both.

1. **A restart mid-apply.** Everything so far restarts between a complete
   apply and a destroy. What happens if Nimbus stops while a long-running
   operation is pending? Operations are in memory by design, and CLAUDE.md
   already says a client fails with `unimplemented polling status
   "Unknown"` against a closed port. Confirm that is still the behaviour and
   that nothing worse happens — a half-created resource that cannot be
   destroyed would be worse.
2. **Two restarts.** Everything adopted once may not be adoptable twice.
   The second startup reads a store that a reattach has already touched.
3. **A restart on a different port.** `--persist` plus `--azure-port 4801`
   on the second run. Azure's storage suffix and ACR's login server embed
   the port, and GCP ids are self links. Expect breakage; the question is
   whether it is *legible* breakage or silent nonsense.
4. **The journal and the activity feed after a restart.** CLAUDE.md records
   that these read different sources and once disagreed. Snapshots exist for
   this. Does the feed still show the real events and not six snapshots?
5. **A container the user removed behind Nimbus's back.** `docker rm` a
   Lambda's container, then restart. Reattach should not adopt a ghost, and
   a delete afterwards should not fail.
6. **An orphan from an older layout.** A pre-0.18.1 store may hold a file
   whose name predates `fsname`. It should load or be ignored, never crash
   the start.

## Build

1. **A persistence round-trip test per store.** `internal/aws` has one for
   S3 objects; `internal/azure/storage` has one for blobs. Cloud Storage
   (`internal/gcp`) does not, and neither does DynamoDB, the Key Vault
   store, or `core.Store` itself at the top level. Write what is missing:
   put a representative item in, make a fresh store over the same
   directory, assert it comes back identical including metadata.
2. **A restart CI job.** One per cloud, or one that covers all three — your
   call, but say why. The shape:
   - `nimbus start --persist`, apply an example
   - **stop Nimbus and start it again**
   - verify the resource still WORKS, not merely that it is listed: invoke
     the Lambda, fetch the Container App's FQDN, `kubectl get nodes`
   - destroy, and assert no `nimbus-*` containers remain
   The verification is the point. "It is in the store" would have passed
   while every one of Milestone 18's four bugs was present.
3. **Whatever the suspicions turn up.**

## Measure first

- **Does a restart actually cost anything?** Adoption lists containers and
  inspects them. If a restart with twenty resources takes a noticeable
  time, say so and where it goes.
- **What does the visualizer show after a restart?** The graph is rebuilt
  from the store and the feed from the journal. They should agree.

## Ask the owner

- **How many Docker-backed restart jobs.** Each costs minutes of CI. One
  cloud proves the mechanism; three prove the wiring per plugin. Recommend
  one Docker-backed job plus a cheap control-plane-only one, and say which
  cloud and why.
- **Whether a restart on a different port should be made to work**, or
  stay a documented divergence. It is currently the latter by accident
  rather than by decision.

## Done when

- [ ] A CI job applies an example, restarts Nimbus, and proves the resource
      still WORKS — not merely that it is listed — then destroys it with no
      container left behind.
- [ ] Every persisted store has a round-trip test: put, fresh store over the
      same directory, identical read-back including metadata.
- [ ] Each suspicion above is recorded as confirmed or disproved.
- [ ] Any divergence a restart causes is in CLAUDE.md, including the ones
      that are deliberate.
- [ ] `gofmt -l .`, `go vet ./...`, `go test ./...` pass, and CI is green on
      all legs afterwards — the new job checked specifically, because a job
      added in a close-out and never looked at can be red for milestones.
