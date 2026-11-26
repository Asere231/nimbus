package aws

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/authz"
)

// AssumeRoleWithWebIdentity: a pod proves who it is with a token, and gets
// credentials for a role.
//
// This is the one call in Nimbus where a SIGNATURE DECIDES. Everywhere
// else "the signature is the routing table" -- a local emulator rejecting
// a SigV4 signature could only ever be reporting a clock skew -- but
// IRSA's entire claim is that a pod's identity cannot be forged, and the
// only thing standing behind that claim is the service-account signing
// key. So this path verifies RS256 against the JWKS it publishes, and a
// token signed by anything else is refused.
//
// # It arrives UNSIGNED, and that is a dispatch rule of its own
//
// A pod holds no AWS credentials: the token IS the credential, which is
// the whole reason IRSA exists. So the request carries no `Authorization`
// header, and `serviceOf` falls back to S3 for an unsigned request -- so
// without a rule for it the call lands in S3 and the pod is told
// `MethodNotAllowed: POST on the service root`, which names nothing it
// asked for. Measured, before this existed.
//
// The rule is narrow by construction: an unsigned POST to `/` whose body
// is form-encoded is a Query-protocol call, and no S3 operation looks like
// that -- S3's unsigned requests are GETs and PUTs on a `/{bucket}/{key}`
// path, and its one POST form upload addresses a bucket. STS is the only
// Query service with an unsigned call, so that shape is STS.

// webIdentityAudienceLimit: see assumeRoleWithWebIdentity. A token with
// several audiences cannot be evaluated against a `:aud` condition here,
// because that needs IAM's ForAnyValue quantifier over a multivalued key
// and every condition key in Nimbus is a single string.

type assumeRoleWithWebIdentityResponse struct {
	XMLName xml.Name `xml:"AssumeRoleWithWebIdentityResponse"`
	Xmlns   string   `xml:"xmlns,attr"`
	Result  struct {
		Credentials struct {
			AccessKeyID     string `xml:"AccessKeyId"`
			SecretAccessKey string `xml:"SecretAccessKey"`
			SessionToken    string `xml:"SessionToken"`
			Expiration      string `xml:"Expiration"`
		} `xml:"Credentials"`
		SubjectFromWebIdentityToken string `xml:"SubjectFromWebIdentityToken"`
		AssumedRoleUser             struct {
			Arn           string `xml:"Arn"`
			AssumedRoleID string `xml:"AssumedRoleId"`
		} `xml:"AssumedRoleUser"`
		Audience         string `xml:"Audience"`
		Provider         string `xml:"Provider"`
		PackedPolicySize int    `xml:"PackedPolicySize"`
	} `xml:"AssumeRoleWithWebIdentityResult"`
	Metadata responseMetadata `xml:"ResponseMetadata"`
}

// saToken is the part of a projected service-account token that decides
// anything. Everything else it carries -- the `kubernetes.io` block, the
// pod's name and uid -- is information for a human and is not read.
type saToken struct {
	Issuer   string
	Subject  string
	Audience string
}

func (s *Server) assumeRoleWithWebIdentity(w http.ResponseWriter, r *http.Request, p params) {
	roleARN := p.str("RoleArn")
	raw := p.str("WebIdentityToken")
	switch {
	case roleARN == "":
		writeError(w, "sts", errorf(400, "ValidationError",
			"1 validation error detected: Value null at 'roleArn' failed to satisfy constraint: Member must not be null"))
		return
	case raw == "":
		writeError(w, "sts", errorf(400, "ValidationError",
			"1 validation error detected: Value null at 'webIdentityToken' failed to satisfy constraint: Member must not be null"))
		return
	}

	tok, aerr := s.verifyWebIdentityToken(raw)
	if aerr != nil {
		writeError(w, "sts", aerr)
		return
	}

	// The issuer has to be registered as an OIDC provider in the account,
	// as real AWS requires: the provider is what a trust policy names as
	// its Federated principal, so without one there is nothing a policy
	// could be written against.
	provider, ok := s.oidcProviderFor(tok.Issuer)
	if !ok {
		writeError(w, "sts", errorf(400, "InvalidIdentityToken",
			"no OIDC identity provider is registered for the token's issuer %q; "+
				"create an aws_iam_openid_connect_provider for it", tok.Issuer))
		return
	}
	// The audience must be one the provider was created with. AWS calls
	// these client ids; for IRSA it is `sts.amazonaws.com`.
	if len(provider.ClientIDList) > 0 && !anyEqual(provider.ClientIDList, tok.Audience) {
		writeError(w, "sts", errorf(403, "AccessDenied",
			"the token's audience %q is not in the client id list of %s",
			tok.Audience, provider.Arn))
		return
	}

	// A role is addressed by ARN here, where every IAM call names it by
	// RoleName -- so the name is read off the ARN rather than a second
	// lookup path being invented. A malformed one simply finds no role.
	var role iamRole
	if aerr := s.loadIAM("role", roleNameFromARN(roleARN), &role); aerr != nil {
		writeError(w, "sts", aerr)
		return
	}

	// The trust policy decides, and NOTHING ELSE does. Real AWS requires
	// no permission of the caller for this call -- it could not, since the
	// caller has no identity until it succeeds -- so the role's own
	// AssumeRolePolicyDocument is the whole of the authorization.
	if decision := s.evaluateTrustPolicy(r, role, provider.Arn, tok); !decision.Allowed {
		w.Header().Set(deniedReasonHeader, decision.Reason)
		writeError(w, "sts", errorf(403, "AccessDenied",
			"%s is not authorized to perform sts:AssumeRoleWithWebIdentity on %s: %s",
			tok.Subject, roleARN, decision.Reason))
		return
	}

	sessionName := p.strOr("RoleSessionName", "nimbus")
	duration := time.Duration(p.intOr("DurationSeconds", 3600)) * time.Second

	doc := assumeRoleWithWebIdentityResponse{Xmlns: namespaceFor("sts")}
	c := &doc.Result.Credentials
	// The SAME derivation AssumeRole uses, so that a later request
	// carrying this key maps back to the role through principal.go. A
	// separate scheme here would leave a pod's calls resolving to the
	// account root -- unrestricted -- and every denial this exists to
	// demonstrate would silently pass.
	c.AccessKeyID = sessionKeyFor(role.Arn)
	c.SecretAccessKey = stableID(role.Arn+"/secret", 40)
	c.SessionToken = "nimbus/" + stableID(role.Arn+"/"+sessionName, 32)
	c.Expiration = time.Now().UTC().Add(duration).Format(time.RFC3339)
	doc.Result.SubjectFromWebIdentityToken = tok.Subject
	doc.Result.Audience = tok.Audience
	doc.Result.Provider = provider.Arn
	doc.Result.AssumedRoleUser.Arn = "arn:aws:sts::" + s.cfg.AccountID +
		":assumed-role/" + role.RoleName + "/" + sessionName
	doc.Result.AssumedRoleUser.AssumedRoleID =
		"AROA" + strings.ToUpper(stableID(role.Arn, 17)) + ":" + sessionName
	doc.Metadata.RequestID = requestID()
	writeXML(w, 200, doc)
}

// evaluateTrustPolicy asks the role's own document whether this web
// identity may assume it.
//
// The principal is the OIDC PROVIDER, not an IAM caller, which is why
// `rawPrincipal` keeps `Federated` separate and `matchesPrincipal`
// dispatches on the kind: folding the two together would let a role whose
// Federated principal happens to equal a caller's ARN be assumed by that
// caller directly.
func (s *Server) evaluateTrustPolicy(r *http.Request, role iamRole, providerARN string, tok *saToken) authz.Decision {
	rules := s.policies.rules([]policyDocument{{
		Source:   "the trust policy of " + role.Arn,
		Document: role.AssumeRolePolicyDocument,
		Kind:     resourcePolicy,
		Attached: role.Arn,
	}})

	// The condition keys are named after the issuer WITHOUT its scheme,
	// which is how every IRSA module writes them and how AWS stores the
	// issuer in the provider ARN.
	bare := strings.TrimPrefix(tok.Issuer, "https://")
	principal := authz.Principal{ID: providerARN, Name: bare, Kind: webIdentityKind}
	ctx := s.conditionContext(r, principal)
	ctx[bare+":sub"] = tok.Subject
	ctx[bare+":aud"] = tok.Audience

	return authz.Evaluate(authz.Request{
		Principal: principal,
		Action:    "sts:AssumeRoleWithWebIdentity",
		Resource:  role.Arn,
		Context:   ctx,
	}, rules)
}

// webIdentityKind marks a principal that is a federated identity rather
// than an IAM user or role.
const webIdentityKind = "webIdentity"

// verifyWebIdentityToken checks the token is one k3s signed with Nimbus's
// service-account key, and returns the three claims that decide anything.
//
// Every failure is refused for its OWN reason. A single "invalid token"
// for all of them is how somebody spends an afternoon on what turns out to
// be a clock skew -- the same argument the SAS work made about blaming a
// 403 on permissions when the expiry was the problem.
func (s *Server) verifyWebIdentityToken(raw string) (*saToken, *apiError) {
	if s.signing == nil {
		return nil, errorf(400, "InvalidIdentityToken",
			"Nimbus has no service-account signing key, so no token can be verified")
	}
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) != 3 {
		return nil, errorf(400, "InvalidIdentityToken",
			"a JWT has three parts separated by dots; this one has %d", len(parts))
	}

	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeJWTSegment(parts[0], &header); err != nil {
		return nil, errorf(400, "InvalidIdentityToken", "the token header: %v", err)
	}
	// `alg: none` is the oldest JWT attack there is: a token with no
	// signature at all, which a verifier that trusts the header happily
	// accepts. The algorithm is OURS to require, never the token's to
	// choose.
	if header.Alg != "RS256" {
		return nil, errorf(400, "InvalidIdentityToken",
			"the token is signed with %q; a service-account token must be RS256", header.Alg)
	}
	if header.Kid != "" && header.Kid != s.signing.KeyID() {
		return nil, errorf(400, "InvalidIdentityToken",
			"the token names kid %q, which is not the key this cluster's issuer publishes",
			header.Kid)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errorf(400, "InvalidIdentityToken", "the signature is not base64url: %v", err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(s.signing.RSAPublicKey(), crypto.SHA256, sum[:], sig); err != nil {
		return nil, errorf(400, "InvalidIdentityToken",
			"the token's signature does not verify against this cluster's issuer")
	}

	var claims struct {
		Issuer    string       `json:"iss"`
		Subject   string       `json:"sub"`
		Audience  stringOrList `json:"aud"`
		Expiry    *int64       `json:"exp"`
		NotBefore *int64       `json:"nbf"`
	}
	if err := decodeJWTSegment(parts[1], &claims); err != nil {
		return nil, errorf(400, "InvalidIdentityToken", "the token claims: %v", err)
	}

	now := time.Now()
	if claims.Expiry == nil {
		return nil, errorf(400, "InvalidIdentityToken", "the token carries no exp")
	}
	if exp := time.Unix(*claims.Expiry, 0); now.After(exp) {
		return nil, errorf(400, "ExpiredTokenException",
			"the web identity token expired at %s", exp.UTC().Format(time.RFC3339))
	}
	if claims.NotBefore != nil {
		if nbf := time.Unix(*claims.NotBefore, 0); now.Before(nbf) {
			return nil, errorf(400, "InvalidIdentityToken",
				"the token is not valid before %s", nbf.UTC().Format(time.RFC3339))
		}
	}
	if claims.Subject == "" {
		return nil, errorf(400, "InvalidIdentityToken", "the token carries no sub")
	}
	// The issuer must be one of ours. A token signed by our key but
	// claiming somebody else's issuer is refused HERE rather than at the
	// provider lookup, so the message says which half is wrong.
	if s.cfg.TLSURL == "" || !strings.HasPrefix(claims.Issuer, s.cfg.TLSURL+oidcPrefix) {
		return nil, errorf(400, "InvalidIdentityToken",
			"the token's issuer %q is not an issuer this Nimbus serves", claims.Issuer)
	}
	// A `:aud` condition is a comparison against a SINGLE string here:
	// IAM evaluates a multivalued key with ForAnyValue/ForAllValues,
	// which Nimbus deliberately does not support, because every condition
	// key it can answer is one string. Refusing says so; picking one of
	// several audiences would quietly evaluate a different policy than
	// the one that is written.
	if len(claims.Audience) != 1 {
		return nil, errorf(400, "InvalidIdentityToken",
			"the token carries %d audiences; Nimbus can evaluate a `:aud` condition "+
				"against one only", len(claims.Audience))
	}

	return &saToken{
		Issuer:   strings.TrimSuffix(claims.Issuer, "/"),
		Subject:  claims.Subject,
		Audience: claims.Audience[0],
	}, nil
}

// decodeJWTSegment is base64url with NO padding, which is JOSE's rule for
// every segment -- the same encoding a JWK's `n` and `e` use.
func decodeJWTSegment(seg string, into any) error {
	body, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return fmt.Errorf("not base64url: %w", err)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("not JSON: %w", err)
	}
	return nil
}

// roleNameFromARN is the last path segment of a role ARN. A role may have
// a path (`arn:aws:iam::1:role/team/app`), and IAM's own calls take the
// NAME alone, so everything after the last slash is what identifies it.
func roleNameFromARN(arn string) string {
	if i := strings.LastIndex(arn, "/"); i >= 0 {
		return arn[i+1:]
	}
	return arn
}
