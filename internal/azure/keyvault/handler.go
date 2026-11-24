package keyvault

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// softDeleteRetention matches Azure's default: a deleted secret is
// recoverable for 90 days. Nothing here expires one -- see the note in the
// ARM-side keyvault.go on why a timer that purged things unprompted would be
// a worse surprise than one that never fires.
const softDeleteRetention = 90 * 24 * time.Hour

// Handler serves one Nimbus instance's vault data planes.
type Handler struct {
	store *Store
	// Login is the authority a 401 challenge points clients at, INCLUDING a
	// tenant path segment, and Audience is the resource it asks for a token
	// for. Both come from the Azure server's configuration, because they must
	// match the metadata document.
	Login    string
	Audience string
	// BaseURI builds a vault's data-plane URI, used to construct secret IDs.
	BaseURI func(vault string) string
	// Authorize, when set, decides whether a data-plane request may
	// proceed. A CALLBACK rather than an import, because this package
	// knows Key Vault's paths and nothing about role assignments, and
	// the evaluator knows the reverse. nil means no enforcement, which
	// is what --enforce-iam being off looks like from here.
	Authorize func(r *http.Request, vault, action string) *Denial
}

// NewHandler creates the data-plane handler over the given store.
func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// Store exposes the underlying store, so the ARM hook can create and purge
// vaults alongside the control-plane resource.
func (h *Handler) Store() *Store { return h.store }

// ParseHost splits a data-plane host into a vault name.
//
// Clients address a vault as {vault}.{suffix}, e.g.
// "kv-nimbus.vault.nimbus.localhost:4701" -- built either from the metadata
// keyVaultDns suffix or from the vault's own vaultUri. ok is false for
// anything else, which is an ARM request and must be left alone.
func ParseHost(host, suffix string) (string, bool) {
	h := strings.ToLower(stripPort(host))
	sfx := strings.ToLower(stripPort(suffix))
	if sfx == "" || !strings.HasSuffix(h, "."+sfx) {
		return "", false
	}
	name := strings.TrimSuffix(h, "."+sfx)
	if name == "" || strings.Contains(name, ".") { // one label only
		return "", false
	}
	return name, true
}

func stripPort(h string) string {
	if i := strings.LastIndex(h, ":"); i != -1 && !strings.Contains(h[i:], "]") {
		return h[:i]
	}
	return h
}

// Serve handles one data-plane request, returning a short note for the
// activity feed.
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, vaultName string) string {
	// The challenge comes FIRST, before the vault is even looked up.
	//
	// Every Key Vault client sends its first request with no token on
	// purpose: it expects a 401 carrying WWW-Authenticate, reads the
	// authority and resource out of it, fetches a token for exactly that
	// resource and retries. Answering the unauthenticated request instead
	// would work by accident here and teach a client nothing -- and the
	// Azure SDKs' own challenge policies refuse to send a token to a service
	// that never asked for one.
	if r.Header.Get("Authorization") == "" {
		h.challenge(w)
		return "challenged for a token"
	}

	v := h.store.Vault(vaultName)
	if v == nil {
		// What Key Vault returns for a vault that does not exist: the DNS
		// name resolves (ours is a wildcard) but there is nothing behind it.
		WriteError(w, 404, "VaultNotFound",
			"The vault '%s' does not exist. Create it through the Resource Manager first.", vaultName)
		return "unknown vault: " + vaultName
	}

	// Authorization sits AFTER the challenge and the vault lookup, and
	// that order is load-bearing. A request with no token must get the
	// 401 that starts the dance, not a 403: every Key Vault SDK is
	// written to expect the challenge, and being told "refused" where
	// it expected "authenticate" stops it before it has tried.
	if h.Authorize != nil {
		if action := DataActionFor(r, r.URL.Path); action != "" {
			if d := h.Authorize(r, vaultName, action); d != nil {
				WriteError(w, d.Status, d.Code, "%s", d.Message)
				return d.Note
			}
		}
	}

	seg := splitPath(r.URL.Path)
	if len(seg) == 0 {
		WriteError(w, 404, "NotFound", "Nothing is served at the vault root.")
		return ""
	}

	switch strings.ToLower(seg[0]) {
	case "secrets":
		return h.serveSecrets(w, r, v, seg[1:])
	case "deletedsecrets":
		return h.serveDeletedSecrets(w, r, v, seg[1:])
	case "certificates":
		if len(seg) == 2 && strings.EqualFold(seg[1], "contacts") {
			return h.serveContacts(w, r, v)
		}
		return h.serveCertificates(w, r, v, seg[1:])
	case "keys":
		return h.serveKeys(w, r, v, seg[1:])
	case "deletedkeys":
		return h.serveDeletedKeys(w, r, v, seg[1:])
	case "deletedcertificates":
		return h.serveDeletedCertificates(w, r, v, seg[1:])
	case "storage", "rng":
		// Deliberately an error, not an empty success: a client that thinks
		// it wrapped a key with a key that does not exist is in a far worse
		// position than one that was told Nimbus cannot do this yet.
		WriteError(w, 501, "NotSupported",
			"Nimbus emulates Key Vault secrets, keys and certificates; %q is not implemented.", seg[0])
		return "unimplemented Key Vault operation: " + seg[0]
	}
	WriteError(w, 404, "NotFound", "Unknown Key Vault path %q.", r.URL.Path)
	return "unknown Key Vault path: " + r.URL.Path
}

// challenge writes the 401 that starts Key Vault's authentication dance.
//
// The parameter names are the ones clients look for: `authorization` (older
// SDKs and the az CLI also accept `authorization_uri`) and `resource`, which
// newer azcore-based SDKs read as `scope`. Sending all three keeps every
// generation of client happy, and they all describe the same token.
//
// The authorization URL MUST carry a tenant path segment. go-autorest, which
// the azurerm provider still uses for Key Vault, parses the challenge and
// takes everything after the first "/" as the tenant:
//
//	bc.values[tenantID] = asURL.Path[1:]
//
// A bare origin has an empty path, so that slice panics -- measured against
// azurerm 4.81.0, where it crashes the provider with
// "slice bounds out of range [1:0]" and no mention of Key Vault at all.
func (h *Handler) challenge(w http.ResponseWriter) {
	login := strings.TrimRight(h.Login, "/")
	audience := h.Audience
	if audience == "" {
		audience = "https://vault.azure.net"
	}
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(
		`Bearer authorization="%s", authorization_uri="%s", resource="%s", scope="%s/.default"`,
		login, login, audience, strings.TrimRight(audience, "/")))
	WriteError(w, 401, "Unauthorized", "Request is missing a bearer token.")
}

// serveContacts handles the certificate contacts list.
//
// This looks like an odd thing to implement in a milestone about secrets, and
// it is not optional: azurerm reads the contacts from the DATA plane every
// time it reads the vault through ARM. Measured against azurerm 4.81.0 -- an
// unexpected status here does not produce an error message, it crashes the
// provider inside its Key Vault client with a nil dereference. Real Key Vault
// answers 404 ContactsNotFound when none are set, which azurerm tolerates
// deliberately, so that is what Nimbus answers.
func (h *Handler) serveContacts(w http.ResponseWriter, r *http.Request, v *Vault) string {
	switch r.Method {
	case http.MethodGet:
		contacts := v.GetContacts()
		if len(contacts) == 0 {
			WriteError(w, 404, "ContactsNotFound", "Contacts not found")
			return ""
		}
		writeJSON(w, 200, h.contactsJSON(v, contacts))
		return ""

	case http.MethodPut:
		var req struct {
			Contacts []Contact `json:"contacts"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			WriteError(w, 400, "BadParameter", "The request body is not a valid contacts list: %v", err)
			return ""
		}
		v.SetContacts(req.Contacts)
		h.store.Save(v)
		writeJSON(w, 200, h.contactsJSON(v, req.Contacts))
		return "set certificate contacts"

	case http.MethodDelete:
		contacts := v.GetContacts()
		if len(contacts) == 0 {
			WriteError(w, 404, "ContactsNotFound", "Contacts not found")
			return ""
		}
		v.SetContacts(nil)
		h.store.Save(v)
		writeJSON(w, 200, h.contactsJSON(v, contacts))
		return "deleted certificate contacts"
	}
	return methodNotAllowed(w, r)
}

func (h *Handler) contactsJSON(v *Vault, contacts []Contact) map[string]any {
	list := []any{}
	for _, c := range contacts {
		list = append(list, map[string]any{"email": c.Email, "name": c.Name, "phone": c.Phone})
	}
	return map[string]any{"id": h.baseURI(v) + "certificates/contacts", "contacts": list}
}

// --- JSON shapes -----------------------------------------------------------

func (h *Handler) baseURI(v *Vault) string {
	if h.BaseURI != nil {
		return h.BaseURI(v.Name)
	}
	return "https://" + strings.ToLower(v.Name) + ".vault.nimbus.localhost/"
}

// --- small helpers ---------------------------------------------------------

func methodNotAllowed(w http.ResponseWriter, r *http.Request) string {
	WriteError(w, 405, "MethodNotAllowed", "%s is not supported on %s", r.Method, r.URL.Path)
	return "unsupported Key Vault method: " + r.Method + " " + r.URL.Path
}

// WriteError writes a Key Vault error, which is shaped differently from ARM's:
// the error object carries code and message, and SDKs surface the code.
func WriteError(w http.ResponseWriter, status int, code, format string, args ...any) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": fmt.Sprintf(format, args...)},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// validSecretName applies Key Vault's rule: 1-127 characters, letters, digits
// and dashes. The name is part of a URL, so anything else would produce an ID
// a client cannot round-trip.
func validSecretName(n string) bool {
	if n == "" || len(n) > 127 {
		return false
	}
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

func unixPtr(v *int64) *time.Time {
	if v == nil {
		return nil
	}
	t := time.Unix(*v, 0).UTC()
	return &t
}

// --- authorization --------------------------------------------------------

// Denial is a refusal handed back by the Authorize hook: Key Vault's own
// error shape, plus a note for the activity feed.
type Denial struct {
	Status  int
	Code    string
	Message string
	Note    string
}
