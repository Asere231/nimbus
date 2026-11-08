package gcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/run"
)

// Cloud Run: a container image served over HTTP, for real.
//
// The container is the shared run engine (internal/engines/run): Nimbus
// pulls the image, starts it with PORT set and proxies every request
// through to it, so the response a client gets is the one the image really
// produced. Nothing of the application is emulated.
//
// Three measured facts shape this file.
//
//   - **Create is a POST with the name in a QUERY PARAMETER**:
//     `POST /v2/projects/{p}/locations/{l}/services?serviceId=hello`, and
//     the body carries no name at all. Compute posts the name in the body;
//     Pub/Sub PUTs to the name's own URL. Three conventions, one cloud.
//
//   - **The provider polls the OPERATION, not the service.** This is the
//     exact opposite of GKE, measured the same way: a create whose
//     operation said `done: true` was accepted at once and the service's
//     own `reconciling: true` was never read; an operation without `done`
//     was polled four times, ten seconds apart, until it finished. So the
//     operation must stay unfinished until the container is actually
//     listening -- otherwise Terraform reports a service as ready while
//     nothing answers it.
//
//   - **`uri` is what everything else uses to reach the service**, and it
//     has to be stable. A container's host port changes every time it
//     starts, so `uri` points at Nimbus, which proxies; see run_proxy.go.
//     This is the same reasoning as the AKS kubeconfig and the stable
//     etag: anything a client stores and re-reads must not move.

const (
	TypeRunService  = "run.googleapis.com/Service"
	TypeRunRevision = "run.googleapis.com/Revision"
)

const (
	serviceTypeURL  = "type.googleapis.com/google.cloud.run.v2.Service"
	metadataTypeURL = "type.googleapis.com/google.cloud.run.v2.Service"
)

func (s *Server) serviceName(location, name string) string {
	return "projects/" + s.cfg.ProjectID + "/locations/" + location + "/services/" + name
}

func (s *Server) revisionName(location, service, revision string) string {
	return s.serviceName(location, service) + "/revisions/" + revision
}

// serveRun routes the Admin v2 API:
// /v2/projects/{p}/locations/{l}/{services|operations}/...
func (s *Server) serveRun(w http.ResponseWriter, r *http.Request, parts []string) string {
	if len(parts) < 5 {
		writeError(w, notFound("Nimbus does not emulate %s %s yet", r.Method, r.URL.Path))
		return "run: unrecognised path"
	}
	location := parts[4]
	rest := parts[5:]
	if len(rest) == 0 {
		writeError(w, notFound("Nimbus does not emulate %s %s yet", r.Method, r.URL.Path))
		return "run: unrecognised path"
	}

	var e *apiError
	switch {
	case rest[0] == "operations" && len(rest) == 2 && r.Method == http.MethodGet:
		return s.getLRO(w, "projects/"+s.cfg.ProjectID+"/locations/"+location+"/operations/"+rest[1])

	case rest[0] != "services":
		e = notFound("Nimbus does not emulate %s %s yet", r.Method, r.URL.Path)

	case len(rest) == 1 && r.Method == http.MethodPost:
		e = s.createService(w, r, location)
	case len(rest) == 1 && r.Method == http.MethodGet:
		e = s.listServices(w, location)

	default:
		name, verb := trimVerb(rest[1])
		switch {
		case verb != "":
			e = s.serviceVerb(w, r, location, name, verb)
		case len(rest) == 2 && r.Method == http.MethodGet:
			e = s.getService(w, location, name)
		case len(rest) == 2 && r.Method == http.MethodPatch:
			e = s.patchService(w, r, location, name)
		case len(rest) == 2 && r.Method == http.MethodDelete:
			e = s.deleteService(w, location, name)
		case len(rest) == 3 && rest[2] == "revisions" && r.Method == http.MethodGet:
			e = s.listRevisions(w, location, name)
		case len(rest) == 4 && rest[2] == "revisions" && r.Method == http.MethodGet:
			e = s.getRevision(w, location, name, rest[3])
		case len(rest) == 4 && rest[2] == "revisions" && r.Method == http.MethodDelete:
			e = s.deleteRevision(w, location, name, rest[3])
		default:
			e = notFound("Nimbus does not emulate %s %s yet", r.Method, r.URL.Path)
		}
	}
	if e != nil {
		writeError(w, e)
		return "run " + r.Method + ": " + e.Code
	}
	return ""
}

func (s *Server) loadService(location, name string) (map[string]any, *apiError) {
	res, err := s.store.Get(s.serviceName(location, name))
	if err != nil || res.Type != TypeRunService {
		return nil, notFound("Resource 'projects/%s/locations/%s/services/%s' was not found",
			s.cfg.ProjectID, location, name)
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body, &body); err != nil {
		return nil, internalError(err)
	}
	return body, nil
}

func (s *Server) saveService(location string, body map[string]any) *apiError {
	name := lastSegment(str(body["name"]))
	raw, err := json.Marshal(body)
	if err != nil {
		return internalError(err)
	}
	s.store.Put(&core.Resource{
		Cloud: "gcp", ID: s.serviceName(location, name), Type: TypeRunService,
		Name: name, ParentID: s.projectSelfLink(), Location: location,
		Backing: "real", Body: raw,
	})
	return nil
}

func (s *Server) createService(w http.ResponseWriter, r *http.Request, location string) *apiError {
	name := r.URL.Query().Get("serviceId")
	if name == "" {
		return badRequest("the serviceId query parameter is required")
	}
	var body map[string]any
	if err := readJSON(r, &body); err != nil {
		return badRequest("%v", err)
	}
	if body == nil {
		body = map[string]any{}
	}
	if s.store.Exists(s.serviceName(location, name)) {
		return alreadyExists("service "+name, s.cfg.ProjectID)
	}
	image := firstImage(body)
	if image == "" {
		return badRequest("the service's template must name a container image")
	}

	s.fillService(body, location, name, 1)
	if aerr := s.saveService(location, body); aerr != nil {
		return aerr
	}
	if aerr := s.putRevision(location, name, body); aerr != nil {
		return aerr
	}

	// Unfinished on purpose: the provider polls this, and marking it done
	// before the container answers would report a service as ready while
	// nothing is listening.
	op := s.newLRO(location, metadataTypeURL, false)
	go s.provisionService(location, name, op.Name)

	writeJSON(w, http.StatusOK, op)
	return nil
}

// fillService adds everything a client reads back. All of it derived.
func (s *Server) fillService(body map[string]any, location, name string, generation int) {
	resourceName := s.serviceName(location, name)
	body["name"] = resourceName
	body["uid"] = hexID(32)
	// Numbers travel as decimal STRINGS in these APIs; a JSON number
	// lands in a client's string field as the zero value, silently.
	body["generation"] = numberOf(generation)
	body["observedGeneration"] = numberOf(generation)
	body["uri"] = s.serviceURI(name)
	body["createTime"] = orDefault(body["createTime"], timestamp())
	body["updateTime"] = timestamp()
	body["creator"] = "nimbus@nimbus.localhost"
	body["lastModifier"] = "nimbus@nimbus.localhost"
	body["launchStage"] = orDefault(body["launchStage"], "GA")
	body["etag"] = etagOf(resourceName)
	body["reconciling"] = false

	revision := revisionID(name, generation)
	body["latestCreatedRevision"] = s.revisionName(location, name, revision)
	body["latestReadyRevision"] = s.revisionName(location, name, revision)
	if _, ok := body["traffic"]; !ok {
		body["traffic"] = []any{map[string]any{
			"type": "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST", "percent": 100,
		}}
	}
	body["trafficStatuses"] = []any{map[string]any{
		"type": "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST", "percent": 100,
		"revision": revision,
	}}
	body["terminalCondition"] = map[string]any{
		"type": "Ready", "state": "CONDITION_SUCCEEDED",
		"lastTransitionTime": timestamp(),
	}
	body["conditions"] = []any{}
}

// revisionID is Cloud Run's own naming: {service}-{generation}-{suffix}.
// Derived from the service and the generation so it is the same on every
// read, rather than random -- the rule that keeps a plan clean.
func revisionID(service string, generation int) string {
	return service + "-" + pad5(generation) + "-" + hexID(3)
}

func pad5(n int) string {
	s := numberOf(n)
	for len(s) < 5 {
		s = "0" + s
	}
	return s
}

// provisionService starts the container and finishes the operation.
func (s *Server) provisionService(location, name, opName string) {
	ctx, cancel := context.WithTimeout(context.Background(), backgroundTimeout)
	defer cancel()
	id := s.serviceName(location, name)

	body, aerr := s.loadService(location, name)
	if aerr != nil {
		s.lros.fail(opName, "the service was deleted while it was starting")
		return
	}
	svc := &run.Service{
		ResourceID:    id,
		Name:          name,
		Image:         firstImage(body),
		Command:       containerCommand(body, "command"),
		Args:          containerCommand(body, "args"),
		Env:           containerEnv(body),
		ContainerPort: containerPort(body),
	}
	// Cloud Run tells a container about itself through these, and a
	// handler written for Cloud Run may read them.
	if svc.Env == nil {
		svc.Env = map[string]string{}
	}
	svc.Env["K_SERVICE"] = name
	svc.Env["K_REVISION"] = lastSegment(str(body["latestCreatedRevision"]))
	svc.Env["K_CONFIGURATION"] = name

	if err := s.services.Start(ctx, svc); err != nil {
		s.services.Forget(id)
		_ = s.store.Update(id, func(b map[string]any) error {
			b["terminalCondition"] = map[string]any{
				"type": "Ready", "state": "CONDITION_FAILED",
				"message": err.Error(), "lastTransitionTime": timestamp(),
			}
			return nil
		})
		// The message reaches the client here, which is what makes a
		// failed image something it can read rather than a timeout.
		s.lros.fail(opName, err.Error())
		return
	}

	_ = s.store.Update(id, func(b map[string]any) error {
		b["terminalCondition"] = map[string]any{
			"type": "Ready", "state": "CONDITION_SUCCEEDED",
			"lastTransitionTime": timestamp(),
		}
		return nil
	})
	if finished, aerr := s.loadService(location, name); aerr == nil {
		s.lros.finish(opName, serviceTypeURL, finished)
	} else {
		s.lros.finish(opName, serviceTypeURL, nil)
	}
}

func (s *Server) getService(w http.ResponseWriter, location, name string) *apiError {
	body, aerr := s.loadService(location, name)
	if aerr != nil {
		return aerr
	}
	writeJSON(w, http.StatusOK, body)
	return nil
}

func (s *Server) listServices(w http.ResponseWriter, location string) *apiError {
	out := []any{}
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return r.Type == TypeRunService && (location == "-" || r.Location == location)
	}) {
		var body map[string]any
		if err := json.Unmarshal(res.Body, &body); err == nil {
			out = append(out, body)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": out})
	return nil
}

// patchService redeploys. A new generation means a new revision and a new
// container, because the image or the environment may have changed and the
// whole point of applying is that the next request hits the new one.
func (s *Server) patchService(w http.ResponseWriter, r *http.Request, location, name string) *apiError {
	var patch map[string]any
	if err := readJSON(r, &patch); err != nil {
		return badRequest("%v", err)
	}
	current, aerr := s.loadService(location, name)
	if aerr != nil {
		return aerr
	}
	generation := atoiOr(str(current["generation"]), 1) + 1
	mask := maskFields(r.URL.Query().Get("updateMask"))
	for k, v := range patch {
		if k == "name" || !mask.allows(k) {
			continue
		}
		current[k] = v
	}
	s.fillService(current, location, name, generation)
	if aerr := s.saveService(location, current); aerr != nil {
		return aerr
	}
	if aerr := s.putRevision(location, name, current); aerr != nil {
		return aerr
	}

	op := s.newLRO(location, metadataTypeURL, false)
	go s.provisionService(location, name, op.Name)
	writeJSON(w, http.StatusOK, op)
	return nil
}

func (s *Server) deleteService(w http.ResponseWriter, location, name string) *apiError {
	id := s.serviceName(location, name)
	body, aerr := s.loadService(location, name)
	if aerr != nil {
		return aerr
	}
	if svc := s.services.Get(id); svc != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*20*1e9)
		s.services.Stop(ctx, svc)
		cancel()
	}
	if _, err := s.store.Delete(id); err != nil {
		return internalError(err)
	}

	// Delete is long-running too, and finished by the time it answers --
	// the container is already gone. Done immediately is honest here,
	// where it would not have been for the create.
	op := s.newLRO(location, metadataTypeURL, true)
	s.lros.finish(op.Name, serviceTypeURL, body)
	writeJSON(w, http.StatusOK, op)
	return nil
}

// serviceVerb handles the IAM calls on a service, which is how a config
// makes one publicly invocable. Recorded, never enforced: nothing here
// refuses a request for lacking a role, exactly as Azure RBAC and AWS IAM
// do not. A local apply cannot prove a deployment's permissions are right.
func (s *Server) serviceVerb(w http.ResponseWriter, r *http.Request, location, name, verb string) *apiError {
	id := s.serviceName(location, name)
	if _, aerr := s.loadService(location, name); aerr != nil {
		return aerr
	}
	switch verb {
	case "getIamPolicy":
		policy := map[string]any{"version": 1, "etag": etagOf(id)}
		if res, err := s.store.Get(id); err == nil {
			var body map[string]any
			if json.Unmarshal(res.Body, &body) == nil {
				if stored, ok := body["x-nimbus-iam"].(map[string]any); ok {
					policy = stored
				}
			}
		}
		writeJSON(w, http.StatusOK, policy)
		return nil
	case "setIamPolicy":
		var req map[string]any
		if err := readJSON(r, &req); err != nil {
			return badRequest("%v", err)
		}
		policy, _ := req["policy"].(map[string]any)
		if policy == nil {
			policy = map[string]any{}
		}
		policy["etag"] = etagOf(id)
		if _, ok := policy["version"]; !ok {
			policy["version"] = 1
		}
		if err := s.store.Update(id, func(body map[string]any) error {
			body["x-nimbus-iam"] = policy
			return nil
		}); err != nil {
			return internalError(err)
		}
		writeJSON(w, http.StatusOK, policy)
		return nil
	case "testIamPermissions":
		var req map[string]any
		_ = readJSON(r, &req)
		writeJSON(w, http.StatusOK, map[string]any{"permissions": req["permissions"]})
		return nil
	}
	return notFound("Nimbus does not emulate the Cloud Run %s verb yet", verb)
}

// --- reading the template -------------------------------------------------

// firstContainer is the container a service runs. Cloud Run allows
// sidecars; Nimbus runs the first one and ignores the rest, which is a
// limitation worth stating rather than a silent partial.
func firstContainer(body map[string]any) map[string]any {
	tmpl, _ := body["template"].(map[string]any)
	if tmpl == nil {
		return nil
	}
	containers, _ := tmpl["containers"].([]any)
	if len(containers) == 0 {
		return nil
	}
	c, _ := containers[0].(map[string]any)
	return c
}

func firstImage(body map[string]any) string {
	if c := firstContainer(body); c != nil {
		return str(c["image"])
	}
	return ""
}

func containerCommand(body map[string]any, field string) []string {
	c := firstContainer(body)
	if c == nil {
		return nil
	}
	return stringList(c[field])
}

// containerEnv reads the container's environment. Cloud Run's env is a
// LIST of {name, value} objects rather than a map, so a handler written
// for it sends them in that shape.
func containerEnv(body map[string]any) map[string]string {
	c := firstContainer(body)
	if c == nil {
		return nil
	}
	items, _ := c["env"].([]any)
	out := map[string]string{}
	for _, item := range items {
		entry, _ := item.(map[string]any)
		if entry == nil {
			continue
		}
		if name := str(entry["name"]); name != "" {
			out[name] = str(entry["value"])
		}
	}
	return out
}

// containerPort reads the port the image listens on, which Cloud Run calls
// a containerPort inside `ports`.
func containerPort(body map[string]any) int {
	c := firstContainer(body)
	if c == nil {
		return 0
	}
	ports, _ := c["ports"].([]any)
	for _, item := range ports {
		p, _ := item.(map[string]any)
		if p == nil {
			continue
		}
		if n, ok := p["containerPort"].(float64); ok && n > 0 {
			return int(n)
		}
		// It may arrive as a string, like every other number here.
		if n := atoiOr(str(p["containerPort"]), 0); n > 0 {
			return n
		}
	}
	return 0
}

// numberOf writes an int the way these APIs do: as a decimal string.
func numberOf(n int) string {
	return strings.TrimSpace(sprintf("%d", n))
}
