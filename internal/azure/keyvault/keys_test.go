package keyvault

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The property the whole service exists for: no response carries the private
// half.
//
// Asserted DIRECTLY rather than by inspection, and over every response a key
// can produce rather than a sample of them. The private JWK members are named
// in RFC 7518: `d` for both RSA and EC, plus `p`, `q`, `dp`, `dq`, `qi` and
// `oth` for RSA's CRT parameters. Finding any of them anywhere in a body --
// at any depth -- is the failure.
//
// A weaker version of this test would read one bundle and look at its `key`
// object. That would pass while a list, a version listing, a deleted-key
// bundle or a crypto result leaked, and those are five different code paths
// that all serialise a key.
func TestNoResponseEverCarriesPrivateKeyMaterial(t *testing.T) {
	srv, _ := newTestVault(t)

	// An RSA key and an EC key, because their private members differ.
	mustCreateKey(t, srv, "rsa-key", `{"kty":"RSA","key_size":2048,"key_ops":["sign","verify"]}`)
	mustCreateKey(t, srv, "ec-key", `{"kty":"EC","crv":"P-256","key_ops":["sign","verify"]}`)

	digest := sha256.Sum256([]byte("message"))
	d := base64.RawURLEncoding.EncodeToString(digest[:])

	// Every path that serialises a key, including the ones a narrower test
	// would miss.
	probes := []struct{ method, path, body string }{
		{"GET", "/keys/rsa-key?api-version=7.4", ""},
		{"GET", "/keys/ec-key?api-version=7.4", ""},
		{"GET", "/keys?api-version=7.4", ""},
		{"GET", "/keys/rsa-key/versions?api-version=7.4", ""},
		{"POST", "/keys/rsa-key/sign?api-version=7.4", `{"alg":"RS256","value":"` + d + `"}`},
		{"POST", "/keys/rsa-key/encrypt?api-version=7.4", `{"alg":"RSA-OAEP-256","value":"aGk"}`},
		{"PATCH", "/keys/rsa-key?api-version=7.4", `{"tags":{"a":"b"}}`},
		{"DELETE", "/keys/rsa-key?api-version=7.4", ""},
		{"GET", "/deletedkeys?api-version=7.4", ""},
		{"GET", "/deletedkeys/rsa-key?api-version=7.4", ""},
	}
	private := []string{"d", "p", "q", "dp", "dq", "qi", "oth"}
	for _, pr := range probes {
		code, body, _ := call(t, srv, pr.method, pr.path, pr.body)
		if code >= 500 {
			t.Fatalf("%s %s = %d: %v", pr.method, pr.path, code, body)
		}
		if found := findKeys(body, private); len(found) > 0 {
			t.Errorf("%s %s leaked private JWK members %v:\n%s",
				pr.method, pr.path, found, mustJSON(body))
		}
	}
}

// findKeys walks a decoded JSON document and reports which of the named
// members appear at any depth.
func findKeys(v any, names []string) []string {
	var found []string
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			for _, n := range names {
				if k == n {
					found = append(found, k)
				}
			}
			found = append(found, findKeys(child, names)...)
		}
	case []any:
		for _, child := range t {
			found = append(found, findKeys(child, names)...)
		}
	}
	return found
}

func mustJSON(v any) string {
	raw, _ := json.MarshalIndent(v, "", "  ")
	return string(raw)
}

func mustCreateKey(t *testing.T, srv *httptest.Server, name, body string) map[string]any {
	t.Helper()
	code, out, _ := call(t, srv, "POST", "/keys/"+name+"/create?api-version=7.4", body)
	if code != 200 {
		t.Fatalf("create key %s = %d: %v", name, code, out)
	}
	return out
}

// A signature the vault made VERIFIES against the public key the vault
// published -- checked with crypto, not by trusting the response.
//
// This is the end-to-end proof that the JWK is real: if `n` or `e` were
// mis-encoded, or the EC point were minimally encoded rather than padded to
// the curve width, the public key rebuilt here would not verify what the
// private half signed.
func TestASignatureFromTheVaultVerifiesAgainstItsPublishedKey(t *testing.T) {
	srv, _ := newTestVault(t)

	t.Run("RSA", func(t *testing.T) {
		bundle := mustCreateKey(t, srv, "signer", `{"kty":"RSA","key_size":2048,"key_ops":["sign","verify"]}`)
		pub := rsaFromJWK(t, bundle)

		digest := sha256.Sum256([]byte("the message that was signed"))
		d := base64.RawURLEncoding.EncodeToString(digest[:])
		code, out, _ := call(t, srv, "POST", "/keys/signer/sign?api-version=7.4",
			`{"alg":"RS256","value":"`+d+`"}`)
		if code != 200 {
			t.Fatalf("sign = %d: %v", code, out)
		}
		sig := decodeB64(t, out["value"].(string))
		if err := rsa.VerifyPKCS1v15(pub, cryptoSHA256, digest[:], sig); err != nil {
			t.Errorf("the vault's RS256 signature does not verify against its own published key: %v", err)
		}

		// And the vault's own verify agrees.
		code, out, _ = call(t, srv, "POST", "/keys/signer/verify?api-version=7.4",
			fmt.Sprintf(`{"alg":"RS256","digest":%q,"value":%q}`, d, out["value"]))
		if code != 200 || out["value"] != true {
			t.Errorf("the vault would not verify its own signature: %d %v", code, out)
		}
	})

	t.Run("EC", func(t *testing.T) {
		bundle := mustCreateKey(t, srv, "ec-signer", `{"kty":"EC","crv":"P-256","key_ops":["sign","verify"]}`)
		pub := ecFromJWK(t, bundle)

		digest := sha256.Sum256([]byte("elliptic"))
		d := base64.RawURLEncoding.EncodeToString(digest[:])
		code, out, _ := call(t, srv, "POST", "/keys/ec-signer/sign?api-version=7.4",
			`{"alg":"ES256","value":"`+d+`"}`)
		if code != 200 {
			t.Fatalf("sign = %d: %v", code, out)
		}
		// JOSE's ES256 signature is the raw R||S pair, each padded to the
		// curve width -- NOT ASN.1 DER. Splitting it in half is the whole
		// decode, and a DER signature here would be the easiest way to make
		// something that looks right and verifies nowhere.
		sig := decodeB64(t, out["value"].(string))
		if len(sig) != 64 {
			t.Fatalf("ES256 signature is %d bytes, want 64 (R||S, 32 each)", len(sig))
		}
		r := new(big.Int).SetBytes(sig[:32])
		s := new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(pub, digest[:], r, s) {
			t.Error("the vault's ES256 signature does not verify against its own published key")
		}
	})
}

// Encrypt with the published public key, decrypt in the vault.
//
// The direction matters: this proves the vault holds the private half that
// matches the JWK it published, which is the other half of the same claim the
// signing test makes.
func TestTheVaultDecryptsWhatItsPublishedKeyEncrypted(t *testing.T) {
	srv, _ := newTestVault(t)
	bundle := mustCreateKey(t, srv, "wrapper", `{"kty":"RSA","key_size":2048,"key_ops":["encrypt","decrypt"]}`)
	pub := rsaFromJWK(t, bundle)

	plain := []byte("a data encryption key")
	ct, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, plain, nil)
	if err != nil {
		t.Fatal(err)
	}
	code, out, _ := call(t, srv, "POST", "/keys/wrapper/decrypt?api-version=7.4",
		fmt.Sprintf(`{"alg":"RSA-OAEP-256","value":%q}`, base64.RawURLEncoding.EncodeToString(ct)))
	if code != 200 {
		t.Fatalf("decrypt = %d: %v", code, out)
	}
	if got := decodeB64(t, out["value"].(string)); string(got) != string(plain) {
		t.Errorf("decrypted %q, want %q", got, plain)
	}
}

// The key lifecycle, mirroring the secret one: versions accumulate, a delete
// moves the key to the recycle bin where it still owns its name, and recover
// and purge both work.
func TestKeyLifecycle(t *testing.T) {
	srv, _ := newTestVault(t)
	first := mustCreateKey(t, srv, "rotating", `{"kty":"RSA","key_size":2048}`)
	second := mustCreateKey(t, srv, "rotating", `{"kty":"RSA","key_size":2048}`)

	v1 := first["key"].(map[string]any)["kid"].(string)
	v2 := second["key"].(map[string]any)["kid"].(string)
	if v1 == v2 {
		t.Error("creating a key twice reused the version id; Key Vault never overwrites")
	}

	// The unversioned read is the latest version.
	_, latest, _ := call(t, srv, "GET", "/keys/rotating?api-version=7.4", "")
	if got := latest["key"].(map[string]any)["kid"]; got != v2 {
		t.Errorf("unversioned read returned %v, want the newest version %s", got, v2)
	}

	// An older version is still readable by its own id.
	code, old, _ := call(t, srv, "GET", "/keys/rotating/"+lastSegment(v1)+"?api-version=7.4", "")
	if code != 200 || old["key"].(map[string]any)["kid"] != v1 {
		t.Errorf("older version read = %d %v", code, old)
	}

	code, vers, _ := call(t, srv, "GET", "/keys/rotating/versions?api-version=7.4", "")
	if code != 200 || len(vers["value"].([]any)) != 2 {
		t.Errorf("versions = %d %v", code, vers)
	}

	// Delete moves it to the recycle bin, where the name is still taken.
	if code, body, _ := call(t, srv, "DELETE", "/keys/rotating?api-version=7.4", ""); code != 200 {
		t.Fatalf("delete = %d %v", code, body)
	}
	if code, _, _ := call(t, srv, "GET", "/keys/rotating?api-version=7.4", ""); code != 404 {
		t.Errorf("a deleted key still reads live: %d", code)
	}
	code, conflict, _ := call(t, srv, "POST", "/keys/rotating/create?api-version=7.4", `{"kty":"RSA"}`)
	if code != 409 {
		t.Errorf("creating a key whose name is in the recycle bin = %d %v, want 409", code, conflict)
	}

	// Recover, then delete and purge for good.
	if code, body, _ := call(t, srv, "POST", "/deletedkeys/rotating?api-version=7.4", ""); code != 200 {
		t.Fatalf("recover = %d %v", code, body)
	}
	if code, _, _ := call(t, srv, "GET", "/keys/rotating?api-version=7.4", ""); code != 200 {
		t.Error("a recovered key is not readable")
	}
	call(t, srv, "DELETE", "/keys/rotating?api-version=7.4", "")
	if code, _, _ := call(t, srv, "DELETE", "/deletedkeys/rotating?api-version=7.4", ""); code != 204 {
		t.Errorf("purge = %d, want 204", code)
	}
	if code, _, _ := call(t, srv, "GET", "/deletedkeys/rotating?api-version=7.4", ""); code != 404 {
		t.Error("a purged key is still in the recycle bin")
	}
}

// What Nimbus will not do is refused BY NAME, never approximated.
//
// Each of these would otherwise produce material a client believes is
// something it is not: an HSM key made in software misrepresents where the
// private half lives, a substituted curve produces signatures nothing can
// verify, and an import that quietly generated a fresh key would hand back a
// key that is not the one supplied.
func TestUnsupportedKeyRequestsAreRefusedByName(t *testing.T) {
	srv, _ := newTestVault(t)
	cases := []struct {
		name, method, path, body, named string
		want                            int
	}{
		{"HSM key type", "POST", "/keys/h/create?api-version=7.4", `{"kty":"RSA-HSM","key_size":2048}`, "Managed HSM", 400},
		{"secp256k1", "POST", "/keys/k/create?api-version=7.4", `{"kty":"EC","crv":"P-256K"}`, "secp256k1", 400},
		{"odd RSA size", "POST", "/keys/s/create?api-version=7.4", `{"kty":"RSA","key_size":1024}`, "2048, 3072 or 4096", 400},
		{"import", "PUT", "/keys/i?api-version=7.4", `{"key":{"kty":"RSA"}}`, "does not import", 501},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body, _ := call(t, srv, c.method, c.path, c.body)
			if code != c.want {
				t.Errorf("= %d, want %d: %v", code, c.want, body)
			}
			msg, _ := body["error"].(map[string]any)["message"].(string)
			if !strings.Contains(msg, c.named) {
				t.Errorf("refusal does not name %q: %s", c.named, msg)
			}
		})
	}
}

// An algorithm the vault cannot perform is refused, not silently defaulted.
func TestAnUnsupportedAlgorithmIsRefused(t *testing.T) {
	srv, _ := newTestVault(t)
	mustCreateKey(t, srv, "ec-only", `{"kty":"EC","crv":"P-256"}`)
	digest := sha256.Sum256([]byte("x"))
	d := base64.RawURLEncoding.EncodeToString(digest[:])

	// An EC key cannot encrypt, and the refusal says why rather than just no.
	code, body, _ := call(t, srv, "POST", "/keys/ec-only/encrypt?api-version=7.4",
		`{"alg":"RSA-OAEP-256","value":"aGk"}`)
	msg, _ := body["error"].(map[string]any)["message"].(string)
	if code != 400 || !strings.Contains(msg, "do not encrypt") {
		t.Errorf("EC encrypt = %d %q", code, msg)
	}

	// And an algorithm nobody implements.
	code, body, _ = call(t, srv, "POST", "/keys/ec-only/sign?api-version=7.4",
		`{"alg":"ES999","value":"`+d+`"}`)
	msg, _ = body["error"].(map[string]any)["message"].(string)
	if code != 400 || !strings.Contains(msg, "ES999") {
		t.Errorf("unknown algorithm = %d %q", code, msg)
	}
}

// Keys survive a reload, and the JWK is byte-identical afterwards.
//
// The second half is the assertion that matters: a key whose public material
// differed between reads would be a permanent Terraform diff, and the
// recomputed-versus-stored distinction is exactly where that goes wrong.
func TestKeysSurviveAReloadUnchanged(t *testing.T) {
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

	before := mustCreateKey(t, srv, "durable", `{"kty":"RSA","key_size":2048,"key_ops":["sign","verify"]}`)
	mustCreateKey(t, srv, "durable-ec", `{"kty":"EC","crv":"P-384"}`)
	call(t, srv, "DELETE", "/keys/durable-ec?api-version=7.4", "")

	// A fresh store over the same directory, as a restart does.
	reloaded := NewStore()
	if err := reloaded.Persist(dir); err != nil {
		t.Fatal(err)
	}
	h2 := NewHandler(reloaded)
	h2.Login = h.Login
	h2.BaseURI = h.BaseURI
	srv2 := serverFor(t, h2)

	code, after, _ := call(t, srv2, "GET", "/keys/durable?api-version=7.4", "")
	if code != 200 {
		t.Fatalf("the key did not survive the reload: %d %v", code, after)
	}
	if mustJSON(before["key"]) != mustJSON(after["key"]) {
		t.Errorf("the JWK changed across a reload, which is a permanent plan diff:\nbefore %s\nafter  %s",
			mustJSON(before["key"]), mustJSON(after["key"]))
	}
	// The recycle bin survives too, or a name that was taken becomes free.
	if code, body, _ := call(t, srv2, "GET", "/deletedkeys/durable-ec?api-version=7.4", ""); code != 200 {
		t.Errorf("the deleted key did not survive the reload: %d %v", code, body)
	}
	// And the restored private half still works, which is the real test of
	// the PKCS#8 round trip -- the public JWK matching proves only that the
	// public half came back.
	digest := sha256.Sum256([]byte("after a restart"))
	d := base64.RawURLEncoding.EncodeToString(digest[:])
	code, out, _ := call(t, srv2, "POST", "/keys/durable/sign?api-version=7.4", `{"alg":"RS256","value":"`+d+`"}`)
	if code != 200 {
		t.Fatalf("signing after a reload = %d %v", code, out)
	}
	pub := rsaFromJWK(t, after)
	if err := rsa.VerifyPKCS1v15(pub, cryptoSHA256, digest[:], decodeB64(t, out["value"].(string))); err != nil {
		t.Errorf("a signature made after the reload does not verify: %v", err)
	}
}

// --- helpers ---------------------------------------------------------------

const cryptoSHA256 = crypto.SHA256

// rsaFromJWK rebuilds a public key from the JWK the vault published, which is
// the only material a client ever has. Everything these tests verify is
// verified against THIS key rather than against anything held server-side.
func rsaFromJWK(t *testing.T, bundle map[string]any) *rsa.PublicKey {
	t.Helper()
	jwk, ok := bundle["key"].(map[string]any)
	if !ok {
		t.Fatalf("no key in bundle: %s", mustJSON(bundle))
	}
	if jwk["kty"] != "RSA" {
		t.Fatalf("kty = %v, want RSA", jwk["kty"])
	}
	n := new(big.Int).SetBytes(decodeB64(t, jwk["n"].(string)))
	e := new(big.Int).SetBytes(decodeB64(t, jwk["e"].(string)))
	return &rsa.PublicKey{N: n, E: int(e.Int64())}
}

func ecFromJWK(t *testing.T, bundle map[string]any) *ecdsa.PublicKey {
	t.Helper()
	jwk := bundle["key"].(map[string]any)
	if jwk["kty"] != "EC" {
		t.Fatalf("kty = %v, want EC", jwk["kty"])
	}
	curve, err := curveFor(jwk["crv"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return &ecdsa.PublicKey{
		Curve: curve,
		X:     new(big.Int).SetBytes(decodeB64(t, jwk["x"].(string))),
		Y:     new(big.Int).SetBytes(decodeB64(t, jwk["y"].(string))),
	}
}

func decodeB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		t.Fatalf("value is not base64url: %v", err)
	}
	return b
}

func lastSegment(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func serverFor(t *testing.T, h *Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.Serve(w, r, "kv-nimbus")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A key's rotation policy answers about the POLICY, not about a version.
//
// azurerm reads GET /keys/{name}/rotationpolicy straight after creating a key
// and tolerates a 404 -- measured, with the apply completing and the plan
// coming back clean. Handled explicitly anyway: falling through to the version
// lookup answered "a key with (name/id) k/rotationpolicy was not found",
// which blames a version nobody asked for. Same argument as Key Vault's
// certificate contacts and S3's bucket sub-resources.
func TestAKeysRotationPolicyAnswersAboutThePolicy(t *testing.T) {
	srv, _ := newTestVault(t)
	mustCreateKey(t, srv, "rot", `{"kty":"RSA","key_size":2048}`)

	code, body, _ := call(t, srv, "GET", "/keys/rot/rotationpolicy?api-version=7.4", "")
	if code != 404 {
		t.Fatalf("rotation policy = %d, want 404: %v", code, body)
	}
	e := body["error"].(map[string]any)
	if e["code"] != "RotationPolicyNotFound" {
		t.Errorf("code = %v, want RotationPolicyNotFound (not a missing KEY)", e["code"])
	}
	if msg, _ := e["message"].(string); strings.Contains(msg, "name/id") {
		t.Errorf("the refusal blames a missing key version rather than the policy: %s", msg)
	}

	// A key that really is absent still reports the key as missing.
	code, body, _ = call(t, srv, "GET", "/keys/absent/rotationpolicy?api-version=7.4", "")
	if code != 404 || body["error"].(map[string]any)["code"] != "KeyNotFound" {
		t.Errorf("rotation policy of a missing key = %d %v, want KeyNotFound", code, body)
	}
}
