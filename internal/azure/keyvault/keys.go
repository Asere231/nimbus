package keyvault

import (
	"crypto/ecdsa"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"strings"
)

// Keys, which are the half of Key Vault that is about CRYPTOGRAPHY rather
// than storage.
//
// A secret is a string you put in and get back. A KEY is the opposite: the
// private half never leaves, and the service performs operations with it on
// your behalf. That distinction is the entire reason Key Vault and KMS exist,
// so the property worth protecting is not the confidentiality of a stored
// value but the fact that NO RESPONSE EVER CARRIES PRIVATE MATERIAL --
// asserted directly by a test rather than left to inspection.
//
// It also shows up in the permissions. A secret needs two data actions,
// `getSecret` and `readMetadata`, because reading its metadata and reading
// its value are different things a role may be given separately. A key needs
// only `keys/read`: there is no "value" to withhold, because the private half
// is never returned to anybody. The asymmetry is not an oversight in Azure's
// model, it is the model.

// serveKeys handles everything addressed at "/keys/...".
func (h *Handler) serveKeys(w http.ResponseWriter, r *http.Request, v *Vault, seg []string) string {
	switch {
	case len(seg) == 0: // /keys
		if r.Method != http.MethodGet {
			return methodNotAllowed(w, r)
		}
		h.listKeys(w, v)
		return ""

	case len(seg) == 2 && strings.EqualFold(seg[1], "create"): // /keys/{name}/create
		if r.Method != http.MethodPost {
			return methodNotAllowed(w, r)
		}
		return h.createKey(w, r, v, seg[0])

	case len(seg) == 2 && strings.EqualFold(seg[1], "versions"): // /keys/{name}/versions
		if r.Method != http.MethodGet {
			return methodNotAllowed(w, r)
		}
		k := v.Key(seg[0])
		if k == nil {
			return h.keyNotFound(w, v, seg[0])
		}
		items := []any{}
		for _, ver := range k.Versions {
			items = append(items, h.keyItemJSON(v, k, ver))
		}
		writeJSON(w, 200, map[string]any{"value": items, "nextLink": nil})
		return ""

	case len(seg) == 2 && strings.EqualFold(seg[1], "backup"):
		WriteError(w, 501, "NotSupported", "Nimbus does not implement key backup.")
		return "unimplemented Key Vault operation: key backup"

	case len(seg) == 2 && strings.EqualFold(seg[1], "rotationpolicy"):
		// azurerm reads this straight after creating a key, and TOLERATES a
		// 404 -- measured: the apply completed and the plan came back clean.
		// It is handled explicitly all the same, because falling through to
		// the version lookup below answered "a key with (name/id)
		// measured-key/rotationpolicy was not found", which blames a version
		// nobody asked for and explains nothing. The same argument as Key
		// Vault's certificate contacts and S3's dozen bucket sub-resources:
		// a client reading a sub-resource deserves an answer about THAT
		// sub-resource.
		if v.Key(seg[0]) == nil {
			return h.keyNotFound(w, v, seg[0])
		}
		if r.Method != http.MethodGet {
			WriteError(w, 501, "NotSupported",
				"Nimbus does not implement key rotation policies; a key is never rotated automatically here.")
			return "unimplemented Key Vault operation: set rotation policy"
		}
		WriteError(w, 404, "RotationPolicyNotFound",
			"Key %s has no rotation policy. Nimbus does not implement automatic rotation.", seg[0])
		return ""
	}

	name := seg[0]
	version := ""
	if len(seg) > 1 {
		version = seg[1]
	}

	// A cryptographic operation is a verb on the key's own URL, so it is
	// recognised before the plain GET/DELETE handling below.
	if len(seg) >= 2 {
		if op := strings.ToLower(seg[len(seg)-1]); isCryptoOp(op) {
			if r.Method != http.MethodPost {
				return methodNotAllowed(w, r)
			}
			ver := ""
			if len(seg) == 3 {
				ver = seg[1]
			}
			return h.cryptoOp(w, r, v, name, ver, op)
		}
	}

	switch r.Method {
	case http.MethodPut:
		// PUT /keys/{name} is IMPORT, not create: it carries the key material
		// in the body. Refused by name, because importing means parsing PEM
		// and PKCS#12 and the latter is not comfortably stdlib-only -- and
		// because a half-supported import that silently generated a fresh key
		// instead would be the worst possible outcome.
		WriteError(w, 501, "NotSupported",
			"Nimbus generates keys and does not import them. `az keyvault key import` is not "+
				"implemented; use `az keyvault key create` or azurerm_key_vault_key, neither of "+
				"which imports.")
		return "unimplemented Key Vault operation: key import"

	case http.MethodGet:
		k := v.Key(name)
		if k == nil {
			return h.keyNotFound(w, v, name)
		}
		ver := k.Version(version)
		if ver == nil {
			WriteError(w, 404, "KeyNotFound",
				"A key with (name/id) %s/%s was not found in this key vault.", name, version)
			return ""
		}
		writeJSON(w, 200, h.keyBundleJSON(v, k, ver))
		return ""

	case http.MethodPatch:
		return h.updateKey(w, r, v, name, version)

	case http.MethodDelete:
		k := v.DeleteKey(name, softDeleteRetention)
		if k == nil {
			return h.keyNotFound(w, v, name)
		}
		k.RecoveryID = h.baseURI(v) + "deletedkeys/" + k.Name
		h.store.Save(v)
		writeJSON(w, 200, h.deletedKeyJSON(v, k))
		return "deleted key " + k.Name
	}
	return methodNotAllowed(w, r)
}

// createKey is POST /keys/{name}/create, which is what both azurerm and
// `az keyvault key create` send.
//
// Measured against azurerm 4.81.0, the body is
//
//	{"attributes":{"enabled":true},
//	 "key_ops":["sign","verify","wrapKey","unwrapKey","encrypt","decrypt"],
//	 "key_size":2048,"kty":"RSA","tags":{}}
//
// Note `key_ops` with an underscore, which is JWK's spelling rather than the
// camelCase the rest of this API uses.
func (h *Handler) createKey(w http.ResponseWriter, r *http.Request, v *Vault, name string) string {
	if !validSecretName(name) {
		WriteError(w, 400, "BadParameter",
			"The key name %q is invalid: 1 to 127 characters, letters, digits and dashes only.", name)
		return ""
	}
	var req struct {
		Kty        string            `json:"kty"`
		KeySize    int               `json:"key_size"`
		Curve      string            `json:"crv"`
		KeyOps     []string          `json:"key_ops"`
		Tags       map[string]string `json:"tags"`
		Attributes *struct {
			Enabled *bool  `json:"enabled"`
			NBF     *int64 `json:"nbf"`
			Exp     *int64 `json:"exp"`
		} `json:"attributes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		WriteError(w, 400, "BadParameter", "The request body is not a valid key creation: %v", err)
		return ""
	}
	if v.DeletedKey(name) != nil {
		WriteError(w, 409, "Conflict",
			"Key %s is currently in a deleted but recoverable state; recover or purge it first.", name)
		return "name held by a deleted key: " + name
	}

	kty, curve, size, ref := normaliseKeyType(req.Kty, req.Curve, req.KeySize)
	if ref != "" {
		// Refused by NAME rather than approximated. An HSM-backed key that
		// was quietly made in software, or a curve quietly substituted, would
		// produce material a client believes is something it is not.
		WriteError(w, 400, "BadParameter", "%s", ref)
		return "unsupported key type: " + req.Kty
	}

	attrs := Attributes{}
	if req.Attributes != nil {
		attrs.Enabled = req.Attributes.Enabled
		attrs.NotBefore = unixPtr(req.Attributes.NBF)
		attrs.Expires = unixPtr(req.Attributes.Exp)
	}
	ver, err := v.CreateKey(name, kty, curve, size, req.KeyOps, req.Tags, attrs)
	if err != nil {
		WriteError(w, 400, "BadParameter", "Could not create the key: %v", err)
		return "key creation failed: " + err.Error()
	}
	h.store.Save(v)
	writeJSON(w, 200, h.keyBundleJSON(v, v.Key(name), ver))
	return "created key " + name
}

// normaliseKeyType settles the type, curve and size, or returns the reason it
// cannot. Azure's defaults: RSA is 2048 bits, EC is P-256.
func normaliseKeyType(kty, curve string, size int) (string, string, int, string) {
	switch strings.ToUpper(kty) {
	case "RSA", "":
		if size == 0 {
			size = 2048
		}
		switch size {
		case 2048, 3072, 4096:
		default:
			return "", "", 0, "RSA key size " + itoa(size) + " is not supported; use 2048, 3072 or 4096."
		}
		return "RSA", "", size, ""
	case "EC":
		if curve == "" {
			curve = "P-256"
		}
		if _, err := curveFor(curve); err != nil {
			return "", "", 0, err.Error()
		}
		return "EC", curve, 0, ""
	case "RSA-HSM", "EC-HSM", "OCT", "OCT-HSM":
		return "", "", 0, "Key type " + kty + " needs a Managed HSM or a Premium vault, which Nimbus " +
			"does not emulate. Use RSA or EC: a key that claimed to be HSM-backed and was not " +
			"would misrepresent where its private half lives, which is the one thing this service is about."
	}
	return "", "", 0, "Unknown key type " + kty + "."
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func (h *Handler) updateKey(w http.ResponseWriter, r *http.Request, v *Vault, name, version string) string {
	var req struct {
		KeyOps     []string          `json:"key_ops"`
		Tags       map[string]string `json:"tags"`
		Attributes *struct {
			Enabled *bool  `json:"enabled"`
			NBF     *int64 `json:"nbf"`
			Exp     *int64 `json:"exp"`
		} `json:"attributes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		WriteError(w, 400, "BadParameter", "The request body is not a valid key update: %v", err)
		return ""
	}
	attrs := Attributes{}
	if req.Attributes != nil {
		attrs.Enabled = req.Attributes.Enabled
		attrs.NotBefore = unixPtr(req.Attributes.NBF)
		attrs.Expires = unixPtr(req.Attributes.Exp)
	}
	ver := v.UpdateKey(name, version, req.KeyOps, req.Tags, attrs)
	if ver == nil {
		return h.keyNotFound(w, v, name)
	}
	h.store.Save(v)
	writeJSON(w, 200, h.keyBundleJSON(v, v.Key(name), ver))
	return "updated key " + name
}

func (h *Handler) listKeys(w http.ResponseWriter, v *Vault) {
	items := []any{}
	for _, k := range v.ListKeys() {
		if ver := k.Latest(); ver != nil {
			items = append(items, h.keyItemJSON(v, k, ver))
		}
	}
	writeJSON(w, 200, map[string]any{"value": items, "nextLink": nil})
}

// serveDeletedKeys is the recycle bin: list, get, recover and purge.
func (h *Handler) serveDeletedKeys(w http.ResponseWriter, r *http.Request, v *Vault, seg []string) string {
	if len(seg) == 0 {
		if r.Method != http.MethodGet {
			return methodNotAllowed(w, r)
		}
		items := []any{}
		for _, k := range v.ListDeletedKeys() {
			items = append(items, h.deletedKeyJSON(v, k))
		}
		writeJSON(w, 200, map[string]any{"value": items, "nextLink": nil})
		return ""
	}

	name := seg[0]
	switch r.Method {
	case http.MethodGet:
		k := v.DeletedKey(name)
		if k == nil {
			return h.deletedKeyNotFound(w, name)
		}
		writeJSON(w, 200, h.deletedKeyJSON(v, k))
		return ""
	case http.MethodPost: // /deletedkeys/{name}/recover
		k := v.RecoverKey(name)
		if k == nil {
			return h.deletedKeyNotFound(w, name)
		}
		h.store.Save(v)
		writeJSON(w, 200, h.keyBundleJSON(v, k, k.Latest()))
		return "recovered key " + name
	case http.MethodDelete:
		if !v.PurgeKey(name) {
			return h.deletedKeyNotFound(w, name)
		}
		h.store.Save(v)
		w.WriteHeader(204)
		return "purged key " + name
	}
	return methodNotAllowed(w, r)
}

func (h *Handler) keyNotFound(w http.ResponseWriter, v *Vault, name string) string {
	if v.DeletedKey(name) != nil {
		WriteError(w, 404, "KeyNotFound",
			"Key %s is currently in a deleted but recoverable state; recover or purge it first.", name)
		return "key is deleted: " + name
	}
	WriteError(w, 404, "KeyNotFound",
		"A key with (name/id) %s was not found in this key vault.", name)
	return ""
}

func (h *Handler) deletedKeyNotFound(w http.ResponseWriter, name string) string {
	WriteError(w, 404, "KeyNotFound", "Deleted key %s was not found in this key vault.", name)
	return ""
}

func (h *Handler) keyID(v *Vault, name, version string) string {
	id := h.baseURI(v) + "keys/" + name
	if version != "" {
		id += "/" + version
	}
	return id
}

// keyBundleJSON is what every read of a key returns: the public JWK, the
// attributes and the tags. There is no "value" field and there is nowhere for
// one to go -- see jwkJSON.
func (h *Handler) keyBundleJSON(v *Vault, k *Key, ver *KeyVersion) map[string]any {
	return h.keyItemJSON(v, k, ver)
}

func (h *Handler) keyItemJSON(v *Vault, k *Key, ver *KeyVersion) map[string]any {
	attrs := map[string]any{
		"enabled":         ver.Enabled,
		"created":         ver.Created.Unix(),
		"updated":         ver.Updated.Unix(),
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
		"key":        h.jwkJSON(v, k, ver),
		"attributes": attrs,
	}
	if len(ver.Tags) > 0 {
		out["tags"] = ver.Tags
	}
	return out
}

// jwkJSON is the PUBLIC half, and only ever the public half.
//
// This function is the one place a key's material is serialised, which is
// what makes "the private half never leaves" checkable rather than hopeful:
// there is no branch here that can reach ver.RSA.D or ver.EC.D, and a test
// walks every response body in the package looking for the private JWK
// members (d, p, q, dp, dq, qi) to prove it.
func (h *Handler) jwkJSON(v *Vault, k *Key, ver *KeyVersion) map[string]any {
	out := map[string]any{
		"kid": h.keyID(v, k.Name, ver.Version),
		"kty": ver.Kty,
	}
	if len(ver.Ops) > 0 {
		out["key_ops"] = ver.Ops
	}
	switch pub := ver.Public().(type) {
	case *rsa.PublicKey:
		out["n"] = b64url(pub.N.Bytes())
		out["e"] = b64url(big.NewInt(int64(pub.E)).Bytes())
	case *ecdsa.PublicKey:
		// x and y are FIXED-WIDTH for the curve, left-padded with zeros. A
		// minimal big-endian encoding would be shorter for roughly one key in
		// 256 and every JOSE implementation would reject it.
		size := (pub.Curve.Params().BitSize + 7) / 8
		out["crv"] = ver.Curve
		out["x"] = b64url(padLeft(pub.X.Bytes(), size))
		out["y"] = b64url(padLeft(pub.Y.Bytes(), size))
	}
	return out
}

func (h *Handler) deletedKeyJSON(v *Vault, k *Key) map[string]any {
	out := map[string]any{}
	if ver := k.Latest(); ver != nil {
		out = h.keyItemJSON(v, k, ver)
	}
	out["recoveryId"] = k.RecoveryID
	if !k.Deleted.IsZero() {
		out["deletedDate"] = k.Deleted.Unix()
		out["scheduledPurgeDate"] = k.ScheduledFor.Unix()
	}
	return out
}

// b64url is JWK's encoding: base64url with no padding.
func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func padLeft(b []byte, size int) []byte {
	if len(b) >= size {
		return b
	}
	out := make([]byte, size)
	copy(out[size-len(b):], b)
	return out
}
