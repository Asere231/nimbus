# Milestone 24: GCP Cloud Functions

## Goal

GCP is the only cloud here that cannot run a function. AWS got Lambda in
Milestone 8, Azure got Functions in Milestone 20, and
`google_cloudfunctions2_function` does nothing at all.

After this milestone all three clouds run code, and the three models can
be compared side by side — which is most of why this project exists.

## Why this one is different from the other two

Lambda and Azure Functions both lean on a vendor image. **Google
publishes no functions host image**, and the roadmap has said so for
several milestones. What it also says, and what makes this tractable:

> Gen 2 IS Cloud Run underneath and the runtime contract is the Functions
> Framework — an ordinary library the user's code imports, whose
> `functions-framework` command is just an HTTP server, so the `run`
> engine's contract already fits.

So the three clouds turn out to have three completely different answers
to one problem, and that contrast is the lesson:

| | how the handler is run |
|---|---|
| **Lambda** | the base image ships the Runtime Interface Emulator; mount the code |
| **Azure Functions** | a .NET host negotiates with a language worker over gRPC; needs an image BUILD |
| **Cloud Functions** | a LIBRARY the user imports, served by an ordinary HTTP server |

**It also works on Apple Silicon**, which Azure Functions does not:
`python:3.11-slim`, `python:3.12-slim` and `node:20-slim` all publish
arm64. Verified against the registry in Milestone 21.

## The open question, which the owner decides

`docs/prompts/m20-functions.md` stated it and deferred it:

- **Build an image** from a plain `python:3.11-slim` with the Functions
  Framework vendored. Closest to `gcloud functions deploy --source`,
  which is what people actually do. Needs the vendoring step Milestone 20
  established for `azure-functions`, and the `Build` verb that milestone
  added to `internal/engines/docker`.
- **Require a container image** and admit it is Cloud Run under another
  name. Far cheaper, arguably honest about Gen 2 — but it makes
  `google_cloudfunctions2_function` a thin alias and teaches less.

Ask before building. The first is recommended; the second is the
documented fallback if deploying from source turns out to need the
network, which would break the offline rule.

## Measure first

- **What does the google provider actually send** for
  `google_cloudfunctions2_function`? It has a `build_config`, a
  `service_config` and an `event_trigger`, and the build config names a
  source bucket object. Find out whether the provider expects a BUILD to
  happen server-side and what it polls while it waits.
- **Gen 2 creates a Cloud Run service underneath.** Does the provider read
  that service back? If it does, the two resources have to agree, and
  that is the parent/child mirror this project has hit four times
  (AKS node pools, Azure subnets, GCP subnetworks, the GKE node count).
- **Which operation does it poll?** Milestone 10's finding was that GKE
  and Cloud Run poll OPPOSITE things, and getting it backwards is
  expensive. Measure before choosing.
- **What does `gcloud functions deploy` send**, and is it the same API?
  `gcloud run` does not speak the Cloud Run Admin API at all — it uses
  Knative. Assume nothing about `gcloud functions` until measured.
- **Does the Functions Framework need the network at import time?** If it
  does, vendoring has to cover it, exactly as `azure-functions` did.

## Build

1. Whatever the owner chose above, behind `internal/engines/run` — which
   already pulls, publishes, waits for a real HTTP answer and proxies
   through, and already serves two callers unchanged.
2. `google_cloudfunctions2_function` applying, planning clean, surviving a
   restart and destroying with no container and no built image left —
   the same bar Milestone 20 met.
3. An example under `examples/terraform/gcp-functions`, with a handler
   that does something observable, and CI driving it.
4. A badge for the type in `types-gcp.js`, or
   `TestEverySpecTypeHasABadge`'s GCP equivalent will say so.

## Done criteria

- [ ] `google_cloudfunctions2_function` applies and `curl` of its URL
      returns what the handler returned.
- [ ] The plan afterwards is clean, it survives a restart, and the
      destroy leaves no container and no built image.
- [ ] It works on arm64 and amd64 — stated, not assumed, since this is
      the milestone picked partly because it can.
- [ ] `docs/ROADMAP.md` loses "GCP still has no Cloud Functions".
- [ ] `docs/CROSS-CLOUD.md` gains Cloud Functions as a vantage point, or
      says why not.
- [ ] CLAUDE.md records how the three function models differ, since that
      comparison is the milestone's real output.
- [ ] Full suite and CI green on all three legs.

## What NOT to do

- Do not reach the network at deploy time. Offline is a ground rule, and
  a source deploy that needs a builder is exactly where it would break —
  if it cannot be done offline, take the container-image option and SAY
  so rather than quietly adding a fetch.
- Do not add a fourth engine. If `run` does not fit, that is a finding
  worth writing down, not a reason to copy it.
