package aws

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/Asere231/nimbus/internal/authz"
	"github.com/Asere231/nimbus/internal/core"
)

// Who is calling.
//
// Nimbus never verifies a signature, so identity here is whatever the
// client claims -- and that is fine, because the question this answers is
// "would these permissions work", not "is this caller genuine". What is
// needed is a way for a client to claim a DIFFERENT identity, so that one
// Nimbus can show two principals getting different answers.
//
// The access key id in the SigV4 credential scope is that way, and it needs
// no new state at all:
//
//   - `sts:AssumeRole` already mints a key DERIVED from the role's ARN
//     (`"ASIA" + stableID(roleARN, 16)`, see sts.go). So a key can be
//     mapped back to its role by deriving each role's key and comparing --
//     nothing to persist, nothing to invalidate, and it survives a restart
//     for the same reason the AKS kubeconfig does.
//   - A key that is literally a role or user NAME maps to that entity,
//     which is what makes `aws --profile` usable without assuming anything.
//
// Anything unmapped is the account ROOT, which is allowed everything. That
// is not a fudge: real root carries no policies and can do anything, so it
// is the faithful answer as well as the one that keeps every existing
// example working with `--enforce-iam` on. A denial is something you opt
// into by assuming a limited role.

// caller is a resolved principal and the documents that govern it.
type caller struct {
	authz.Principal
	docs []policyDocument
}

// anonymousPrincipalID is the caller behind an unsigned request. Not an
// ARN, because it is not an identity: nothing but a resource policy
// granting `Principal: "*"` can ever satisfy it.
const anonymousPrincipalID = "anonymous"

// resolveCaller works out who is asking, and what policies apply to them.
//
// An UNSIGNED request to S3 is anonymous, and that is a deliberate
// departure from everything else here. Every other unmapped caller is the
// account root, which is unrestricted -- faithful, and what keeps the
// flag free for configurations that say nothing about IAM. But an
// unsigned request is not an unmapped caller, it is NO caller, and
// treating it as root meant that with --enforce-iam on, sending no
// credentials at all was the most powerful thing you could do. It is also
// the one path `Principal: "*"` exists for: a public bucket cannot be
// demonstrated, or accidentally created, until anonymous is its own
// principal.
//
// Scoped to S3 because S3 is the only service here ever called without
// credentials -- `serviceOf` says the same thing when it routes an
// unsigned request. Narrow on purpose: this is the one change in the
// milestone that can refuse something that worked yesterday.
func (s *Server) resolveCaller(r *http.Request, service string) caller {
	key := accessKeyOf(r)
	if key == "" {
		// --principal wins over anonymous: it is an explicit instruction
		// from whoever started the server, and it exists precisely for
		// clients that cannot set an access key.
		key = s.cfg.Principal
	}
	if key == "" {
		if service == "s3" {
			return caller{Principal: authz.Principal{
				ID: anonymousPrincipalID, Name: "anonymous", Kind: "anonymous",
			}}
		}
		return s.rootCaller()
	}
	if c, ok := s.callerForKey(key); ok {
		return c
	}
	// A --principal naming something that does not exist is deliberately
	// NOT treated as root. It is a principal with no policies, so nothing
	// is allowed and the reason says why -- which is better than silently
	// granting everything to a typo. It also means `--principal app`
	// before Terraform has created `app` refuses, including the call that
	// would create it; the flag's help says so.
	if s.cfg.Principal != "" && key == s.cfg.Principal {
		return caller{Principal: authz.Principal{
			ID: key, Name: key, Kind: "unknown",
		}}
	}
	return s.rootCaller()
}

func (s *Server) rootCaller() caller {
	return caller{Principal: authz.Principal{
		ID: s.accountARN(), Name: "root", Kind: "root", Unrestricted: true,
	}}
}

// accessKeyOf is the access key id the request signed with, from the
// Authorization header or from a presigned URL's query parameter.
func accessKeyOf(r *http.Request) string {
	if c := parseAuthorization(r.Header.Get("Authorization")); c.AccessKey != "" {
		return c.AccessKey
	}
	if q := r.URL.Query().Get("X-Amz-Credential"); q != "" {
		if i := strings.Index(q, "/"); i > 0 {
			return q[:i]
		}
	}
	return ""
}

// callerForKey maps an access key id to a principal, by the two rules
// above. A session key wins over a name, because a session key is an exact
// derivation while a name is a convention.
func (s *Server) callerForKey(key string) (caller, bool) {
	if strings.HasPrefix(key, "ASIA") {
		for _, res := range s.store.List(func(r *core.Resource) bool { return r.Type == typeRole }) {
			var role iamRole
			if json.Unmarshal(res.Body, &role) != nil {
				continue
			}
			if sessionKeyFor(role.Arn) == key {
				return s.callerForRole(role), true
			}
		}
	}

	// A key that is a role's or a user's own name.
	if res, err := s.store.Get(s.iamARN("role", "/", key)); err == nil && res.Type == typeRole {
		var role iamRole
		if json.Unmarshal(res.Body, &role) == nil {
			return s.callerForRole(role), true
		}
	}
	if res, err := s.store.Get(s.iamARN("user", "/", key)); err == nil && res.Type == typeUser {
		var user iamUser
		if json.Unmarshal(res.Body, &user) == nil {
			return caller{
				Principal: authz.Principal{ID: user.Arn, Name: user.UserName, Kind: "user"},
				docs:      s.documentsFor(user.Arn, user.Inline, user.Attached),
			}, true
		}
	}
	return caller{}, false
}

func (s *Server) callerForRole(role iamRole) caller {
	return caller{
		Principal: authz.Principal{ID: role.Arn, Name: role.RoleName, Kind: "role"},
		docs:      s.documentsFor(role.Arn, role.Inline, role.Attached),
	}
}

// documentsFor gathers everything that governs a principal: its inline
// policies and its attached managed ones.
//
// The order is stable -- inline policies by name, then attachments in the
// order they were made -- because the document set is the cache key, and a
// map's iteration order would otherwise compile the same policies afresh
// every few requests.
func (s *Server) documentsFor(arn string, inline map[string]string, attached []attachedPolicy) []policyDocument {
	var docs []policyDocument

	names := make([]string, 0, len(inline))
	for name := range inline {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		docs = append(docs, policyDocument{
			Source:   "inline policy " + name + " on " + arn,
			Document: inline[name],
		})
	}

	for _, a := range attached {
		docs = append(docs, s.attachedDocument(a))
	}
	return docs
}

// attachedDocument finds an attached policy's text: a customer-managed one
// from the store, an AWS-managed one from what Nimbus bundles. Either may
// be missing, and a missing one is carried as an error rather than dropped
// -- see policyDocument.Err.
func (s *Server) attachedDocument(a attachedPolicy) policyDocument {
	doc := policyDocument{Source: "policy " + a.PolicyArn}

	if res, err := s.store.Get(a.PolicyArn); err == nil && res.Type == typePolicy {
		var p iamPolicy
		if json.Unmarshal(res.Body, &p) == nil && p.Document != "" {
			doc.Document = p.Document
			return doc
		}
	}
	if text, err := managedPolicyDocument(a.PolicyArn); err == nil {
		doc.Document = text
		return doc
	} else {
		doc.Err = err
	}
	return doc
}

// sessionKeyFor is the access key id AssumeRole hands out for a role. The
// one definition, used both to mint a key in sts.go and to map one back
// here -- two copies of this derivation would drift and the symptom would
// be a principal that silently became root.
func sessionKeyFor(roleARN string) string {
	return "ASIA" + strings.ToUpper(stableID(roleARN, 16))
}
