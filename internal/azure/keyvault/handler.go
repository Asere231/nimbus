package keyvault

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
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
		WriteError(w, 501, "NotSupported",
			"Nimbus emulates Key Vault secrets only; certificates are not implemented.")
		return "unimplemented Key Vault operation: certificates"
	case "keys", "storage", "rng":
		// Deliberately an error, not an empty success: a client that thinks
		// it wrapped a key with a key that does not exist is in a far worse
		// position than one that was told Nimbus cannot do this yet.
		WriteError(w, 501, "NotSupported",
			"Nimbus emulates Key Vault secrets only; %q is not implemented.", seg[0])
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

// --- secrets ---------------------------------------------------------------

func (h *Handler) serveSecrets(w http.ResponseWriter, r *http.Request, v *Vault, seg []string) string {
	switch {
	case len(seg) == 0: // /secrets
		if r.Method != http.MethodGet {
			return methodNotAllowed(w, r)
		}
		h.listSecrets(w, r, v)
		return ""

	case len(seg) == 2 && strings.EqualFold(seg[1], "versions"): // /secrets/{name}/versions
		if r.Method != http.MethodGet {
			return methodNotAllowed(w, r)
		}
		sec := v.Secret(seg[0])
		if sec == nil {
			return h.secretNotFound(w, v, seg[0])
		}
		items := []any{}
		for _, ver := range sec.Versions {
			items = append(items, h.secretItemJSON(v, sec, ver))
		}
		writeJSON(w, 200, map[string]any{"value": items, "nextLink": nil})
		return ""

	case len(seg) == 2 && strings.EqualFold(seg[1], "backup"):
		WriteError(w, 501, "NotSupported", "Nimbus does not implement secret backup.")
		return "unimplemented Key Vault operation: backup"
	}

	name := seg[0]
	version := ""
	if len(seg) > 1 {
		version = seg[1]
	}

	switch r.Method {
	case http.MethodPut:
		return h.setSecret(w, r, v, name)
	case http.MethodGet:
		sec := v.Secret(name)
		if sec == nil {
			return h.secretNotFound(w, v, name)
		}
		ver := sec.Latest()
		if version != "" {
			ver = sec.Version(version)
		}
		if ver == nil {
			WriteError(w, 404, "SecretNotFound",
				"A secret with (name/id) %s/%s was not found in this key vault.", name, version)
			return ""
		}
		writeJSON(w, 200, h.secretBundleJSON(v, sec, ver, true))
		return ""
	case http.MethodPatch:
		return h.updateSecret(w, r, v, name, version)
	case http.MethodDelete:
		sec := v.DeleteSecret(name, softDeleteRetention)
		if sec == nil {
			return h.secretNotFound(w, v, name)
		}
		sec.RecoveryID = h.baseURI(v) + "deletedsecrets/" + sec.Name
		h.store.Save(v)
		writeJSON(w, 200, h.deletedBundleJSON(v, sec))
		return "deleted secret " + sec.Name
	}
	return methodNotAllowed(w, r)
}

// setSecret is PUT /secrets/{name}: it always creates a new VERSION, which is
// why a client that re-sets the same value gets a different secret ID back.
func (h *Handler) setSecret(w http.ResponseWriter, r *http.Request, v *Vault, name string) string {
	if !validSecretName(name) {
		WriteError(w, 400, "BadParameter",
			"The secret name %q is invalid: 1 to 127 characters, letters, digits and dashes only.", name)
		return ""
	}
	var req struct {
		Value       string            `json:"value"`
		ContentType string            `json:"contentType"`
		Tags        map[string]string `json:"tags"`
		Attributes  *struct {
			Enabled *bool  `json:"enabled"`
			NBF     *int64 `json:"nbf"`
			Exp     *int64 `json:"exp"`
		} `json:"attributes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		WriteError(w, 400, "BadParameter", "The request body is not a valid secret: %v", err)
		return ""
	}
	// A secret in the recycle bin still owns its name, exactly as a
	// soft-deleted vault does.
	if v.DeletedSecret(name) != nil {
		WriteError(w, 409, "Conflict",
			"Secret %s is currently in a deleted but recoverable state; recover or purge it first.", name)
		return "name held by a deleted secret: " + name
	}

	attrs := Attributes{}
	if req.Attributes != nil {
		attrs.Enabled = req.Attributes.Enabled
		attrs.NotBefore = unixPtr(req.Attributes.NBF)
		attrs.Expires = unixPtr(req.Attributes.Exp)
	}
	sec := v.Secret(name)
	ver := v.SetSecret(name, req.Value, req.ContentType, req.Tags, attrs)
	if sec == nil {
		sec = v.Secret(name)
	}
	h.store.Save(v)
	writeJSON(w, 200, h.secretBundleJSON(v, sec, ver, true))
	return "set secret " + name
}

func (h *Handler) updateSecret(w http.ResponseWriter, r *http.Request, v *Vault, name, version string) string {
	var req struct {
		ContentType *string           `json:"contentType"`
		Tags        map[string]string `json:"tags"`
		Attributes  *struct {
			Enabled *bool  `json:"enabled"`
			NBF     *int64 `json:"nbf"`
			Exp     *int64 `json:"exp"`
		} `json:"attributes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		WriteError(w, 400, "BadParameter", "The request body is not a valid secret update: %v", err)
		return ""
	}
	attrs := Attributes{}
	if req.Attributes != nil {
		attrs.Enabled = req.Attributes.Enabled
		attrs.NotBefore = unixPtr(req.Attributes.NBF)
		attrs.Expires = unixPtr(req.Attributes.Exp)
	}
	ver := v.UpdateVersion(name, version, req.ContentType, req.Tags, attrs)
	if ver == nil {
		return h.secretNotFound(w, v, name)
	}
	h.store.Save(v)
	// A PATCH answers without the value, like Key Vault does.
	writeJSON(w, 200, h.secretBundleJSON(v, v.Secret(name), ver, false))
	return "updated secret " + name
}

func (h *Handler) listSecrets(w http.ResponseWriter, r *http.Request, v *Vault) {
	max := 0
	if s := r.URL.Query().Get("maxresults"); s != "" {
		max, _ = strconv.Atoi(s)
	}
	items := []any{}
	for _, sec := range v.List() {
		ver := sec.Latest()
		if ver == nil {
			continue
		}
		if max > 0 && len(items) >= max {
			break
		}
		// A list item carries the VERSIONLESS id and never a value: Key Vault
		// does not hand out secret values in bulk.
		item := h.secretItemJSON(v, sec, ver)
		item["id"] = h.baseURI(v) + "secrets/" + sec.Name
		items = append(items, item)
	}
	// nextLink stays null: everything fits in one page at emulator scale, and
	// a client that paginates handles a single page correctly.
	writeJSON(w, 200, map[string]any{"value": items, "nextLink": nil})
}

func (h *Handler) serveDeletedSecrets(w http.ResponseWriter, r *http.Request, v *Vault, seg []string) string {
	switch {
	case len(seg) == 0:
		if r.Method != http.MethodGet {
			return methodNotAllowed(w, r)
		}
		items := []any{}
		for _, sec := range v.ListDeleted() {
			items = append(items, h.deletedBundleJSON(v, sec))
		}
		writeJSON(w, 200, map[string]any{"value": items, "nextLink": nil})
		return ""

	case len(seg) == 2 && strings.EqualFold(seg[1], "recover"):
		if r.Method != http.MethodPost {
			return methodNotAllowed(w, r)
		}
		sec := v.RecoverSecret(seg[0])
		if sec == nil {
			return h.deletedSecretNotFound(w, seg[0])
		}
		h.store.Save(v)
		writeJSON(w, 200, h.secretBundleJSON(v, sec, sec.Latest(), false))
		return "recovered secret " + sec.Name
	}

	name := seg[0]
	switch r.Method {
	case http.MethodGet:
		sec := v.DeletedSecret(name)
		if sec == nil {
			return h.deletedSecretNotFound(w, name)
		}
		writeJSON(w, 200, h.deletedBundleJSON(v, sec))
		return ""
	case http.MethodDelete: // purge
		if !v.PurgeSecret(name) {
			return h.deletedSecretNotFound(w, name)
		}
		h.store.Save(v)
		w.WriteHeader(204)
		return "purged secret " + name
	}
	return methodNotAllowed(w, r)
}

// --- JSON shapes -----------------------------------------------------------

func (h *Handler) baseURI(v *Vault) string {
	if h.BaseURI != nil {
		return h.BaseURI(v.Name)
	}
	return "https://" + strings.ToLower(v.Name) + ".vault.nimbus.localhost/"
}

func (h *Handler) secretID(v *Vault, name, version string) string {
	id := h.baseURI(v) + "secrets/" + name
	if version != "" {
		id += "/" + version
	}
	return id
}

// secretBundleJSON is Key Vault's SecretBundle. withValue is false for
// responses that deliberately omit it.
func (h *Handler) secretBundleJSON(v *Vault, sec *Secret, ver *SecretVersion, withValue bool) map[string]any {
	out := h.secretItemJSON(v, sec, ver)
	if withValue {
		out["value"] = ver.Value
	}
	return out
}

func (h *Handler) secretItemJSON(v *Vault, sec *Secret, ver *SecretVersion) map[string]any {
	attrs := map[string]any{
		"enabled": ver.Enabled,
		"created": ver.Created.Unix(),
		"updated": ver.Updated.Unix(),
		// Soft delete on, purge protection off: the level Azure reports for a
		// default vault, and what tells a client purge is available.
		"recoveryLevel":   "Recoverable+Purgeable",
		"recoverableDays": 90,
	}
	if ver.NotBefore != nil {
		attrs["nbf"] = ver.NotBefore.Unix()
	}
	if ver.Expires != nil {
		attrs["exp"] = ver.Expires.Unix()
	}
	out := map[string]any{
		"id":         h.secretID(v, sec.Name, ver.Version),
		"attributes": attrs,
	}
	if ver.ContentType != "" {
		out["contentType"] = ver.ContentType
	}
	if len(ver.Tags) > 0 {
		out["tags"] = ver.Tags
	}
	return out
}

func (h *Handler) deletedBundleJSON(v *Vault, sec *Secret) map[string]any {
	out := map[string]any{}
	if ver := sec.Latest(); ver != nil {
		out = h.secretItemJSON(v, sec, ver)
	} else {
		out["id"] = h.secretID(v, sec.Name, "")
	}
	recovery := sec.RecoveryID
	if recovery == "" {
		recovery = h.baseURI(v) + "deletedsecrets/" + sec.Name
	}
	out["recoveryId"] = recovery
	out["deletedDate"] = sec.Deleted.Unix()
	out["scheduledPurgeDate"] = sec.ScheduledFor.Unix()
	return out
}

// --- small helpers ---------------------------------------------------------

func (h *Handler) secretNotFound(w http.ResponseWriter, v *Vault, name string) string {
	if sec := v.DeletedSecret(name); sec != nil {
		WriteError(w, 404, "SecretNotFound",
			"Secret %s is currently in a deleted but recoverable state; recover or purge it first.", name)
		return "secret is deleted: " + name
	}
	WriteError(w, 404, "SecretNotFound",
		"A secret with (name/id) %s was not found in this key vault.", name)
	return ""
}

func (h *Handler) deletedSecretNotFound(w http.ResponseWriter, name string) string {
	WriteError(w, 404, "SecretNotFound",
		"Deleted secret not found: %s", name)
	return ""
}

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
