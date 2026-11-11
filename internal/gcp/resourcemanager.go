package gcp

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Resource Manager: projects, and the IAM policy hung off one.
//
// The google provider barely touches this -- a whole apply never asks for
// the project -- but gcloud does, and `gcloud projects list` finding
// nothing is how a CLI ends up inventing an identity of its own. Azure's
// `az login` did exactly that in Milestone 5: no subscription listed, so it
// used the TENANT id as the subscription id and put everything in the wrong
// place.
//
// Two versions are live at once. v1 is `/v1/projects/{id}`, v3 is
// `/v3/projects/{id}` with a `projects/{id}` name instead of a bare id and
// `state` instead of `lifecycleState`. Clients use both, sometimes in the
// same session, so both are served from the one stored project.

func (s *Server) serveResourceManager(w http.ResponseWriter, r *http.Request, parts []string) string {
	v3 := parts[0] == "v3"

	// A POST to projects/{id}:getIamPolicy -- the colon-verb form the newer
	// APIs use, where the method is part of the last path segment.
	if len(parts) >= 3 && strings.Contains(parts[len(parts)-1], ":") {
		seg := parts[len(parts)-1]
		id, verb, _ := strings.Cut(seg, ":")
		return s.projectIAM(w, r, id, verb)
	}

	switch {
	case len(parts) == 2 && parts[1] == "projects" && r.Method == http.MethodGet:
		return s.listProjects(w, v3)
	case len(parts) == 3 && parts[1] == "projects" && r.Method == http.MethodGet:
		return s.getProject(w, parts[2], v3)
	case len(parts) == 2 && parts[1] == "projects" && r.Method == http.MethodPost:
		// Creating a project is accepted and returns the one project there
		// is: Nimbus serves a single project, and pretending otherwise
		// would mean every self link in the store could be wrong.
		return s.getProject(w, s.cfg.ProjectID, v3)
	}
	e := notFound("Nimbus does not implement %s %s for Resource Manager yet", r.Method, r.URL.Path)
	writeError(w, e)
	return "resourcemanager " + r.Method + " " + r.URL.Path
}

// projectDoc is the project in whichever version was asked for.
func (s *Server) projectDoc(v3 bool) map[string]any {
	if v3 {
		return map[string]any{
			"name":        "projects/" + DefaultProjectNumber,
			"parent":      "",
			"projectId":   s.cfg.ProjectID,
			"state":       "ACTIVE",
			"displayName": "Nimbus Local",
			"createTime":  timestamp(),
			"updateTime":  timestamp(),
			"etag":        etagOf(s.cfg.ProjectID),
		}
	}
	return map[string]any{
		"kind":           "cloudresourcemanager#project",
		"projectId":      s.cfg.ProjectID,
		"projectNumber":  DefaultProjectNumber,
		"name":           "Nimbus Local",
		"lifecycleState": "ACTIVE",
		"createTime":     timestamp(),
	}
}

func (s *Server) getProject(w http.ResponseWriter, id string, v3 bool) string {
	// v3 addresses a project by number as well as by id, and gcloud uses
	// both. Either is this one project.
	if id != s.cfg.ProjectID && id != DefaultProjectNumber &&
		id != "projects/"+s.cfg.ProjectID && id != "projects/"+DefaultProjectNumber {
		writeError(w, notFound("Project '%s' not found.", id))
		return "no such project " + id
	}
	writeJSON(w, http.StatusOK, s.projectDoc(v3))
	return ""
}

func (s *Server) listProjects(w http.ResponseWriter, v3 bool) string {
	if v3 {
		writeJSON(w, http.StatusOK, map[string]any{"projects": []any{s.projectDoc(true)}})
		return ""
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"projects": []any{s.projectDoc(false)},
	})
	return ""
}

// projectIAM is getIamPolicy and setIamPolicy, recorded and never enforced
// -- the same honest limitation Azure's RBAC and AWS's IAM have. A local
// apply cannot prove a deployment's permissions are right, and saying so is
// better than a policy engine that is wrong in a different way.
func (s *Server) projectIAM(w http.ResponseWriter, r *http.Request, id, verb string) string {
	switch verb {
	case "getIamPolicy":
		writeJSON(w, http.StatusOK, s.storedPolicy())
		return ""
	case "setIamPolicy":
		var req struct {
			Policy map[string]any `json:"policy"`
		}
		if err := readJSON(r, &req); err != nil {
			writeError(w, badRequest("%v", err))
			return "bad policy"
		}
		policy := req.Policy
		if policy == nil {
			policy = map[string]any{}
		}
		policy["version"] = 1
		policy["etag"] = etagOf(s.cfg.ProjectID)
		body, _ := json.Marshal(map[string]any{"iamPolicy": policy})
		_ = s.store.Update(s.projectSelfLink(), func(doc map[string]any) error {
			var wrapper map[string]any
			_ = json.Unmarshal(body, &wrapper)
			doc["iamPolicy"] = wrapper["iamPolicy"]
			return nil
		})
		writeJSON(w, http.StatusOK, policy)
		return ""
	case "testIamPermissions":
		// This answers "which of these may I do", and it has to tell the
		// TRUTH once there is a truth to tell. It used to echo back
		// whatever was asked about, because nothing was enforced -- which
		// was honest then and would be a lie now: a client that trusts it
		// and then gets a 403 has been misled by the one call whose job is
		// to prevent that.
		//
		// With enforcement off it still echoes, because then everything
		// really is permitted.
		var req struct {
			Permissions []string `json:"permissions"`
		}
		_ = readJSON(r, &req)
		writeJSON(w, http.StatusOK, map[string]any{"permissions": s.heldPermissions(r, req.Permissions)})
		return ""
	}
	e := notFound("Nimbus does not implement the %q verb yet", verb)
	writeError(w, e)
	return "resourcemanager " + verb
}

func (s *Server) storedPolicy() map[string]any {
	res, err := s.store.Get(s.projectSelfLink())
	if err == nil {
		var doc map[string]any
		if json.Unmarshal(res.Body, &doc) == nil {
			if policy, ok := doc["iamPolicy"].(map[string]any); ok {
				return policy
			}
		}
	}
	return map[string]any{
		"version":  1,
		"etag":     etagOf(s.cfg.ProjectID),
		"bindings": []any{},
	}
}
