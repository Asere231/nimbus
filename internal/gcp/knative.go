package gcp

import (
	"encoding/json"
	"net/http"

	"github.com/Asere231/nimbus/internal/core"
)

// `gcloud run` does not speak the Cloud Run Admin API.
//
// This was measured, not expected. The Terraform provider uses
// `/v2/projects/{p}/locations/{l}/services`; gcloud asks for
//
//	GET /apis/serving.knative.dev/v1/namespaces/{project}/services
//
// which is the KNATIVE API -- Kubernetes-shaped, with apiVersion, kind,
// metadata, spec and status, and the project standing in for a namespace.
// Two wire formats for the same resource, in the same product, and nothing
// announces which one a given tool uses.
//
// A second surprise came with it: **gcloud prefixes the REGION onto the
// HOST**, because real Cloud Run's endpoint is
// `https://us-central1-run.googleapis.com`. Pointed at
// `http://127.0.0.1:4703/` it tries to resolve `us-central1-127.0.0.1` and
// fails. The fix is a name rather than an address, and it is one Nimbus
// already proved in Milestone 2: nested `*.localhost` resolves to loopback
// on macOS and Windows with nothing installed, so
//
//	api_endpoint_overrides/run = http://run.nimbus.localhost:4703/
//
// becomes `us-central1-run.nimbus.localhost:4703`, which resolves to
// 127.0.0.1 and arrives here. Measured on macOS 27 with gcloud 592.0.0.
//
// Only the READS are projected. Deploying through gcloud would mean
// accepting the Knative shape as the source of truth as well, and nothing
// in this milestone needs it -- so a write says so plainly instead of
// half-working.

// serveKnative answers gcloud's view of Cloud Run.
func (s *Server) serveKnative(w http.ResponseWriter, r *http.Request, parts []string) string {
	// parts: apis serving.knative.dev v1 namespaces {project} {collection} [{name}]
	if len(parts) < 6 || parts[3] != "namespaces" {
		writeError(w, notFound("Nimbus does not emulate %s %s yet", r.Method, r.URL.Path))
		return "knative: unrecognised path"
	}
	collection := parts[5]
	var name string
	if len(parts) > 6 {
		name = parts[6]
	}

	if r.Method != http.MethodGet {
		writeError(w, errorf(http.StatusNotImplemented, "notImplemented", "UNIMPLEMENTED",
			"Nimbus serves the Knative API read-only. Deploy with Terraform "+
				"(google_cloud_run_v2_service) or the Admin API at /v2."))
		return "knative: writes are not implemented"
	}

	switch collection {
	case "services":
		if name == "" {
			return s.knativeList(w, TypeRunService, "Service")
		}
		return s.knativeGet(w, TypeRunService, "Service", name)
	case "revisions":
		if name == "" {
			return s.knativeList(w, TypeRunRevision, "Revision")
		}
		return s.knativeGet(w, TypeRunRevision, "Revision", name)
	case "configurations", "routes":
		// Collections gcloud may ask for that Nimbus does not model.
		// An empty list rather than a 404: an unimplemented sub-resource
		// that fails is read by a client as something else entirely --
		// the lesson Cloud Storage's anywhereCaches taught in Milestone 9.
		writeJSON(w, http.StatusOK, map[string]any{
			"apiVersion": "serving.knative.dev/v1",
			"kind":       "List",
			"items":      []any{},
		})
		return ""
	}
	writeError(w, notFound("Nimbus does not emulate the Knative %s collection yet", collection))
	return "knative: unknown collection " + collection
}

func (s *Server) knativeList(w http.ResponseWriter, resourceType, kind string) string {
	items := []any{}
	for _, res := range s.store.List(func(r *core.Resource) bool { return r.Type == resourceType }) {
		var body map[string]any
		if err := json.Unmarshal(res.Body, &body); err == nil {
			items = append(items, s.toKnative(body, kind, res.Name))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": "serving.knative.dev/v1",
		"kind":       kind + "List",
		"metadata":   map[string]any{},
		"items":      items,
	})
	return ""
}

func (s *Server) knativeGet(w http.ResponseWriter, resourceType, kind, name string) string {
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return r.Type == resourceType && r.Name == name
	}) {
		var body map[string]any
		if err := json.Unmarshal(res.Body, &body); err != nil {
			continue
		}
		writeJSON(w, http.StatusOK, s.toKnative(body, kind, res.Name))
		return ""
	}
	writeError(w, notFound("%s %q not found", kind, name))
	return "knative: no such " + kind
}

// toKnative projects a stored Admin-API resource into the Kubernetes-shaped
// document gcloud expects. The container spec is the same either way,
// which is what makes this a projection rather than a second model.
func (s *Server) toKnative(body map[string]any, kind, name string) map[string]any {
	tmpl, _ := body["template"].(map[string]any)
	if tmpl == nil {
		// A revision carries the template's fields directly.
		tmpl = body
	}
	ready := "True"
	if cond, ok := body["terminalCondition"].(map[string]any); ok {
		if str(cond["state"]) != "CONDITION_SUCCEEDED" {
			ready = "False"
		}
	}
	return map[string]any{
		"apiVersion": "serving.knative.dev/v1",
		"kind":       kind,
		"metadata": map[string]any{
			"name":              name,
			"namespace":         s.cfg.ProjectID,
			"selfLink":          "/apis/serving.knative.dev/v1/namespaces/" + s.cfg.ProjectID + "/services/" + name,
			"uid":               str(body["uid"]),
			"generation":        body["generation"],
			"creationTimestamp": str(body["createTime"]),
			"labels":            body["labels"],
			"annotations":       body["annotations"],
		},
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"name": lastSegment(str(body["latestCreatedRevision"])),
				},
				"spec": map[string]any{
					"containers": tmpl["containers"],
				},
			},
			"traffic": body["traffic"],
		},
		"status": map[string]any{
			"url":                       str(body["uri"]),
			"address":                   map[string]any{"url": str(body["uri"])},
			"latestReadyRevisionName":   lastSegment(str(body["latestReadyRevision"])),
			"latestCreatedRevisionName": lastSegment(str(body["latestCreatedRevision"])),
			"observedGeneration":        body["observedGeneration"],
			"conditions": []any{map[string]any{
				"type": "Ready", "status": ready,
				"lastTransitionTime": timestamp(),
			}},
		},
	}
}
