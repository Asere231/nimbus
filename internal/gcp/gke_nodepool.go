package gcp

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/Asere231/nimbus/internal/core"
)

// GKE node pools, and the parent/child mirror they need.
//
// This is the third time this exact shape has come up: an Azure subnet
// inside its VNet, an AKS agent pool inside its cluster, a GCP subnetwork
// inside its network -- and now a node pool. A client writes the CHILD and
// then reads the result off the PARENT on its next refresh, so a pool that
// is not mirrored into `cluster.nodePools[]` produces an apply that
// reports success, a cluster with the wrong node count, and a plan that is
// dirty for ever.
//
// It goes through core.Store.Update for the reason CLAUDE.md gives in
// full: read-change-write looks safe and is not. The cluster's own
// background provision writes the same body, and whichever write lands
// second silently restores what the first one changed. That was measured
// on AKS -- a node pool scale lost on a loaded CI runner while passing
// fifty times in a row locally.

// serveNodePools handles .../clusters/{c}/nodePools[/{np}[:verb]].
//
// The verb is a COLON SUFFIX on the pool's own name --
// `nodePools/workers:setSize` -- not a path segment. Measured: a scale
// failed with
//
//	Nimbus does not emulate POST .../nodePools/nimbus-workers:setSize yet
//
// against a handler that expected `/setSize` as its own segment. Both
// spellings are accepted now, since accepting the other costs nothing and
// gcloud has used different ones before.
func (s *Server) serveNodePools(w http.ResponseWriter, r *http.Request, location, cluster string, rest []string) string {
	// Split a colon verb off the pool name before anything matches on
	// shape, or the name carries the verb into every lookup.
	if len(rest) >= 1 {
		if name, verb := trimVerb(rest[0]); verb != "" {
			rest = append([]string{name, verb}, rest[1:]...)
		}
	}

	var e *apiError
	switch {
	case len(rest) == 0 && r.Method == http.MethodPost:
		e = s.createNodePool(w, r, location, cluster)
	case len(rest) == 0 && r.Method == http.MethodGet:
		e = s.listNodePools(w, location, cluster)

	case len(rest) == 1 && r.Method == http.MethodGet:
		e = s.getNodePool(w, location, cluster, rest[0])
	case len(rest) == 1 && r.Method == http.MethodDelete:
		e = s.deleteNodePool(w, location, cluster, rest[0])

	case len(rest) == 2 && r.Method == http.MethodPost:
		// The scaling and update verbs are separate sub-paths rather than
		// a PATCH: .../nodePools/{np}/setSize, /setManagement, /update.
		e = s.nodePoolVerb(w, r, location, cluster, rest[0], rest[1])

	default:
		e = notFound("Nimbus does not emulate %s %s yet", r.Method, r.URL.Path)
	}
	if e != nil {
		writeError(w, e)
		return "gke nodePool " + r.Method + ": " + e.Code
	}
	return ""
}

func (s *Server) loadNodePool(location, cluster, name string) (map[string]any, *apiError) {
	res, err := s.store.Get(s.nodePoolLink(location, cluster, name))
	if err != nil || res.Type != TypeGKENodePool {
		return nil, notFound("Not found: projects/%s/locations/%s/clusters/%s/nodePools/%s",
			s.cfg.ProjectID, location, cluster, name)
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body, &body); err != nil {
		return nil, internalError(err)
	}
	return body, nil
}

func (s *Server) saveNodePool(location, cluster string, body map[string]any) *apiError {
	name := str(body["name"])
	raw, err := json.Marshal(body)
	if err != nil {
		return internalError(err)
	}
	s.store.Put(&core.Resource{
		Cloud: "gcp", ID: s.nodePoolLink(location, cluster, name), Type: TypeGKENodePool,
		Name: name, ParentID: s.clusterLink(location, cluster), Location: location,
		Backing: "real", Body: raw,
	})
	return nil
}

func (s *Server) createNodePool(w http.ResponseWriter, r *http.Request, location, cluster string) *apiError {
	// Wrapped, like the cluster's.
	var req struct {
		NodePool map[string]any `json:"nodePool"`
	}
	if err := readJSON(r, &req); err != nil {
		return badRequest("%v", err)
	}
	body := req.NodePool
	if body == nil {
		return badRequest("the request must carry a `nodePool` object")
	}
	name := str(body["name"])
	if name == "" {
		return badRequest("nodePool.name is required")
	}
	if _, aerr := s.loadCluster(location, cluster); aerr != nil {
		return aerr
	}
	if s.store.Exists(s.nodePoolLink(location, cluster, name)) {
		return alreadyExists("node pool "+name, s.cfg.ProjectID)
	}

	body["selfLink"] = s.nodePoolLink(location, cluster, name)
	body["status"] = "RUNNING"
	// A create sends `initialNodeCount`; the provider reads `nodeCount`
	// back. Filling only the one that arrived left every plan wanting to
	// change the count from 0 to what it already was -- the same rule
	// setSize already follows, which is that a client reads whichever
	// field it set.
	if n := poolCount(body); n > 0 {
		body["nodeCount"] = n
		body["initialNodeCount"] = n
	}
	body["instanceGroupUrls"] = s.instanceGroupURLs(location, cluster, name)
	if str(body["version"]) == "" {
		body["version"] = s.k3s.Version()
	}
	if body["config"] == nil {
		body["config"] = map[string]any{}
	}
	if aerr := s.saveNodePool(location, cluster, body); aerr != nil {
		return aerr
	}
	if aerr := s.mirrorPool(location, cluster, name, body); aerr != nil {
		return aerr
	}

	go s.rescale(location, cluster)

	writeJSON(w, http.StatusOK, s.newGKEOperation(location, "CREATE_NODE_POOL",
		s.nodePoolLink(location, cluster, name)))
	return nil
}

// instanceGroupURLs is why a pool needs instance groups at all, and it is
// the subtlest thing measured in this milestone.
//
// `node_count` is not READ from the API: the provider COMPUTES it as
//
//	initialNodeCount / len(instanceGroupUrls)
//
// so a pool with no instance groups reports a count of zero -- by
// division, not by absence -- and every plan after a successful apply
// wanted to change the count from 0 to what it already was. Nothing in
// the error said "instance group".
//
// The number of groups is the number of ZONES the pool spans, not the
// number of nodes, so it does not change when the pool is scaled. One
// group for the zonal clusters Nimbus runs. The URL is derived from the
// pool's identity, so it is the same on every read -- the rule that keeps
// a plan clean, applied once more.
func (s *Server) instanceGroupURLs(location, cluster, pool string) []any {
	group := "gke-" + cluster + "-" + pool + "-" + hexID(8) + "-grp"
	return []any{s.computeLink("zones/"+location, "instanceGroupManagers", group)}
}

// mirrorPool writes a pool into its cluster's nodePools array, replacing
// an entry of the same name. Through Store.Update, so it cannot lose a
// concurrent write to the same cluster.
func (s *Server) mirrorPool(location, cluster, name string, pool map[string]any) *apiError {
	err := s.store.Update(s.clusterLink(location, cluster), func(body map[string]any) error {
		pools, _ := body["nodePools"].([]any)
		out := make([]any, 0, len(pools)+1)
		for _, p := range pools {
			if existing, ok := p.(map[string]any); ok && str(existing["name"]) == name {
				continue
			}
			out = append(out, p)
		}
		if pool != nil {
			out = append(out, pool)
		}
		body["nodePools"] = out
		// `gcloud container clusters list` reads NUM_NODES from here, not
		// from the pools, so a cluster with two nodes reported one until
		// this was kept in step. The provider does not read it -- it
		// derives its count from the instance groups -- which is why this
		// only ever showed up in the CLI.
		body["currentNodeCount"] = totalNodes(body)
		return nil
	})
	if err != nil {
		return internalError(err)
	}
	return nil
}

// rescale brings the cluster's agent containers in line with what its
// pools now add up to. One node is the k3s server container itself, so a
// cluster of N nodes needs N-1 agents -- which ScaleAgents handles.
//
// It holds gkeMu for the whole operation, and so does the cluster's
// delete. Without that, a destroy LEAVES CONTAINERS RUNNING: Terraform
// deletes the node pool, whose rescale runs in the background, then
// deletes the cluster, whose stop removes the containers -- and the
// rescale, already past its check that the cluster exists, starts a fresh
// agent behind it. Measured: a clean `terraform destroy` with nothing left
// in the store and two k3s containers still up.
//
// This is the engine-level cousin of the lost update core.Store.Update
// exists for. The check and the act have to be one step.
func (s *Server) rescale(location, cluster string) {
	ctx, cancel := context.WithTimeout(context.Background(), backgroundTimeout)
	defer cancel()
	s.gkeMu.Lock()
	defer s.gkeMu.Unlock()

	id := s.clusterLink(location, cluster)
	cl := s.k3s.Get(id)
	if cl == nil {
		return // not started yet, or already gone
	}
	want := s.wantedNodes(id)
	if err := s.k3s.ScaleAgents(ctx, cl, want); err != nil {
		_ = s.store.Update(id, func(body map[string]any) error {
			body["statusMessage"] = "scaling the cluster failed: " + err.Error()
			return nil
		})
	}
}

func (s *Server) getNodePool(w http.ResponseWriter, location, cluster, name string) *apiError {
	body, aerr := s.loadNodePool(location, cluster, name)
	if aerr != nil {
		return aerr
	}
	writeJSON(w, http.StatusOK, body)
	return nil
}

func (s *Server) listNodePools(w http.ResponseWriter, location, cluster string) *apiError {
	if _, aerr := s.loadCluster(location, cluster); aerr != nil {
		return aerr
	}
	parent := s.clusterLink(location, cluster)
	out := []any{}
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return r.Type == TypeGKENodePool && r.ParentID == parent
	}) {
		var body map[string]any
		if err := json.Unmarshal(res.Body, &body); err == nil {
			out = append(out, body)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodePools": out})
	return nil
}

// nodePoolVerb handles the POST sub-paths that change a pool.
func (s *Server) nodePoolVerb(w http.ResponseWriter, r *http.Request, location, cluster, name, verb string) *apiError {
	if _, aerr := s.loadNodePool(location, cluster, name); aerr != nil {
		return aerr
	}
	var req map[string]any
	if err := readJSON(r, &req); err != nil {
		return badRequest("%v", err)
	}

	switch verb {
	case "setSize":
		count, ok := req["nodeCount"].(float64)
		if !ok {
			return badRequest("nodeCount is required")
		}
		if aerr := s.setPoolSize(location, cluster, name, int(count)); aerr != nil {
			return aerr
		}
	case "update", "setManagement", "setAutoscaling", "rollback":
		// Stored as sent. Nothing here autoscales or upgrades, so the
		// honest thing is to round-trip the setting and run nothing.
		if aerr := s.patchPool(location, cluster, name, req); aerr != nil {
			return aerr
		}
	default:
		return notFound("Nimbus does not emulate the %s verb on a node pool yet", verb)
	}

	writeJSON(w, http.StatusOK, s.newGKEOperation(location, "SET_NODE_POOL_SIZE",
		s.nodePoolLink(location, cluster, name)))
	return nil
}

func (s *Server) setPoolSize(location, cluster, name string, count int) *apiError {
	id := s.nodePoolLink(location, cluster, name)
	var updated map[string]any
	err := s.store.Update(id, func(body map[string]any) error {
		// Both fields, because a client reads whichever it set: a scale
		// that moved only one of them reads back as unchanged on the
		// field the provider happens to look at.
		body["nodeCount"] = count
		body["initialNodeCount"] = count
		updated = cloneMap(body)
		return nil
	})
	if err != nil {
		return internalError(err)
	}
	if aerr := s.mirrorPool(location, cluster, name, updated); aerr != nil {
		return aerr
	}
	go s.rescale(location, cluster)
	return nil
}

func (s *Server) patchPool(location, cluster, name string, patch map[string]any) *apiError {
	id := s.nodePoolLink(location, cluster, name)
	var updated map[string]any
	err := s.store.Update(id, func(body map[string]any) error {
		for k, v := range patch {
			body[k] = v
		}
		updated = cloneMap(body)
		return nil
	})
	if err != nil {
		return internalError(err)
	}
	return s.mirrorPool(location, cluster, name, updated)
}

func (s *Server) deleteNodePool(w http.ResponseWriter, location, cluster, name string) *apiError {
	id := s.nodePoolLink(location, cluster, name)
	if _, aerr := s.loadNodePool(location, cluster, name); aerr != nil {
		return aerr
	}
	if _, err := s.store.Delete(id); err != nil {
		return internalError(err)
	}
	// Remove it from the parent too, or the cluster keeps reporting a
	// pool that is gone and the next plan wants to delete it again.
	if aerr := s.mirrorPool(location, cluster, name, nil); aerr != nil {
		return aerr
	}
	go s.rescale(location, cluster)

	writeJSON(w, http.StatusOK, s.newGKEOperation(location, "DELETE_NODE_POOL", id))
	return nil
}

// cloneMap copies a body so it can be handed outside the store's lock.
// Store.Update holds that lock while its callback runs, and keeping a
// reference to the live map would be a data race the moment anything read
// it afterwards.
func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
