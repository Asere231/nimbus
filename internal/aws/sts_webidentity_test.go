package aws

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
)

// AssumeRoleWithWebIdentity: the call that makes IRSA work, and the one
// place in Nimbus where a signature genuinely decides something.

// newIRSAServer is an AWS server with a TLS url, so it has an OIDC issuer,
// and a signing key, so it can verify a token against the JWKS it serves.
func newIRSAServer(t *testing.T) *Server {
	t.Helper()
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	key, err := core.EnsureSigningKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{
		PublicURL: "http://localhost:4702",
		TLSURL:    "https://localhost:4704",
		DataDir:   dir,
	}, store, bus)
	s.useSigningKey(key)
	f := docker.NewFake()
	f.Unavailable = true
	s.UseDocker(f)
	return s
}

// parsePEMKey reads the private half off disk. Deliberately done here
// rather than exported from `core`: a test must not grow the production
// surface, and nothing in Nimbus signs a service-account token -- k3s
// does, and this stands in for it.
func parsePEMKey(t *testing.T, path string) *rsa.PrivateKey {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(body)
	if block == nil {
		t.Fatalf("%s is not a PEM file", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		t.Fatalf("%s does not hold an RSA key", path)
	}
	return key
}

// signSAToken mints a token the way k3s does: RS256 over the signing key,
// with the kid the JWKS advertises.
func signSAToken(t *testing.T, s *Server, claims map[string]any) string {
	t.Helper()
	return signSATokenWith(t, s.signing.Private, s.signing.KeyID(), "RS256", claims)
}

func signSATokenWith(t *testing.T, keyPath, kid, alg string, claims map[string]any) string {
	t.Helper()
	key := parsePEMKey(t, keyPath)
	b64 := func(v any) string {
		body, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(body)
	}
	header := map[string]any{"alg": alg, "kid": kid, "typ": "JWT"}
	signingInput := b64(header) + "." + b64(claims)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// saClaimsFor is what a projected service-account token carries.
func saClaimsFor(issuer, namespace, name string) map[string]any {
	return map[string]any{
		"iss": issuer,
		"sub": "system:serviceaccount:" + namespace + ":" + name,
		"aud": []string{"sts.amazonaws.com"},
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
		"kubernetes.io": map[string]any{
			"namespace":      namespace,
			"serviceaccount": map[string]any{"name": name},
		},
	}
}

// irsaSetup creates the cluster's provider and a role trusting it.
func irsaSetup(t *testing.T, s *Server, namespace, name string) (issuer, roleARN string) {
	t.Helper()
	issuer = s.IssuerURL("demo")
	if issuer == "" {
		t.Fatal("the test server has no OIDC issuer")
	}
	if code, body := iamQuery(t, s, "CreateOpenIDConnectProvider",
		"Url", issuer,
		"ClientIDList.member.1", "sts.amazonaws.com"); code != 200 {
		t.Fatalf("creating the provider: %d %s", code, body)
	}
	bare := strings.TrimPrefix(issuer, "https://")
	trust := fmt.Sprintf(`{
	  "Version": "2012-10-17",
	  "Statement": [{
	    "Effect": "Allow",
	    "Principal": {"Federated": %q},
	    "Action": "sts:AssumeRoleWithWebIdentity",
	    "Condition": {"StringEquals": {
	      %q: "system:serviceaccount:%s:%s",
	      %q: "sts.amazonaws.com"
	    }}
	  }]
	}`, s.oidcProviderARN(issuer), bare+":sub", namespace, name, bare+":aud")
	if code, body := iamQuery(t, s, "CreateRole",
		"RoleName", "app", "AssumeRolePolicyDocument", trust); code != 200 {
		t.Fatalf("creating the role: %d %s", code, body)
	}
	return issuer, "arn:aws:iam::" + s.cfg.AccountID + ":role/app"
}

// webIdentity posts the call the way a pod's SDK does: UNSIGNED, because
// the pod holds no AWS credentials -- the token is the whole credential.
func webIdentity(t *testing.T, s *Server, roleARN, token string) (int, string) {
	t.Helper()
	form := url.Values{
		"Action":           {"AssumeRoleWithWebIdentity"},
		"Version":          {stsVersion},
		"RoleArn":          {roleARN},
		"RoleSessionName":  {"pod"},
		"WebIdentityToken": {token},
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestAPodAssumesItsRoleWithAProjectedToken(t *testing.T) {
	s := newIRSAServer(t)
	issuer, roleARN := irsaSetup(t, s, "demo", "app")
	token := signSAToken(t, s, saClaimsFor(issuer, "demo", "app"))

	code, body := webIdentity(t, s, roleARN, token)
	if code != 200 {
		t.Fatalf("assume: %d %s", code, body)
	}
	for _, want := range []string{
		"<AccessKeyId>ASIA",
		"system:serviceaccount:demo:app", // SubjectFromWebIdentityToken
		"sts.amazonaws.com",              // Audience
		"oidc-provider/",                 // Provider
		"assumed-role/app/pod",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the response is missing %q:\n%s", want, body)
		}
	}
	// The credentials must map BACK to the role, or an S3 call made with
	// them would be judged as the account root and every denial this
	// milestone exists to demonstrate would silently pass.
	keyStart := strings.Index(body, "<AccessKeyId>") + len("<AccessKeyId>")
	key := body[keyStart : keyStart+strings.Index(body[keyStart:], "<")]
	c, ok := s.callerForKey(key)
	if !ok {
		t.Fatalf("the issued key %q maps to no principal", key)
	}
	if c.Principal.ID != roleARN {
		t.Errorf("the key maps to %q, want the role %q", c.Principal.ID, roleARN)
	}
}

// Every way a token can be wrong, and each must be refused for its OWN
// reason -- a single "invalid token" for all of them is how somebody
// spends an afternoon on a clock skew.
func TestABadTokenIsRefusedForItsOwnReason(t *testing.T) {
	s := newIRSAServer(t)
	issuer, roleARN := irsaSetup(t, s, "demo", "app")

	// A second key, to stand for a token signed by somebody else.
	otherDir := t.TempDir()
	other, err := core.EnsureSigningKey(otherDir)
	if err != nil {
		t.Fatal(err)
	}

	expired := saClaimsFor(issuer, "demo", "app")
	expired["exp"] = time.Now().Add(-time.Minute).Unix()

	wrongIssuer := saClaimsFor("https://evil.example.com/oidc/ABC", "demo", "app")

	// The sharper case: an issuer that IS registered as an OIDC provider
	// in this account, but is not one this Nimbus serves -- a real EKS
	// issuer, say. Our signing key says nothing about it, so without the
	// issuer check a token we signed would be accepted as coming from
	// somebody else's cluster. This is the only case that can reach the
	// check, because an unregistered issuer is stopped one step later by
	// the provider lookup.
	foreign := "https://oidc.eks.us-east-1.amazonaws.com/id/FOREIGN"
	if code, body := iamQuery(t, s, "CreateOpenIDConnectProvider",
		"Url", foreign, "ClientIDList.member.1", "sts.amazonaws.com"); code != 200 {
		t.Fatalf("registering the foreign issuer: %d %s", code, body)
	}

	multiAud := saClaimsFor(issuer, "demo", "app")
	multiAud["aud"] = []string{"sts.amazonaws.com", "something-else"}

	cases := []struct {
		name, token, wantCode, wantIn string
	}{
		{"not a jwt at all", "nonsense", "InvalidIdentityToken", "three parts"},
		{"signed by another key",
			signSATokenWith(t, other.Private, s.signing.KeyID(), "RS256", saClaimsFor(issuer, "demo", "app")),
			"InvalidIdentityToken", "signature"},
		{"alg none -- the oldest JWT attack",
			signSATokenWith(t, s.signing.Private, s.signing.KeyID(), "none", saClaimsFor(issuer, "demo", "app")),
			"InvalidIdentityToken", "RS256"},
		{"an unknown kid",
			signSATokenWith(t, s.signing.Private, "not-our-kid", "RS256", saClaimsFor(issuer, "demo", "app")),
			"InvalidIdentityToken", "kid"},
		{"expired", signSAToken(t, s, expired), "ExpiredTokenException", "expired"},
		{"an issuer nobody registered", signSAToken(t, s, wrongIssuer),
			"InvalidIdentityToken", "not an issuer this Nimbus serves"},
		{"an issuer registered but served elsewhere",
			signSAToken(t, s, saClaimsFor(foreign, "demo", "app")),
			"InvalidIdentityToken", "not an issuer this Nimbus serves"},
		{"more than one audience", signSAToken(t, s, multiAud), "InvalidIdentityToken", "audience"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := webIdentity(t, s, roleARN, c.token)
			if code == 200 {
				t.Fatalf("the token was accepted:\n%s", body)
			}
			if !strings.Contains(body, c.wantCode) {
				t.Errorf("want error code %s, got:\n%s", c.wantCode, body)
			}
			if !strings.Contains(body, c.wantIn) {
				t.Errorf("the message does not mention %q:\n%s", c.wantIn, body)
			}
		})
	}
}

// The token being genuine is only half of it: the ROLE has to trust this
// particular service account. A valid token for the wrong one is the whole
// point of the `:sub` condition.
func TestTheTrustPolicyDecidesWhichServiceAccount(t *testing.T) {
	s := newIRSAServer(t)
	issuer, roleARN := irsaSetup(t, s, "demo", "app")

	// Same cluster, same signing key, a different service account.
	token := signSAToken(t, s, saClaimsFor(issuer, "demo", "intruder"))
	code, body := webIdentity(t, s, roleARN, token)
	if code == 200 {
		t.Fatalf("a role trusting demo:app was assumed by demo:intruder:\n%s", body)
	}
	if !strings.Contains(body, "AccessDenied") {
		t.Errorf("want AccessDenied, got:\n%s", body)
	}
	// The refusal has to name the subject, because the condition that
	// refused it is the one thing somebody has to go and look at.
	if !strings.Contains(body, "system:serviceaccount:demo:intruder") {
		t.Errorf("the refusal does not name the subject:\n%s", body)
	}

	// A role whose trust policy says nothing about web identities cannot
	// be assumed this way either, however good the token is.
	if code, body := iamQuery(t, s, "CreateRole", "RoleName", "other",
		"AssumeRolePolicyDocument", `{"Version":"2012-10-17","Statement":[{
		   "Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},
		   "Action":"sts:AssumeRole"}]}`); code != 200 {
		t.Fatalf("creating the second role: %d %s", code, body)
	}
	good := signSAToken(t, s, saClaimsFor(issuer, "demo", "app"))
	code, body = webIdentity(t, s, "arn:aws:iam::"+s.cfg.AccountID+":role/other", good)
	if code == 200 {
		t.Fatalf("a role with no web-identity trust was assumed:\n%s", body)
	}
	if !strings.Contains(body, "AccessDenied") {
		t.Errorf("want AccessDenied, got:\n%s", body)
	}
}

// An unsigned POST to `/` carrying a Query action is STS, and nothing else
// in AWS looks like that. Without this rule the call lands in S3 --
// `serviceOf` falls back to it for an unsigned request -- and the pod gets
// an XML error about a bucket.
func TestAnUnsignedQueryCallRoutesToSTS(t *testing.T) {
	s := newIRSAServer(t)
	form := url.Values{"Action": {"AssumeRoleWithWebIdentity"}, "Version": {stsVersion}}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	// It reached STS: the complaint is about the missing parameters, in
	// the STS namespace, not about a bucket.
	if !strings.Contains(rec.Body.String(), "sts.amazonaws.com/doc/") {
		t.Errorf("an unsigned Query POST did not reach STS:\n%s", rec.Body.String())
	}
	// An unsigned GET is still S3, which is what path-style S3 needs.
	if got := serviceOf(httptest.NewRequest(http.MethodGet, "/bucket/key", nil)); got != "s3" {
		t.Errorf("an unsigned GET routed to %q, want s3", got)
	}
}

// The arguments that make a cluster sign tokens Nimbus can verify have to
// reach the CONTAINER. Asserting that `irsaClusterArgs` returns them would
// prove the function and not the wiring -- this repo's signature trap,
// and the one that left `vm.Engine.Reattach` unit-tested and called by
// nobody -- so this goes through the real create path and reads what
// Docker was asked to run.
func TestAClusterIsStartedSigningWithNimbusKey(t *testing.T) {
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	key, err := core.EnsureSigningKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The key goes in the CONFIG, because the k3s arguments are built in
	// New(): a key set afterwards reaches no cluster. Short timeout
	// because the readiness probe cannot succeed against the Fake and
	// would otherwise hold a goroutine for the default four minutes.
	s := New(Config{
		PublicURL:      "http://localhost:4702",
		TLSURL:         "https://localhost:4704",
		DataDir:        dir,
		SigningKey:     key,
		ClusterTimeout: 200 * time.Millisecond,
	}, store, bus)
	f := docker.NewFake()
	s.UseDocker(f)
	makeIRSACluster(t, s, "irsa")

	// Through the Fake's LOCKED accessors, never `f.Runs` directly. The
	// provisioning goroutine is still appending to that slice while this
	// polls it, and reading it raw is a data race -- caught by the
	// `-race` leg on Linux, which is what that leg is for. `Containers`
	// and `RunOptionsFor` already existed; scanning the slice by hand was
	// reinventing a shared helper, which is its own recurring mistake
	// here.
	var opts docker.RunOptions
	found := false
	for deadline := time.Now().Add(10 * time.Second); !found && time.Now().Before(deadline); {
		for _, c := range f.Containers() {
			if strings.Contains(c.Name, "irsa") {
				if opts, found = f.RunOptionsFor(c.Name); found {
					break
				}
			}
		}
		if !found {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !found {
		t.Fatal("no k3s container was started for the cluster")
	}

	args := strings.Join(opts.Args, " ")
	issuer := s.IssuerURL("irsa")
	for _, want := range []string{
		"--service-account-issuer=" + issuer,
		"--service-account-signing-key-file=" + saSignerKeyInContainer,
		"--service-account-key-file=" + saSignerPubInContainer,
		// k3s's own key stays listed, or the cluster stops accepting
		// anything it signed with it.
		"--service-account-key-file=" + k3sOwnSAKey,
		"--api-audiences=" + irsaAudience,
	} {
		if !strings.Contains(args, want) {
			t.Errorf("the k3s container was not given %q:\n%s", want, args)
		}
	}
	// The issuer must be in the audiences too: that is Kubernetes' own
	// default, and in-cluster clients ask for tokens with it.
	if !strings.Contains(args, "--api-audiences="+irsaAudience+","+issuer) {
		t.Errorf("the issuer is not among the api audiences:\n%s", args)
	}
	// And the key has to be MOUNTED, not merely named. k3s reads the path
	// at startup, so a file written afterwards is a file that was never
	// there.
	if opts.Volumes[key.Private] != saSignerKeyInContainer {
		t.Errorf("the private key is not mounted at %s: %v", saSignerKeyInContainer, opts.Volumes)
	}
	if opts.Volumes[key.Public] != saSignerPubInContainer {
		t.Errorf("the public key is not mounted at %s: %v", saSignerPubInContainer, opts.Volumes)
	}
}
