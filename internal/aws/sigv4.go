package aws

import (
	"net/http"
	"strings"
)

// Working out which AWS service a request is for.
//
// This is the part that is least like Azure. ARM routes by path: the URL
// says /providers/Microsoft.Network/virtualNetworks and the server knows
// everything from that. AWS does not. Measured against aws-cli 1.40.33,
// every Query service posts to exactly `/`:
//
//	POST / HTTP/1.1
//	Content-Type: application/x-www-form-urlencoded; charset=utf-8
//	Authorization: AWS4-HMAC-SHA256 Credential=nimbus/20261106/us-east-1/sts/aws4_request,
//	               SignedHeaders=content-type;host;x-amz-date, Signature=140c73...
//
//	Action=GetCallerIdentity&Version=2011-06-15
//
// STS, EC2 and IAM differ only in the fourth field of the credential scope
// and in the Action. So the SIGNATURE is the routing table: the thing a
// signed request cannot lie about without breaking its own signature.
//
// Nimbus does not verify the signature. It is a local emulator with no
// secrets to protect, and a rejected signature would only ever mean the
// clock is wrong or the region differs -- never that someone is attacking
// a cloud that exists on one laptop. The same decision as the storage data
// plane's Shared Key in Milestone 2.

// credential is what the Authorization header tells us.
type credential struct {
	AccessKey string
	Date      string
	Region    string
	Service   string
}

// parseAuthorization pulls the credential scope out of a SigV4 header.
// Anything it cannot parse comes back empty rather than as an error: an
// unsigned request is still worth serving, and the caller decides.
func parseAuthorization(header string) credential {
	const prefix = "AWS4-HMAC-SHA256 "
	if !strings.HasPrefix(header, prefix) {
		return credential{}
	}
	for _, part := range strings.Split(strings.TrimPrefix(header, prefix), ",") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "Credential=") {
			continue
		}
		// AK/20261106/us-east-1/sts/aws4_request
		fields := strings.Split(strings.TrimPrefix(part, "Credential="), "/")
		if len(fields) < 5 {
			return credential{}
		}
		return credential{AccessKey: fields[0], Date: fields[1], Region: fields[2], Service: fields[3]}
	}
	return credential{}
}

// serviceOf decides which service a request belongs to.
//
// The Authorization header first, then the X-Amz-Credential query parameter
// that presigned URLs carry instead of a header. An unsigned request is
// taken to be S3: it is the only one of these services with endpoints that
// are ever called without credentials, and it is also the only one whose
// requests have a path to go on.
func serviceOf(r *http.Request) string {
	if c := parseAuthorization(r.Header.Get("Authorization")); c.Service != "" {
		return c.Service
	}
	if q := r.URL.Query().Get("X-Amz-Credential"); q != "" {
		fields := strings.Split(q, "/")
		if len(fields) >= 4 {
			return fields[3]
		}
	}
	return "s3"
}

// regionOf is the region the client signed for, which responses echo back.
func regionOf(r *http.Request, fallback string) string {
	if c := parseAuthorization(r.Header.Get("Authorization")); c.Region != "" {
		return c.Region
	}
	return fallback
}
