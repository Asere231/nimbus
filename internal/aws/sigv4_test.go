package aws

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The routing key is the credential scope, because the URL says nothing:
// every Query service posts to "/". These headers are copied from what
// aws-cli 1.40.33 actually sent, captured against a logging server.
func TestServiceComesFromTheCredentialScope(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"AWS4-HMAC-SHA256 Credential=nimbus/20261106/us-east-1/sts/aws4_request, " +
			"SignedHeaders=content-type;host;x-amz-date, Signature=140c73865eeb6a69", "sts"},
		{"AWS4-HMAC-SHA256 Credential=nimbus/20261106/us-east-1/ec2/aws4_request, " +
			"SignedHeaders=content-type;host;x-amz-date, Signature=d76fcd19d66b3e6b", "ec2"},
		{"AWS4-HMAC-SHA256 Credential=nimbus/20261106/us-east-1/iam/aws4_request, " +
			"SignedHeaders=content-type;host;x-amz-date, Signature=19350352e4f5706d", "iam"},
		{"AWS4-HMAC-SHA256 Credential=nimbus/20261106/eu-west-2/s3/aws4_request, " +
			"SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=2c90a61f", "s3"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("Authorization", c.header)
		if got := serviceOf(req); got != c.want {
			t.Errorf("serviceOf = %q, want %q", got, c.want)
		}
	}
}

func TestCredentialScopeIsParsedWhole(t *testing.T) {
	c := parseAuthorization("AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/20261106/eu-west-2/ec2/aws4_request, " +
		"SignedHeaders=host, Signature=abc")
	if c.AccessKey != "AKIAEXAMPLE" || c.Date != "20261106" || c.Region != "eu-west-2" || c.Service != "ec2" {
		t.Errorf("credential = %+v", c)
	}
}

// Nothing here may panic or guess wildly on a header it does not
// understand: a malformed request still has to get a sensible answer.
func TestMalformedAuthorizationIsEmptyNotFatal(t *testing.T) {
	for _, header := range []string{
		"", "Basic dXNlcjpwYXNz", "AWS4-HMAC-SHA256 ", "AWS4-HMAC-SHA256 Credential=short/scope",
		"AWS4-HMAC-SHA256 SignedHeaders=host, Signature=abc",
	} {
		if c := parseAuthorization(header); c.Service != "" {
			t.Errorf("parseAuthorization(%q) = %+v, want empty", header, c)
		}
	}
}

// A presigned URL carries the scope in the query string instead.
func TestPresignedURLsRouteByQueryParameter(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet,
		"/bucket/key?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=nimbus%2F20261106%2Fus-east-1%2Fs3%2Faws4_request", nil)
	if got := serviceOf(req); got != "s3" {
		t.Errorf("serviceOf = %q, want s3", got)
	}
}

// An unsigned request is taken to be S3: it is the only one of these
// services ever called without credentials, and the only one whose request
// has a path worth reading.
func TestUnsignedRequestsAreTreatedAsS3(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/some-bucket/key.txt", nil)
	if got := serviceOf(req); got != "s3" {
		t.Errorf("serviceOf = %q, want s3", got)
	}
}

func TestRegionComesFromTheScopeWithAFallback(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=nimbus/20261106/ap-south-1/ec2/aws4_request, Signature=x")
	if got := regionOf(req, "us-east-1"); got != "ap-south-1" {
		t.Errorf("regionOf = %q", got)
	}
	if got := regionOf(httptest.NewRequest(http.MethodPost, "/", nil), "us-east-1"); got != "us-east-1" {
		t.Errorf("regionOf with no header = %q, want the fallback", got)
	}
}

// The Query protocol carries a list as indexed, one-based, contiguous
// parameters. EC2 writes Filter.1.Name; IAM writes Tags.member.1.Key.
func TestIndexedListParameters(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/?Filter.1.Name=vpc-id&Filter.2.Name=state&Tags.member.1.Key=env", nil)
	p, err := readParams(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.list("Filter", "Name"); len(got) != 2 || got[0] != "vpc-id" || got[1] != "state" {
		t.Errorf("EC2-style list = %v", got)
	}
	if got := p.list("Tags.member", "Key"); len(got) != 1 || got[0] != "env" {
		t.Errorf("IAM-style list = %v", got)
	}
	if got := p.list("Nothing", "Here"); len(got) != 0 {
		t.Errorf("a missing list = %v, want empty", got)
	}
}

func TestParamsReadsBothBodyAndQueryString(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/?Extra=yes",
		stringReader("Action=GetCallerIdentity&Version=2011-06-15"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	p, err := readParams(req)
	if err != nil {
		t.Fatal(err)
	}
	if p.str("Action") != "GetCallerIdentity" || p.str("Extra") != "yes" {
		t.Errorf("params = %v", p.Values)
	}
	if p.intOr("Missing", 7) != 7 || p.boolOr("Missing", true) != true {
		t.Error("defaults are not applied")
	}
}

func stringReader(s string) *strings.Reader { return strings.NewReader(s) }
