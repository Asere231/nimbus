// Package azure emulates the Azure Resource Manager (ARM) control plane plus
// the small identity surface that SDKs and Terraform need to get a token.
package azure

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/azure/storage"
	"github.com/Asere231/nimbus/internal/core"
)

// Config for the Azure emulator.
type Config struct {
	// PublicURL is how clients reach this server, e.g. https://localhost:4701.
	// It is returned as the login and Resource Manager endpoint in metadata.
	PublicURL string
	TenantID  string
	ClientID  string
}

// Server is the Azure plugin.
type Server struct {
	cfg     Config
	store   *core.Store
	bus     *core.Bus
	hooks   map[string]Hook
	storage *storage.Handler
}

// New creates the Azure emulator.
func New(cfg Config, store *core.Store, bus *core.Bus) *Server {
	if cfg.TenantID == "" {
		cfg.TenantID = DefaultTenantID
	}
	if cfg.ClientID == "" {
		cfg.ClientID = DefaultClientID
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	s := &Server{
		cfg: cfg, store: store, bus: bus,
		hooks:   map[string]Hook{},
		storage: storage.NewHandler(storage.NewStore()),
	}
	registerNetworkHooks(s)
	registerStorageHooks(s)
	return s
}

// Well-known IDs the emulator hands out. Any values work for clients; these
// are just stable defaults so examples can hard-code them.
const (
	DefaultTenantID       = "00000000-0000-0000-0000-000000000001"
	DefaultClientID       = "00000000-0000-0000-0000-000000000002"
	DefaultSubscriptionID = "00000000-0000-0000-0000-000000000000"
	DefaultObjectID       = "00000000-0000-0000-0000-000000000003"
)

// storageSuffix is the DNS suffix for the storage data plane, including our
// port. Clients join it as {account}.{service}.{suffix}; see the comment in
// handleMetadata for why the port belongs in it.
func (s *Server) storageSuffix() string {
	suffix := core.StorageSuffix
	if u, err := url.Parse(s.cfg.PublicURL); err == nil {
		if p := u.Port(); p != "" {
			suffix += ":" + p
		}
	}
	return suffix
}

// ServeHTTP routes every request and records it on the event bus.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: 200}
	note := s.route(rec, r)
	s.bus.Publish(core.Event{
		Kind:       "request",
		Action:     "call",
		Cloud:      "azure",
		Method:     r.Method,
		Path:       r.URL.Path,
		Status:     rec.status,
		DurationMs: float64(time.Since(start).Microseconds()) / 1000,
		Note:       note,
	})
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) (note string) {
	// The storage data plane shares this listener and is told apart by Host:
	// {account}.{service}.storage.nimbus.localhost. ARM requests arrive on the
	// plain host, so this never shadows them.
	if t, ok := storage.ParseHost(r.Host, s.storageSuffix()); ok {
		return s.storage.Serve(w, r, t)
	}

	p := r.URL.Path
	switch {
	case p == "/metadata/endpoints":
		s.handleMetadata(w, r)
		return "metadata"
	case strings.HasSuffix(p, "/oauth2/v2.0/token") || strings.HasSuffix(p, "/oauth2/token"):
		s.handleToken(w, r)
		return "token issued"
	case strings.HasSuffix(p, "/.well-known/openid-configuration"):
		s.handleOpenID(w, r)
		return "openid configuration"
	case strings.HasSuffix(p, "/discovery/instance"):
		s.handleInstanceDiscovery(w, r)
		return "instance discovery"
	case isOperationPath(p):
		// Async operation status: every operation completes immediately.
		writeJSON(w, 200, map[string]any{"id": p, "name": p[strings.LastIndex(p, "/")+1:], "status": "Succeeded"})
		return ""
	case p == "/" || p == "":
		writeJSON(w, 200, map[string]any{"emulator": "nimbus", "cloud": "azure", "resourceManager": s.cfg.PublicURL})
		return ""
	}
	return s.handleARM(w, r)
}

// --- helpers ---------------------------------------------------------------

const operationsPath = "/providers/Microsoft.Resources/operations/nimbus-"

func isOperationPath(p string) bool {
	return strings.Contains(strings.ToLower(p), strings.ToLower(operationsPath))
}

// markAsync tells the client this was a long-running operation and where to
// poll. Every write in Nimbus finishes before it is answered, so the status
// URL always says Succeeded; Retry-After: 1 keeps clients from waiting the
// SDK default of 10 seconds per resource.
func (s *Server) markAsync(w http.ResponseWriter, t target) {
	op := fmt.Sprintf("%s/subscriptions/%s%s%s?api-version=2020-01-01", s.cfg.PublicURL, t.Subscription, operationsPath, newUUID())
	w.Header().Set("Azure-AsyncOperation", op)
	w.Header().Set("Retry-After", "1")
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("x-ms-request-id", newUUID())
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// writeError writes an ARM-shaped error, which SDKs parse into readable messages.
func writeError(w http.ResponseWriter, status int, code, format string, args ...any) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": fmt.Sprintf(format, args...)},
	})
}

func readBody(r *http.Request) (map[string]any, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("request body is not a JSON object: %w", err)
	}
	return m, nil
}
