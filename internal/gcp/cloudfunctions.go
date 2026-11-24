package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/funcs"
	"github.com/Asere231/nimbus/internal/engines/run"
)

// Cloud Functions, which completes the set: all three clouds run code now.
//
// The three answers to one problem are completely different, and that
// contrast is most of what this is for:
//
//	Lambda            the base image ships the Runtime Interface Emulator,
//	                  so the code is MOUNTED into somebody else's runtime
//	Azure Functions   a .NET host negotiates with a language worker over
//	                  gRPC, and only finds the code if it is BAKED IN
//	Cloud Functions   the Functions Framework is a LIBRARY the user's code
//	                  brings, served by an ordinary HTTP server reading PORT
//
// The third needs the least from Nimbus, which is why `internal/engines/run`
// fits unchanged: a function is a container that answers HTTP on $PORT, and
// that is the whole of the run engine's contract.
//
// Gen 2 only. Gen 1 is a different API (`cloud_functions_custom_endpoint`,
// v1) with a different resource, and nothing here pretends to serve it.

const (
	TypeFunction = "cloudfunctions.googleapis.com/Function"

	functionTypeURL         = "type.googleapis.com/google.cloud.functions.v2.Function"
	functionMetadataTypeURL = "type.googleapis.com/google.cloud.functions.v2.OperationMetadata"

	// FunctionProxyPrefix is where functions answer. Its own prefix rather
	// than Cloud Run's, so a function and a service may share a name.
	FunctionProxyPrefix = "/_nimbus/functions/"

	// functionPort is what the Functions Framework listens on inside the
	// container. The engine sets PORT to the same value, which is how the
	// framework learns it -- no cloud-specific plumbing required.
	functionPort = 8080
)

func (s *Server) functionName(location, name string) string {
	return fmt.Sprintf("%sv2/projects/%s/locations/%s/functions/%s",
		s.base()+"/", s.cfg.ProjectID, location, name)
}

// functionURI is a function's stable address. Stable is the point: the
// provider stores it, so it must not change when the container restarts on
// a different host port.
func (s *Server) functionURI(name string) string {
	return s.base() + FunctionProxyPrefix + name
}

func (s *Server) serveFunctions(w http.ResponseWriter, r *http.Request, parts []string) string {
	if len(parts) < 6 {
		writeError(w, notFound("Nimbus does not emulate %s %s yet", r.Method, r.URL.Path))
		return "functions: unrecognised path"
	}
	location := parts[4]
	rest := parts[5:]

	var e *apiError
	switch {
	case len(rest) == 1 && r.Method == http.MethodPost:
		e = s.createFunction(w, r, location)
	case len(rest) == 1 && r.Method == http.MethodGet:
		e = s.listFunctions(w, location)
	case len(rest) == 2:
		name, verb := trimVerb(rest[1])
		switch {
		case verb != "":
			e = notFound("Nimbus does not emulate the %q verb on a function yet", verb)
		case r.Method == http.MethodGet:
			e = s.getFunction(w, location, name)
		case r.Method == http.MethodPatch:
			e = s.patchFunction(w, r, location, name)
		case r.Method == http.MethodDelete:
			e = s.deleteFunction(w, location, name)
		default:
			e = notFound("Nimbus does not emulate %s %s yet", r.Method, r.URL.Path)
		}
	default:
		e = notFound("Nimbus does not emulate %s %s yet", r.Method, r.URL.Path)
	}
	if e != nil {
		writeError(w, e)
		return "functions " + r.Method + ": " + e.Code
	}
	return ""
}

func (s *Server) loadFunction(location, name string) (map[string]any, *apiError) {
	res, err := s.store.Get(s.functionName(location, name))
	if err != nil || res.Type != TypeFunction {
		return nil, notFound("Resource 'projects/%s/locations/%s/functions/%s' was not found",
			s.cfg.ProjectID, location, name)
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body, &body); err != nil {
		return nil, internalError(err)
	}
	return body, nil
}

func (s *Server) saveFunction(location string, body map[string]any) *apiError {
	name := lastSegment(str(body["name"]))
	raw, err := json.Marshal(body)
	if err != nil {
		return internalError(err)
	}
	s.store.Put(&core.Resource{
		Cloud: "gcp", ID: s.functionName(location, name), Type: TypeFunction,
		Name: name, ParentID: s.projectSelfLink(), Location: location,
		Backing: "real", Body: raw,
	})
	return nil
}

// createFunction is POST .../functions?functionId={name}.
//
// Measured against google 6.50.0, the body is
//
//	{"buildConfig":{"automaticUpdatePolicy":null,"entryPoint":"hello",
//	  "onDeployUpdatePolicy":null,"runtime":"python311",
//	  "source":{"storageSource":{"bucket":"...","object":"...zip"}}},
//	 "labels":{...},"name":"projects/.../functions/nimbus-hello",
//	 "serviceConfig":{"allTrafficOnLatestRevision":true,"availableMemory":"256M",
//	  "ingressSettings":"ALLOW_ALL","maxInstanceCount":1,"timeoutSeconds":60}}
//
// Two things worth noting. The name arrives TWICE -- as the functionId query
// parameter and in the body -- which is Cloud Run's `?serviceId=` convention
// again. And the two update policies arrive as explicit JSON nulls, so
// anything reading them has to survive present-and-null as well as absent;
// the Pub/Sub lesson, in a second service.
func (s *Server) createFunction(w http.ResponseWriter, r *http.Request, location string) *apiError {
	name := r.URL.Query().Get("functionId")
	if name == "" {
		return badRequest("the functionId query parameter is required")
	}
	var body map[string]any
	if err := readJSON(r, &body); err != nil {
		return badRequest("%v", err)
	}
	if body == nil {
		body = map[string]any{}
	}
	if s.store.Exists(s.functionName(location, name)) {
		return alreadyExists("function "+name, s.cfg.ProjectID)
	}

	build := mapOf(body["buildConfig"])
	entry := str(build["entryPoint"])
	if entry == "" {
		return badRequest("buildConfig.entryPoint is required: it names the function to call")
	}
	runtime := str(build["runtime"])
	if _, err := runtimeImage(runtime); err != nil {
		// Refused BY NAME. A runtime quietly served by a different
		// interpreter would run the code and could not be trusted to mean
		// anything about the runtime the configuration asked for.
		return badRequest("%v", err)
	}
	bucket, object := sourceOf(build)
	if bucket == "" || object == "" {
		return badRequest("buildConfig.source.storageSource must name a bucket and an object: " +
			"Nimbus builds a function from its source archive, as `gcloud functions deploy --source` does")
	}
	if _, _, ok := s.objects.get(bucket, object); !ok {
		return badRequest("the source archive gs://%s/%s does not exist", bucket, object)
	}

	s.fillFunction(body, location, name)
	if aerr := s.saveFunction(location, body); aerr != nil {
		return aerr
	}

	// Unfinished on purpose. Cloud Run -- and Cloud Functions, which is
	// Cloud Run underneath -- polls the OPERATION rather than the resource,
	// the opposite of GKE. Marking it done before the container answers
	// would report a function as deployed while nothing is listening.
	op := s.newLRO(location, functionMetadataTypeURL, false)
	go s.provisionFunction(location, name, op.Name)

	writeJSON(w, http.StatusOK, op)
	return nil
}

// fillFunction adds everything a client reads back, all of it derived.
//
// url and serviceConfig.uri are filled HERE, at create, not when the
// container becomes ready. The provider writes state from a read taken while
// the function is still deploying and never looks again, so a late address is
// empty in state for ever -- the GKE endpoint lesson, which this is the
// second instance of.
func (s *Server) fillFunction(body map[string]any, location, name string) {
	resourceName := fmt.Sprintf("projects/%s/locations/%s/functions/%s", s.cfg.ProjectID, location, name)
	body["name"] = resourceName
	body["state"] = "ACTIVE"
	body["updateTime"] = timestamp()
	body["environment"] = orDefault(body["environment"], "GEN_2")
	body["url"] = s.functionURI(name)

	svc := mapOf(body["serviceConfig"])
	if svc == nil {
		svc = map[string]any{}
	}
	svc["uri"] = s.functionURI(name)
	// The Cloud Run service a Gen 2 function really runs on. Reported as a
	// NAME rather than created as a separate resource: the provider stores
	// it and nothing here reads it back, so inventing a second resource
	// would put a Cloud Run service in the graph that no configuration
	// asked for.
	svc["service"] = fmt.Sprintf("projects/%s/locations/%s/services/%s", s.cfg.ProjectID, location, name)
	svc["revision"] = name + "-00001-nim"
	svc["availableMemory"] = orDefault(svc["availableMemory"], "256M")
	svc["availableCpu"] = orDefault(svc["availableCpu"], "1")
	svc["timeoutSeconds"] = orDefault(svc["timeoutSeconds"], 60)
	svc["maxInstanceCount"] = orDefault(svc["maxInstanceCount"], 100)
	svc["ingressSettings"] = orDefault(svc["ingressSettings"], "ALLOW_ALL")
	svc["allTrafficOnLatestRevision"] = orDefault(svc["allTrafficOnLatestRevision"], true)
	svc["serviceAccountEmail"] = orDefault(svc["serviceAccountEmail"],
		s.cfg.ProjectID+"@appspot.gserviceaccount.com")
	body["serviceConfig"] = svc

	build := mapOf(body["buildConfig"])
	if build == nil {
		build = map[string]any{}
	}
	// A build id and a Cloud Build name, derived from the resource so they
	// are the same on every read. Generated values would be a permanent
	// plan diff, which is the rule every cloud in this project has paid for.
	build["build"] = fmt.Sprintf("projects/%s/locations/%s/builds/%s",
		s.cfg.ProjectID, location, slugOf(resourceName))
	build["dockerRepository"] = orDefault(build["dockerRepository"],
		fmt.Sprintf("projects/%s/locations/%s/repositories/gcf-artifacts", s.cfg.ProjectID, location))
	body["buildConfig"] = build
}

func (s *Server) getFunction(w http.ResponseWriter, location, name string) *apiError {
	body, aerr := s.loadFunction(location, name)
	if aerr != nil {
		return aerr
	}
	writeJSON(w, http.StatusOK, body)
	return nil
}

func (s *Server) listFunctions(w http.ResponseWriter, location string) *apiError {
	items := []any{}
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return r.Type == TypeFunction && (location == "-" || r.Location == location)
	}) {
		var body map[string]any
		if json.Unmarshal(res.Body, &body) == nil {
			items = append(items, body)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"functions": items})
	return nil
}

func (s *Server) patchFunction(w http.ResponseWriter, r *http.Request, location, name string) *apiError {
	existing, aerr := s.loadFunction(location, name)
	if aerr != nil {
		return aerr
	}
	var patch map[string]any
	if err := readJSON(r, &patch); err != nil {
		return badRequest("%v", err)
	}
	// updateMask is a WHITELIST, and for this API it is in the query string
	// as it is for Compute -- IAM puts it in the body instead. Applying the
	// whole patch regardless would silently clear every field the client
	// left out.
	mask := maskFields(r.URL.Query().Get("updateMask"))
	for field, value := range patch {
		if !mask.allows(field) {
			continue
		}
		switch field {
		// Never patchable: they are the function's identity.
		case "name", "state", "updateTime", "url", "environment":
			continue
		}
		existing[field] = value
	}
	s.fillFunction(existing, location, name)
	if aerr := s.saveFunction(location, existing); aerr != nil {
		return aerr
	}
	op := s.newLRO(location, functionMetadataTypeURL, false)
	go s.provisionFunction(location, name, op.Name)
	writeJSON(w, http.StatusOK, op)
	return nil
}

func (s *Server) deleteFunction(w http.ResponseWriter, location, name string) *apiError {
	id := s.functionName(location, name)
	if !s.store.Exists(id) {
		return notFound("Resource 'projects/%s/locations/%s/functions/%s' was not found",
			s.cfg.ProjectID, location, name)
	}
	// Synchronously, as an instance delete is: there is nothing slow in
	// stopping a container, and answering a pending operation let a destroy
	// return with the container still up -- measured for GCE.
	ctx, cancel := context.WithTimeout(context.Background(), backgroundTimeout)
	defer cancel()
	if fn := s.functions.Get(id); fn != nil {
		s.functions.Stop(ctx, fn)
	}
	s.removeFunctionCode(id)
	if _, err := s.store.Delete(id); err != nil {
		return internalError(err)
	}
	op := s.newLRO(location, functionMetadataTypeURL, true)
	writeJSON(w, http.StatusOK, op)
	return nil
}

// provisionFunction builds the image and starts the container.
func (s *Server) provisionFunction(location, name, opName string) {
	ctx, cancel := context.WithTimeout(context.Background(), backgroundTimeout)
	defer cancel()
	id := s.functionName(location, name)

	body, aerr := s.loadFunction(location, name)
	if aerr != nil {
		s.lros.fail(opName, "the function was deleted while it was deploying")
		return
	}
	build := mapOf(body["buildConfig"])
	bucket, object := sourceOf(build)
	archive, _, ok := s.objects.get(bucket, object)
	if !ok {
		s.failFunction(id, opName, fmt.Sprintf("the source archive gs://%s/%s is gone", bucket, object))
		return
	}
	base, err := runtimeImage(str(build["runtime"]))
	if err != nil {
		s.failFunction(id, opName, err.Error())
		return
	}

	dir, err := s.functionCodeDir(id)
	if err != nil {
		s.failFunction(id, opName, err.Error())
		return
	}
	// Unpacked with the Lambda engine's own unpacker rather than a second
	// copy of it. The alternative was duplicating a traversal guard -- a zip
	// ENTRY is a name and not a path, and `../../etc/passwd` is a legal one
	// -- and two copies of that check is one too many: the day they diverge,
	// the wrong one is the one nobody tested.
	if err := funcs.Unpack(archive, dir); err != nil {
		s.failFunction(id, opName, err.Error())
		return
	}
	// Written AFTER the unpack, because the unpack empties the directory --
	// which is the right behaviour for the code, so the Dockerfile has to be
	// replaced each time rather than written once at create. It also settles
	// a question: a source archive carrying its own Dockerfile cannot
	// hijack the build.
	if err := writeFunctionDockerfile(dir, base, str(mapOf(body["buildConfig"])["entryPoint"])); err != nil {
		s.failFunction(id, opName, err.Error())
		return
	}

	svc := &run.Service{
		ResourceID:    id,
		Name:          name,
		Image:         functionImageTag(name, id),
		BuildDir:      dir,
		BuildBase:     base,
		ContainerPort: functionPort,
		Env:           functionEnv(body, name),
	}
	if err := s.functions.Start(ctx, svc); err != nil {
		s.functions.Forget(id)
		s.failFunction(id, opName, err.Error())
		return
	}
	if finished, aerr := s.loadFunction(location, name); aerr == nil {
		s.lros.finish(opName, functionTypeURL, finished)
	} else {
		s.lros.finish(opName, functionTypeURL, nil)
	}
}

// failFunction records the failure on the resource AND on the operation.
//
// Both, because they are read by different things: the operation's error is
// what reaches the terraform output, and the resource's state is what a later
// read or `gcloud functions describe` shows.
func (s *Server) failFunction(id, opName, msg string) {
	_ = s.store.Update(id, func(b map[string]any) error {
		b["state"] = "FAILED"
		b["stateMessages"] = []any{map[string]any{
			"severity": "ERROR", "type": "DEPLOYMENT_FAILED", "message": msg,
		}}
		return nil
	})
	s.lros.fail(opName, msg)
}

// functionEnv is what the container is told about itself.
func functionEnv(body map[string]any, name string) map[string]string {
	env := map[string]string{}
	for k, v := range stringMap(mapOf(mapOf(body["serviceConfig"])["environmentVariables"])) {
		env[k] = v
	}
	// The framework reads FUNCTION_TARGET to decide what to call, and a
	// handler may read the other two. PORT is the engine's job.
	env["FUNCTION_TARGET"] = str(mapOf(body["buildConfig"])["entryPoint"])
	env["FUNCTION_SIGNATURE_TYPE"] = "http"
	env["K_SERVICE"] = name
	return env
}

// mapOf reads a nested object out of a decoded document.
//
// Everything that comes back from json.Unmarshal into map[string]any is
// either a map or it is not, and a missing field is simply absent -- but a
// field the provider sent as an explicit JSON null is PRESENT and nil, which
// a plain type assertion handles identically and callers must not be able to
// tell apart. Hence one helper rather than an assertion per field.
func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// sourceOf reads the bucket and object a build's source names.
func sourceOf(build map[string]any) (bucket, object string) {
	src := mapOf(mapOf(build["source"])["storageSource"])
	return str(src["bucket"]), str(src["object"])
}
