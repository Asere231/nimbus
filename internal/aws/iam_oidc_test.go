package aws

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// The IAM OIDC identity provider, and the trust-policy shapes that go
// with it. Every EKS IRSA module creates one of these per cluster.

func iamQuery(t *testing.T, s *Server, action string, pairs ...string) (int, string) {
	t.Helper()
	form := url.Values{"Action": {action}, "Version": {"2010-05-08"}}
	for i := 0; i+1 < len(pairs); i += 2 {
		form.Set(pairs[i], pairs[i+1])
	}
	code, body := query(t, s, "iam", form.Encode())
	return code, string(body)
}

func TestAnOIDCProviderRoundTrips(t *testing.T) {
	s := newEKSTestServer(t)
	issuer := "https://localhost:4704/oidc/ABC123"

	code, body := iamQuery(t, s, "CreateOpenIDConnectProvider",
		"Url", issuer,
		"ClientIDList.member.1", "sts.amazonaws.com",
		"ThumbprintList.member.1", "9e99a48a9960b14926bb7f3b02e22da2b0ab7280")
	if code != 200 {
		t.Fatalf("create: %d %s", code, body)
	}
	// The ARN is AWS's own derivation -- the url with its scheme stripped
	// -- because a role's trust policy names it as its Federated
	// principal, and the two have to be writable in one plan.
	wantARN := "arn:aws:iam::000000000000:oidc-provider/localhost:4704/oidc/ABC123"
	if !strings.Contains(body, wantARN) {
		t.Errorf("the ARN is not derived from the issuer:\n%s", body)
	}

	code, body = iamQuery(t, s, "GetOpenIDConnectProvider", "OpenIDConnectProviderArn", wantARN)
	if code != 200 {
		t.Fatalf("get: %d %s", code, body)
	}
	for _, want := range []string{issuer, "sts.amazonaws.com", "9e99a48a9960b14926bb7f3b02e22da2b0ab7280"} {
		if !strings.Contains(body, want) {
			t.Errorf("the provider read back is missing %q:\n%s", want, body)
		}
	}

	if code, _ := iamQuery(t, s, "ListOpenIDConnectProviders"); code != 200 {
		t.Errorf("list: %d", code)
	}
	// Creating the same issuer twice is a conflict, not two providers.
	if code, _ := iamQuery(t, s, "CreateOpenIDConnectProvider", "Url", issuer); code != 409 {
		t.Errorf("a duplicate provider was accepted with %d", code)
	}
	if code, _ := iamQuery(t, s, "DeleteOpenIDConnectProvider",
		"OpenIDConnectProviderArn", wantARN); code != 200 {
		t.Errorf("delete: %d", code)
	}
	if code, _ := iamQuery(t, s, "GetOpenIDConnectProvider",
		"OpenIDConnectProviderArn", wantARN); code != 404 {
		t.Errorf("the deleted provider still reads: %d", code)
	}
}

// A client may send the url with or without its scheme, and with or
// without a trailing slash. All of them have to be the same provider, or
// a token's `iss` would fail to match for a reason nothing would name.
func TestAnIssuerIsNormalisedBeforeItIsCompared(t *testing.T) {
	cases := map[string]string{
		"https://localhost:4704/oidc/A":     "https://localhost:4704/oidc/A",
		"localhost:4704/oidc/A":             "https://localhost:4704/oidc/A",
		"https://localhost:4704/oidc/A/":    "https://localhost:4704/oidc/A",
		"  https://localhost:4704/oidc/A  ": "https://localhost:4704/oidc/A",
	}
	for given, want := range cases {
		got, err := normaliseIssuer(given)
		if err != nil {
			t.Errorf("%q: %v", given, err)
			continue
		}
		if got != want {
			t.Errorf("%q normalised to %q, want %q", given, got, want)
		}
	}
	if _, err := normaliseIssuer(""); err == nil {
		t.Error("an empty url was accepted")
	}
}

// A trust policy with a Federated principal and web-identity conditions
// has to COMPILE. It used to refuse: the principal was lumped in with
// Service and CanonicalUser, and `<issuer>:sub` is not in the supported
// key table and never can be, because the key name contains the issuer.
func TestAWebIdentityTrustPolicyCompiles(t *testing.T) {
	policy := `{
      "Version": "2012-10-17",
      "Statement": [{
        "Effect": "Allow",
        "Principal": {"Federated": "arn:aws:iam::000000000000:oidc-provider/localhost:4704/oidc/ABC"},
        "Action": "sts:AssumeRoleWithWebIdentity",
        "Condition": {"StringEquals": {
          "localhost:4704/oidc/ABC:sub": "system:serviceaccount:demo:app",
          "localhost:4704/oidc/ABC:aud": "sts.amazonaws.com"
        }}
      }]
    }`
	var doc rawDocument
	if err := json.Unmarshal([]byte(policy), &doc); err != nil {
		t.Fatalf("the trust policy did not parse: %v", err)
	}
	if len(doc.Statement) != 1 {
		t.Fatalf("%d statements", len(doc.Statement))
	}
	// A trust policy is a RESOURCE policy: it names a principal and no
	// resource, which an identity policy requires.
	st, err := compileStatement("trust policy", doc.Statement[0], resourcePolicy,
		"arn:aws:iam::000000000000:role/app")
	if err != nil {
		t.Fatalf("the statement did not compile: %v", err)
	}
	if st.unsupported != nil {
		t.Fatalf("the statement was refused: %v", st.unsupported)
	}
	if len(st.conditions) != 2 {
		t.Errorf("%d conditions survived, want 2", len(st.conditions))
	}
	if !doc.Statement[0].Principal.matchesFederated(
		"arn:aws:iam::000000000000:oidc-provider/localhost:4704/oidc/ABC") {
		t.Error("the Federated principal does not match its own ARN")
	}
	// And the two questions must not be interchangeable. `matches` asks
	// "is the IAM caller in front of me this principal", and the answer
	// for a federated principal is always no -- even when the caller's
	// own id is the provider ARN, which is the only way this assertion
	// can tell a correct implementation from one that folds the two
	// together.
	provider := "arn:aws:iam::000000000000:oidc-provider/localhost:4704/oidc/ABC"
	if doc.Statement[0].Principal.matches(provider) {
		t.Error("a Federated principal was matched as an ordinary IAM caller")
	}
	if doc.Statement[0].Principal.matches("arn:aws:iam::000000000000:role/app") {
		t.Error("a Federated principal matched an IAM role")
	}
}

// Only `:sub` and `:aud` behind something issuer-shaped. An ordinary
// mistyped key must still be refused by name rather than slipping
// through as a web-identity key and never matching.
func TestOnlyRealWebIdentityKeysAreAccepted(t *testing.T) {
	accept := []string{
		"localhost:4704/oidc/abc:sub",
		"oidc.eks.us-east-1.amazonaws.com/id/abc:aud",
	}
	refuse := []string{
		"aws:sourceip",            // a real key, handled by the table
		"notakey",                 // no colon at all
		"localhost:4704/oidc/a:x", // not sub or aud
		"sub",                     // bare
		"thing:sub",               // nothing issuer-shaped in front
	}
	for _, k := range accept {
		if !isWebIdentityConditionKey(k) {
			t.Errorf("%q was not recognised as a web-identity key", k)
		}
	}
	for _, k := range refuse {
		if isWebIdentityConditionKey(k) {
			t.Errorf("%q was taken for a web-identity key", k)
		}
	}
}
