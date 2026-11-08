package gcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/k3s"
)

// GKE: a real Kubernetes cluster behind the Google API.
//
// The cluster is the shared k3s engine, the same one AKS and EKS use --
// `kubectl get nodes` talks to a real API server in a container. What is
// GCP-shaped is only the surface. And because the engine stamps every
// mirrored pod with the cloud it belongs to, the visualizer's Kubernetes
// view draws a GKE cluster with no new drawing code at all: that was the
// point of lifting the engine out in Milestone 8.
//
// Four things about this API were measured against the provider 6.50.0 and
// are not what the rest of GCP does:
//
//   - **The request body is WRAPPED**: `{"cluster": {...}}` on create,
//     `{"nodePool": {...}}` for a pool. Nothing else in GCP wraps, and a
//     handler that reads the body directly finds an empty object and
//     creates a cluster with no name.
//   - **It is a different generated client.** The user agent says
//     `google-api-go-client/0.5` and every request carries
//     `prettyPrint=false`, where the rest of the provider sends neither.
//     That is why its endpoint behaves differently too; see router.go.
//   - **The provider ignores the operation and polls the CLUSTER.** After
//     a create it GETs the cluster until `status` is RUNNING. Measured:
//     four PROVISIONING reads in a row and it waited for all of them. So
//     this works exactly like EKS -- answer at once, finish in the
//     background, flip the status.
//   - **`location` holds a ZONE** for a zonal cluster:
//     `/locations/us-central1-a/clusters`. The legacy `/zones/{z}/`
//     spelling still exists and is accepted here, because accepting it
//     costs one line.

const (
	TypeGKECluster  = "container.googleapis.com/Cluster"
	TypeGKENodePool = "container.googleapis.com/NodePool"
)

// gkeDefaultNodeCount is what a cluster has when a client says nothing.
const gkeDefaultNodeCount = 1

// clusterLink is a GKE cluster's self link, which is its id in the store.
func (s *Server) clusterLink(location, name string) string {
	return s.base() + "/v1/projects/" + s.cfg.ProjectID + "/locations/" + location + "/clusters/" + name
}

func (s *Server) nodePoolLink(location, cluster, name string) string {
	return s.clusterLink(location, cluster) + "/nodePools/" + name
}

// serveGKE routes the container API.
//
// Paths are /v1/projects/{p}/{locations|zones}/{loc}/... and the collection
// after that is `clusters` or `operations`.
func (s *Server) serveGKE(w http.ResponseWriter, r *http.Request, parts []string) string {
	// parts: v1 projects {p} locations {loc} ...
	if len(parts) < 6 {
		writeError(w, notFound("Nimbus does not emulate %s %s yet", r.Method, r.URL.Path))
		return "gke: unrecognised path"
	}
	location := parts[4]
	rest := parts[5:]

	switch rest[0] {
	case "operations":
		if len(rest) == 2 && r.Method == http.MethodGet {
			// GKE's operation is Compute's shape, not the longrunning one,
			// so it comes out of the same registry.
			return s.getOperation(w, s.gkeOperationLink(location, rest[1]))
		}
	case "clusters":
		return s.serveGKEClusters(w, r, location, rest[1:])
	}
	writeError(w, notFound("Nimbus does not emulate %s %s yet", r.Method, r.URL.Path))
	return "gke: unrecognised path"
}

// serveGKEClusters handles everything under .../clusters.
func (s *Server) serveGKEClusters(w http.ResponseWriter, r *http.Request, location string, rest []string) string {
	var e *apiError
	switch {
	case len(rest) == 0 && r.Method == http.MethodPost:
		e = s.createCluster(w, r, location)
	case len(rest) == 0 && r.Method == http.MethodGet:
		e = s.listClusters(w, location)

	case len(rest) == 1 && r.Method == http.MethodGet:
		e = s.getCluster(w, location, rest[0])
	case len(rest) == 1 && r.Method == http.MethodDelete:
		e = s.deleteCluster(w, location, rest[0])
	case len(rest) == 1 && (r.Method == http.MethodPut || r.Method == http.MethodPatch):
		e = s.updateCluster(w, r, location, rest[0])

	default:
		name, verb := trimVerb(strings.Join(rest, "/"))
		if np := nodePoolPath(rest); np != nil {
			return s.serveNodePools(w, r, location, np.cluster, np.rest)
		}
		e = notFound("Nimbus does not emulate %s on %s yet", verb, name)
	}
	if e != nil {
		writeError(w, e)
		return "gke " + r.Method + ": " + e.Code
	}
	return ""
}

type nodePoolRef struct {
	cluster string
	rest    []string
}

// nodePoolPath recognises .../clusters/{c}/nodePools[/{np}[/{verb}]].
func nodePoolPath(rest []string) *nodePoolRef {
	if len(rest) >= 2 && rest[1] == "nodePools" {
		return &nodePoolRef{cluster: rest[0], rest: rest[2:]}
	}
	return nil
}

// --- clusters -------------------------------------------------------------

func (s *Server) loadCluster(location, name string) (map[string]any, *apiError) {
	res, err := s.store.Get(s.clusterLink(location, name))
	if err != nil || res.Type != TypeGKECluster {
		return nil, notFound("Not found: projects/%s/locations/%s/clusters/%s",
			s.cfg.ProjectID, location, name)
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body, &body); err != nil {
		return nil, internalError(err)
	}
	return body, nil
}

func (s *Server) saveCluster(location string, body map[string]any) *apiError {
	name := str(body["name"])
	raw, err := json.Marshal(body)
	if err != nil {
		return internalError(err)
	}
	s.store.Put(&core.Resource{
		Cloud: "gcp", ID: s.clusterLink(location, name), Type: TypeGKECluster,
		Name: name, ParentID: s.clusterParent(body), Location: location,
		Backing: "real", Body: raw,
	})
	return nil
}

// clusterParent hangs a cluster off its network when that network is a
// resource here, and off the project otherwise. A cluster on the implicit
// `default` network -- which the provider fills in when a config says
// nothing -- would otherwise float under a network that does not exist.
func (s *Server) clusterParent(body map[string]any) string {
	if ref := str(body["network"]); ref != "" {
		if link := s.linkFor(ref); link != "" && s.store.Exists(link) {
			return link
		}
	}
	return s.projectSelfLink()
}

func (s *Server) createCluster(w http.ResponseWriter, r *http.Request, location string) *apiError {
	// The body is wrapped. Measured, and unlike anything else in GCP.
	var req struct {
		Cluster map[string]any `json:"cluster"`
	}
	if err := readJSON(r, &req); err != nil {
		return badRequest("%v", err)
	}
	body := req.Cluster
	if body == nil {
		return badRequest("the request must carry a `cluster` object")
	}
	name := str(body["name"])
	if name == "" {
		return badRequest("cluster.name is required")
	}
	if s.store.Exists(s.clusterLink(location, name)) {
		return alreadyExists("cluster "+name, s.cfg.ProjectID)
	}

	s.fillCluster(body, location, name)

	// The endpoint is set NOW, before the container exists.
	//
	// Measured: with it filled in only when the cluster became RUNNING,
	// `endpoint` was empty in Terraform state after a successful apply and
	// the next plan reported it as having "changed outside of Terraform".
	// The provider writes state from a read that happens while the cluster
	// is still PROVISIONING, and never reads it again.
	//
	// Nothing is being faked: reserving the cluster allocates its host
	// port, so the address is genuinely known -- the container will answer
	// there. This is the same move as deriving an etag or a kubeconfig
	// instead of generating one, pulled earlier in the lifecycle.
	cl, err := s.k3s.NewCluster(s.clusterLink(location, name), name)
	if err != nil {
		return internalError(err)
	}
	body["endpoint"] = fmt.Sprintf("127.0.0.1:%d", cl.Port)

	// PROVISIONING, because the container really is not up yet. This is
	// the field the provider polls -- not the operation.
	body["status"] = "PROVISIONING"
	body["statusMessage"] = "Nimbus is starting a k3s container for this cluster"
	if aerr := s.saveCluster(location, body); aerr != nil {
		return aerr
	}
	// The default pool has to be a REAL resource, not only an entry in the
	// cluster's array.
	//
	// Measured: `remove_default_node_pool = true` -- which almost every
	// real GKE config sets, so that a managed node pool owns the nodes --
	// makes the provider DELETE `default-pool` immediately after the
	// create, and the apply fails on
	//
	//	Error deleting default node pool: googleapi: Error 404:
	//	Not found: .../clusters/nimbus-gke/nodePools/default-pool
	//
	// A mirrored array entry is not something that can be deleted. Real
	// GKE's default pool is an ordinary node pool, and so is this one.
	if aerr := s.saveInitialPools(location, name, body); aerr != nil {
		return aerr
	}

	go s.provisionCluster(location, name)

	op := s.newGKEOperation(location, "CREATE_CLUSTER", s.clusterLink(location, name))
	writeJSON(w, http.StatusOK, op)
	return nil
}

// fillCluster adds everything the provider reads back. Every value here is
// DERIVED from the cluster's identity rather than generated: the rule that
// has cost this project a day per cloud is that anything a client re-reads
// must be the same on the second read, or the plan is never clean.
func (s *Server) fillCluster(body map[string]any, location, name string) {
	link := s.clusterLink(location, name)
	body["selfLink"] = link
	body["id"] = hexID(32)
	body["location"] = location
	body["zone"] = location
	body["locations"] = []any{location}
	body["currentMasterVersion"] = s.k3s.Version()
	body["currentNodeVersion"] = s.k3s.Version()
	if str(body["initialClusterVersion"]) == "" {
		body["initialClusterVersion"] = s.k3s.Version()
	}
	body["createTime"] = timestamp()
	body["servicesIpv4Cidr"] = "10.43.0.0/16"
	body["clusterIpv4Cidr"] = "10.42.0.0/16"
	body["labelFingerprint"] = etagOf(link)
	body["nodePools"] = s.initialNodePools(body, location, name)
	body["currentNodeCount"] = totalNodes(body)
	if str(body["network"]) == "" {
		body["network"] = "default"
	}
	// The network has to come back where the provider LOOKS for it, which
	// is `networkConfig`, not the top-level field the create sent it in.
	//
	// Measured: with only the top-level `network` and `subnetwork` filled,
	// a plan after a successful apply wanted to REPLACE the cluster --
	// both fields are ForceNew, and the provider read them as null. Real
	// GKE answers in both places. This is the same shape as every other
	// perpetual diff in this project: the value is right, and it is not
	// where the client reads it.
	netCfg, _ := body["networkConfig"].(map[string]any)
	if netCfg == nil {
		netCfg = map[string]any{}
	}
	netCfg["network"] = relativeName(str(body["network"]))
	if sub := str(body["subnetwork"]); sub != "" {
		netCfg["subnetwork"] = relativeName(sub)
	}
	body["networkConfig"] = netCfg
	if str(body["loggingService"]) == "" {
		body["loggingService"] = "none"
	}
	if str(body["monitoringService"]) == "" {
		body["monitoringService"] = "none"
	}
}

// initialNodePools gives a cluster the default pool its initialNodeCount
// asks for, which is what `google_container_cluster` with
// `initial_node_count` expects to read back.
func (s *Server) initialNodePools(body map[string]any, location, name string) []any {
	if pools, ok := body["nodePools"].([]any); ok && len(pools) > 0 {
		return pools
	}
	count := gkeDefaultNodeCount
	if n := asInt(body["initialNodeCount"]); n > 0 {
		count = n
	}
	pool := map[string]any{
		"name":             "default-pool",
		"initialNodeCount": count,
		"nodeCount":        count,
		"status":           "PROVISIONING",
		"version":          s.k3s.Version(),
		"selfLink":         s.nodePoolLink(location, name, "default-pool"),
		"config":           orDefault(body["nodeConfig"], map[string]any{}),
		// Without these the provider computes a node count of zero; see
		// instanceGroupURLs in gke_nodepool.go.
		"instanceGroupUrls": s.instanceGroupURLs(location, name, "default-pool"),
	}
	return []any{pool}
}

// saveInitialPools stores the pools a create implied as real resources,
// so each one can be read, scaled and deleted like any other.
func (s *Server) saveInitialPools(location, cluster string, body map[string]any) *apiError {
	pools, _ := body["nodePools"].([]any)
	for _, p := range pools {
		pool, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if aerr := s.saveNodePool(location, cluster, pool); aerr != nil {
			return aerr
		}
	}
	return nil
}

// provisionCluster starts the container and moves the cluster to RUNNING,
// or to ERROR with the reason attached. A cluster stuck in PROVISIONING
// with no explanation is the worst thing a local emulator can do to
// someone's afternoon.
func (s *Server) provisionCluster(location, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), backgroundTimeout)
	defer cancel()
	id := s.clusterLink(location, name)

	fail := func(reason string) {
		_ = s.store.Update(id, func(body map[string]any) error {
			body["status"] = "ERROR"
			body["statusMessage"] = reason
			return nil
		})
	}

	// Already reserved by createCluster, which needed the port to report
	// the endpoint; this returns the same cluster.
	s.gkeMu.Lock()
	defer s.gkeMu.Unlock()

	cl, err := s.k3s.NewCluster(id, name)
	if err != nil {
		fail(err.Error())
		return
	}
	if err := s.k3s.Start(ctx, cl); err != nil {
		s.k3s.Remove(id)
		fail(err.Error())
		return
	}
	// Any pool the create asked for beyond the first node.
	if want := s.wantedNodes(id); want > 1 {
		if err := s.k3s.ScaleAgents(ctx, cl, want); err != nil {
			fail(err.Error())
			return
		}
	}

	// Store.Update rather than read-change-write: a node pool's own create
	// may be writing this same body, and the loser of that race silently
	// restores the other's copy. Measured on AKS, and the reason the rule
	// exists.
	if err := s.store.Update(id, func(body map[string]any) error {
		body["status"] = "RUNNING"
		delete(body, "statusMessage")
		// The endpoint is already there from the create; only confirm it,
		// in case the port was adopted from a container that outlived us.
		body["endpoint"] = fmt.Sprintf("127.0.0.1:%d", cl.Port)
		body["masterAuth"] = map[string]any{
			"clusterCaCertificate": base64.StdEncoding.EncodeToString(cl.CA),
		}
		markPoolsRunning(body)
		return nil
	}); err != nil {
		// The cluster was deleted while it was starting.
		s.k3s.Stop(ctx, cl)
	}
}

// wantedNodes is how many nodes a cluster's pools add up to.
func (s *Server) wantedNodes(id string) int {
	res, err := s.store.Get(id)
	if err != nil {
		return gkeDefaultNodeCount
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body, &body); err != nil {
		return gkeDefaultNodeCount
	}
	return totalNodes(body)
}

func totalNodes(body map[string]any) int {
	total := 0
	pools, _ := body["nodePools"].([]any)
	for _, p := range pools {
		pool, ok := p.(map[string]any)
		if !ok {
			continue
		}
		total += poolCount(pool)
	}
	if total == 0 {
		total = asInt(body["initialNodeCount"])
	}
	if total < 1 {
		total = gkeDefaultNodeCount
	}
	return total
}

// poolCount reads a pool's node count from whichever field carries it.
// `nodeCount` is what a client sets; `initialNodeCount` is what a create
// sends and what GKE keeps reporting afterwards.
//
// It accepts an int as well as a float64, which is not pedantry: a number
// DECODED from JSON is a float64, and the same number just assigned in Go
// is an int. Reading only float64 made a count written moments earlier in
// this process invisible, while the identical value read back from the
// store worked -- so `gcloud container clusters list` reported one node
// for a three-node cluster and every test that round-tripped through the
// store passed.
func poolCount(pool map[string]any) int {
	for _, field := range []string{"nodeCount", "initialNodeCount"} {
		if n := asInt(pool[field]); n > 0 {
			return n
		}
	}
	return 0
}

// asInt reads a number however it arrived.
func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		return atoiOr(n, 0)
	}
	return 0
}

func markPoolsRunning(body map[string]any) {
	pools, _ := body["nodePools"].([]any)
	for _, p := range pools {
		if pool, ok := p.(map[string]any); ok {
			pool["status"] = "RUNNING"
		}
	}
}

func (s *Server) getCluster(w http.ResponseWriter, location, name string) *apiError {
	body, aerr := s.loadCluster(location, name)
	if aerr != nil {
		return aerr
	}
	writeJSON(w, http.StatusOK, body)
	return nil
}

func (s *Server) listClusters(w http.ResponseWriter, location string) *apiError {
	out := []any{}
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return r.Type == TypeGKECluster && (location == "-" || r.Location == location)
	}) {
		var body map[string]any
		if err := json.Unmarshal(res.Body, &body); err == nil {
			out = append(out, body)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"clusters": out})
	return nil
}

// updateCluster applies whatever a client sent. GKE's update is a wrapped
// `{"update": {...}}` of individual fields rather than a whole object, so
// only the ones Nimbus models are applied and the rest are stored as sent.
func (s *Server) updateCluster(w http.ResponseWriter, r *http.Request, location, name string) *apiError {
	var req struct {
		Update  map[string]any `json:"update"`
		Cluster map[string]any `json:"cluster"`
	}
	if err := readJSON(r, &req); err != nil {
		return badRequest("%v", err)
	}
	patch := req.Update
	if patch == nil {
		patch = req.Cluster
	}
	id := s.clusterLink(location, name)
	if err := s.store.Update(id, func(body map[string]any) error {
		for k, v := range patch {
			body[k] = v
		}
		return nil
	}); err != nil {
		return notFound("Not found: projects/%s/locations/%s/clusters/%s",
			s.cfg.ProjectID, location, name)
	}
	writeJSON(w, http.StatusOK, s.newGKEOperation(location, "UPDATE_CLUSTER", id))
	return nil
}

func (s *Server) deleteCluster(w http.ResponseWriter, location, name string) *apiError {
	id := s.clusterLink(location, name)
	if _, aerr := s.loadCluster(location, name); aerr != nil {
		return aerr
	}
	// The container goes in the background; the API answers now. The
	// provider GETs the cluster afterwards and reads a 404 as "gone",
	// which is what the delete below produces.
	_ = s.store.Update(id, func(body map[string]any) error {
		body["status"] = "STOPPING"
		return nil
	})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), backgroundTimeout)
		defer cancel()
		// Under gkeMu, so a node pool's rescale cannot start an agent
		// behind the stop; see rescale in gke_nodepool.go.
		s.gkeMu.Lock()
		defer s.gkeMu.Unlock()
		cl := s.k3s.Remove(id)
		if cl == nil {
			cl = s.k3s.ClusterFor(id, name)
		}
		s.k3s.Stop(ctx, cl)
		_, _ = s.store.Delete(id)
	}()

	writeJSON(w, http.StatusOK, s.newGKEOperation(location, "DELETE_CLUSTER", id))
	return nil
}

// --- operations -----------------------------------------------------------

func (s *Server) gkeOperationLink(location, name string) string {
	return s.base() + "/v1/projects/" + s.cfg.ProjectID + "/locations/" + location + "/operations/" + name
}

// newGKEOperation builds the container API's operation, which is Compute's
// shape rather than the longrunning one -- `status: "DONE"`, not
// `done: true`. Nothing polls it (the provider polls the cluster), but
// gcloud does, and an operation it cannot find must 404 rather than be
// invented.
func (s *Server) newGKEOperation(location, opType, targetLink string) *operation {
	name := "operation-" + hexID(16)
	op := &operation{
		Name:          name,
		OperationType: opType,
		TargetLink:    targetLink,
		Status:        "DONE",
		// No `progress`: see operations.go. The container API's is an
		// object where Compute's is a number.
		InsertTime: timestamp(),
		StartTime:  timestamp(),
		EndTime:    timestamp(),
		SelfLink:   s.gkeOperationLink(location, name),
		Zone:       location,
	}
	s.operations.put(op)
	return op
}

// setGKEStatus records what reattaching a cluster found, after a restart.
// A container that is gone or unreachable means an ERROR cluster: the
// resource is what the provider reads, so it has to say so rather than
// keep claiming RUNNING.
func (s *Server) setGKEStatus(found k3s.Found) {
	_ = s.store.Update(found.Cluster.ResourceID, func(body map[string]any) error {
		if !found.Ready {
			body["status"] = "ERROR"
			body["statusMessage"] = "the cluster's container is not running"
			return nil
		}
		body["status"] = "RUNNING"
		body["endpoint"] = fmt.Sprintf("127.0.0.1:%d", found.Cluster.Port)
		body["masterAuth"] = map[string]any{
			"clusterCaCertificate": base64.StdEncoding.EncodeToString(found.Cluster.CA),
		}
		return nil
	})
}
