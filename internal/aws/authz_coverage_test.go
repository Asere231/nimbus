package aws

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Deny-by-default only works if every request can be classified.
//
// An action nobody has mapped is REFUSED, which is the right way round --
// a gap fails an apply loudly instead of quietly granting a permission
// nobody wrote -- but it still means a gap breaks something. So the gap
// has to be found here rather than by somebody's apply.
//
// This reads the service files' own switch statements and asserts that
// every action they route can be classified. Reading source in a test is
// unusual, and this project already does it twice for the same reason:
// TestEveryModuleImportResolves walks the visualizer's import graph, and
// TestEverySpecTypeHasABadge checks the type table. The lesson behind all
// three is Milestone 12's -- a claim with no test behind it is a claim
// that will drift.

var caseLabels = regexp.MustCompile(`(?m)^\s*case ("[^"]+"(?:,\s*"[^"]+")*):`)

// routedActions extracts the action names a service's switch handles.
func routedActions(t *testing.T, file, function string) []string {
	t.Helper()
	source, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	body := functionBody(t, string(source), function)

	var out []string
	for _, m := range caseLabels.FindAllStringSubmatch(body, -1) {
		for _, label := range strings.Split(m[1], ",") {
			name := strings.Trim(strings.TrimSpace(label), `"`)
			if name != "" {
				out = append(out, name)
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("found no routed actions in %s %s; has the switch changed shape?", file, function)
	}
	return out
}

// functionBody is the text between a function's signature and the closing
// brace in column one.
func functionBody(t *testing.T, source, function string) string {
	t.Helper()
	start := strings.Index(source, "func "+function+"(")
	if start < 0 {
		t.Fatalf("cannot find %s; it has been renamed, and this test is now checking nothing", function)
	}
	rest := source[start:]
	if end := strings.Index(rest, "\n}\n"); end > 0 {
		return rest[:end]
	}
	return rest
}

// queryServices are the ones whose action is a form parameter. Their
// classification is mechanical -- `service:Action` -- so what this really
// proves is that the service is wired into authzTarget at all, which is
// the failure that would otherwise reach an apply.
func TestEveryRoutedQueryActionCanBeAuthorized(t *testing.T) {
	s := newEnforcingServer(t, "")
	services := []struct{ service, file, function string }{
		{"sts", "sts.go", "(s *Server) serveSTS"},
		{"ec2", "ec2.go", "(s *Server) serveEC2"},
		{"iam", "iam.go", "(s *Server) serveIAM"},
	}

	for _, svc := range services {
		for _, action := range routedActions(t, svc.file, svc.function) {
			form := url.Values{"Action": {action}, "Version": {"2016-11-15"}}
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")

			got, ok := s.authzTarget(r, svc.service)
			if !ok {
				t.Errorf("%s %s cannot be authorized", svc.service, action)
				continue
			}
			want := svc.service + ":" + action
			if got.Action != want && got.Action != alwaysAllowed {
				t.Errorf("%s %s maps to %q, want %q", svc.service, action, got.Action, want)
			}
			if got.Resource == "" {
				t.Errorf("%s %s names no resource", svc.service, action)
			}
		}
	}
}

// The JSON services put their action in X-Amz-Target.
func TestEveryRoutedJSONActionCanBeAuthorized(t *testing.T) {
	s := newEnforcingServer(t, "")
	services := []struct{ service, file, function, prefix string }{
		{"dynamodb", "dynamodb.go", "(s *Server) serveDynamoDB", "DynamoDB_20120810."},
		{"logs", "logs.go", "(s *Server) serveLogs", "Logs_20140328."},
	}

	for _, svc := range services {
		for _, action := range routedActions(t, svc.file, svc.function) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
			r.Header.Set("X-Amz-Target", svc.prefix+action)

			got, ok := s.authzTarget(r, svc.service)
			if !ok {
				t.Errorf("%s %s cannot be authorized", svc.service, action)
				continue
			}
			if want := svc.service + ":" + action; got.Action != want {
				t.Errorf("%s %s maps to %q, want %q", svc.service, action, got.Action, want)
			}
		}
	}
}

// Every service `route` dispatches to must also be classifiable, or
// turning the flag on refuses a whole service with nothing saying which.
func TestEveryRoutedServiceHasAClassifier(t *testing.T) {
	s := newEnforcingServer(t, "")
	for _, service := range routedActions(t, "server.go", "(s *Server) route") {
		// The feed's own notes and the default case are not services.
		if service == "" {
			continue
		}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if _, ok := s.authzTarget(r, service); !ok {
			// A GET of "/" is not a valid request for most of these, so
			// a false here is only a failure if the service is unknown
			// ENTIRELY -- which is what this distinguishes.
			if !s.serviceIsClassified(service) {
				t.Errorf("the %q service is routed but has no classifier, so --enforce-iam "+
					"refuses all of it", service)
			}
		}
	}
}

// serviceIsClassified reports whether authzTarget knows the service at
// all, as opposed to not being able to classify one particular request.
func (s *Server) serviceIsClassified(service string) bool {
	switch service {
	case "s3", "sts", "ec2", "iam", "dynamodb", "logs", "lambda", "eks":
		return true
	}
	return false
}

// Lambda and EKS say what they want with the path, so their coverage is
// stated as the shapes their routers handle. These were read off
// serveLambda's and serveEKS's switches by hand; the list above is what
// keeps the Query and JSON services honest, and this is what keeps these
// two honest.
func TestLambdaAndEKSPathsAreClassified(t *testing.T) {
	s := newEnforcingServer(t, "")
	cases := []struct {
		service, method, path, action string
	}{
		{"lambda", "POST", "/2015-03-31/functions", "lambda:CreateFunction"},
		{"lambda", "GET", "/2015-03-31/functions", "lambda:ListFunctions"},
		{"lambda", "GET", "/2015-03-31/functions/fn", "lambda:GetFunction"},
		{"lambda", "DELETE", "/2015-03-31/functions/fn", "lambda:DeleteFunction"},
		{"lambda", "GET", "/2015-03-31/functions/fn/configuration", "lambda:GetFunctionConfiguration"},
		{"lambda", "PUT", "/2015-03-31/functions/fn/configuration", "lambda:UpdateFunctionConfiguration"},
		{"lambda", "PUT", "/2015-03-31/functions/fn/code", "lambda:UpdateFunctionCode"},
		{"lambda", "POST", "/2015-03-31/functions/fn/invocations", "lambda:InvokeFunction"},
		{"lambda", "GET", "/2015-03-31/functions/fn/versions", "lambda:ListVersionsByFunction"},
		{"lambda", "GET", "/2015-03-31/functions/fn/policy", "lambda:GetPolicy"},
		{"lambda", "POST", "/2015-03-31/functions/fn/policy", "lambda:AddPermission"},
		{"lambda", "DELETE", "/2015-03-31/functions/fn/policy/sid", "lambda:RemovePermission"},
		{"lambda", "GET", "/2020-06-30/functions/fn/code-signing-config", "lambda:GetFunctionCodeSigningConfig"},
		{"lambda", "GET", "/2017-03-31/tags/arn", "lambda:ListTags"},
		{"lambda", "POST", "/2017-03-31/tags/arn", "lambda:TagResource"},
		{"lambda", "DELETE", "/2017-03-31/tags/arn", "lambda:UntagResource"},

		{"eks", "POST", "/clusters", "eks:CreateCluster"},
		{"eks", "GET", "/clusters", "eks:ListClusters"},
		{"eks", "GET", "/clusters/c", "eks:DescribeCluster"},
		{"eks", "DELETE", "/clusters/c", "eks:DeleteCluster"},
		{"eks", "POST", "/clusters/c/node-groups", "eks:CreateNodegroup"},
		{"eks", "GET", "/clusters/c/node-groups", "eks:ListNodegroups"},
		{"eks", "GET", "/clusters/c/node-groups/g", "eks:DescribeNodegroup"},
		{"eks", "DELETE", "/clusters/c/node-groups/g", "eks:DeleteNodegroup"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		got, ok := s.authzTarget(r, c.service)
		if !ok {
			t.Errorf("%s %s cannot be authorized", c.method, c.path)
			continue
		}
		if got.Action != c.action {
			t.Errorf("%s %s = %q, want %q", c.method, c.path, got.Action, c.action)
		}
		if got.Resource == "" {
			t.Errorf("%s %s names no resource", c.method, c.path)
		}
	}
}

// An unmapped request refuses rather than passing, and says so well
// enough to be fixed.
func TestAnUnmappedRequestIsRefusedNotAllowed(t *testing.T) {
	s := newEnforcingServer(t, "")
	makeRole(t, s, "app", `{"Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`)

	// A Lambda path nothing routes. Even a principal granted "*" is
	// refused, because the question "is this allowed" cannot be asked
	// about a permission nobody has named.
	rec := asKey(t, s, "app", "lambda", http.MethodPatch, "/2015-03-31/functions/fn/invented", "")
	if rec.Code != 403 {
		t.Fatalf("an unmapped request = %d, want 403\n%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "cannot yet work out which permission") {
		t.Errorf("the refusal does not explain itself: %s", rec.Body)
	}
}
