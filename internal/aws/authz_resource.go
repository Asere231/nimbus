package aws

import (
	"fmt"
	"strings"

	"github.com/Asere231/nimbus/internal/authz"
)

// Policies attached to the RESOURCE rather than to the caller.
//
// Milestones 13 and 14 evaluated only what a caller holds. A bucket
// policy is the other half of AWS's model, and the half that grants
// public access -- which is the single most instructive thing in cloud
// permissions, and the thing people get wrong.
//
// The surprise here is how little it took. `authz.Evaluate` already ranks
// a flat rule set as "an explicit deny beats everything, an allow from
// anywhere allows", and that IS AWS's same-account rule for an identity
// policy plus a resource policy. So a resource's documents are simply
// appended to the caller's and the core was not touched -- the same
// result Milestone 14 reached for three policy languages.
//
// What the core could NOT give us is who a statement is about: a
// resource policy names its principals, and without that element every
// bucket policy would grant to everyone. That lives in authz_iam.go.

// resourcePolicies returns the documents attached to whatever a request
// acts on. Only S3 has one today; Lambda's resource policy is stored for
// `GetPolicy` to answer and is not yet evaluated.
func (s *Server) resourcePolicies(resource string, p authz.Principal) []policyDocument {
	name := bucketOfARN(resource)
	if name == "" {
		return nil
	}
	b, aerr := s.loadBucket(name)
	if aerr != nil {
		// A policy on a bucket that does not exist cannot grant
		// anything, and the handler will answer NoSuchBucket in a
		// moment. Nothing to add.
		return nil
	}
	return bucketPolicyDocs(b, p)
}

// bucketPolicyDocs is the same thing for a bucket already in hand, which
// is what saveBucket has when it derives the public flag -- and it must
// not read the store, because the version being written is not there yet.
func bucketPolicyDocs(b *bucket, p authz.Principal) []policyDocument {
	var docs []policyDocument
	if b.Policy != "" {
		docs = append(docs, policyDocument{
			Source:   "the bucket policy on " + b.ARN,
			Document: b.Policy,
			Kind:     resourcePolicy,
			Attached: b.ARN,
		})
	}
	if doc, ok := publicAccessDocument(b, p); ok {
		docs = append(docs, doc)
	}
	return docs
}

// publicAccessDocument is the public access block, expressed as a policy
// so that it can REFUSE AND SAY SO.
//
// `RestrictPublicBuckets` is the setting that bites at evaluation time:
// a bucket policy granting `*` still exists and still says `*`, and the
// bucket is not public. That gap -- the policy says one thing, the bucket
// does another -- is exactly the case somebody needs explained, so this
// is a Deny that names itself rather than the policy simply being left
// out, which would produce "nothing grants this" and teach nobody.
//
// (`BlockPublicPolicy` is about the WRITE: it rejects a policy that would
// make a bucket public. `BlockPublicAcls` and `IgnorePublicAcls` are
// about ACLs, which Nimbus stores and does not evaluate.)
func publicAccessDocument(b *bucket, p authz.Principal) (policyDocument, bool) {
	if p.Kind != "anonymous" || b.PublicAccess == nil || !b.PublicAccess.RestrictPublicBuckets {
		return policyDocument{}, false
	}
	return policyDocument{
		Source: "the public access block on " + b.ARN,
		Document: fmt.Sprintf(`{"Version":"2012-10-17","Statement":[
			{"Sid":"RestrictPublicBuckets","Effect":"Deny","Principal":"*",
			 "Action":"*","Resource":["%s","%s/*"]}]}`, b.ARN, b.ARN),
		Kind:     resourcePolicy,
		Attached: b.ARN,
	}, true
}

// bucketOfARN pulls the bucket name out of an S3 ARN, which is the one
// place a resource policy can currently be attached. An S3 ARN has no
// region or account -- `arn:aws:s3:::bucket/key` -- and a key may contain
// slashes, so only the first segment after the bucket matters.
func bucketOfARN(arn string) string {
	const prefix = "arn:aws:s3:::"
	if !strings.HasPrefix(arn, prefix) {
		return ""
	}
	name, _, _ := strings.Cut(strings.TrimPrefix(arn, prefix), "/")
	return name
}

// isPublicBucket reports whether a bucket's own policy grants an
// anonymous caller anything, which is what the visualizer badges.
//
// It asks the EVALUATOR rather than pattern-matching the JSON, so the
// badge cannot disagree with what a request actually gets -- the same
// argument that keeps /api/authz backed by this code instead of a second
// implementation in JavaScript.
func (s *Server) isPublicBucket(b *bucket) (bool, string) {
	if b.Policy == "" {
		return false, ""
	}
	anon := authz.Principal{ID: anonymousPrincipalID, Name: "anonymous", Kind: "anonymous"}
	docs := bucketPolicyDocs(b, anon)

	// The actions worth asking about: a bucket is "public" in the sense
	// people mean when an anonymous caller can read its contents or list
	// them, or write to it.
	for _, probe := range []struct{ action, resource string }{
		{"s3:GetObject", b.ARN + "/*"},
		{"s3:PutObject", b.ARN + "/*"},
		{"s3:ListBucket", b.ARN},
	} {
		d := authz.Evaluate(authz.Request{
			Principal: anon, Action: probe.action, Resource: probe.resource,
		}, s.policies.rules(docs))
		if d.Allowed {
			return true, d.Source
		}
	}
	return false, ""
}
