# Milestone 3: AKS on k3s

## Goal

`azurerm_kubernetes_cluster` starts a real Kubernetes cluster (k3s in Docker), Terraform's `kube_config` output works with `kubectl`, and the visualizer shows nodes, deployments, pods and services under the cluster, live.

## Ask the owner first

- Which container runtime I use on each machine (Docker Desktop, Podman, Rancher Desktop, OrbStack). Verify with `docker version` output I paste.
- Default k3s image tag to pin (recommend a current stable `rancher/k3s:v1.xx.y-k3s1`, pre-pullable for offline use).
- On `nimbus stop`: tear clusters down, or leave them running for fast restarts? (Recommend: leave running and reattach on start when `--persist` is set; otherwise tear down.)

## Design

- **Talk to Docker through the `docker` CLI** (`exec.Command`), not the Engine API: it works the same on macOS, Windows (named pipe) and Linux without third-party Go modules. Wrap it in `internal/engines/docker` with a fake for tests.
- **One k3s container per cluster**: `docker run -d --privileged --name nimbus-aks-<id> -p 127.0.0.1:<port>:6443 rancher/k3s:<tag> server --tls-san 127.0.0.1 --disable traefik`. Ports from a configurable range starting at 6443. Agent pools: extra `k3s agent` containers on a Docker network, so `node_count` is real.
- **Provisioning**: `PUT managedClusters` returns quickly with `provisioningState: Creating` and an async operation that reports `InProgress` until k3s is ready (`/readyz` via the mapped port), then `Succeeded`. This is the first real long-running operation: make the operations store keep per-operation state.
- **Credentials**: `POST listClusterUserCredential` / `listClusterAdminCredential` / `listClusterMonitoringUserCredential` return `{"kubeconfigs":[{"name":"clusterUser","value":"<base64>"}]}`. Read `/etc/rancher/k3s/k3s.yaml` from the container, rewrite the server to `https://127.0.0.1:<port>`, keep its CA.
- **Server fields azurerm reads**: `fqdn`, `kubernetesVersion`, `currentKubernetesVersion`, `nodeResourceGroup` (create that RG too, like Azure does), `agentPoolProfiles` with counts and VM sizes echoed back, `identity` with principal and tenant IDs, `oidcIssuerProfile` if requested.
- **Child resources**: `.../agentPools/{name}` CRUD maps to scaling agent containers.
- **Visualizer**: for each running cluster, poll its API (nodes, namespaces, deployments, pods, services) with the cluster's credentials and draw them as children of the AKS node, with pod status colours. Keep polling in Go and push changes through the existing event bus, so the browser stays simple.

## Done when

- [ ] `examples/terraform/azure-aks`: RG + VNet + subnet + AKS (1 node pool, `node_count = 2`) + `local_file` with the kubeconfig. Apply succeeds; `kubectl --kubeconfig ... get nodes` shows 2 Ready nodes; plan is clean; destroy removes the containers.
- [ ] `kubectl apply` of an nginx Deployment and Service: pods appear in the visualizer within a few seconds and disappear on delete.
- [ ] Scaling `node_count` to 3 in Terraform adds a node.
- [ ] Docker missing or stopped → a clear error in the ARM response and the activity feed, not a hang.
- [ ] Tests use the fake Docker; one opt-in integration test (`NIMBUS_DOCKER_TESTS=1`) runs real k3s.

## Watch out for

- k3s needs `--privileged`. On macOS/Windows it runs inside Docker's VM, which is fine.
- Terraform calls the credential actions on every refresh: keep them fast and stable (same kubeconfig each time).
- `docs/OFFLINE.md` needs the exact image tag to pre-pull.
