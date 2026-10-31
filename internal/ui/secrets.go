package ui

import (
	"net/http"
	"time"
)

// SecretBrowser is the slice of a secret store the visualizer needs.
// Declared here as an interface for the same reason BlobBrowser is: so that
// internal/ui knows nothing about Key Vault, and AWS Secrets Manager can
// implement the same thing later.
type SecretBrowser interface {
	// ListSecrets returns the secrets in one vault. Names and metadata only.
	ListSecrets(vault string) []SecretInfo
	// ReadSecret returns one secret's current value. Called only when
	// somebody asks for it explicitly.
	ReadSecret(vault, name string) (value string, ok bool)
}

// SecretInfo is what the inspector shows for a secret.
//
// Deliberately not the value: the list is drawn whenever a vault is selected,
// so a value in it would end up on screen -- and in any screenshot -- without
// anyone asking. The value has its own endpoint behind a button.
type SecretInfo struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Versions    int       `json:"versions"`
	ContentType string    `json:"contentType,omitempty"`
	Enabled     bool      `json:"enabled"`
	Updated     time.Time `json:"updated"`
	Deleted     bool      `json:"deleted,omitempty"`
}

// registerSecretRoutes adds the endpoints the inspector uses to list secret
// names and, on request, reveal one value.
func registerSecretRoutes(mux *http.ServeMux, secrets SecretBrowser) {
	if secrets == nil {
		return
	}

	mux.HandleFunc("/api/keyvault/secrets", func(w http.ResponseWriter, r *http.Request) {
		vault := r.URL.Query().Get("vault")
		if vault == "" {
			http.Error(w, "vault is required", 400)
			return
		}
		list := secrets.ListSecrets(vault)
		if list == nil {
			list = []SecretInfo{}
		}
		writeJSON(w, list)
	})

	mux.HandleFunc("/api/keyvault/secret", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		value, ok := secrets.ReadSecret(q.Get("vault"), q.Get("name"))
		if !ok {
			http.Error(w, "not found", 404)
			return
		}
		// No caching: a revealed secret should not sit in a disk cache or come
		// back from one after it has been rotated.
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, map[string]string{"value": value})
	})
}
