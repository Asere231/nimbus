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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/core"
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
}

// Server is the GCP emulator.
type Server struct {
	cfg     Config
	store   *core.Store
	bus     *core.Bus
	objects *objectStore
	// operations are kept here rather than in the store; see
	// operations.go for why.
	operations *operations
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
	s := &Server{cfg: cfg, store: store, bus: bus,
		objects: newObjectStore(cfg.DataDir), operations: newOperations()}
	s.objects.load()
	s.ensureProject()
	return s
}

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

// route dispatches on the first path segment, which names the API.
//
// The versions are part of the route rather than parameters: `/compute/v1`,
// `/storage/v1`, `/v1` for IAM and the older Resource Manager, `/v3` for the
// newer one. A client's *_custom_endpoint setting already carries the
// version for some services and not for others -- iam_custom_endpoint
// includes it, compute_custom_endpoint does not -- so both spellings are
// accepted rather than insisting on the tidy one.
func (s *Server) route(w http.ResponseWriter, r *http.Request) string {
	parts := pathParts(r.URL.Path)
	if len(parts) == 0 {
		writeError(w, notFound("the request path is empty"))
		return "empty path"
	}

	switch parts[0] {
	case "compute":
		return s.serveCompute(w, r, parts)
	case "storage":
		return s.serveStorage(w, r, parts)
	case "upload":
		// Uploads live under their own prefix: /upload/storage/v1/b/{b}/o.
		return s.serveStorage(w, r, parts[1:])
	case "v1", "v1beta1":
		// Shared by IAM and the older Resource Manager, told apart by what
		// comes after the project.
		if containsSegment(parts, "serviceAccounts") {
			return s.serveIAM(w, r, parts)
		}
		return s.serveResourceManager(w, r, parts)
	case "v3":
		return s.serveResourceManager(w, r, parts)
	case "iam":
		return s.serveIAM(w, r, parts[1:])
	default:
		// 404 with the path named, so the activity feed reads as the to-do
		// list for the next checkpoint -- the same way AWS's InvalidAction
		// does.
		e := notFound(fmt.Sprintf("Nimbus does not emulate %s %s yet", r.Method, r.URL.Path))
		writeError(w, e)
		return "unrecognised API " + parts[0]
	}
}

func containsSegment(parts []string, want string) bool {
	for _, p := range parts {
		if p == want {
			return true
		}
	}
	return false
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
