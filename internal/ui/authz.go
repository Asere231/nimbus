package ui

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Asere231/nimbus/internal/authz"
	"github.com/Asere231/nimbus/internal/core"
)

// Why a principal can or cannot do something.
//
// A refusal in the activity feed is enough to debug one call. It is not
// enough to learn a permission model, which is most of what this project
// is for -- so the inspector answers the question in advance, and
// `nimbus can` asks the same endpoint.
//
// One endpoint, backed by each cloud's own evaluator: the explanation
// cannot drift from the behaviour, because the thing that explains is the
// thing that decides. That is the whole reason this is not a second
// implementation living in JavaScript.

// Explainer is what a cloud plugin contributes. The per-cloud part is
// which actions are worth asking about and what its rules are called;
// the shape of the answer is `authz`'s and is the same for all three.
type Explainer interface {
	// ExplainAccess answers for a handful of representative actions.
	ExplainAccess(principal, resourceID, resourceType string) authz.Report
	// ExplainOne answers a single named action, which is what the CLI asks.
	ExplainOne(principal, action, resource string) authz.Check
}

// registerAuthzRoutes serves /api/authz.
//
//	?resource=<id>[&principal=<id>]          the representative actions
//	?resource=<id>&action=<name>[&principal] one named action
//
// The resource decides which cloud answers, because a resource knows its
// own cloud -- the same dispatch the visualizer's presentation uses, and
// the reason no shared module here keys on a type name.
func registerAuthzRoutes(mux *http.ServeMux, store *core.Store, explainers map[string]Explainer) {
	mux.HandleFunc("/api/authz", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("resource")
		if id == "" {
			http.Error(w, "a resource is required", http.StatusBadRequest)
			return
		}
		res, err := store.Get(id)
		if err != nil {
			// The most interesting question is usually about something
			// Nimbus does not store as a resource of its own: an S3
			// OBJECT, whose permissions are a different family from its
			// bucket's. So the nearest ancestor decides which cloud
			// answers, and the id that was asked about is still the one
			// evaluated.
			res, err = nearestAncestor(store, id)
			if err != nil {
				http.Error(w, "no such resource", http.StatusNotFound)
				return
			}
		}
		ex := explainers[res.Cloud]
		if ex == nil {
			// A cloud that is not running, or one with no evaluator. An
			// empty report rather than an error: the inspector shows
			// "not enforced here", which is the truth.
			writeJSON(w, authz.Report{Resource: id})
			return
		}

		principal := r.URL.Query().Get("principal")
		if action := r.URL.Query().Get("action"); action != "" {
			check := ex.ExplainOne(principal, action, id)
			report := ex.ExplainAccess(principal, id, "")
			report.Checks = []authz.Check{check}
			writeJSON(w, report)
			return
		}
		writeJSON(w, ex.ExplainAccess(principal, id, res.Type))
	})
}

// nearestAncestor finds the longest resource id Nimbus holds that
// CONTAINS the one asked about.
//
// It exists because the question people most want answered is about
// something that is not a resource in its own right: `s3:GetObject` on
// `arn:aws:s3:::bucket/key`, where only the bucket is stored. The same
// shape covers a blob under a container and an object under a GCS
// bucket, so it is written once here rather than per cloud.
//
// Containment is SEGMENT-ALIGNED, never a bare prefix: without that,
// `arn:aws:s3:::logs` would be taken to contain `arn:aws:s3:::logs-old`
// and a question about one bucket would be answered by another's cloud.
// The same rule, and the same reason, as Azure's scopeCovers.
func nearestAncestor(store *core.Store, id string) (*core.Resource, error) {
	var best *core.Resource
	for _, r := range store.List(func(r *core.Resource) bool {
		return contains(r.ID, id)
	}) {
		if best == nil || len(r.ID) > len(best.ID) {
			best = r
		}
	}
	if best == nil {
		return nil, errNoAncestor
	}
	return best, nil
}

var errNoAncestor = errors.New("no resource contains that id")

func contains(outer, inner string) bool {
	if len(outer) >= len(inner) || !strings.EqualFold(outer, inner[:len(outer)]) {
		return false
	}
	switch inner[len(outer)] {
	case '/', ':':
		return true
	}
	return false
}
