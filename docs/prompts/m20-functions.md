# Milestone 20 — Azure Functions, and GCP Cloud Functions

Azure and GCP are the two clouds here that cannot run a FUNCTION. AWS can,
and the reason it was cheap is worth stating before anything else: every
`public.ecr.aws/lambda/<runtime>` image ships the Runtime Interface
Emulator, the runtime interface client and the SDK inside it, so Nimbus
mounts a handler into a container that already knows how to run it.

The roadmap has said for several milestones that the other two clouds have
"no equivalent to lean on". **That was measured in this session and it is
wrong** — Microsoft publishes a Functions host image that runs offline, and
a Python function really answered through it. What is different is not
"there is no host" but "the host expects a DEPLOY, not a mount". Read the
findings below before designing anything, because two of the three cost an
afternoon to discover and the third is the one that unlocks it.

## What was measured (2026-11-18, Docker Desktop on Windows)

Against `mcr.microsoft.com/azure-functions/python:4-python3.11`:

1. **The host starts and serves with no network.** It binds `:80`, logs
   `Job host started`, and answers 404 for an unknown route. The only
   unhealthy component is `AzureWebJobsStorage` — "Unable to create client"
   — and an HTTP-triggered function does not need it. Nimbus HAS a storage
   data plane (blob, queue, table), so pointing `AzureWebJobsStorage` at
   Nimbus itself is an obvious next step and was NOT tried.

2. **The app's own SDK is not in the image.** `import azure.functions`
   fails with `ModuleNotFoundError: No module named 'azure'`. Real Azure
   installs `requirements.txt` at deploy time with a remote build, which
   needs the network. The image already sets
   `PYTHONPATH=/home/site/wwwroot/.python_packages/lib/site-packages`, so
   **vendored dependencies are the supported mechanism** and a function app
   deployed to Nimbus has to carry its own, exactly as a zip deploy with
   `--build remote` avoids having to.

3. **A BIND MOUNT does not work and a `COPY` does.** This is the finding
   that matters. With `/home/site/wwwroot` bind-mounted the host reported

       0 functions found (Custom)
       0 functions loaded
       No functions were found. This can occur before you deploy code...

   for BOTH programming models — the v2 decorator model (`function_app.py`,
   with and without `AzureWebJobsFeatureFlags=EnableWorkerIndexing`) and the
   classic v1 model (`hello/function.json` + `__init__.py`) — with nothing
   in the logs saying why. The identical tree baked in with

       FROM mcr.microsoft.com/azure-functions/python:4-python3.11
       COPY . /home/site/wwwroot

   gave

       1 functions loaded
       Found the following functions:
       Host.Functions.hello
       Mapped function route 'api/hello' [get,post] to 'hello'

   and `curl http://127.0.0.1:7072/api/hello?name=nimbus` answered
   `hello nimbus from azure functions`. The handler's own `print` appeared
   in the container console beside the host's
   `Executed 'Functions.hello' (Succeeded, ... Duration=5ms)`.

   So the console carries both the function's output and its invocation
   record, which means **Log Analytics ingestion works unchanged** — the
   tailer in `internal/azure/logstore.go` reads a container's console and
   knows nothing about what produced it.

The cause of (3) was not chased further. The mechanism does not matter as
much as the consequence: **a Functions deployment needs an image BUILD, not
a mount.** `docker build` with a local base image and a `COPY` needs no
network, so this is still fully offline — but `internal/engines/docker` has
no `Build` verb today, and that is the first piece of work.

## The shape this suggests

- `docker.Client` gains `Build(ctx, dir, tag)`. The `Fake` gains it too,
  recording the call, because no unit test may reach the real daemon.
- A new engine, or an option on `funcs`. Note it is NOT `run`: a function
  app is indexed from a deploy directory and invoked by route, and `run`'s
  contract is "this image listens on $PORT". Weigh carefully — the engines
  are shared and `run` already serves two clouds unchanged.
- `Microsoft.Web/sites` with `kind: functionapp,linux`, which is what
  `azurerm_linux_function_app` creates, plus the storage account it
  REQUIRES (azurerm will not create the app without one — Nimbus has
  storage accounts already, which is a real advantage here).
- The zip deploy path. `azurerm_function_app_flex_consumption` and
  `zip_deploy_file` push a zip; `func azure functionapp publish` uses
  `POST /api/zipdeploy` on the SCM site. Measure which one the provider
  actually sends before implementing either.
- Dependencies are the user's problem and the error must SAY so. A
  function app whose `requirements.txt` is not vendored into
  `.python_packages` will start and find its function and then fail on
  import — so Nimbus should detect the missing directory at deploy time and
  refuse with a message naming `--build` / vendoring, rather than letting
  it fail at invoke time where nothing connects the two.

## GCP Cloud Functions

Not measured. What is known from the existing code and docs:

- Gen 2 IS Cloud Run underneath, and Nimbus already runs Cloud Run.
- The runtime contract is the **Functions Framework**, an ordinary library
  the user's code imports. `functions-framework --target=hello` is just an
  HTTP server, so the `run` engine's contract fits exactly.
- Google publishes no "functions host" image to lean on. Deploying from
  source goes through buildpacks, which needs a builder and the network.
- So the honest options are (a) the same build-an-image path as Azure, with
  the framework vendored by the user, or (b) require a container image and
  admit it is Cloud Run with a different resource name. **Ask the owner
  which**, and note that (a) uses a non-Microsoft/non-Google base image
  (`python:3.11-slim`), which is the kind of dependency this project
  declined for `sshd` — the difference being that here the user supplies
  the framework, not Nimbus.

## Done criteria

- `terraform apply` of an `azurerm_linux_function_app` with one
  HTTP-triggered Python function, then a `curl` of its hostname returning
  what the handler returned.
- Its `print` output queryable through `az monitor log-analytics query`.
- A clean `terraform plan` after the apply, and again after a restart.
- `terraform destroy` leaving no container and no image behind. (A built
  image per deployment is new rubbish to collect — decide early whether it
  is tagged per resource and removed on delete.)
- CI exercises it on Linux, where the base image and `docker build` behave
  differently from Docker Desktop.

## What NOT to do

- Do not implement a function protocol. The host is the protocol, as RIE is
  for Lambda; if you find yourself parsing `function.json` to dispatch, the
  design has gone wrong.
- Do not install dependencies at deploy time. It needs the network, which
  is the one thing this project will not do.
- Do not bind-mount the code and hope. It was tried, in both programming
  models, and the host silently finds nothing.
