package keyvault

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// SECRETS: the half of Key Vault that stores a value and gives it back.
//
// Kept apart from handler.go so neither runs past this project's 500-line
// limit, which the file had already exceeded before keys and certificates
// were added to it. Nothing here changed in the move.
//
// The contrast with keys.go is the whole shape of the service. A secret has a
// VALUE, so reading its metadata and reading its value are separately
// grantable -- hence two data actions, getSecret and readMetadata, and hence
// the reveal button in the inspector. A key has no value to return at all.

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
