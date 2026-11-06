package aws

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Identifiers.
//
// AWS has two kinds and Nimbus uses both as the store's resource ID: an ARN
// where the service has one (IAM, S3) and a prefixed id where it does not
// (vpc-0a1b2c3d4e5f67890). Either way the ID is decided ONCE, when the
// resource is created, and stored -- never recomputed on a read.
//
// That distinction matters and is easy to get wrong. The rule CLAUDE.md
// records as "anything the client re-reads must be derived, not generated"
// is about values that must AGREE between two reads. An id stored at create
// time agrees with itself for free. What it must not be is derived from
// something the client can change: two VPCs with the same CIDR are two
// VPCs, and giving them one id because their inputs hash the same would
// merge them.

// stableID is a lower-case hex digest of s, n characters long. It is for
// the values that genuinely must be reproducible -- an account's user id,
// an assumed role's key -- not for resource ids.
func stableID(s string, n int) string {
	sum := sha256.Sum256([]byte("nimbus/aws/" + s))
	out := hex.EncodeToString(sum[:])
	if n > len(out) {
		n = len(out)
	}
	return out[:n]
}

// newID is a fresh AWS-style id: a prefix, a dash and 17 hex characters,
// which is the shape AWS moved to in 2016 and what clients validate
// against. Called once per resource, at create.
func newID(prefix string) string {
	return prefix + "-0" + hexID(16)
}

// arn builds an ARN. Region and account are empty for the services that
// leave them out: an S3 bucket is arn:aws:s3:::name, because a bucket name
// is globally unique and belongs to no region.
func arn(service, region, account, resource string) string {
	return strings.Join([]string{"arn", "aws", service, region, account, resource}, ":")
}
