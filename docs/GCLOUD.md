# Using `gcloud` with Nimbus

The Terraform `google` provider takes an endpoint per service and a bearer
token, so pointing it at Nimbus is a provider block (see
`examples/terraform/gcp-basic/main.tf`). `gcloud` needs two more things,
and both are a `gcloud config set`:

- **endpoint overrides**, one per API, under `api_endpoint_overrides/`
- **credentials turned off**, because `gcloud` will otherwise try to
  authenticate against Google — which both reaches the internet and fails

Nothing here is specific to this machine; the only variable is the port.

## Set it up

Do it in a named configuration rather than in your default one, so your real
`gcloud` is untouched and one command switches back:

```bash
gcloud config configurations create nimbus
gcloud config set core/project nimbus-local
gcloud config set auth/disable_credentials true
gcloud config set api_endpoint_overrides/compute http://localhost:4703/compute/v1/
gcloud config set api_endpoint_overrides/storage http://localhost:4703/storage/v1/
gcloud config set api_endpoint_overrides/iam http://localhost:4703/
gcloud config set api_endpoint_overrides/cloudresourcemanager http://localhost:4703/
gcloud config set api_endpoint_overrides/container http://localhost:4703/
gcloud config set api_endpoint_overrides/pubsub http://localhost:4703/
gcloud config set api_endpoint_overrides/run http://run.nimbus.localhost:4703/
```

Or let Nimbus print them: `nimbus gcp register`, or `nimbus gcp register
--run` to execute them.

The trailing slash matters: `gcloud` joins paths onto these. Some services
carry their version in the override and some do not — `compute` and
`storage` do, `iam`, `container` and `pubsub` do not — and **there is no
rule**. It is not even a property of the service: `gcloud` wants the bare
origin for `pubsub`, while the Terraform provider wants
`pubsub_custom_endpoint` to end in `/v1/` for the same API. The version
belongs to the client, not to the service. Nimbus routes a path with or
without the prefix, so a wrong guess still works.

`run` is the odd one. `gcloud run` prefixes the REGION onto the HOST,
because real Cloud Run lives at `us-central1-run.googleapis.com` — so an
IP address cannot work: it would try to resolve `us-central1-127.0.0.1`.
A nested `*.localhost` name can take the prefix, and resolves to loopback
on macOS and Windows with nothing installed, which is why the override is
`run.nimbus.localhost` rather than `localhost`.

Each override prints a warning about the **universe domain** not being
`googleapis.com`. That is expected and harmless: it is a newer gcloud
concept for alternative Google universes, and everything below works
through it. `--universe-domain` is not the answer for a local emulator.

Switch between Nimbus and the real thing with:

```bash
gcloud config configurations activate nimbus     # or: default
```

## Use it

All of these were run against Nimbus 0.10.0 with gcloud 592.0.0 on macOS:

```bash
gcloud compute networks list
gcloud compute networks subnets list --regions us-central1
gcloud compute firewall-rules list
gcloud iam service-accounts list
gcloud storage ls
gcloud storage cp README.md gs://nimbus-data/
gcloud storage cat gs://nimbus-data/README.md
```

### Kubernetes (GKE)

`gcloud container clusters get-credentials` writes a kubeconfig whose user
runs `gke-gcloud-auth-plugin`, and that plugin exists to exchange a Google
OAuth credential for a Kubernetes token. With `auth/disable_credentials`
there is no credential, so it produces nothing and **nothing ever reaches
Nimbus**. By default the plugin is not even installed, and
`USE_GKE_GCLOUD_AUTH_PLUGIN=False` falls back to the in-tree `gcp` auth
provider that kubectl removed in 1.26.

There are two ways through, and the simpler one needs no gcloud at all:

```bash
# 1. Nimbus writes the kubeconfig itself. Works in CI, no plugin, no PATH.
nimbus gcp kubeconfig nimbus-gke --out kubeconfig
kubectl --kubeconfig ./kubeconfig get nodes
```

```bash
# 2. Or make gcloud's own kubeconfig work, by installing a shim.
nimbus gcp install-auth-plugin        # prints the PATH line to add
export PATH="$HOME/.nimbus/bin:$PATH"

gcloud container clusters list
gcloud container clusters get-credentials nimbus-gke --zone us-central1-a
kubectl get nodes
```

The shim is a two-line script that answers Kubernetes' ExecCredential
protocol with a token the cluster accepts. It reads `KUBERNETES_EXEC_INFO`
and only answers for clusters on this machine, handing off to a real
`gke-gcloud-auth-plugin` elsewhere on PATH for anything else — so a
machine that also talks to real GKE is unaffected.

The token itself is accepted by a **token authentication webhook**, which
is how real GKE and real EKS both do it: a bearer token is built on the
client side, and the only thing that can validate it is something the
cluster's API server asks. Nimbus says yes to every token, the same
decision every other signature here got.

### Pub/Sub and Cloud Run

```bash
gcloud pubsub topics list
gcloud pubsub topics publish nimbus-events --message '{"hello":"world"}'
gcloud pubsub subscriptions create puller --topic nimbus-events
gcloud pubsub subscriptions pull puller --auto-ack --limit 5

gcloud run services list --region us-central1
gcloud run services describe nimbus-echo --region us-central1
```

Publishing to a topic with a **push** subscription really POSTs the
message to its endpoint, so a Cloud Run service subscribed to a topic is
invoked for real — see `examples/terraform/gcp-run-pubsub`. Nimbus only
pushes to endpoints on this machine; anything else is recorded in the
activity feed and not called.

`gcloud run` is served through a read-only projection of the **Knative**
API, which is what it speaks — the Terraform provider uses the Cloud Run
Admin v2 API instead, and the two are different documents for the same
resource. Deploying through `gcloud run deploy` is not implemented; use
Terraform or the Admin API.

`gcloud storage cp` uploads as `multipart/related` with its boundary in
single quotes, which is not what RFC 2045 allows — Nimbus reads it anyway,
because the alternative is a CLI that cannot upload. See CLAUDE.md.

An object the CLI adds to a bucket Terraform manages is removed by
`terraform destroy` when the bucket has `force_destroy = true`, the same as
on real Cloud Storage.

`auth/disable_credentials` sends every request unauthenticated, which
Nimbus accepts: it never verifies a token, the same decision SigV4 and
Azure's bearer tokens got. A local emulator rejecting a signature could
only ever be reporting a clock skew.

## What does not work

- **`gcloud auth login`** and anything that needs a real identity. There is
  no OAuth flow here and nothing to log in to; `disable_credentials` is the
  substitute.
- **`gke-gcloud-auth-plugin` on its own.** It needs a Google credential, so
  the shim above (or `nimbus gcp kubeconfig`) is the way in.
- **`gcloud run deploy`.** The Knative API is served read-only.
- **Anything Nimbus does not emulate.** The activity feed at
  <http://localhost:4700> flags every 4xx with the path that caused it, so
  an unimplemented call shows up there as the next thing to build rather
  than as a mystery.
