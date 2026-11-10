// Package gcp emulates the Google Cloud APIs on one endpoint, on top of the
// same store, event bus and visualizer the other two clouds use.
//
// After Azure's one protocol and AWS's four, GCP is the straightforward
// one: REST-JSON everywhere, and the PATH says which service and which
// resource. No signature has to be parsed to find out what was asked --
// `/compute/v1/projects/{p}/global/networks/{n}` says it.
//
// Three things are different enough to be worth stating up front, all of
// them measured against the google provider 6.x rather than read in the
// documentation:
//
//   - **The provider makes NO startup calls.** azurerm probes, the aws
//     provider reads three VPC attributes nobody asked about; google goes
//     straight to the resource it was told to create. A whole apply of five
//     resources is fifteen requests.
//
//   - **An operation that is already DONE is never polled.** Compute writes
//     answer with an Operation object; the provider reads its `status`, and
//     when that says DONE on the spot it never fetches the operation URL at
//     all. The endpoint still exists, because a client that does poll must
//     find it, but the fast path needs nothing.
//
//   - **Identity is a self link**, a full URL, which is what goes in the
//     store as the resource ID -- as core.Resource's own comment has said
//     since Milestone 1.
//
// Plain HTTP, like AWS. The provider's *_custom_endpoint settings and
// gcloud's api_endpoint_overrides both take any URL, buckets are addressed
// by path (/storage/v1/b/{bucket}) so no name needs a certificate, and
// nothing has to be trusted on the machine.
package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
	"github.com/Asere231/nimbus/internal/engines/k3s"
	"github.com/Asere231/nimbus/internal/engines/run"
	"github.com/Asere231/nimbus/internal/engines/vm"
)

// DefaultProjectID is the project everything belongs to when a client does
// not say. Not a real project id, which is the point.
const DefaultProjectID = "nimbus-local"

// DefaultProjectNumber is the numeric id GCP gives a project. It comes back
// as a STRING in most APIs and as a number in a few, which is a trap of its
// own; see numberString.
const DefaultProjectNumber = "100000000001"

// DefaultRegion and DefaultZone are what a client gets when it does not say.
const (
	DefaultRegion = "us-central1"
	DefaultZone   = "us-central1-a"
)

// Config is how cmd/nimbus sets the server up.
type Config struct {
	// PublicURL is how clients reach this server, e.g. http://localhost:4703.
	// Every self link Nimbus hands out is built from it, so a client that
	// follows one comes back here.
	PublicURL string
	// ProjectID, Region and Zone default to the constants above.
	ProjectID string
	Region    string
	Zone      string
	// DataDir, when set, is where Cloud Storage keeps object bytes so they
	// survive a restart. Empty = memory.
	DataDir string

	// K3sImage and KubernetesVersion are what GKE clusters run and report.
	K3sImage          string
	KubernetesVersion string
	// ClusterPortBase is the first host port tried for a cluster's API
	// server. Above AWS's 6600, so three clouds' clusters never race for
	// the same port in one process.
	ClusterPortBase int
	// ServicePortBase is the first host port tried for a Cloud Run
	// container, clear of Lambda's range for the same reason.
	ServicePortBase int
	// ClusterTimeout bounds how long a cluster may take to become ready.
	ClusterTimeout time.Duration
	// KeepClusters leaves containers running when Nimbus stops.
	KeepClusters bool
}

// Port bases, each above the cloud before it.
const (
	DefaultClusterPortBase = 6700
	DefaultServicePortBase = 9300
)

// Server is the GCP emulator.
type Server struct {
	cfg     Config
	store   *core.Store
	bus     *core.Bus
	objects *objectStore
	// operations are kept here rather than in the store; see
	// operations.go for why. lros are the other kind -- see lro.go.
	operations *operations
	lros       *lros
	// messages holds Pub/Sub's, beside the store rather than in it: a
	// topic is a resource, the messages on it are data.
	messages *messageStore
	// k3s runs the GKE clusters, services the Cloud Run containers. Both
	// engines are shared with the other clouds and know about none of them.
	k3s      *k3s.Engine
	services *run.Engine
	// vms runs the Compute instances, on the engine every cloud's
	// instances use.
	vms *vm.Engine
	// gkeMu serialises everything that starts or stops a cluster's
	// containers. See rescale in gke_nodepool.go for the race it closes.
	gkeMu sync.Mutex
	// vmMu does the same for an instance: a boot or a stop that overtakes
	// a delete leaves a container running after a destroy reported
	// success.
	vmMu sync.Mutex
}

// New builds the server and makes sure the project exists.
func New(cfg Config, store *core.Store, bus *core.Bus) *Server {
	if cfg.ProjectID == "" {
		cfg.ProjectID = DefaultProjectID
	}
	if cfg.Region == "" {
		cfg.Region = DefaultRegion
	}
	if cfg.Zone == "" {
		cfg.Zone = DefaultZone
	}
	if cfg.ClusterPortBase == 0 {
		cfg.ClusterPortBase = DefaultClusterPortBase
	}
	if cfg.ServicePortBase == 0 {
		cfg.ServicePortBase = DefaultServicePortBase
	}
	s := &Server{cfg: cfg, store: store, bus: bus,
		objects: newObjectStore(cfg.DataDir), operations: newOperations(),
		lros: newLROs(), messages: newMessageStore()}

	k3sCfg := k3s.Config{
		Image:       cfg.K3sImage,
		Version:     cfg.KubernetesVersion,
		PortBase:    cfg.ClusterPortBase,
		Timeout:     cfg.ClusterTimeout,
		KeepRunning: cfg.KeepClusters,
		NamePrefix:  "nimbus-gke",
	}
	// The same token webhook EKS uses, for the same reason: a bearer token
	// a client builds for itself can never be predicted here, so the only
	// mechanism that works is the one real GKE and real EKS both use.
	// See gke_auth.go.
	if path, err := s.writeTokenWebhookConfig(cfg); err == nil {
		k3sCfg.ServerArgs = []string{
			"--kube-apiserver-arg=--authentication-token-webhook-config-file=" + webhookPathInContainer,
			"--kube-apiserver-arg=--authentication-token-webhook-version=v1",
		}
		k3sCfg.ServerVolumes = map[string]string{path: webhookPathInContainer}
	} else {
		log.Printf("nimbus: GKE token webhook disabled: %v", err)
	}
	s.k3s = k3s.New(k3sCfg, docker.NewCLI(), store, "gcp")
	s.services = run.New(run.Config{
		NamePrefix:   "nimbus-run",
		PortBase:     cfg.ServicePortBase,
		KeepRunning:  cfg.KeepClusters,
		StartTimeout: cfg.ClusterTimeout,
	}, docker.NewCLI())

	s.vms = vm.New(vm.Config{
		NamePrefix:   "nimbus-gce",
		KeepRunning:  cfg.KeepClusters,
		StartTimeout: cfg.ClusterTimeout,
	}, docker.NewCLI())

	s.objects.load()
	s.ensureProject()
	return s
}

// Start runs the background workers and returns the function that stops
// them -- reattaching clusters a previous Nimbus left running, then
// watching what is inside them. The same shape the AWS server has.
func (s *Server) Start() func() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		reattachCtx, done := context.WithTimeout(ctx, 2*time.Minute)
		for _, found := range s.k3s.Reattach(reattachCtx, s.store.Exists) {
			s.setGKEStatus(found)
		}
		done()
		s.k3s.Watch(ctx)
	}()
	return cancel
}

// Shutdown stops what this cloud started, unless it is meant to outlive us.
func (s *Server) Shutdown(ctx context.Context) {
	if s.cfg.KeepClusters {
		return
	}
	for _, cl := range s.k3s.All() {
		s.k3s.Stop(ctx, cl)
	}
	for _, svc := range s.services.All() {
		s.services.Stop(ctx, svc)
	}
}

// UseDocker swaps the Docker client, for tests.
func (s *Server) UseDocker(d docker.Client) {
	s.k3s.SetDocker(d)
	s.services.SetDocker(d)
	s.vms.SetDocker(d)
}

// K3s exposes the engine, which the GKE tests drive directly.
func (s *Server) K3s() *k3s.Engine { return s.k3s }

// Services exposes the Cloud Run engine, likewise.
func (s *Server) Services() *run.Engine { return s.services }

// VMs exposes the instance engine, likewise.
func (s *Server) VMs() *vm.Engine { return s.vms }

// ProjectID is the project this server serves.
func (s *Server) ProjectID() string { return s.cfg.ProjectID }

// ServeHTTP routes every request and records it on the event bus, the same
// way the other two clouds do, so all three fill one activity feed.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: 200}
	note := s.route(rec, r)
	s.bus.Publish(core.Event{
		Kind:       "request",
		Action:     "call",
		Cloud:      "gcp",
		Method:     r.Method,
		Path:       r.URL.Path,
		Status:     rec.status,
		DurationMs: float64(time.Since(start).Microseconds()) / 1000,
		Note:       note,
	})
}

// route sends a request to the API that owns it; router.go decides which
// that is, and its table is where the reasoning lives.
//
// Two things are matched before any of that, because neither is a Google
// API and neither carries a path a client's endpoint setting produced: the
// GKE token-review webhook, called by a cluster's own API server, and the
// Cloud Run proxy, called by anything holding a service's URL.
func (s *Server) route(w http.ResponseWriter, r *http.Request) string {
	if note, handled := s.routeNimbusOwn(w, r); handled {
		return note
	}

	parts := pathParts(r.URL.Path)
	if len(parts) == 0 {
		writeError(w, notFound("the request path is empty"))
		return "empty path"
	}

	switch apiFor(parts) {
	case apiCompute:
		return s.serveCompute(w, r, parts)
	case apiStorage:
		if parts[0] == "upload" {
			return s.serveStorage(w, r, parts[1:])
		}
		return s.serveStorage(w, r, parts)
	case apiIAM:
		if parts[0] == "iam" {
			return s.serveIAM(w, r, parts[1:])
		}
		return s.serveIAM(w, r, parts)
	case apiResourceManager:
		return s.serveResourceManager(w, r, parts)
	case apiGKE:
		return s.serveGKE(w, r, parts)
	case apiPubSub:
		return s.servePubSub(w, r, parts)
	case apiRun:
		return s.serveRun(w, r, parts)
	case apiKnative:
		return s.serveKnative(w, r, parts)
	default:
		// 404 with the path named, so the activity feed reads as the to-do
		// list for the next checkpoint -- the same way AWS's InvalidAction
		// does.
		e := notFound(fmt.Sprintf("Nimbus does not emulate %s %s yet", r.Method, r.URL.Path))
		writeError(w, e)
		return "unrecognised API " + parts[0]
	}
}

// routeNimbusOwn handles the paths that are Nimbus's rather than Google's.
// They are prefixed so they can never collide with a real API path.
func (s *Server) routeNimbusOwn(w http.ResponseWriter, r *http.Request) (string, bool) {
	switch {
	case r.URL.Path == TokenReviewPath:
		return s.serveTokenReview(w, r), true
	case strings.HasPrefix(r.URL.Path, RunProxyPrefix):
		return s.serveRunProxy(w, r), true
	}
	return "", false
}

// ensureProject puts the project in the store, so the graph has a root to
// hang networks and buckets off. Azure does the same with its subscription
// and AWS with its account -- and all three have to exist before the first
// request, because a client that lists them and finds nothing invents
// something worse.
func (s *Server) ensureProject() {
	id := s.projectSelfLink()
	if s.store.Exists(id) {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"kind":           "cloudresourcemanager#project",
		"projectId":      s.cfg.ProjectID,
		"projectNumber":  DefaultProjectNumber,
		"name":           "Nimbus Local",
		"lifecycleState": "ACTIVE",
		"createTime":     timestamp(),
		"selfLink":       id,
	})
	s.store.Put(&core.Resource{
		Cloud: "gcp", ID: id, Type: TypeProject, Name: s.cfg.ProjectID,
		Backing: "state", Body: body,
	})
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

// timestamp is RFC3339 with milliseconds, which is how GCP stamps
// createTime and creationTimestamp.
func timestamp() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// lastSegment is the resource's own name out of a path or a self link.
func lastSegment(s string) string {
	s = strings.TrimSuffix(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}
