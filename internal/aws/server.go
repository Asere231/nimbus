// Package aws emulates the AWS APIs on one endpoint, on top of the same
// store, event bus and visualizer the Azure plugin uses.
//
// The shape of the problem is different from Azure's. There, one protocol
// (ARM over HTTPS) serves every service and the URL says what you want.
// Here, one endpoint serves several protocols -- Query/XML for EC2, IAM
// and STS, REST-XML for S3 -- and the URL frequently says nothing at all:
// every Query request is a POST to "/". The service comes out of the SigV4
// credential scope instead; see sigv4.go.
//
// Plain HTTP, not HTTPS. No AWS client refuses it the way the Azure SDKs
// refuse to talk to anything but TLS, S3 is addressed path-style so no
// bucket hostname needs a certificate, and `aws --endpoint-url
// http://localhost:4702` then works on any machine with nothing trusted.
package aws

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
	"github.com/Asere231/nimbus/internal/engines/funcs"
	"github.com/Asere231/nimbus/internal/engines/k3s"
	"github.com/Asere231/nimbus/internal/engines/vm"
)

// DefaultAccountID is the account every emulated resource belongs to. The
// all-zeroes account is not a real one, which is the point: an ARN from
// Nimbus can never be mistaken for an ARN from AWS.
const DefaultAccountID = "000000000000"

// DefaultRegion is what a client gets when it does not say.
const DefaultRegion = "us-east-1"

// Config is how cmd/nimbus sets the server up.
type Config struct {
	// PublicURL is how clients reach this server, e.g. http://localhost:4702.
	PublicURL string
	// AccountID and Region default to the constants above.
	AccountID string
	Region    string
	// DataDir, when set, is where data planes keep their bytes so they
	// survive a restart: S3 objects under {DataDir}/s3. Empty = memory.
	DataDir string

	// K3sImage and KubernetesVersion are what EKS clusters run and report.
	K3sImage          string
	KubernetesVersion string
	// ClusterPortBase is the first host port tried for a cluster's API.
	// Deliberately above Azure's, so the two clouds' clusters never race
	// for the same port.
	ClusterPortBase int
	// ClusterTimeout bounds how long a cluster may take to become ready.
	ClusterTimeout time.Duration
	// KeepClusters leaves containers running when Nimbus stops.
	KeepClusters bool
	// FunctionPortBase is the first host port tried for a Lambda
	// container's runtime emulator.
	FunctionPortBase int

	// EnforceIAM turns authorization on. With it off nothing is ever
	// refused, which is how every cloud here behaved before Milestone 13
	// and is still the default: existing examples must apply unchanged.
	EnforceIAM bool
	// Principal is who an unsigned or unmapped request is taken to be.
	// Empty means the account root, which is allowed everything -- see
	// principal.go for why that is the faithful answer and not a fudge.
	Principal string
}

// DefaultClusterPortBase starts above the range AKS uses.
const DefaultClusterPortBase = 6600

// DefaultFunctionPortBase is where Lambda containers start, well clear of
// the cluster range so a busy machine does not interleave them.
const DefaultFunctionPortBase = 9100

// Server is the AWS emulator.
type Server struct {
	cfg     Config
	store   *core.Store
	bus     *core.Bus
	objects *objectStore
	// uploads holds multipart uploads in flight. Beside the object store
	// rather than in it: an upload is not an object until it completes.
	uploads *multipartStore
	// items holds DynamoDB's rows, beside the tables themselves in the
	// store -- a table is a resource, its items are data.
	items *itemStore
	// lambda runs the functions, and logs holds what they printed.
	lambda *funcs.Engine
	logs   *logStore
	// k3s runs the EKS clusters. The same engine Azure's AKS uses, which
	// knows about neither -- see internal/engines/k3s.
	k3s *k3s.Engine
	// vms runs the EC2 instances, on the engine every cloud's instances
	// use. vmMu is held across check-and-act on one: a boot or a stop that
	// overtakes a terminate leaves a container running after a destroy
	// reported success, which is the lost update at engine level.
	vms  *vm.Engine
	vmMu sync.Mutex
	// policies compiles IAM documents once per distinct document set.
	// Keyed by the documents themselves, so there is no invalidation to
	// get wrong -- see authz_iam.go.
	policies *policyCache
}

// New builds the server and makes sure the account exists.
func New(cfg Config, store *core.Store, bus *core.Bus) *Server {
	if cfg.AccountID == "" {
		cfg.AccountID = DefaultAccountID
	}
	if cfg.Region == "" {
		cfg.Region = DefaultRegion
	}
	if cfg.ClusterPortBase == 0 {
		cfg.ClusterPortBase = DefaultClusterPortBase
	}
	s := &Server{cfg: cfg, store: store, bus: bus,
		objects: newObjectStore(cfg.DataDir), items: newItemStore(cfg.DataDir),
		uploads: newMultipartStore(),
		logs:    newLogStore(), policies: newPolicyCache()}
	s.lambda = funcs.New(funcs.Config{
		NamePrefix:   "nimbus-lambda",
		PortBase:     orZero(cfg.FunctionPortBase, DefaultFunctionPortBase),
		KeepRunning:  cfg.KeepClusters,
		StartTimeout: cfg.ClusterTimeout,
		Dir:          lambdaDir(cfg.DataDir),
	}, docker.NewCLI())
	s.vms = vm.New(vm.Config{
		NamePrefix:   "nimbus-ec2",
		KeepRunning:  cfg.KeepClusters,
		StartTimeout: cfg.ClusterTimeout,
	}, docker.NewCLI())
	k3sCfg := k3s.Config{
		Image:       cfg.K3sImage,
		Version:     cfg.KubernetesVersion,
		PortBase:    cfg.ClusterPortBase,
		Timeout:     cfg.ClusterTimeout,
		KeepRunning: cfg.KeepClusters,
		NamePrefix:  "nimbus-eks",
	}
	// The token webhook is what makes `aws eks update-kubeconfig` produce
	// a kubeconfig that works; see eks_auth.go for why nothing simpler
	// does. The file has to be mounted rather than written afterwards:
	// k3s reads its arguments at startup, so there is no moment between
	// the container existing and needing the file.
	if path, err := s.writeTokenWebhookConfig(cfg); err == nil {
		k3sCfg.ServerArgs = []string{
			"--kube-apiserver-arg=--authentication-token-webhook-config-file=" + webhookPathInContainer,
			"--kube-apiserver-arg=--authentication-token-webhook-version=v1",
		}
		k3sCfg.ServerVolumes = map[string]string{path: webhookPathInContainer}
	} else {
		log.Printf("nimbus: EKS token webhook disabled: %v", err)
	}
	s.k3s = k3s.New(k3sCfg, docker.NewCLI(), store, "aws")
	s.objects.load()
	s.items.load()
	s.ensureAccount()
	return s
}

// Start runs the background workers and returns the function that stops
// them: reattaching clusters a previous Nimbus left running, then watching
// what is inside them.
func (s *Server) Start() func() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		reattachCtx, done := context.WithTimeout(ctx, 2*time.Minute)
		for _, found := range s.k3s.Reattach(reattachCtx, s.store.Exists) {
			s.setClusterStatus(found.Cluster.ResourceID, found)
		}
		done()
		s.k3s.Watch(ctx)
	}()
	return cancel
}

// setClusterStatus records what reattaching found. A container that is
// gone or unreachable means a FAILED cluster: the resource is what the
// provider reads, so it has to say so rather than claim ACTIVE.
func (s *Server) setClusterStatus(clusterARN string, found k3s.Found) {
	_ = s.store.Update(clusterARN, func(body map[string]any) error {
		if found.Ready {
			body["status"] = "ACTIVE"
			body["endpoint"] = fmt.Sprintf("https://127.0.0.1:%d", found.Cluster.Port)
			body["certificateAuthority"] = map[string]any{
				"data": base64.StdEncoding.EncodeToString(found.Cluster.CA),
			}
		} else {
			body["status"] = "FAILED"
		}
		return nil
	})
}

// Shutdown stops the clusters this cloud started, unless they are meant to
// outlive Nimbus.
func (s *Server) Shutdown(ctx context.Context) {
	if s.cfg.KeepClusters {
		return
	}
	for _, cl := range s.k3s.All() {
		s.k3s.Stop(ctx, cl)
	}
	s.shutdownLambdas(ctx)
}

// UseDocker swaps the Docker client, for tests.
func (s *Server) UseDocker(d docker.Client) {
	s.k3s.SetDocker(d)
	s.lambda.SetDocker(d)
}

// K3s exposes the engine, which the EKS tests drive directly.
func (s *Server) K3s() *k3s.Engine { return s.k3s }

// AccountID is the account this server issues ARNs for.
func (s *Server) AccountID() string { return s.cfg.AccountID }

// ServeHTTP routes every request and records it on the event bus, the same
// way the Azure server does, so both clouds fill one activity feed.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: 200}
	note := s.route(rec, r)
	s.bus.Publish(core.Event{
		Kind:       "request",
		Action:     "call",
		Cloud:      "aws",
		Method:     r.Method,
		Path:       r.URL.Path,
		Status:     rec.status,
		DurationMs: float64(time.Since(start).Microseconds()) / 1000,
		Note:       note,
	})
}

// route dispatches on the service in the credential scope, and returns the
// note that goes in the activity feed.
func (s *Server) route(w http.ResponseWriter, r *http.Request) string {
	// The token-review webhook is Nimbus's own, not an AWS API: it is
	// called by a cluster's API server, which signs nothing, so it has to
	// be matched before anything looks at a credential scope.
	if r.URL.Path == TokenReviewPath {
		return s.serveTokenReview(w, r)
	}
	service := serviceOf(r)

	// Authorization, in one place in front of every service rather than
	// as a call each handler has to remember -- a handler that forgets to
	// ask would be a silent grant. With --enforce-iam off this returns
	// immediately and nothing below can tell the difference.
	//
	// The body is buffered first for everything except S3, because
	// readParams and readJSON consume it and the check needs to see it
	// too; S3's bodies are the large ones and it needs none of it to know
	// what is being asked. See authz_request.go.
	if s.cfg.EnforceIAM {
		var buffered []byte
		if service != "s3" {
			var err error
			if buffered, err = bufferBody(r); err != nil {
				writeError(w, service, errorf(400, "MalformedInput", "reading the request: %v", err))
				return "unreadable request body"
			}
		}
		denial, note := s.authorize(r, service)
		// Put the body back for the handler: working out which permission
		// the request needs reads it to EOF.
		restoreBody(r, buffered)
		if denial != nil {
			// A HEAD response carries NO BODY, by HTTP's own rule, so the
			// error document is discarded and the SDK reports only
			// "Forbidden: Forbidden" -- measured against aws provider
			// 5.100.0, where a denied HeadObject during a refresh says
			// exactly that and nothing about which permission was
			// missing. Real S3 is no better. The reason is in the
			// activity feed, and this header is the only way it can
			// reach a person reading the response itself.
			w.Header().Set(deniedReasonHeader, denial.Message)
			s.writeServiceError(w, service, denial)
			return note
		}
	}

	switch service {
	case "sts":
		return s.serveSTS(w, r)
	case "ec2":
		return s.serveEC2(w, r)
	case "s3":
		return s.serveS3(w, r)
	case "iam":
		return s.serveIAM(w, r)
	case "eks":
		return s.serveEKS(w, r)
	case "dynamodb":
		return s.serveDynamoDB(w, r)
	case "lambda":
		return s.serveLambda(w, r)
	case "logs":
		return s.serveLogs(w, r)
	default:
		// A service nobody has built. 400 rather than 501, because
		// measured against aws-cli a 5xx is retried five times with
		// backoff: one legible line in the activity feed instead of five,
		// and the feed stays the to-do list.
		writeError(w, "", errorf(400, "InvalidAction",
			"Nimbus does not emulate the %q service", service))
		return "unrecognised service " + service
	}
}

// actionOf is the Action parameter, for the feed's benefit. A request whose
// body cannot be read at all still gets a name.
func actionOf(r *http.Request) string {
	if r.Method == http.MethodGet || r.Method == http.MethodPut || r.Method == http.MethodHead {
		// REST services say what they want with the method and the path.
		return r.Method + " " + r.URL.Path
	}
	p, err := readParams(r)
	if err != nil || p.str("Action") == "" {
		return r.Method + " " + r.URL.Path
	}
	return p.str("Action")
}

// ensureAccount puts the account in the store, so the graph has a root to
// hang VPCs and buckets off. Azure does the same with its subscription.
func (s *Server) ensureAccount() {
	id := s.accountARN()
	if s.store.Exists(id) {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"id":        id,
		"accountId": s.cfg.AccountID,
		"name":      "Nimbus Local",
		"region":    s.cfg.Region,
		"arn":       id,
	})
	s.store.Put(&core.Resource{
		Cloud: "aws", ID: id, Type: "AWS::Account",
		Name: s.cfg.AccountID, Backing: "state", Body: body,
	})
}

func (s *Server) accountARN() string {
	return "arn:aws:iam::" + s.cfg.AccountID + ":root"
}

// statusRecorder remembers the status for the activity feed.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func sprintf(format string, args ...any) string {
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}
