package ui

import (
	"net/http"

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
			http.Error(w, "no such resource", http.StatusNotFound)
			return
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
