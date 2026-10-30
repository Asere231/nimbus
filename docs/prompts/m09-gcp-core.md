# Milestone 9: GCP core — projects, networks, Cloud Storage

## Goal

Add Google Cloud on port 4703 so the Terraform `google` provider and `gcloud` work against Nimbus for projects, VPC networking, service accounts and Cloud Storage.

## Background: how GCP differs

- **REST-JSON everywhere**, one API per service under paths like `/compute/v1/projects/{p}/global/networks/{n}`. Resources are addressed by **self links** (full URLs); the store uses the self link as `Resource.ID`.
- **Long-running operations** are explicit `Operation` resources (`/compute/v1/projects/{p}/global/operations/{op}`) that clients poll until `status: DONE`. Same idea as Azure async operations, different shape.
- **Auth**: OAuth2 bearer tokens. The provider can take an `access_token` directly, which avoids emulating Google's token exchange at first.
- **Clients**: the google provider has a `*_custom_endpoint` setting per service (`compute_custom_endpoint`, `storage_custom_endpoint`, …); `gcloud` uses `gcloud config set api_endpoint_overrides/<service>`.

## Build

- **Resource Manager**: projects (get, create, list), and the calls the provider makes on startup.
- **Compute networking**: networks (custom-mode and auto-mode), subnetworks with secondary ranges, firewalls, routes, routers + Cloud NAT (state only), global and regional operations, `regions` and `zones` lists.
- **IAM**: service accounts and keys, project IAM policy get/set (no enforcement).
- **Cloud Storage** (JSON API): buckets and objects, simple and resumable uploads, versioning, labels. Reuse the storage backend patterns from Milestone 2 and the S3 work.
- **Visualizer**: GCP tab; project → networks → subnetworks/firewalls.

## Done when

- [ ] `examples/terraform/gcp-basic`: custom VPC, 2 subnets, firewall rule, service account, bucket. Apply, clean plan, destroy.
- [ ] `gcloud compute networks list` and `gcloud storage cp` work with endpoint overrides (document the exact commands in `docs/`).
- [ ] Azure and AWS examples still pass.

## Watch out for

- The google provider sends PATCH with field masks (`updateMask`); apply only listed fields.
- Many GCP fields are returned as strings even when numeric (e.g. `"id": "1234"`); match the real shapes or plans won't settle.
