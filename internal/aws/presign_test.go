package aws

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// presignGet replays a presigned GET: no Authorization header at all,
// which is what makes it presigned -- the credentials are in the URL.
func presignGet(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	return rec
}

// v2URL is the shape `aws s3 presign` emits under aws-cli 1.x. Measured
// against aws-cli 1.40.33:
//
//	/presign-demo/ps.txt?AWSAccessKeyId=nimbus&Signature=Nrte…%3D&Expires=1795569729
//
// Note `Expires` is an ABSOLUTE unix time, not a duration.
func v2URL(path, key string, expires time.Time) string {
	q := url.Values{
		"AWSAccessKeyId": {key},
		"Signature":      {"NrteNLDBPIt1NaeGScTcA1vgMlY="},
		"Expires":        {fmt.Sprint(expires.Unix())},
	}
	return path + "?" + q.Encode()
}

// v4URL is SigV4's shape, where the deadline is a start time plus a
// duration rather than a single absolute field.
func v4URL(path, key string, signed time.Time, expires int) string {
	q := url.Values{
		"X-Amz-Algorithm":     {"AWS4-HMAC-SHA256"},
		"X-Amz-Credential":    {key + "/20261124/us-east-1/s3/aws4_request"},
		"X-Amz-Date":          {signed.UTC().Format("20060102T150405Z")},
		"X-Amz-Expires":       {fmt.Sprint(expires)},
		"X-Amz-SignedHeaders": {"host"},
		"X-Amz-Signature":     {"2c90a61f"},
	}
	return path + "?" + q.Encode()
}

func seedObject(t *testing.T, s *Server) {
	t.Helper()
	makeBucket(t, s, "presign-demo")
	if rec := s3req(t, s, http.MethodPut, "/presign-demo/ps.txt", "hello from a presigned url"); rec.Code != 200 {
		t.Fatalf("putting the object: %d %s", rec.Code, rec.Body)
	}
}

// A presigned URL inside its window serves the object, in both forms.
// This is the control: without it, the refusal tests below could pass
// because presigned URLs do not work at all.
func TestAPresignedURLInsideItsWindowServes(t *testing.T) {
	s := newTestServer(t)
	seedObject(t, s)

	for _, tc := range []struct{ name, path string }{
		{"SigV2", v2URL("/presign-demo/ps.txt", "nimbus", time.Now().Add(time.Hour))},
		{"SigV4", v4URL("/presign-demo/ps.txt", "nimbus", time.Now(), 3600)},
	} {
		rec := presignGet(t, s, tc.path)
		if rec.Code != 200 {
			t.Errorf("%s: %d %s, want 200", tc.name, rec.Code, rec.Body)
		}
		if got := rec.Body.String(); got != "hello from a presigned url" {
			t.Errorf("%s: body = %q", tc.name, got)
		}
	}
}

// The bug this file exists for: an expired presigned URL used to answer
// 200 with the content. Measured before the fix with `aws s3 presign
// --expires-in 1`, waiting three seconds, and fetching it.
func TestAnExpiredPresignedURLIsRefused(t *testing.T) {
	s := newTestServer(t)
	seedObject(t, s)

	for _, tc := range []struct{ name, path string }{
		{"SigV2", v2URL("/presign-demo/ps.txt", "nimbus", time.Now().Add(-time.Second))},
		{"SigV4", v4URL("/presign-demo/ps.txt", "nimbus", time.Now().Add(-time.Hour), 60)},
	} {
		rec := presignGet(t, s, tc.path)
		if rec.Code != 403 {
			t.Errorf("%s: %d, want 403", tc.name, rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, "Request has expired") {
			t.Errorf("%s: body does not say the request expired: %s", tc.name, body)
		}
		// Never the content.
		if strings.Contains(rec.Body.String(), "hello from a presigned url") {
			t.Errorf("%s: served the object anyway", tc.name)
		}
		// The reason has to reach somebody reading the response, because
		// a HEAD loses the body entirely.
		if rec.Header().Get(deniedReasonHeader) == "" {
			t.Errorf("%s: no %s header", tc.name, deniedReasonHeader)
		}
	}
}

// An expiry that cannot be READ is refused, not ignored. Ignoring it
// would turn a typo into a URL that never expires -- the rule a DynamoDB
// FilterExpression and a KQL operator each taught.
func TestAnUnreadablePresignExpiryIsRefused(t *testing.T) {
	s := newTestServer(t)
	seedObject(t, s)

	cases := []struct{ name, query string }{
		{"SigV2 expiry is not a number", "AWSAccessKeyId=nimbus&Signature=x&Expires=soon"},
		{"SigV2 has no expiry at all", "AWSAccessKeyId=nimbus&Signature=x"},
		{"SigV4 expiry is not a number", "X-Amz-Credential=nimbus%2F20261124%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Signature=x&X-Amz-Date=20261124T000000Z&X-Amz-Expires=soon"},
		{"SigV4 has no expiry at all", "X-Amz-Credential=nimbus%2F20261124%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Signature=x&X-Amz-Date=20261124T000000Z"},
		{"SigV4 expiry is beyond a week", "X-Amz-Credential=nimbus%2F20261124%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Signature=x&X-Amz-Date=20261124T000000Z&X-Amz-Expires=604801"},
		{"SigV4 has no date", "X-Amz-Credential=nimbus%2F20261124%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Signature=x&X-Amz-Expires=60"},
		{"SigV4 date is unparseable", "X-Amz-Credential=nimbus%2F20261124%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Signature=x&X-Amz-Date=2026-11-24&X-Amz-Expires=60"},
	}
	for _, tc := range cases {
		rec := presignGet(t, s, "/presign-demo/ps.txt?"+tc.query)
		if rec.Code == 200 {
			t.Errorf("%s: served the object (%s)", tc.name, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "hello from a presigned url") {
			t.Errorf("%s: served the content", tc.name)
		}
	}
}

// A request that carries no presigned credentials must be untouched by
// any of this -- including an ordinary signed request whose client also
// fills X-Amz-Credential in the query, which is legal and is not a
// presigned URL because there is no signature there.
func TestAnOrdinaryRequestIsNotTreatedAsPresigned(t *testing.T) {
	s := newTestServer(t)
	seedObject(t, s)

	if rec := s3req(t, s, http.MethodGet, "/presign-demo/ps.txt", ""); rec.Code != 200 {
		t.Errorf("a header-signed GET = %d %s", rec.Code, rec.Body)
	}
	long := "/presign-demo/ps.txt?X-Amz-Credential=" +
		url.QueryEscape("nimbus/20261124/us-east-1/s3/aws4_request")
	if rec := s3req(t, s, http.MethodGet, long, ""); rec.Code != 200 {
		t.Errorf("a credential in the query with no signature = %d %s", rec.Code, rec.Body)
	}
	if got := presignedForm(url.Values{"X-Amz-Credential": {"nimbus/x"}}); got != presignNone {
		t.Errorf("presignedForm with no signature = %q, want none", got)
	}
}

// A presigned URL names its signer, and the permissions checked have to
// be that principal's. SigV2's spelling was the one being dropped, which
// made every `aws s3 presign` URL resolve to anonymous.
func TestAPresignedURLNamesItsSigner(t *testing.T) {
	cases := []struct{ name, query, want string }{
		{"SigV2", "AWSAccessKeyId=AKIAEXAMPLE&Signature=x&Expires=1", "AKIAEXAMPLE"},
		{"SigV4", "X-Amz-Credential=" + url.QueryEscape("ASIAEXAMPLE/20261124/us-east-1/s3/aws4_request") +
			"&X-Amz-Signature=x", "ASIAEXAMPLE"},
		{"no credentials", "", ""},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/b/k?"+tc.query, nil)
		if got := accessKeyOf(r); got != tc.want {
			t.Errorf("%s: accessKeyOf = %q, want %q", tc.name, got, tc.want)
		}
	}

	// The header still wins: a client that signs by header is not
	// presigning, whatever else is in the URL.
	r := httptest.NewRequest(http.MethodGet, "/b/k?AWSAccessKeyId=FROMQUERY&Signature=x&Expires=1", nil)
	r.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=FROMHEADER/20261124/us-east-1/s3/aws4_request, Signature=x")
	if got := accessKeyOf(r); got != "FROMHEADER" {
		t.Errorf("accessKeyOf preferred the query: %q", got)
	}
}

// The boundary itself, so "expired" cannot quietly mean "expiring".
func TestThePresignDeadlineIsTheBoundary(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()

	at := httptest.NewRequest(http.MethodGet, v2URL("/b/k", "nimbus", now), nil)
	if e := checkPresign(at, now); e != nil {
		t.Errorf("a URL expiring exactly now was refused: %s", e.Message)
	}
	after := httptest.NewRequest(http.MethodGet, v2URL("/b/k", "nimbus", now.Add(-time.Second)), nil)
	if e := checkPresign(after, now); e == nil {
		t.Error("a URL that expired a second ago was allowed")
	}
}
