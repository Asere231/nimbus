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
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Asere231/nimbus/internal/core"
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
}

// Server is the AWS emulator.
type Server struct {
	cfg   Config
	store *core.Store
	bus   *core.Bus
}

// New builds the server and makes sure the account exists.
func New(cfg Config, store *core.Store, bus *core.Bus) *Server {
	if cfg.AccountID == "" {
		cfg.AccountID = DefaultAccountID
	}
	if cfg.Region == "" {
		cfg.Region = DefaultRegion
	}
	s := &Server{cfg: cfg, store: store, bus: bus}
	s.ensureAccount()
	return s
}

// Start runs any background workers and returns the function that stops
// them. Nothing needs one yet; the signature matches the Azure plugin's so
// cmd/nimbus treats both the same way.
func (s *Server) Start() func() { return func() {} }

// Shutdown stops anything this cloud is running. Nothing yet.
func (s *Server) Shutdown(context.Context) {}

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
	service := serviceOf(r)
	switch service {
	case "sts":
		return s.serveSTS(w, r)
	case "iam", "ec2", "s3":
		// Built in later checkpoints. Answering 400 rather than 501 keeps
		// the client from retrying five times over something that will
		// never succeed, and puts one legible line in the feed.
		action := actionOf(r)
		writeError(w, service, invalidAction(service, action))
		return fmt.Sprintf("unimplemented: %s %s", service, action)
	default:
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
