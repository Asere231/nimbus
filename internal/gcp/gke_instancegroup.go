package gcp

import (
	"encoding/json"
	"net/http"

	"github.com/Asere231/nimbus/internal/core"
)

// Where a node pool's node count actually comes from.
//
// This was the last and least obvious thing in the milestone, and it was
// found by reading the provider rather than by guessing. `node_count` on
// `google_container_node_pool` is not a field the container API returns at
// all -- the provider's own comment says so:
//
//	// Node pools don't expose the current node count in their API, so
//	// read the
//
// What it does instead is take the pool's `instanceGroupUrls`, GET each
// one from the COMPUTE API, sum their `targetSize` and divide:
//
//	nodeCount = size / len(igmUrls)
//
// So a node pool with no instance groups reports a count of zero, and one
// whose instance groups 404 fails the read. The symptom either way is a
// plan that forever wants to change node_count from 0 to the value it
// already has, with nothing anywhere mentioning an instance group.
//
// Nimbus derives the manager rather than storing it. There is no second
// piece of state to keep in step, nothing extra appears in the graph, and
// it survives a restart because the answer is computed from the node pool
// in the store -- which is the same reason the AKS kubeconfig and the
// etags are derived.

// getInstanceGroupManager answers for the manager behind a node pool.
func (s *Server) getInstanceGroupManager(w http.ResponseWriter, scope, name string) string {
	selfLink := s.computeLink(scope, "instanceGroupManagers", name)
	pool := s.poolForInstanceGroup(selfLink)
	if pool == nil {
		writeError(w, notFound("The resource '%s' was not found", selfLink))
		return "no such instance group manager"
	}
	size := poolCount(pool)
	writeJSON(w, http.StatusOK, map[string]any{
		"kind":     "compute#instanceGroupManager",
		"id":       numericID(selfLink),
		"name":     name,
		"selfLink": selfLink,
		"zone":     s.scopeLink(scope),
		// The one field the provider is here for. It is the pool's own
		// count, so the two can never disagree.
		"targetSize":        size,
		"baseInstanceName":  name,
		"instanceGroup":     s.computeLink(scope, "instanceGroups", name),
		"creationTimestamp": timestamp(),
		"currentActions": map[string]any{
			"none": size, "creating": 0, "deleting": 0, "recreating": 0,
			"abandoning": 0, "restarting": 0, "refreshing": 0, "verifying": 0,
			"creatingWithoutRetries": 0,
		},
	})
	return ""
}

// poolForInstanceGroup finds the node pool that owns a manager URL.
//
// A scan rather than an index: the URL is in the pool's own
// `instanceGroupUrls`, so the store is the only place this has to be
// true, and there is no mapping to rebuild after a restart. There are
// never many node pools.
func (s *Server) poolForInstanceGroup(selfLink string) map[string]any {
	for _, res := range s.store.List(func(r *core.Resource) bool { return r.Type == TypeGKENodePool }) {
		var body map[string]any
		if err := json.Unmarshal(res.Body, &body); err != nil {
			continue
		}
		urls, _ := body["instanceGroupUrls"].([]any)
		for _, u := range urls {
			if str(u) == selfLink {
				return body
			}
		}
	}
	return nil
}
