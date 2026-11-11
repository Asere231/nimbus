# Milestone 15: finishing Azure — code that runs, and logs that hold logs

## Goal

Azure is the cloud this project started with and the only one that cannot
run your code. AWS runs a Lambda in AWS's own base image; GCP runs a Cloud
Run service in any image the daemon can pull; Azure runs a Kubernetes
cluster and a VM and nothing in between. And whatever any of them prints,
Azure has nowhere to keep it: `internal/azure/loganalytics.go` is 83 lines
that accept a workspace and answer `sharedKeys`.

This milestone closes both, and it should be the cheapest headline feature
in the project so far, because neither needs a new engine:

- `internal/engines/run` already pulls an image, publishes a port, waits
  for it to answer and proxies any method and path through to it. It is
  cloud-neutral, it is behind Cloud Run, and Azure does not use it.
- `microsoft.app/containerapps` is **already in the spec table**
  (`spectypes_gen.go`, 55 types), so it round-trips today with
  `provisioningState: Succeeded` and nothing behind it. The shape is there;
  what is missing is the hook.

## Build

1. **Azure Container Apps that really run**, on `internal/engines/run`.
   A container per app from `properties.template.containers[].image`, the
   environment from `env`, and an FQDN that reaches it. Decide and state
   what a Container App *Environment* is here — real Container Apps need
   one and it has no running counterpart, so it is probably a modelled
   parent that the app's `ParentID` points at.
2. **Ingress, and the URL azurerm reads back.** `properties.configuration.
   ingress.fqdn` is what the provider puts in state and what a person
   curls. It has to be filled by the CREATE and never change between
   reads — the `endpoint` lesson from GKE, which the roadmap already
   records as "a derived value also has to exist by the time the client
   first reads it".
3. **Log Analytics that ingests and answers.** A workspace that holds log
   lines, a container app's stdout landing in it the way a Lambda's lands
   in `/aws/lambda/<name>`, and enough of the query surface for `az monitor
   log-analytics query` to return rows. KQL is a language, not a filter:
   say plainly which subset is supported and **refuse the rest rather than
   matching everything** — the `FilterExpression` rule, which this project
   has now paid for twice.
4. **The visualizer.** A badge and colour in `types-azure.js`
   (`TestEverySpecTypeHasABadge` will fail until there is one), the app
   drawn inside its environment, and its log lines reachable from the
   inspector the way a container's blobs and a vault's secrets are.

## Measure first

- **What does azurerm actually send for `azurerm_container_app`?** Capture
  it with `TF_LOG=DEBUG`. The resource has a large body and the provider
  reads a lot of it back; the questions that matter are which fields it
  re-reads (those must be derived, not generated) and whether the create is
  long-running. Compare against Cloud Run, which polls the OPERATION, and
  GKE, which polls the RESOURCE — getting that backwards cost Milestone 10
  real time, and the roadmap says so.
- **What does `az monitor log-analytics query` put on the wire?** It is a
  different host from ARM in real Azure (`api.loganalytics.io`), so find
  out whether the CLI reads the endpoint from the metadata document the way
  `az keyvault` reads `vaultUri` — if it does, the suffix has to carry our
  port, as `storage` and `keyVaultDns` already do.
- **Does anything poll the app's URL before reporting success?** Cloud Run
  does, which is why its operation must stay unfinished until the container
  answers.

## Ask the owner

- **Container Apps or Functions.** Container Apps is the better fit — it
  takes any image, which is what `run` does, and it is already in the spec
  table. Azure Functions would need a runtime per language the way Lambda
  does, and Lambda only works because AWS publishes base images with the
  runtime interface emulator in them; there is no Azure equivalent to lean
  on. Recommend Container Apps and say what Functions would cost.
- **How much KQL.** A tiny subset honestly stated (`|` with `where`,
  `project`, `take`, `count`) and a refusal for everything else, or no
  query language at all and a REST listing only. The subset is probably
  right, but it is a decision about what a half-implemented query language
  teaches.
- **Where a container app's logs go when Log Analytics is not configured.**
  Lambda creates its log group implicitly; CloudWatch refuses writes to a
  group nobody made. Pick one and be consistent with whichever reasoning
  this project already recorded.

## Done when

- [ ] `examples/terraform/azure-containerapp`: an image pulled, a container
      running, and `curl` against the FQDN from the output returning what
      the app serves. Applies, plans clean, destroys, leaves no containers.
- [ ] That app's stdout is readable through Log Analytics, with `az monitor
      log-analytics query` (or the documented substitute) returning rows.
- [ ] An unsupported KQL construct is REFUSED and named, not matched.
- [ ] The container app appears in the visualizer with its own badge,
      inside its environment, and its logs are reachable from the
      inspector.
- [ ] `--enforce-iam` governs the new types: a principal without
      `Microsoft.App/containerApps/write` cannot create one.
- [ ] README's Azure tier table lists Container Apps under "Real", and
      `docs/ROADMAP.md` loses "Azure cannot run your code" as a gap.
- [ ] Full test suite and CI green on macOS, Windows and Linux, with the
      new Docker-backed job CHECKED on its first run rather than assumed —
      a job added in a close-out has been red for three milestones before.
