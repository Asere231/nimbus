package gcp

import (
	"encoding/json"
	"net/http"

	"github.com/Asere231/nimbus/internal/core"
)

// Revisions: the immutable record of what a service was deployed as.
//
// A revision is a real resource in Cloud Run -- it has its own name, it is
// listed and read, and traffic is split across several of them. Nimbus
// records one per generation so a redeploy leaves a history the graph can
// show, but only the latest one has a container behind it: splitting
// traffic would mean running every revision at once, and nothing a local
// apply does needs that. The limitation is stated in ROADMAP rather than
// hidden behind a `traffic` field that looks like it works.

func (s *Server) putRevision(location, service string, svc map[string]any) *apiError {
	name := lastSegment(str(svc["latestCreatedRevision"]))
	if name == "" {
		return nil
	}
	resourceName := s.revisionName(location, service, name)
	tmpl, _ := svc["template"].(map[string]any)
	body := map[string]any{
		"name":        resourceName,
		"uid":         hexID(32),
		"generation":  svc["generation"],
		"service":     s.serviceName(location, service),
		"createTime":  timestamp(),
		"updateTime":  timestamp(),
		"launchStage": "GA",
		"etag":        etagOf(resourceName),
		"conditions": []any{map[string]any{
			"type": "Ready", "state": "CONDITION_SUCCEEDED",
			"lastTransitionTime": timestamp(),
		}},
	}
	for k, v := range tmpl {
		// The revision IS the template: its containers, scaling and
		// service account are the template's own fields, one level up.
		body[k] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return internalError(err)
	}
	s.store.Put(&core.Resource{
		Cloud: "gcp", ID: resourceName, Type: TypeRunRevision, Name: name,
		ParentID: s.serviceName(location, service), Location: location,
		Backing: "state", Body: raw,
	})
	return nil
}

func (s *Server) listRevisions(w http.ResponseWriter, location, service string) *apiError {
	if _, aerr := s.loadService(location, service); aerr != nil {
		return aerr
	}
	parent := s.serviceName(location, service)
	out := []any{}
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return r.Type == TypeRunRevision && r.ParentID == parent
	}) {
		var body map[string]any
		if err := json.Unmarshal(res.Body, &body); err == nil {
			out = append(out, body)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"revisions": out})
	return nil
}

func (s *Server) getRevision(w http.ResponseWriter, location, service, name string) *apiError {
	res, err := s.store.Get(s.revisionName(location, service, name))
	if err != nil || res.Type != TypeRunRevision {
		return notFound("Resource 'projects/%s/locations/%s/services/%s/revisions/%s' was not found",
			s.cfg.ProjectID, location, service, name)
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body, &body); err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, body)
	return nil
}

func (s *Server) deleteRevision(w http.ResponseWriter, location, service, name string) *apiError {
	id := s.revisionName(location, service, name)
	if !s.store.Exists(id) {
		return notFound("Resource 'projects/%s/locations/%s/services/%s/revisions/%s' was not found",
			s.cfg.ProjectID, location, service, name)
	}
	if _, err := s.store.Delete(id); err != nil {
		return internalError(err)
	}
	op := s.newLRO(location, metadataTypeURL, true)
	writeJSON(w, http.StatusOK, op)
	return nil
}
