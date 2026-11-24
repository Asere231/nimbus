package keyvault

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// Certificates, which are THREE objects under one name.
//
// A Key Vault certificate is a certificate, a key and a secret, all sharing a
// name and all addressable separately: the certificate carries the public
// x509, the key is the private half (usable for signing without ever being
// exported), and the secret returns the whole thing including the private key
// for clients that want to load it into a TLS stack. That indirection is the
// surprising part of the service and is why creating one here writes to three
// places.
//
// Only the `Self` issuer can work offline, so it is the only one implemented.
// DigiCert and GlobalSign are refused by name: they need the network, and the
// offline rule is a ground rule. Pretending to issue from a public CA would
// be the worst option available.

// serveCertificates handles "/certificates/..." other than the contacts list,
// which handler.go routes first.
func (h *Handler) serveCertificates(w http.ResponseWriter, r *http.Request, v *Vault, seg []string) string {
	switch {
	case len(seg) == 0: // /certificates
		if r.Method != http.MethodGet {
			return methodNotAllowed(w, r)
		}
		items := []any{}
		for _, c := range v.ListCertificates() {
			if ver := c.Latest(); ver != nil {
				items = append(items, h.certItemJSON(v, c, ver))
			}
		}
		writeJSON(w, 200, map[string]any{"value": items, "nextLink": nil})
		return ""

	case len(seg) == 2 && strings.EqualFold(seg[1], "create"):
		if r.Method != http.MethodPost {
			return methodNotAllowed(w, r)
		}
		return h.createCertificate(w, r, v, seg[0])

	case len(seg) == 2 && strings.EqualFold(seg[1], "pending"):
		// The operation a client polls while a certificate is being issued.
		// Nimbus signs synchronously, so it is always already complete -- but
		// the endpoint has to exist, because a client that asks is told to by
		// the create's own response.
		c := v.Certificate(seg[0])
		if c == nil {
			return h.certNotFound(w, v, seg[0])
		}
		if r.Method == http.MethodDelete {
			WriteError(w, 400, "BadParameter",
				"Certificate %s is already issued; there is no pending operation to cancel.", seg[0])
			return ""
		}
		writeJSON(w, 200, h.certOperationJSON(v, c))
		return ""

	case len(seg) == 2 && strings.EqualFold(seg[1], "policy"):
		c := v.Certificate(seg[0])
		if c == nil {
			return h.certNotFound(w, v, seg[0])
		}
		if r.Method == http.MethodPatch {
			WriteError(w, 501, "NotSupported",
				"Nimbus does not implement updating a certificate policy after issue.")
			return "unimplemented: update certificate policy"
		}
		writeJSON(w, 200, h.certPolicyJSON(v, c))
		return ""

	case len(seg) == 2 && strings.EqualFold(seg[1], "versions"):
		if r.Method != http.MethodGet {
			return methodNotAllowed(w, r)
		}
		c := v.Certificate(seg[0])
		if c == nil {
			return h.certNotFound(w, v, seg[0])
		}
		items := []any{}
		for _, ver := range c.Versions {
			items = append(items, h.certItemJSON(v, c, ver))
		}
		writeJSON(w, 200, map[string]any{"value": items, "nextLink": nil})
		return ""
	}

	name := seg[0]
	version := ""
	if len(seg) > 1 {
		version = seg[1]
	}

	switch r.Method {
	case http.MethodGet:
		c := v.Certificate(name)
		if c == nil {
			return h.certNotFound(w, v, name)
		}
		ver := c.Version(version)
		if ver == nil {
			WriteError(w, 404, "CertificateNotFound",
				"A certificate with (name/id) %s/%s was not found in this key vault.", name, version)
			return ""
		}
		writeJSON(w, 200, h.certBundleJSON(v, c, ver))
		return ""

	case http.MethodPatch:
		c := v.Certificate(name)
		if c == nil {
			return h.certNotFound(w, v, name)
		}
		writeJSON(w, 200, h.certBundleJSON(v, c, c.Latest()))
		return "updated certificate " + name

	case http.MethodDelete:
		c := v.DeleteCertificate(name, softDeleteRetention)
		if c == nil {
			return h.certNotFound(w, v, name)
		}
		c.RecoveryID = h.baseURI(v) + "deletedcertificates/" + c.Name
		// The key and the secret share the certificate's name and go with it,
		// which is what makes the three-way addressing one object rather than
		// three that happen to agree.
		v.DeleteKey(name, softDeleteRetention)
		v.DeleteSecret(name, softDeleteRetention)
		h.store.Save(v)
		writeJSON(w, 200, h.certBundleJSON(v, c, c.Latest()))
		return "deleted certificate " + name
	}
	return methodNotAllowed(w, r)
}

// createCertificate generates a self-signed certificate, its key and its
// secret.
//
// Measured body, from azurerm 4.81.0:
//
//	{"policy":{"issuer":{"name":"Self"},
//	  "key_props":{"exportable":true,"kty":"RSA","key_size":2048,"reuse_key":false},
//	  "lifetime_actions":[],"secret_props":{"contentType":"application/x-pkcs12"},
//	  "x509_props":{"subject":"CN=...","ekus":[],"sans":{},
//	    "key_usage":["keyEncipherment","digitalSignature"],"validity_months":12}},
//	 "tags":{}}
//
// Note the mixed conventions inside one object: key_props, x509_props and
// validity_months are snake_case while contentType INSIDE secret_props is
// camelCase. Azure's own wart, not a transcription error.
func (h *Handler) createCertificate(w http.ResponseWriter, r *http.Request, v *Vault, name string) string {
	if !validSecretName(name) {
		WriteError(w, 400, "BadParameter",
			"The certificate name %q is invalid: 1 to 127 characters, letters, digits and dashes only.", name)
		return ""
	}
	raw, err := readAll(w, r)
	if err != nil {
		WriteError(w, 400, "BadParameter", "The request body could not be read: %v", err)
		return ""
	}
	var req struct {
		Policy json.RawMessage   `json:"policy"`
		Tags   map[string]string `json:"tags"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		WriteError(w, 400, "BadParameter", "The request body is not a valid certificate creation: %v", err)
		return ""
	}
	var pol certPolicy
	if len(req.Policy) > 0 {
		if err := json.Unmarshal(req.Policy, &pol); err != nil {
			WriteError(w, 400, "BadParameter", "The certificate policy is not valid: %v", err)
			return ""
		}
	}
	if v.DeletedCertificate(name) != nil {
		WriteError(w, 409, "Conflict",
			"Certificate %s is currently in a deleted but recoverable state; recover or purge it first.", name)
		return "name held by a deleted certificate: " + name
	}

	issuer := pol.Issuer.Name
	if issuer == "" {
		issuer = "Self"
	}
	if !strings.EqualFold(issuer, "Self") {
		// Refused by name. A certificate that claimed a public CA had issued
		// it would be a lie about the one thing a certificate asserts.
		WriteError(w, 400, "BadParameter",
			"Certificate issuer %q needs a network call to a public CA, which Nimbus will not make. "+
				"Only issuer_name = \"Self\" works offline, and Nimbus signs it itself.", issuer)
		return "unsupported certificate issuer: " + issuer
	}

	kty, curve, size, ref := normaliseKeyType(pol.KeyProps.Kty, pol.KeyProps.Curve, pol.KeyProps.KeySize)
	if ref != "" {
		WriteError(w, 400, "BadParameter", "%s", ref)
		return "unsupported certificate key type: " + pol.KeyProps.Kty
	}

	// The key is created first, because the certificate is signed with it.
	keyVer, err := v.CreateKey(name, kty, curve, size, []string{"sign", "verify"}, nil, Attributes{})
	if err != nil {
		WriteError(w, 400, "BadParameter", "Could not create the certificate's key: %v", err)
		return ""
	}

	der, err := selfSign(keyVer, pol)
	if err != nil {
		WriteError(w, 400, "BadParameter", "Could not sign the certificate: %v", err)
		return "certificate signing failed: " + err.Error()
	}

	// The SECRET is the whole thing, private key included, which is how most
	// clients actually consume a certificate -- the indirection that makes
	// this three objects rather than one.
	bundle, err := pemBundle(keyVer, der)
	if err != nil {
		WriteError(w, 400, "BadParameter", "Could not encode the certificate: %v", err)
		return ""
	}
	secVer := v.SetSecret(name, bundle, certSecretContentType, nil, Attributes{})

	ver := v.AddCertificate(name, der, keyVer.Version, secVer.Version, req.Policy, req.Tags)
	h.store.Save(v)

	// Create answers with a certificate OPERATION, not with the certificate:
	// real Key Vault issues asynchronously and the client polls
	// /certificates/{name}/pending. Nimbus signs synchronously, so the
	// operation is already complete -- but it must still be shaped like one,
	// or a client waits for something that never arrives.
	//
	// And it must be 202, not 200. azurerm tolerates a 200; `az keyvault
	// certificate create` does NOT, and fails with "Operation returned an
	// invalid status 'OK'" while quoting the entirely correct body back --
	// so it reads as a content problem and is a status problem. The second
	// time this exact confusion has come up, after Microsoft.Web's creates.
	_ = ver
	writeJSON(w, 202, h.certOperationJSON(v, v.Certificate(name)))
	return "created certificate " + name
}

// certSecretContentType is what the SECRET under a certificate reports.
//
// azurerm asks for application/x-pkcs12 in secret_props, and Nimbus cannot
// honour that: the Go standard library has no PKCS#12 ENCODER (crypto/x509
// can only decode one, via pkcs12.ToPEM in a third-party package), and the
// stdlib-only rule is not negotiable. So the secret carries a PEM bundle and
// SAYS it is PEM rather than claiming a format it is not -- a stated
// divergence instead of a hidden one. A client that loads it with a PEM
// reader works; one that insists on PKCS#12 sees the content type and knows
// why.
const certSecretContentType = "application/x-pem-file"

// certPolicy is the measured shape of what a client sends. Only the fields
// Nimbus acts on are decoded; the policy is echoed back verbatim from the raw
// JSON, so anything not named here still round-trips.
type certPolicy struct {
	Issuer struct {
		Name string `json:"name"`
	} `json:"issuer"`
	KeyProps struct {
		Exportable bool   `json:"exportable"`
		Kty        string `json:"kty"`
		KeySize    int    `json:"key_size"`
		Curve      string `json:"crv"`
		ReuseKey   bool   `json:"reuse_key"`
	} `json:"key_props"`
	SecretProps struct {
		ContentType string `json:"contentType"`
	} `json:"secret_props"`
	X509Props struct {
		Subject        string   `json:"subject"`
		KeyUsage       []string `json:"key_usage"`
		Ekus           []string `json:"ekus"`
		ValidityMonths int      `json:"validity_months"`
		Sans           struct {
			DNSNames []string `json:"dns_names"`
			Emails   []string `json:"emails"`
			Upns     []string `json:"upns"`
		} `json:"sans"`
	} `json:"x509_props"`
}

// selfSign issues the certificate, signed with its own key.
func selfSign(ver *KeyVersion, pol certPolicy) ([]byte, error) {
	subject := pol.X509Props.Subject
	if subject == "" {
		subject = "CN=nimbus"
	}
	months := pol.X509Props.ValidityMonths
	if months == 0 {
		months = 12
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      parseDN(subject),
		NotBefore:    now.Add(-5 * time.Minute),
		// Computed as a plain duration in UTC rather than with AddDate,
		// which works in the local zone and shifts by an hour across a
		// daylight-saving boundary -- the trap the server certificates
		// already record.
		NotAfter:              now.Add(time.Duration(months) * 30 * 24 * time.Hour),
		KeyUsage:              keyUsageFrom(pol.X509Props.KeyUsage),
		BasicConstraintsValid: true,
		DNSNames:              pol.X509Props.Sans.DNSNames,
		EmailAddresses:        pol.X509Props.Sans.Emails,
	}
	for _, e := range pol.X509Props.Ekus {
		switch e {
		case "1.3.6.1.5.5.7.3.1":
			tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
		case "1.3.6.1.5.5.7.3.2":
			tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageClientAuth)
		}
	}
	pub := ver.Public()
	priv := any(ver.RSA)
	if ver.EC != nil {
		priv = ver.EC
	}
	return x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
}

// parseDN reads the handful of RDN types a certificate policy realistically
// carries. A full RFC 4514 parser is not warranted: azurerm sends a subject
// its own schema validated, and anything unrecognised lands in the CN so the
// certificate still has a usable name rather than being rejected.
func parseDN(dn string) pkix.Name {
	var out pkix.Name
	for _, part := range strings.Split(dn, ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.ToUpper(strings.TrimSpace(k)) {
		case "CN":
			out.CommonName = val
		case "O":
			out.Organization = append(out.Organization, val)
		case "OU":
			out.OrganizationalUnit = append(out.OrganizationalUnit, val)
		case "C":
			out.Country = append(out.Country, val)
		case "L":
			out.Locality = append(out.Locality, val)
		case "ST":
			out.Province = append(out.Province, val)
		default:
			if out.CommonName == "" {
				out.CommonName = val
			}
		}
	}
	if out.CommonName == "" {
		out.CommonName = dn
	}
	return out
}

func keyUsageFrom(names []string) x509.KeyUsage {
	var ku x509.KeyUsage
	for _, n := range names {
		switch n {
		case "digitalSignature":
			ku |= x509.KeyUsageDigitalSignature
		case "keyEncipherment":
			ku |= x509.KeyUsageKeyEncipherment
		case "dataEncipherment":
			ku |= x509.KeyUsageDataEncipherment
		case "keyAgreement":
			ku |= x509.KeyUsageKeyAgreement
		case "keyCertSign":
			ku |= x509.KeyUsageCertSign
		case "cRLSign":
			ku |= x509.KeyUsageCRLSign
		case "nonRepudiation", "contentCommitment":
			ku |= x509.KeyUsageContentCommitment
		}
	}
	if ku == 0 {
		ku = x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
	}
	return ku
}

// pemBundle is the certificate and its private key, which is what the SECRET
// under a certificate returns.
func pemBundle(ver *KeyVersion, der []byte) (string, error) {
	var priv any
	switch {
	case ver.RSA != nil:
		priv = ver.RSA
	case ver.EC != nil:
		priv = ver.EC
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := pem.Encode(&b, &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}); err != nil {
		return "", err
	}
	if err := pem.Encode(&b, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (h *Handler) certID(v *Vault, name, version string) string {
	id := h.baseURI(v) + "certificates/" + name
	if version != "" {
		id += "/" + version
	}
	return id
}

func (h *Handler) certBundleJSON(v *Vault, c *Certificate, ver *CertVersion) map[string]any {
	out := h.certItemJSON(v, c, ver)
	out["cer"] = base64.StdEncoding.EncodeToString(ver.DER)
	out["kid"] = h.keyID(v, c.Name, ver.KeyVersion)
	// Both ids are VERSIONED. azurerm parses sid with a versioned-id parser
	// and fails with "expected a versioned ID" otherwise -- after the
	// certificate has already been created, so the retry then hits a name
	// that is taken.
	out["sid"] = h.secretID(v, c.Name, ver.SecretVersion)
	out["policy"] = h.certPolicyJSON(v, c)
	return out
}

func (h *Handler) certItemJSON(v *Vault, c *Certificate, ver *CertVersion) map[string]any {
	attrs := map[string]any{
		"enabled":         ver.Enabled,
		"created":         ver.Created.Unix(),
		"updated":         ver.Updated.Unix(),
		"recoveryLevel":   "Recoverable+Purgeable",
		"recoverableDays": 90,
	}
	// x5t is the SHA-1 thumbprint, base64url. SHA-1 is not a choice here: the
	// field is defined as SHA-1 and a client comparing thumbprints would see
	// a mismatch against anything else.
	sum := sha1.Sum(ver.DER)
	out := map[string]any{
		"id":         h.certID(v, c.Name, ver.Version),
		"attributes": attrs,
		"x5t":        b64url(sum[:]),
	}
	if len(ver.Tags) > 0 {
		out["tags"] = ver.Tags
	}
	return out
}

// certPolicyJSON echoes the policy back as it was sent, with the id added.
//
// Byte-for-byte, deliberately: azurerm reads the policy back and compares it
// with what its configuration says, so a policy Nimbus normalised or filled
// in with defaults would differ from the one the client sent and every plan
// would want to change it. The Content-MD5 lesson, one service over.
func (h *Handler) certPolicyJSON(v *Vault, c *Certificate) map[string]any {
	out := map[string]any{}
	if ver := c.Latest(); ver != nil && len(ver.Policy) > 0 {
		_ = json.Unmarshal(ver.Policy, &out)
	}
	out["id"] = h.baseURI(v) + "certificates/" + c.Name + "/policy"
	return out
}

// certOperationJSON is the shape a create answers with, and the shape
// /certificates/{name}/pending returns.
//
// status is always "completed" because Nimbus signs synchronously. Real Key
// Vault reports "inProgress" and the client polls -- which is why the pending
// endpoint exists at all even though it can never be caught mid-issue here.
func (h *Handler) certOperationJSON(v *Vault, c *Certificate) map[string]any {
	ver := c.Latest()
	out := map[string]any{
		"id":                     h.baseURI(v) + "certificates/" + c.Name + "/pending",
		"issuer":                 map[string]any{"name": "Self"},
		"cancellation_requested": false,
		"status":                 "completed",
		"target":                 h.certID(v, c.Name, ""),
		"request_id":             c.RequestID,
	}
	if ver != nil {
		sum := sha1.Sum(ver.DER)
		out["x5t"] = b64url(sum[:])
	}
	return out
}

func (h *Handler) certNotFound(w http.ResponseWriter, v *Vault, name string) string {
	if v.DeletedCertificate(name) != nil {
		WriteError(w, 404, "CertificateNotFound",
			"Certificate %s is currently in a deleted but recoverable state; recover or purge it first.", name)
		return "certificate is deleted: " + name
	}
	WriteError(w, 404, "CertificateNotFound",
		"A certificate with (name/id) %s was not found in this key vault.", name)
	return ""
}

func readAll(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return readLimited(http.MaxBytesReader(w, r.Body, 1<<20))
}

// serveDeletedCertificates is the certificate recycle bin.
func (h *Handler) serveDeletedCertificates(w http.ResponseWriter, r *http.Request, v *Vault, seg []string) string {
	if len(seg) == 0 {
		if r.Method != http.MethodGet {
			return methodNotAllowed(w, r)
		}
		items := []any{}
		for _, c := range v.ListDeletedCertificates() {
			items = append(items, h.deletedCertJSON(v, c))
		}
		writeJSON(w, 200, map[string]any{"value": items, "nextLink": nil})
		return ""
	}
	name := seg[0]
	switch r.Method {
	case http.MethodGet:
		c := v.DeletedCertificate(name)
		if c == nil {
			return h.deletedCertNotFound(w, name)
		}
		writeJSON(w, 200, h.deletedCertJSON(v, c))
		return ""
	case http.MethodPost:
		c := v.RecoverCertificate(name)
		if c == nil {
			return h.deletedCertNotFound(w, name)
		}
		// The key and the secret come back with it, as they went with it.
		v.RecoverKey(name)
		v.RecoverSecret(name)
		h.store.Save(v)
		writeJSON(w, 200, h.certBundleJSON(v, c, c.Latest()))
		return "recovered certificate " + name
	case http.MethodDelete:
		if !v.PurgeCertificate(name) {
			return h.deletedCertNotFound(w, name)
		}
		v.PurgeKey(name)
		v.PurgeSecret(name)
		h.store.Save(v)
		w.WriteHeader(204)
		return "purged certificate " + name
	}
	return methodNotAllowed(w, r)
}

func (h *Handler) deletedCertJSON(v *Vault, c *Certificate) map[string]any {
	out := map[string]any{}
	if ver := c.Latest(); ver != nil {
		out = h.certItemJSON(v, c, ver)
	}
	out["recoveryId"] = c.RecoveryID
	if !c.Deleted.IsZero() {
		out["deletedDate"] = c.Deleted.Unix()
		out["scheduledPurgeDate"] = c.ScheduledFor.Unix()
	}
	return out
}

func (h *Handler) deletedCertNotFound(w http.ResponseWriter, name string) string {
	WriteError(w, 404, "CertificateNotFound",
		"Deleted certificate %s was not found in this key vault.", name)
	return ""
}
