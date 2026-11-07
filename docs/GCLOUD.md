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
```

The trailing slash matters: `gcloud` joins paths onto these. Some services
carry their version in the override and some do not — `compute` and
`storage` do, `iam` does not — which is the same asymmetry the Terraform
provider's `*_custom_endpoint` settings have.

Each override prints a warning about the **universe domain** not being
`googleapis.com`. That is expected and harmless: it is a newer gcloud
concept for alternative Google universes, and everything below works
through it. `--universe-domain` is not the answer for a local emulator.

Switch between Nimbus and the real thing with:

```bash
gcloud config configurations activate nimbus     # or: default
```

## Use it

All of these were run against Nimbus 0.9.0 with gcloud 592.0.0 on macOS:

```bash
gcloud compute networks list
gcloud compute networks subnets list --regions us-central1
gcloud compute firewall-rules list
gcloud iam service-accounts list
gcloud storage ls
gcloud storage cp README.md gs://nimbus-data/
gcloud storage cat gs://nimbus-data/README.md
```

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
- **Anything Nimbus does not emulate.** The activity feed at
  <http://localhost:4700> flags every 4xx with the path that caused it, so
  an unimplemented call shows up there as the next thing to build rather
  than as a mystery.
