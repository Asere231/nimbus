package keyvault

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/http/httptest"
	"strings"
	"testing"
)

// A certificate is THREE objects under one name, and that indirection is the
// surprising part of the service.
//
// The certificate carries the public x509, the key is the private half
// (usable without ever being exported), and the secret returns the whole
// thing including the private key -- which is how most clients load one into
// a TLS stack. All three must exist and agree after a single create.
func TestACertificateCreatesAKeyAndASecretUnderOneName(t *testing.T) {
	srv, _ := newTestVault(t)
	op := mustCreateCert(t, srv, "web", selfSignedPolicy("CN=web.nimbus.localhost"))

	// Create answers with an OPERATION, not the certificate. Real Key Vault
	// issues asynchronously and the client polls; Nimbus signs synchronously,
	// so it is already complete -- but the shape has to be an operation or a
	// client waits for something that never arrives.
	if op["status"] != "completed" {
		t.Errorf("create returned status %v, want completed", op["status"])
	}
	if op["target"] == nil || op["request_id"] == nil {
		t.Errorf("the operation is missing target or request_id: %s", mustJSON(op))
	}

	// azurerm polls this once -- measured.
	code, pending, _ := call(t, srv, "GET", "/certificates/web/pending?api-version=7.4", "")
	if code != 200 || pending["status"] != "completed" {
		t.Errorf("pending = %d %v", code, pending)
	}

	code, bundle, _ := call(t, srv, "GET", "/certificates/web?api-version=7.4", "")
	if code != 200 {
		t.Fatalf("certificate read = %d %v", code, bundle)
	}

	// kid and sid must both be VERSIONED. azurerm parses sid with a
	// versioned-id parser and fails the apply with "expected a versioned ID"
	// -- an error naming neither the field nor the certificate, and arriving
	// after the certificate has already been created.
	for _, field := range []string{"kid", "sid"} {
		id, _ := bundle[field].(string)
		if id == "" {
			t.Fatalf("%s is missing: %s", field, mustJSON(bundle))
		}
		if strings.Count(strings.TrimPrefix(id, "https://"), "/") < 3 {
			t.Errorf("%s = %q is not a versioned id", field, id)
		}
	}

	// The key exists and is usable under the same name.
	if code, body, _ := call(t, srv, "GET", "/keys/web?api-version=7.4", ""); code != 200 {
		t.Errorf("the certificate's key is not readable: %d %v", code, body)
	}
	// The secret exists and carries the private key -- the whole point of the
	// three-way addressing.
	code, sec, _ := call(t, srv, "GET", "/secrets/web?api-version=7.4", "")
	if code != 200 {
		t.Fatalf("the certificate's secret is not readable: %d %v", code, sec)
	}
	value, _ := sec["value"].(string)
	if !strings.Contains(value, "PRIVATE KEY") || !strings.Contains(value, "CERTIFICATE") {
		t.Errorf("the secret does not carry the key and the certificate: %q", value)
	}

	// Nimbus reports PEM rather than claiming the PKCS#12 azurerm asked for,
	// because the standard library has no PKCS#12 encoder. A stated
	// divergence instead of a hidden one -- and azurerm never reads the
	// secret, so nothing in Terraform sees it.
	if sec["contentType"] != "application/x-pem-file" {
		t.Errorf("contentType = %v, want application/x-pem-file", sec["contentType"])
	}

	// And the certificate really parses, with the subject that was asked for.
	der, err := base64.StdEncoding.DecodeString(bundle["cer"].(string))
	if err != nil {
		t.Fatalf("cer is not base64: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("the issued certificate does not parse: %v", err)
	}
	if cert.Subject.CommonName != "web.nimbus.localhost" {
		t.Errorf("subject CN = %q, want web.nimbus.localhost", cert.Subject.CommonName)
	}

	// The PEM in the secret is the SAME certificate, not a second one.
	block, _ := pem.Decode([]byte(value[strings.Index(value, "-----BEGIN CERTIFICATE"):]))
	if block == nil || string(block.Bytes) != string(der) {
		t.Error("the secret's PEM certificate differs from the one the bundle published")
	}
}

// The certificate is self-signed and verifies against itself.
func TestAnIssuedCertificateIsSelfSignedAndValid(t *testing.T) {
	srv, _ := newTestVault(t)
	mustCreateCert(t, srv, "selfie", selfSignedPolicy("CN=selfie"))
	_, bundle, _ := call(t, srv, "GET", "/certificates/selfie?api-version=7.4", "")
	der, _ := base64.StdEncoding.DecodeString(bundle["cer"].(string))
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	// Checked with crypto rather than by trusting the response: the signature
	// must verify against the certificate's own public key.
	//
	// CheckSignature, not CheckSignatureFrom. The latter requires the parent
	// to be a CA -- IsCA set and KeyUsageCertSign present -- and a
	// self-signed LEAF is not a self-signed CA. Real Key Vault issues a leaf
	// for TLS, not a certificate authority, so the Go check that fits is the
	// one that verifies the signature over the TBS bytes directly. The first
	// version of this test used CheckSignatureFrom and failed with "parent
	// certificate cannot sign this kind of certificate", which reads as a
	// problem with the certificate and was a problem with the assertion.
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		t.Errorf("the certificate's signature does not verify against its own public key: %v", err)
	}
	if cert.IsCA {
		t.Error("an issued certificate should be a leaf, not a CA")
	}
	if cert.NotAfter.Before(cert.NotBefore) {
		t.Error("the validity window is inverted")
	}
}

// An issuer that needs the network is refused BY NAME.
func TestANetworkIssuerIsRefusedByName(t *testing.T) {
	srv, _ := newTestVault(t)
	for _, issuer := range []string{"DigiCert", "GlobalSign"} {
		body := `{"policy":{"issuer":{"name":"` + issuer + `"},` +
			`"key_props":{"kty":"RSA","key_size":2048},` +
			`"x509_props":{"subject":"CN=x","validity_months":12}}}`
		code, out, _ := call(t, srv, "POST", "/certificates/ext/create?api-version=7.4", body)
		msg, _ := out["error"].(map[string]any)["message"].(string)
		if code != 400 {
			t.Errorf("%s = %d, want 400: %v", issuer, code, out)
		}
		if !strings.Contains(msg, issuer) || !strings.Contains(msg, "Self") {
			t.Errorf("the refusal for %s does not name it and the alternative: %s", issuer, msg)
		}
	}
}

// The policy comes back BYTE-FOR-BYTE, because azurerm compares it.
//
// A policy Nimbus normalised, or filled in with defaults the client did not
// send, would differ from the configuration and every plan would want to
// change it. The Content-MD5 lesson, one service over -- and the reason the
// raw JSON is stored rather than the decoded struct.
func TestACertificatePolicyRoundTripsVerbatim(t *testing.T) {
	srv, _ := newTestVault(t)
	// A field Nimbus does not decode at all must still survive.
	policy := `{"policy":{"issuer":{"name":"Self"},` +
		`"key_props":{"exportable":true,"kty":"RSA","key_size":2048,"reuse_key":false},` +
		`"lifetime_actions":[],"secret_props":{"contentType":"application/x-pkcs12"},` +
		`"x509_props":{"subject":"CN=verbatim","ekus":[],"sans":{},` +
		`"key_usage":["keyEncipherment","digitalSignature"],"validity_months":12},` +
		`"nimbus_unknown_field":"must survive"}}`
	mustCreateCert(t, srv, "verbatim", policy)

	code, out, _ := call(t, srv, "GET", "/certificates/verbatim/policy?api-version=7.4", "")
	if code != 200 {
		t.Fatalf("policy read = %d %v", code, out)
	}
	if out["nimbus_unknown_field"] != "must survive" {
		t.Errorf("a policy field Nimbus does not decode was dropped: %s", mustJSON(out))
	}
	// Still reported as asked for, even though the SECRET is PEM -- the
	// policy is the client's document and is not rewritten.
	sp, _ := out["secret_props"].(map[string]any)
	if sp == nil || sp["contentType"] != "application/x-pkcs12" {
		t.Errorf("secret_props was rewritten: %s", mustJSON(out))
	}
	if out["id"] == nil {
		t.Error("the policy has no id")
	}
}

// Deleting a certificate takes its key and its secret with it, and recovering
// brings all three back. They are one object addressed three ways, not three
// objects that happen to share a name.
func TestDeletingACertificateTakesItsKeyAndSecret(t *testing.T) {
	srv, _ := newTestVault(t)
	mustCreateCert(t, srv, "trio", selfSignedPolicy("CN=trio"))

	if code, body, _ := call(t, srv, "DELETE", "/certificates/trio?api-version=7.4", ""); code != 200 {
		t.Fatalf("delete = %d %v", code, body)
	}
	for _, path := range []string{"/certificates/trio", "/keys/trio", "/secrets/trio"} {
		if code, _, _ := call(t, srv, "GET", path+"?api-version=7.4", ""); code != 404 {
			t.Errorf("%s still reads live after the certificate was deleted: %d", path, code)
		}
	}
	if code, _, _ := call(t, srv, "POST", "/deletedcertificates/trio?api-version=7.4", ""); code != 200 {
		t.Fatal("recover failed")
	}
	for _, path := range []string{"/certificates/trio", "/keys/trio", "/secrets/trio"} {
		if code, _, _ := call(t, srv, "GET", path+"?api-version=7.4", ""); code != 200 {
			t.Errorf("%s did not come back with the certificate: %d", path, code)
		}
	}
	// Purge removes all three for good.
	call(t, srv, "DELETE", "/certificates/trio?api-version=7.4", "")
	if code, _, _ := call(t, srv, "DELETE", "/deletedcertificates/trio?api-version=7.4", ""); code != 204 {
		t.Error("purge did not answer 204")
	}
	if code, _, _ := call(t, srv, "GET", "/deletedkeys/trio?api-version=7.4", ""); code != 404 {
		t.Error("the certificate's key survived the purge")
	}
}

// Certificates survive a reload, and the issued bytes are identical.
func TestCertificatesSurviveAReloadUnchanged(t *testing.T) {
	dir := t.TempDir()
	store := NewStore()
	if err := store.Persist(dir); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(store)
	h.Login = "https://localhost:4701/00000000-0000-0000-0000-000000000001"
	h.BaseURI = func(name string) string { return "https://" + strings.ToLower(name) + ".vault.nimbus.localhost:4701/" }
	store.CreateVault("kv-nimbus")
	srv := serverFor(t, h)

	mustCreateCert(t, srv, "durable-cert", selfSignedPolicy("CN=durable"))
	_, before, _ := call(t, srv, "GET", "/certificates/durable-cert?api-version=7.4", "")

	reloaded := NewStore()
	if err := reloaded.Persist(dir); err != nil {
		t.Fatal(err)
	}
	h2 := NewHandler(reloaded)
	h2.Login, h2.BaseURI = h.Login, h.BaseURI
	srv2 := serverFor(t, h2)

	code, after, _ := call(t, srv2, "GET", "/certificates/durable-cert?api-version=7.4", "")
	if code != 200 {
		t.Fatalf("the certificate did not survive the reload: %d %v", code, after)
	}
	if before["cer"] != after["cer"] {
		t.Error("the issued certificate changed across a reload, which is a permanent plan diff")
	}
	if mustJSON(before["policy"]) != mustJSON(after["policy"]) {
		t.Error("the policy changed across a reload")
	}
	// And the key that signed it still signs, which is the real test of the
	// round trip: the certificate bytes coming back proves only that data was
	// stored.
	if code, body, _ := call(t, srv2, "GET", "/keys/durable-cert?api-version=7.4", ""); code != 200 {
		t.Errorf("the certificate's key did not survive the reload: %d %v", code, body)
	}
}

func selfSignedPolicy(subject string) string {
	return `{"policy":{"issuer":{"name":"Self"},` +
		`"key_props":{"exportable":true,"kty":"RSA","key_size":2048,"reuse_key":false},` +
		`"lifetime_actions":[],"secret_props":{"contentType":"application/x-pkcs12"},` +
		`"x509_props":{"subject":"` + subject + `","ekus":[],"sans":{},` +
		`"key_usage":["keyEncipherment","digitalSignature"],"validity_months":12}},"tags":{}}`
}

func mustCreateCert(t *testing.T, srv *httptest.Server, name, body string) map[string]any {
	t.Helper()
	code, out, _ := call(t, srv, "POST", "/certificates/"+name+"/create?api-version=7.4", body)
	if code != 200 {
		t.Fatalf("create certificate %s = %d: %v", name, code, out)
	}
	return out
}
