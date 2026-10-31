package storage

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Service is one of the storage services a data-plane host can address.
type Service string

const (
	ServiceBlob  Service = "blob"
	ServiceQueue Service = "queue"
	ServiceTable Service = "table"
	ServiceFile  Service = "file"
)

// Target is what a data-plane Host header points at.
type Target struct {
	Account string
	Service Service
}

// ParseHost splits a data-plane host into account and service.
//
// Clients address storage as {account}.{service}.{suffix}, e.g.
// "stnimbus.blob.storage.nimbus.localhost:4701". ok is false for anything that
// is not such a name -- those are ARM requests and must be left alone.
//
// suffix may carry a port (ours does; see the metadata comment), and the Host
// header may carry one whether or not the suffix does, so both are compared
// without it.
func ParseHost(host, suffix string) (Target, bool) {
	h := strings.ToLower(stripPort(host))
	sfx := strings.ToLower(stripPort(suffix))
	if !strings.HasSuffix(h, "."+sfx) {
		return Target{}, false
	}
	// What is left is "{account}.{service}".
	head := strings.TrimSuffix(h, "."+sfx)
	dot := strings.LastIndex(head, ".")
	if dot <= 0 || dot == len(head)-1 {
		return Target{}, false
	}
	account, svc := head[:dot], Service(head[dot+1:])
	switch svc {
	case ServiceBlob, ServiceQueue, ServiceTable, ServiceFile:
	default:
		return Target{}, false
	}
	if strings.Contains(account, ".") { // only one label of account name
		return Target{}, false
	}
	return Target{Account: account, Service: svc}, true
}

func stripPort(h string) string {
	if i := strings.LastIndex(h, ":"); i != -1 && !strings.Contains(h[i:], "]") {
		return h[:i]
	}
	return h
}

// Handler serves the storage data plane.
type Handler struct {
	store *Store
	// OnChange, if set, is called when a container is created, deleted or has
	// its contents change, so the visualizer can mirror containers as
	// resources without this package knowing anything about core.Store.
	OnChange func(account, container, action string)
}

// NewHandler creates the data-plane handler over the given store.
func NewHandler(store *Store) *Handler { return &Handler{store: store} }

func (h *Handler) changed(account, container, action string) {
	if h.OnChange != nil {
		h.OnChange(account, container, action)
	}
}

// persist writes the account to disk if persistence is on. Called after any
// mutation, so a restart with --persist finds exactly what was there.
func (h *Handler) persist(a *Account) { h.store.Save(a) }

// Store exposes the underlying data store, so the ARM hook can create and
// delete accounts alongside the control-plane resource.
func (h *Handler) Store() *Store { return h.store }

// Serve handles one data-plane request. It returns a short note for the
// activity feed, so unimplemented operations are visible in the visualizer
// while the service is being built out.
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, t Target) string {
	a := h.store.Account(t.Account)
	if a == nil {
		// What Azure returns for a host whose account does not exist.
		WriteError(w, http.StatusNotFound, "AccountNotFound",
			"The specified account does not exist.")
		return "unknown storage account: " + t.Account
	}

	// Shared Key is verified for real: a wrong key must fail here exactly as
	// it would against Azure.
	if res, msg := Authenticate(r, a, t.Service); res == AuthFailed {
		WriteError(w, http.StatusForbidden, "AuthenticationFailed",
			"Server failed to authenticate the request. "+msg)
		return "storage auth failed: " + t.Account
	}

	switch t.Service {
	case ServiceBlob:
		return h.serveBlob(w, r, a)
	case ServiceQueue:
		q := r.URL.Query()
		if q.Get("restype") == "service" {
			return h.serviceProperties(w, r, a, t.Service, q.Get("comp"))
		}
		return h.serveQueue(w, r, a)
	case ServiceTable:
		q := r.URL.Query()
		if q.Get("restype") == "service" {
			return h.serviceProperties(w, r, a, t.Service, q.Get("comp"))
		}
		return h.serveTable(w, r, a)
	case ServiceFile:
		// azurerm reads queue service properties immediately after creating an
		// account, alongside the blob ones, and will not finish the apply
		// without them. The settings document has the same shape for every
		// service, so the blob handler serves them all; the queue and table
		// operations proper are built out separately.
		q := r.URL.Query()
		if q.Get("restype") == "service" {
			return h.serviceProperties(w, r, a, t.Service, q.Get("comp"))
		}
		return h.serveOther(w, r, a, t.Service)
	}

	WriteError(w, http.StatusNotImplemented, "FeatureNotYetImplemented",
		fmt.Sprintf("Nimbus does not implement the %s service yet.", t.Service))
	return fmt.Sprintf("unimplemented storage service: %s %s %s%s", t.Service, r.Method, r.URL.Path, queryNote(r))
}

func queryNote(r *http.Request) string {
	if q := r.URL.RawQuery; q != "" {
		return "?" + q
	}
	return ""
}

// storageError is the XML body Azure returns for a failed data-plane call.
// Storage speaks XML, not the JSON that ARM uses.
type storageError struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

// WriteError writes an Azure-shaped storage error.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-ms-error-code", code)
	setCommonHeaders(w)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(storageError{Code: code, Message: message})
}

// APIVersion is the x-ms-version Nimbus reports. Clients send their own and
// expect it echoed; this is a recent one that the SDKs accept.
const APIVersion = "2025-05-05"

func setCommonHeaders(w http.ResponseWriter) {
	w.Header().Set("x-ms-version", APIVersion)
	w.Header().Set("x-ms-request-id", newRequestID())
	w.Header().Set("Server", "Nimbus")
}

// --- visualizer support ----------------------------------------------------

// BlobSummary is one blob as the visualizer sees it.
type BlobSummary struct {
	Name        string
	Size        int64
	ContentType string
	Modified    time.Time
}

// ListBlobs returns the blobs in a container, for the inspector.
func (h *Handler) ListBlobs(account, container string) []BlobSummary {
	a := h.store.Account(account)
	if a == nil {
		return nil
	}
	c := a.Container(container)
	if c == nil {
		return nil
	}
	out := []BlobSummary{}
	for _, b := range c.Blobs("") {
		out = append(out, BlobSummary{
			Name: b.Name, Size: int64(len(b.Content)),
			ContentType: contentTypeOf(b), Modified: b.Modified,
		})
	}
	return out
}

// ReadBlob returns a blob's bytes, for the inspector's download link.
func (h *Handler) ReadBlob(account, container, name string) ([]byte, string, bool) {
	a := h.store.Account(account)
	if a == nil {
		return nil, "", false
	}
	c := a.Container(container)
	if c == nil {
		return nil, "", false
	}
	b := c.Blob(name)
	if b == nil {
		return nil, "", false
	}
	return b.Content, contentTypeOf(b), true
}
