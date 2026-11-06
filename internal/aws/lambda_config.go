package aws

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/funcs"
)

// Reading a function back, and changing it.
//
// Everything here is read on every `terraform plan`, so every field has to
// come back the same way twice: CodeSha256 is the digest of the code rather
// than a fresh value, and a configuration change only restarts the
// container when the container is what carries it -- a timeout is read per
// invocation, an environment variable is baked in at start.

func (s *Server) getFunction(w http.ResponseWriter, name string) *apiError {
	fn, aerr := s.loadFunction(name)
	if aerr != nil {
		return aerr
	}
	writeJSON(w, 200, map[string]any{
		"Configuration": fn,
		"Code": map[string]any{
			"RepositoryType": "S3",
			// There is no download URL: nothing Nimbus serves hands the zip
			// back, and an empty string is more honest than a URL that 404s.
			"Location": "",
		},
		"Tags": fn.NimbusTags,
	})
	return nil
}

func (s *Server) getFunctionConfiguration(w http.ResponseWriter, name string) *apiError {
	fn, aerr := s.loadFunction(name)
	if aerr != nil {
		return aerr
	}
	writeJSON(w, 200, fn)
	return nil
}

func (s *Server) listFunctions(w http.ResponseWriter) *apiError {
	out := []*lambdaFunction{}
	for _, res := range s.store.List(func(r *core.Resource) bool { return r.Type == typeLambdaFunction }) {
		var fn lambdaFunction
		if err := json.Unmarshal(res.Body, &fn); err == nil {
			out = append(out, &fn)
		}
	}
	writeJSON(w, 200, map[string]any{"Functions": out})
	return nil
}

func (s *Server) listVersions(w http.ResponseWriter, name string) *apiError {
	fn, aerr := s.loadFunction(name)
	if aerr != nil {
		return aerr
	}
	// Only $LATEST. Publishing a version would mean keeping a copy of the
	// code and a second container per version, and nothing a local apply
	// does needs it.
	writeJSON(w, 200, map[string]any{"Versions": []*lambdaFunction{fn}})
	return nil
}

func (s *Server) updateFunctionCode(w http.ResponseWriter, r *http.Request, name string) *apiError {
	fn, aerr := s.loadFunction(name)
	if aerr != nil {
		return aerr
	}
	var req struct {
		ZipFile  string `json:"ZipFile"`
		S3Bucket string `json:"S3Bucket"`
		S3Key    string `json:"S3Key"`
		ImageUri string `json:"ImageUri"`
	}
	if err := readJSON(r, &req); err != nil {
		return &apiError{Status: 400, Code: "InvalidParameterValueException", Message: err.Error()}
	}
	code, aerr := s.lambdaCode(req.ZipFile, req.S3Bucket, req.S3Key)
	if aerr != nil {
		return aerr
	}
	image, aerr := lambdaImage(fn.Runtime, req.ImageUri)
	if aerr != nil {
		return aerr
	}

	fn.CodeSize = int64(len(code))
	fn.CodeSha256 = sha256Base64(code)
	fn.LastModified = lambdaTime()
	fn.RevisionId = uuidLike()
	fn.State = "Pending"
	fn.LastUpdateStatus = "InProgress"
	if aerr := s.saveFunction(fn); aerr != nil {
		return aerr
	}

	// The container is replaced rather than reloaded: its bind mount points
	// at the code, and a runtime that has already imported the old module
	// would keep serving it.
	go func(name, image string, code []byte) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if running := s.lambda.Get(s.lambdaARN(name)); running != nil {
			s.lambda.Stop(ctx, running)
		}
		s.startFunction(name, image, code)
	}(fn.FunctionName, image, code)

	writeJSON(w, 200, fn)
	return nil
}

func (s *Server) updateFunctionConfiguration(w http.ResponseWriter, r *http.Request, name string) *apiError {
	fn, aerr := s.loadFunction(name)
	if aerr != nil {
		return aerr
	}
	var req map[string]any
	if err := readJSON(r, &req); err != nil {
		return &apiError{Status: 400, Code: "InvalidParameterValueException", Message: err.Error()}
	}
	restart := false
	if v, ok := req["Timeout"].(float64); ok {
		fn.Timeout = int(v)
	}
	if v, ok := req["MemorySize"].(float64); ok {
		fn.MemorySize = int(v)
	}
	if v, ok := req["Handler"].(string); ok && v != fn.Handler {
		fn.Handler = v
		restart = true
	}
	if v, ok := req["Role"].(string); ok {
		fn.Role = v
	}
	if v, ok := req["Description"].(string); ok {
		fn.Description = v
	}
	if v, ok := req["Runtime"].(string); ok && v != fn.Runtime {
		fn.Runtime = v
		restart = true
	}
	if env, ok := req["Environment"].(map[string]any); ok {
		fn.Environment = env
		restart = true
	}
	fn.LastModified = lambdaTime()
	fn.RevisionId = uuidLike()
	if aerr := s.saveFunction(fn); aerr != nil {
		return aerr
	}
	if restart {
		// Only a change the CONTAINER carries needs one: a timeout is read
		// per invocation, an environment variable is not.
		go s.restartFunction(fn.FunctionName)
	}
	writeJSON(w, 200, fn)
	return nil
}

// restartFunction brings a function's container back with the configuration
// it has now, reusing the code already on disk.
func (s *Server) restartFunction(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	fn, aerr := s.loadFunction(name)
	if aerr != nil {
		return
	}
	image, aerr := lambdaImage(fn.Runtime, "")
	if aerr != nil {
		return
	}
	if running := s.lambda.Get(fn.FunctionArn); running != nil {
		s.lambda.Stop(ctx, running)
	}
	command := []string{fn.Handler}
	if fn.Handler == "" {
		command = nil
	}
	running := &funcs.Function{
		ResourceID: fn.FunctionArn, Name: fn.FunctionName, Image: image,
		Command: command, Dir: s.lambda.CodeDir(fn.FunctionArn), Env: s.lambdaEnv(fn),
	}
	if err := s.lambda.Start(ctx, running); err != nil {
		_ = s.store.Update(fn.FunctionArn, func(body map[string]any) error {
			body["State"] = "Failed"
			body["StateReason"] = err.Error()
			return nil
		})
	}
}

func (s *Server) deleteFunction(w http.ResponseWriter, name string) *apiError {
	fn, aerr := s.loadFunction(name)
	if aerr != nil {
		return aerr
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if running := s.lambda.Get(fn.FunctionArn); running != nil {
		s.lambda.Stop(ctx, running)
	}
	s.lambda.RemoveCode(fn.FunctionArn)
	_, _ = s.store.Delete(fn.FunctionArn)
	// 204 with no body, which is what the SDK expects for this one.
	w.WriteHeader(204)
	return nil
}
