package aws

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Asere231/nimbus/internal/core"
)

// The IAM OIDC identity provider: `aws_iam_openid_connect_provider`.
//
// This is the resource that tells IAM "tokens from this issuer may be
// used to assume roles here". Every EKS IRSA module creates exactly one
// per cluster, from the cluster's own issuer and a thumbprint read with
// `data.tls_certificate`.
//
// Its ARN is DERIVED from the issuer rather than generated, because that
// is how AWS builds it -- `arn:aws:iam::<account>:oidc-provider/<host and
// path of the url, without the scheme>` -- and because a role's trust
// policy names that ARN as its `Federated` principal. A generated id
// would mean the trust policy and the provider could not be written in
// one Terraform plan, since the policy is built from the ARN.

const typeOIDCProvider = "AWS::IAM::OIDCProvider"

// oidcProvider is the stored shape.
type oidcProvider struct {
	Arn            string   `json:"Arn"`
	Url            string   `json:"Url"`
	ClientIDList   []string `json:"ClientIDList"`
	ThumbprintList []string `json:"ThumbprintList"`
	// A TIMESTAMP, not an epoch, and `iamNow()` is the one every other
	// IAM resource here uses. Measured the hard way: as a JSON number it
	// serialised as `1.795653789975e+09`, which the `aws` CLI accepted
	// and aws provider 5.100.0 refused outright -- "unable to parse time
	// string ... cannot parse as 2006" -- with a 200 status, so it reads
	// as a deserialization bug in the client. The CLI being happy is not
	// evidence the shape is right.
	CreateDate string            `json:"CreateDate"`
	Tags       map[string]string `json:"nimbusTags,omitempty"`
}

// oidcProviderARN is AWS's own derivation: the URL with its scheme
// stripped. `https://oidc.eks.../id/X` becomes
// `arn:aws:iam::123:oidc-provider/oidc.eks.../id/X`.
func (s *Server) oidcProviderARN(issuer string) string {
	return s.iamARN("oidc-provider", "/", strings.TrimPrefix(
		strings.TrimPrefix(issuer, "https://"), "http://"))
}

// normaliseIssuer is how a URL is compared. AWS stores the url without
// its scheme in the ARN but reports it WITH `https://` in `Url`, and a
// client may send either -- so both are reduced to the same thing before
// anything is looked up.
func normaliseIssuer(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("Url is required")
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%q is not a URL", raw)
	}
	// A trailing slash is not part of an issuer: a token's `iss` never
	// carries one, and comparing them unequal would make every
	// verification fail for a reason nothing would name.
	return "https://" + u.Host + strings.TrimSuffix(u.Path, "/"), nil
}

func (s *Server) createOIDCProvider(w http.ResponseWriter, p params) *apiError {
	issuer, err := normaliseIssuer(p.str("Url"))
	if err != nil {
		return errorf(400, "InvalidInput", "%v", err)
	}
	arn := s.oidcProviderARN(issuer)
	if s.store.Exists(arn) {
		return errorf(409, "EntityAlreadyExists",
			"An OpenID Connect provider for %s already exists", issuer)
	}
	prov := oidcProvider{
		Arn:            arn,
		Url:            issuer,
		ClientIDList:   p.list("ClientIDList.member", ""),
		ThumbprintList: p.list("ThumbprintList.member", ""),
		CreateDate:     iamNow(),
		Tags:           tagMap(readIAMTags(p)),
	}
	if aerr := s.saveOIDCProvider(&prov); aerr != nil {
		return aerr
	}
	writeFlat(w, "CreateOpenIDConnectProvider", "CreateOpenIDConnectProviderResult",
		createOIDCResult{Arn: arn})
	return nil
}

func (s *Server) saveOIDCProvider(p *oidcProvider) *apiError {
	body, err := json.Marshal(p)
	if err != nil {
		return errorf(500, "ServiceFailure", "%v", err)
	}
	s.store.Put(&core.Resource{
		Cloud: "aws", ID: p.Arn, Type: typeOIDCProvider,
		Name: p.Url, ParentID: s.accountARN(), Body: body,
	})
	return nil
}

func (s *Server) loadOIDCProvider(arn string) (*oidcProvider, *apiError) {
	res, err := s.store.Get(arn)
	if err != nil || res.Type != typeOIDCProvider {
		return nil, errorf(404, "NoSuchEntity",
			"OpenIDConnect Provider not found for arn %s", arn)
	}
	var p oidcProvider
	if err := json.Unmarshal(res.Body, &p); err != nil {
		return nil, errorf(500, "ServiceFailure", "%v", err)
	}
	return &p, nil
}

// oidcProviderFor finds the provider registered for an issuer, which is
// what AssumeRoleWithWebIdentity needs: a token names its issuer, and
// only an issuer somebody registered may be used to assume a role.
func (s *Server) oidcProviderFor(issuer string) (*oidcProvider, bool) {
	normalised, err := normaliseIssuer(issuer)
	if err != nil {
		return nil, false
	}
	p, aerr := s.loadOIDCProvider(s.oidcProviderARN(normalised))
	if aerr != nil {
		return nil, false
	}
	return p, true
}

// tagMap flattens IAM's tag list, which is how this resource stores them.
func tagMap(tags []iamTag) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, t := range tags {
		out[t.Key] = t.Value
	}
	return out
}

// listOIDCProviders answers ListOpenIDConnectProviders, which reports ARNs
// only -- the full document needs a Get per provider, as the real API
// does.
func (s *Server) listOIDCProviders(w http.ResponseWriter) *apiError {
	var arns []oidcProviderEntry
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return r.Type == typeOIDCProvider
	}) {
		arns = append(arns, oidcProviderEntry{Arn: res.ID})
	}
	writeMembers(w, "ListOpenIDConnectProviders", "ListOpenIDConnectProvidersResult",
		"OpenIDConnectProviderList", arns)
	return nil
}

func (s *Server) getOIDCProvider(w http.ResponseWriter, p params) *apiError {
	prov, aerr := s.loadOIDCProvider(p.str("OpenIDConnectProviderArn"))
	if aerr != nil {
		return aerr
	}
	// Note what is NOT here: the Arn. The real GetOpenIDConnectProvider
	// result reports Url, ClientIDList, ThumbprintList, CreateDate and
	// Tags -- the caller already has the ARN, since it asked by it.
	writeFlat(w, "GetOpenIDConnectProvider", "GetOpenIDConnectProviderResult",
		getOIDCResult{prov.Url, prov.ClientIDList, prov.ThumbprintList, prov.CreateDate})
	return nil
}

func (s *Server) deleteOIDCProvider(w http.ResponseWriter, p params) *apiError {
	arn := p.str("OpenIDConnectProviderArn")
	if _, aerr := s.loadOIDCProvider(arn); aerr != nil {
		return aerr
	}
	_, _ = s.store.Delete(arn)
	writeIAMEmpty(w, "DeleteOpenIDConnectProvider")
	return nil
}

// updateOIDCProviderThumbprint answers
// UpdateOpenIDConnectProviderThumbprint, which the provider calls when a
// thumbprint changes rather than replacing the resource.
func (s *Server) updateOIDCProviderThumbprint(w http.ResponseWriter, p params) *apiError {
	prov, aerr := s.loadOIDCProvider(p.str("OpenIDConnectProviderArn"))
	if aerr != nil {
		return aerr
	}
	prov.ThumbprintList = p.list("ThumbprintList.member", "")
	if aerr := s.saveOIDCProvider(prov); aerr != nil {
		return aerr
	}
	writeIAMEmpty(w, "UpdateOpenIDConnectProviderThumbprint")
	return nil
}

// Named result types, because encoding/xml cannot marshal an anonymous
// struct: it names an element after its Go type and an anonymous one has
// no name to use.
type createOIDCResult struct {
	Arn string `xml:"OpenIDConnectProviderArn"`
}

type getOIDCResult struct {
	Url            string   `xml:"Url"`
	ClientIDList   []string `xml:"ClientIDList>member"`
	ThumbprintList []string `xml:"ThumbprintList>member"`
	CreateDate     string   `xml:"CreateDate"`
}

type oidcProviderEntry struct {
	Arn string `xml:"Arn"`
}
