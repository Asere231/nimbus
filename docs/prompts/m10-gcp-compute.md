# Milestone 10: GCP compute — GKE, Pub/Sub, Cloud Run

## Goal

Finish the third cloud: GKE on the shared k3s engine, Pub/Sub messaging, and Cloud Run services in Docker, so the same kinds of apps run on all three clouds locally.

## Build

1. **GKE** (`container.googleapis.com` v1): clusters and node pools mapped to k3s, operations reporting progress, `masterAuth.clusterCaCertificate` and `endpoint` filled in. `gcloud container clusters get-credentials` must produce a working kubeconfig: its auth plugin (`gke-gcloud-auth-plugin`) needs a token k3s accepts, so reuse the static-token approach from EKS.
2. **Pub/Sub** (REST): topics, subscriptions (pull and push to a local URL), publish, pull, acknowledge, ack deadlines. Compatible with the `PUBSUB_EMULATOR_HOST` convention where practical.
3. **Cloud Run** (Admin API v2): services from a container image (including images in a local registry), revisions, a local URL per service, invoke over HTTP.
4. **Cross-cloud wrap-up**: one `examples/multicloud` that creates a network and a Kubernetes cluster on each cloud; the visualizer shows all three tabs populated. Update `README.md` with a feature matrix per cloud.

## Done when

- [ ] `examples/terraform/gcp-gke`: VPC + GKE + node pool; `get-credentials` then `kubectl get nodes` works.
- [ ] `examples/terraform/gcp-run-pubsub`: a Cloud Run service subscribed to a Pub/Sub topic via push; publishing a message invokes it.
- [ ] `examples/multicloud` applies and destroys cleanly.
- [ ] Full test suite and CI green on macOS, Windows and Linux.
