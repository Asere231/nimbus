package aws

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Presigned URLs: the one AWS credential whose authority is in the URL.
//
// A presigned URL is how S3 delegates a single operation without handing
// over a key -- the local twin of an Azure SAS, and it was being waved
// through in exactly the same way the SAS was before Milestone 22.
// Nothing read the expiry, so a URL minted with `--expires-in 1` still
// served its object minutes later (measured: HTTP 200 with the content),
// and `accessKeyOf` did not read SigV2's `AWSAccessKeyId` at all, so a
// presigned request resolved to ANONYMOUS -- discarding the authority of
// whoever signed it, which is the entire point of presigning.
//
// There are two forms on the wire and the first one is the surprise:
//
//   - **SigV2**, which is what `aws s3 presign` emits under aws-cli 1.x:
//     `?AWSAccessKeyId=…&Signature=…&Expires=1795569729`, where `Expires`
//     is an ABSOLUTE unix time. Measured against aws-cli 1.40.33.
//   - **SigV4**: `?X-Amz-Credential=…&X-Amz-Signature=…&X-Amz-Date=…
//     &X-Amz-Expires=3600`, where `X-Amz-Expires` is a DURATION in
//     seconds from `X-Amz-Date`. Two fields for one deadline, and they
//     are not interchangeable with SigV2's one.
//
// # What is enforced, and what is not
//
// The **expiry** is enforced, unconditionally -- not behind
// `--enforce-iam`. A presigned URL needs no principal, so there is no
// "unmapped caller is root" fallback for it to land in, and an expiry
// nobody checks makes a presigned URL useless for the thing it is for:
// seeing that a deadline works. This is M22's reasoning about a SAS, one
// cloud over.
//
// The **signature** is NOT verified, and the reason is that Nimbus does
// not hold the key. Base credentials here have no secret at all (see
// sigv4.go: "the signature is the routing table"), so there is nothing to
// recompute against. The one exception is a key minted by
// `sts:AssumeRole`, whose secret IS derived (`stableID(roleARN+"/secret",
// 40)` in sts.go) -- so an assumed-role presigned URL could be verified.
// It is deliberately not, because verifying one form and not the other
// would make a failure mean two different things, and the thing a local
// emulator can usefully refuse is an expiry, not a forgery.
//
// What a presigned URL DOES decide is identity: the key in it names the
// signer, so the permissions checked are theirs. That is the half that
// was missing, and `accessKeyOf` reads all three spellings now.

// presignMaxExpires is SigV4's own ceiling on X-Amz-Expires: one week.
// Real S3 refuses anything larger, naming the number.
const presignMaxExpires = 604800

// presignForm names which generation of presigned URL a request carries,
// or "" for a request that carries none.
type presignForm string

const (
	presignNone presignForm = ""
	presignV2   presignForm = "SigV2"
	presignV4   presignForm = "SigV4"
)

// presignedForm reads the query for a presigned URL's credentials.
//
// A SIGNATURE is what makes a URL presigned -- not the access key, which
// SigV4 also puts in the query of requests signed by header when a client
// is being thorough. Checking for the signature keeps an ordinary signed
// request out of this path entirely.
func presignedForm(q url.Values) presignForm {
	switch {
	case q.Get("X-Amz-Signature") != "":
		return presignV4
	case q.Get("Signature") != "" && q.Get("AWSAccessKeyId") != "":
		return presignV2
	default:
		return presignNone
	}
}

// checkPresign refuses a presigned request whose deadline has passed, or
// whose deadline cannot be read.
//
// An unreadable deadline is REFUSED rather than ignored -- the rule this
// repo keeps relearning (an unparsed DynamoDB FilterExpression, an
// unplaceable KQL operator). Treating a malformed `Expires` as "no expiry"
// would turn a typo into a URL that never dies.
//
// The refusals are S3's own, which is what makes them legible to a client:
// an expired URL is `AccessDenied` / "Request has expired", because that
// is what real S3 answers and what the SDKs are written to report.
func checkPresign(r *http.Request, now time.Time) *apiError {
	q := r.URL.Query()
	switch presignedForm(q) {
	case presignV2:
		return checkPresignV2(q, now)
	case presignV4:
		return checkPresignV4(r, q, now)
	}
	return nil
}

// checkPresignV2 reads SigV2's single absolute `Expires`.
func checkPresignV2(q url.Values, now time.Time) *apiError {
	raw := q.Get("Expires")
	if raw == "" {
		return errorf(403, "AccessDenied",
			"a presigned URL must carry Expires; this one signs with %s and has none", presignV2)
	}
	secs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		// S3's own wording for this one: the field is seconds since the
		// epoch, and a client sending a date string gets told so.
		return errorf(403, "AccessDenied",
			"Invalid date (should be seconds since epoch): %q", raw)
	}
	expires := time.Unix(secs, 0).UTC()
	if now.After(expires) {
		return expiredPresign(expires, now)
	}
	return nil
}

// checkPresignV4 reads SigV4's `X-Amz-Date` plus `X-Amz-Expires`, a
// starting point and a duration rather than a deadline.
func checkPresignV4(r *http.Request, q url.Values, now time.Time) *apiError {
	raw := q.Get("X-Amz-Expires")
	if raw == "" {
		return errorf(403, "AuthorizationQueryParametersError",
			"X-Amz-Expires must be specified in a presigned URL")
	}
	secs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return errorf(400, "AuthorizationQueryParametersError",
			"X-Amz-Expires should be a number, not %q", raw)
	}
	if secs < 1 || secs > presignMaxExpires {
		return errorf(400, "AuthorizationQueryParametersError",
			"X-Amz-Expires must be between 1 and %d seconds; got %d",
			presignMaxExpires, secs)
	}

	signed, ok := presignSignedAt(r, q)
	if !ok {
		return errorf(403, "AuthorizationQueryParametersError",
			"X-Amz-Date must be specified in a presigned URL, as an ISO 8601 basic timestamp")
	}
	expires := signed.Add(time.Duration(secs) * time.Second)
	if now.After(expires) {
		return expiredPresign(expires, now)
	}
	return nil
}

// presignSignedAt is when a SigV4 presigned URL was signed. The query
// parameter is where a presigned URL puts it; the header is read as a
// fallback because a client that signs by header and then also fills the
// query (some SDKs do) still says the time only once.
func presignSignedAt(r *http.Request, q url.Values) (time.Time, bool) {
	for _, raw := range []string{q.Get("X-Amz-Date"), r.Header.Get("X-Amz-Date")} {
		if raw == "" {
			continue
		}
		if t, err := time.Parse("20060102T150405Z", raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// expiredPresign is the one refusal this file exists for. It quotes both
// times, for the reason M22's SAS refusals do: a bare 403 sends people to
// look at permissions, and the thing that went wrong is a clock.
func expiredPresign(expires, now time.Time) *apiError {
	return errorf(403, "AccessDenied",
		"Request has expired: the presigned URL was valid until %s and it is now %s",
		expires.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
}

// presignAccessKey is the access key id a presigned URL names, in either
// form. SigV4 puts a whole credential scope in X-Amz-Credential and the
// key is its first field; SigV2 names the key on its own.
func presignAccessKey(q url.Values) string {
	if c := q.Get("X-Amz-Credential"); c != "" {
		if i := strings.Index(c, "/"); i > 0 {
			return c[:i]
		}
		return c
	}
	return q.Get("AWSAccessKeyId")
}
