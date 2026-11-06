package aws

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/funcs"
)

// Lambda: handlers that really run.
//
// REST-JSON again, like EKS, but the paths are versioned per operation
// rather than per service -- /2015-03-31/functions for most of it,
// /2017-03-31/tags/{arn} for tags and /2020-06-30/... for the newer calls.
// Captured from aws-cli 1.40.33; the version in the path is part of the
// route, not a parameter.
//
// The code arrives as a base64 zip in the create call (or a reference to an
// S3 object, which Nimbus can resolve against its own S3). It is unpacked
// into a directory bind-mounted at /var/task in the matching
// public.ecr.aws/lambda/<runtime> image, and invoked through the Runtime
// Interface Emulator that image already contains -- so the handler is
// executed by the same runtime client that runs it in production. See
// internal/engines/funcs.

const typeLambdaFunction = "AWS::Lambda::Function"

// runtimeImages maps a Lambda runtime to the image that runs it. AWS's own
// base images, which is why no handler protocol has to be implemented here.
//
// This table is the AWS-specific half of running a function, which is why
// it lives in the plugin and not in the engine.
var runtimeImages = map[string]string{
	"python3.9":       "public.ecr.aws/lambda/python:3.9",
	"python3.10":      "public.ecr.aws/lambda/python:3.10",
	"python3.11":      "public.ecr.aws/lambda/python:3.11",
	"python3.12":      "public.ecr.aws/lambda/python:3.12",
	"python3.13":      "public.ecr.aws/lambda/python:3.13",
	"nodejs18.x":      "public.ecr.aws/lambda/nodejs:18",
	"nodejs20.x":      "public.ecr.aws/lambda/nodejs:20",
	"nodejs22.x":      "public.ecr.aws/lambda/nodejs:22",
	"java17":          "public.ecr.aws/lambda/java:17",
	"java21":          "public.ecr.aws/lambda/java:21",
	"ruby3.2":         "public.ecr.aws/lambda/ruby:3.2",
	"ruby3.3":         "public.ecr.aws/lambda/ruby:3.3",
	"dotnet8":         "public.ecr.aws/lambda/dotnet:8",
	"provided.al2":    "public.ecr.aws/lambda/provided:al2",
	"provided.al2023": "public.ecr.aws/lambda/provided:al2023",
}

// lambdaFunction is the stored shape. The field names are the wire's,
// because most of them are echoed straight back into
// GetFunctionConfiguration -- which the provider reads on every refresh.
type lambdaFunction struct {
	FunctionName     string         `json:"FunctionName"`
	FunctionArn      string         `json:"FunctionArn"`
	Runtime          string         `json:"Runtime"`
	Role             string         `json:"Role"`
	Handler          string         `json:"Handler"`
	CodeSize         int64          `json:"CodeSize"`
	Description      string         `json:"Description"`
	Timeout          int            `json:"Timeout"`
	MemorySize       int            `json:"MemorySize"`
	LastModified     string         `json:"LastModified"`
	CodeSha256       string         `json:"CodeSha256"`
	Version          string         `json:"Version"`
	Environment      map[string]any `json:"Environment,omitempty"`
	TracingConfig    map[string]any `json:"TracingConfig,omitempty"`
	RevisionId       string         `json:"RevisionId"`
	PackageType      string         `json:"PackageType"`
	Architectures    []string       `json:"Architectures"`
	EphemeralStorage map[string]any `json:"EphemeralStorage,omitempty"`
	LoggingConfig    map[string]any `json:"LoggingConfig,omitempty"`

	// State is what the provider waits on before it will invoke anything.
	State            string `json:"State"`
	StateReason      string `json:"StateReason,omitempty"`
	StateReasonCode  string `json:"StateReasonCode,omitempty"`
	LastUpdateStatus string `json:"LastUpdateStatus"`

	// Nimbus's own bookkeeping, outside the API's shape.
	NimbusTags        map[string]string `json:"nimbusTags,omitempty"`
	NimbusPolicy      map[string]any    `json:"nimbusPolicy,omitempty"`
	NimbusInvocations int               `json:"nimbusInvocations"`
	NimbusLogGroup    string            `json:"nimbusLogGroup"`
}

func (s *Server) lambdaARN(name string) string {
	return arn("lambda", s.cfg.Region, s.cfg.AccountID, "function:"+name)
}

// serveLambda routes on the path. The leading version segment is dropped:
// which version a call uses is AWS's history, not information.
func (s *Server) serveLambda(w http.ResponseWriter, r *http.Request) string {
	parts := pathParts(r.URL.Path)
	var aerr *apiError

	switch {
	case len(parts) == 2 && parts[1] == "functions" && r.Method == http.MethodPost:
		aerr = s.createFunction(w, r)
	case len(parts) == 2 && parts[1] == "functions" && r.Method == http.MethodGet:
		aerr = s.listFunctions(w)
	case len(parts) == 3 && parts[1] == "functions" && r.Method == http.MethodGet:
		aerr = s.getFunction(w, parts[2])
	case len(parts) == 3 && parts[1] == "functions" && r.Method == http.MethodDelete:
		aerr = s.deleteFunction(w, parts[2])

	case len(parts) == 4 && parts[1] == "functions" && parts[3] == "configuration" && r.Method == http.MethodGet:
		aerr = s.getFunctionConfiguration(w, parts[2])
	case len(parts) == 4 && parts[1] == "functions" && parts[3] == "configuration" && r.Method == http.MethodPut:
		aerr = s.updateFunctionConfiguration(w, r, parts[2])
	case len(parts) == 4 && parts[1] == "functions" && parts[3] == "code" && r.Method == http.MethodPut:
		aerr = s.updateFunctionCode(w, r, parts[2])
	case len(parts) == 4 && parts[1] == "functions" && parts[3] == "invocations":
		aerr = s.invokeFunction(w, r, parts[2])
	case len(parts) == 4 && parts[1] == "functions" && parts[3] == "versions" && r.Method == http.MethodGet:
		aerr = s.listVersions(w, parts[2])

	// A resource policy. The provider reads it after a create and is
	// written to tolerate exactly ResourceNotFoundException when there is
	// none -- the Key Vault certificate-contacts lesson again.
	case len(parts) == 4 && parts[1] == "functions" && parts[3] == "policy" && r.Method == http.MethodGet:
		aerr = s.getFunctionPolicy(w, parts[2])
	case len(parts) == 4 && parts[1] == "functions" && parts[3] == "policy" && r.Method == http.MethodPost:
		aerr = s.addFunctionPermission(w, r, parts[2])
	case len(parts) == 5 && parts[1] == "functions" && parts[3] == "policy" && r.Method == http.MethodDelete:
		aerr = s.removeFunctionPermission(w, parts[2], parts[4])

	case len(parts) == 3 && parts[1] == "tags":
		aerr = s.lambdaTags(w, r, parts[2])

	// Code signing is read on a refresh and is simply not configured.
	case len(parts) == 4 && parts[3] == "code-signing-config" && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{})

	default:
		aerr = &apiError{Status: 404, Code: "ResourceNotFoundException",
			Message: fmt.Sprintf("Nimbus does not implement %s %s for Lambda yet", r.Method, r.URL.Path)}
	}

	if aerr != nil {
		writeJSONError(w, aerr)
		return "lambda " + r.Method + " " + r.URL.Path + ": " + aerr.Code
	}
	return ""
}

func (s *Server) loadFunction(name string) (*lambdaFunction, *apiError) {
	// A client may name a function by name, by ARN or by a qualified ARN.
	name = lambdaName(name)
	res, err := s.store.Get(s.lambdaARN(name))
	if err != nil || res.Type != typeLambdaFunction {
		return nil, &apiError{Status: 404, Code: "ResourceNotFoundException",
			Message: "Function not found: " + s.lambdaARN(name)}
	}
	var fn lambdaFunction
	if err := json.Unmarshal(res.Body, &fn); err != nil {
		return nil, &apiError{Status: 500, Code: "ServiceException", Message: err.Error()}
	}
	return &fn, nil
}

// lambdaName reduces whatever a client used to the bare function name.
func lambdaName(ref string) string {
	if decoded, err := url.PathUnescape(ref); err == nil {
		ref = decoded
	}
	if strings.HasPrefix(ref, "arn:") {
		parts := strings.Split(ref, ":")
		// arn:aws:lambda:region:account:function:name[:qualifier]
		if len(parts) >= 7 {
			return parts[6]
		}
	}
	return ref
}

func (s *Server) saveFunction(fn *lambdaFunction) *apiError {
	body, err := json.Marshal(fn)
	if err != nil {
		return &apiError{Status: 500, Code: "ServiceException", Message: err.Error()}
	}
	s.store.Put(&core.Resource{
		Cloud: "aws", ID: fn.FunctionArn, Type: typeLambdaFunction, Name: fn.FunctionName,
		ParentID: s.accountARN(), Location: s.cfg.Region, Backing: "real", Body: body,
	})
	return nil
}

func (s *Server) createFunction(w http.ResponseWriter, r *http.Request) *apiError {
	var req struct {
		FunctionName  string            `json:"FunctionName"`
		Runtime       string            `json:"Runtime"`
		Role          string            `json:"Role"`
		Handler       string            `json:"Handler"`
		Description   string            `json:"Description"`
		Timeout       int               `json:"Timeout"`
		MemorySize    int               `json:"MemorySize"`
		Environment   map[string]any    `json:"Environment"`
		Tags          map[string]string `json:"Tags"`
		Architectures []string          `json:"Architectures"`
		PackageType   string            `json:"PackageType"`
		Code          struct {
			ZipFile  string `json:"ZipFile"`
			S3Bucket string `json:"S3Bucket"`
			S3Key    string `json:"S3Key"`
			ImageUri string `json:"ImageUri"`
		} `json:"Code"`
	}
	if err := readJSON(r, &req); err != nil {
		return &apiError{Status: 400, Code: "InvalidParameterValueException", Message: err.Error()}
	}
	if req.FunctionName == "" {
		return &apiError{Status: 400, Code: "InvalidParameterValueException",
			Message: "FunctionName is required"}
	}
	if s.store.Exists(s.lambdaARN(req.FunctionName)) {
		return &apiError{Status: 409, Code: "ResourceConflictException",
			Message: "Function already exist: " + req.FunctionName}
	}
	image, aerr := lambdaImage(req.Runtime, req.Code.ImageUri)
	if aerr != nil {
		return aerr
	}
	code, aerr := s.lambdaCode(req.Code.ZipFile, req.Code.S3Bucket, req.Code.S3Key)
	if aerr != nil {
		return aerr
	}

	fn := &lambdaFunction{
		FunctionName: req.FunctionName,
		FunctionArn:  s.lambdaARN(req.FunctionName),
		Runtime:      req.Runtime,
		Role:         req.Role,
		Handler:      req.Handler,
		Description:  req.Description,
		CodeSize:     int64(len(code)),
		Timeout:      orZero(req.Timeout, 3),
		MemorySize:   orZero(req.MemorySize, 128),
		Environment:  req.Environment,
		// Derived from the code, not generated: the provider compares it
		// between reads to decide whether the code changed, so a fresh
		// value on every read would mean a perpetual diff.
		CodeSha256:       sha256Base64(code),
		Version:          "$LATEST",
		RevisionId:       uuidLike(),
		PackageType:      orDefault(req.PackageType, "Zip"),
		Architectures:    req.Architectures,
		TracingConfig:    map[string]any{"Mode": "PassThrough"},
		EphemeralStorage: map[string]any{"Size": 512},
		LoggingConfig: map[string]any{
			"LogFormat": "Text", "LogGroup": "/aws/lambda/" + req.FunctionName,
		},
		LastModified:     lambdaTime(),
		State:            "Pending",
		LastUpdateStatus: "Successful",
		NimbusTags:       req.Tags,
		NimbusLogGroup:   "/aws/lambda/" + req.FunctionName,
	}
	if len(fn.Architectures) == 0 {
		fn.Architectures = []string{"x86_64"}
	}
	if aerr := s.saveFunction(fn); aerr != nil {
		return aerr
	}
	// The log group exists from the moment the function does, which is what
	// `aws logs tail` expects even before the first invocation.
	s.ensureLogGroup(fn.NimbusLogGroup)

	// Starting a container takes seconds and a first pull takes minutes, so
	// the create answers now with State: Pending and the provider waits for
	// Active -- the same shape as an EKS cluster.
	go s.startFunction(fn.FunctionName, image, code)

	writeJSON(w, 201, fn)
	return nil
}

// lambdaImage picks the runtime image, and refuses a runtime nobody mapped
// rather than starting something that cannot work.
func lambdaImage(runtime, imageURI string) (string, *apiError) {
	if imageURI != "" {
		// A container-image function brings its own.
		return imageURI, nil
	}
	if image, ok := runtimeImages[runtime]; ok {
		return image, nil
	}
	known := make([]string, 0, len(runtimeImages))
	for name := range runtimeImages {
		known = append(known, name)
	}
	sort.Strings(known)
	return "", &apiError{Status: 400, Code: "InvalidParameterValueException",
		Message: fmt.Sprintf("Nimbus has no image for the runtime %q. It runs AWS's own base "+
			"images, so the runtimes it knows are: %s", runtime, strings.Join(known, ", "))}
}

// lambdaCode resolves the deployment package, from the request or from our
// own S3 -- which is the whole point of having built S3 first.
func (s *Server) lambdaCode(zipB64, bucket, key string) ([]byte, *apiError) {
	if zipB64 != "" {
		code, err := base64.StdEncoding.DecodeString(zipB64)
		if err != nil {
			return nil, &apiError{Status: 400, Code: "InvalidParameterValueException",
				Message: "Could not unzip uploaded file. Please check your file, then try to upload again."}
		}
		return code, nil
	}
	if bucket != "" && key != "" {
		data, _, ok := s.objects.get(bucket, key)
		if !ok {
			return nil, &apiError{Status: 400, Code: "InvalidParameterValueException",
				Message: "Error occurred while GetObject. S3 Error Code: NoSuchKey"}
		}
		return data, nil
	}
	return nil, &apiError{Status: 400, Code: "InvalidParameterValueException",
		Message: "Code must contain either ZipFile or S3Bucket and S3Key"}
}

// startFunction unpacks the code and brings the container up, moving the
// function to Active or to Failed with the reason attached.
func (s *Server) startFunction(name, image string, code []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	fail := func(reason string) {
		_ = s.store.Update(s.lambdaARN(name), func(body map[string]any) error {
			body["State"] = "Failed"
			body["StateReason"] = reason
			body["StateReasonCode"] = "InternalError"
			return nil
		})
	}

	fn, aerr := s.loadFunction(name)
	if aerr != nil {
		return
	}
	dir := s.lambda.CodeDir(fn.FunctionArn)
	if err := funcs.Unpack(code, dir); err != nil {
		fail(err.Error())
		return
	}
	// The handler is the image's command for every AWS base image.
	command := []string{fn.Handler}
	if fn.Handler == "" {
		command = nil
	}
	running := &funcs.Function{
		ResourceID: fn.FunctionArn,
		Name:       fn.FunctionName,
		Image:      image,
		Command:    command,
		Dir:        dir,
		Env:        s.lambdaEnv(fn),
	}
	if err := s.lambda.Start(ctx, running); err != nil {
		fail(err.Error())
		return
	}
	_ = s.store.Update(fn.FunctionArn, func(body map[string]any) error {
		body["State"] = "Active"
		body["LastUpdateStatus"] = "Successful"
		delete(body, "StateReason")
		delete(body, "StateReasonCode")
		return nil
	})
}

// lambdaEnv is the handler's environment: what the client asked for, plus
// the variables that let a function talk to Nimbus.
//
// AWS_ENDPOINT_URL is the one that matters. Every current SDK reads it, so
// a handler written against real AWS reaches THIS server with no code
// change -- and it points at host.docker.internal, because inside the
// container localhost is the container.
func (s *Server) lambdaEnv(fn *lambdaFunction) map[string]string {
	env := map[string]string{}
	if vars, ok := fn.Environment["Variables"].(map[string]any); ok {
		for k, v := range vars {
			env[k] = str(v)
		}
	}
	endpoint := s.cfg.PublicURL
	endpoint = strings.Replace(endpoint, "localhost", "host.docker.internal", 1)
	endpoint = strings.Replace(endpoint, "127.0.0.1", "host.docker.internal", 1)

	defaults := map[string]string{
		"AWS_ENDPOINT_URL":      endpoint,
		"AWS_DEFAULT_REGION":    s.cfg.Region,
		"AWS_REGION":            s.cfg.Region,
		"AWS_ACCESS_KEY_ID":     "nimbus",
		"AWS_SECRET_ACCESS_KEY": "nimbus",
		// Path-style, because Nimbus's S3 is addressed that way: a
		// bucket.host name would need DNS and a certificate.
		"AWS_S3_FORCE_PATH_STYLE":  "true",
		"AWS_LAMBDA_FUNCTION_NAME": fn.FunctionName,
	}
	// A variable the client set wins: it may be pointing somewhere on
	// purpose, and silently overriding it would be baffling.
	for k, v := range defaults {
		if _, ok := env[k]; !ok {
			env[k] = v
		}
	}
	return env
}

// --- small helpers --------------------------------------------------------

// lambdaTime is the format Lambda stamps LastModified with: RFC3339 with
// milliseconds and a "+0000" offset rather than a "Z".
func lambdaTime() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000-0700")
}

// shutdownLambdas stops every function container this cloud started.
func (s *Server) shutdownLambdas(ctx context.Context) {
	for _, fn := range s.lambda.All() {
		s.lambda.Stop(ctx, fn)
	}
}

// lambdaDir is where function code is kept: beside the rest of the state
// with --persist, in a temporary directory otherwise. A bind mount needs a
// real path either way.
func lambdaDir(dataDir string) string {
	if dataDir != "" {
		return filepath.Join(dataDir, "lambda")
	}
	return filepath.Join(os.TempDir(), "nimbus-aws", "lambda")
}
