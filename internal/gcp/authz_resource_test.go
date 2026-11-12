package gcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A binding on the RESOURCE rather than on the project.
//
// GCP's version of this milestone is the smallest of the three, because
// bindingRule already carried a `resource` field and compileBinding
// already took one -- both unused since Milestone 14, which is the design
// anticipating exactly this. What was missing is gathering them, and the
// containment rule that lets a bucket's binding reach its objects.

const otherMember = "serviceAccount:other@nimbus-local.iam.gserviceaccount.com"

// setResourcePolicy binds a role on one resource through its own
// setIamPolicy, the way google_pubsub_topic_iam_policy and
// google_cloud_run_service_iam_policy do.
func setResourcePolicy(t *testing.T, s *Server, path, role, member string) {
	t.Helper()
	body := `{"policy":{"bindings":[{"role":"` + role + `","members":["` + member + `"]}]}}`
	rec := asMember(t, s, "", http.MethodPost, path, body)
	if rec.Code != 200 {
		t.Fatalf("setting the policy on %s: %d %s", path, rec.Code, rec.Body)
	}
}

// setBucketPolicy uses Cloud Storage's own shape: a PUT to the bucket's
// `iam` SUB-RESOURCE, carrying the Policy object directly rather than
// wrapped in {"policy": ...}. A fourth convention for IAM in one cloud.
func setBucketPolicy(t *testing.T, s *Server, bucket, role, member string) {
	t.Helper()
	body := `{"bindings":[{"role":"` + role + `","members":["` + member + `"]}]}`
	rec := asMember(t, s, "", http.MethodPut, "/storage/v1/b/"+bucket+"/iam", body)
	if rec.Code != 200 {
		t.Fatalf("setting the bucket policy: %d %s", rec.Code, rec.Body)
	}
}

func makeTopic(t *testing.T, s *Server, name string) {
	t.Helper()
	rec := asMember(t, s, "", http.MethodPut,
		"/v1/projects/"+s.cfg.ProjectID+"/topics/"+name, `{}`)
	if rec.Code != 200 {
		t.Fatalf("creating the topic: %d %s", rec.Code, rec.Body)
	}
}

// Pub/Sub's setIamPolicy used to ECHO the policy back and store nothing,
// while its own comment said "Recorded, never enforced" -- the first word
// was false. A binding a client successfully set and then reads back as
// absent is worse than no support, because it looks like it worked.
func TestAPubSubPolicyIsActuallyStored(t *testing.T) {
	s := newEnforcingServer(t, "")
	makeTopic(t, s, "orders")
	path := "/v1/projects/" + s.cfg.ProjectID + "/topics/orders:setIamPolicy"
	setResourcePolicy(t, s, path, "roles/pubsub.publisher", appMember)

	rec := asMember(t, s, "", http.MethodPost,
		"/v1/projects/"+s.cfg.ProjectID+"/topics/orders:getIamPolicy", `{}`)
	if rec.Code != 200 {
		t.Fatalf("reading the policy back: %d %s", rec.Code, rec.Body)
	}
	var policy map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &policy); err != nil {
		t.Fatalf("the policy is not JSON: %v", err)
	}
	bindings, _ := policy["bindings"].([]any)
	if len(bindings) != 1 {
		t.Fatalf("read back %d bindings, want 1: %s", len(bindings), rec.Body)
	}
	b, _ := bindings[0].(map[string]any)
	if str(b["role"]) != "roles/pubsub.publisher" {
		t.Errorf("the role came back as %q", str(b["role"]))
	}
}

// The milestone's GCP headline: a member with NO project binding at all
// publishes to one topic, because that topic's own policy says so.
func TestATopicBindingGrantsOnThatTopicAlone(t *testing.T) {
	s := newEnforcingServer(t, "")
	makeTopic(t, s, "orders")
	makeTopic(t, s, "secrets")
	setResourcePolicy(t, s, "/v1/projects/"+s.cfg.ProjectID+"/topics/orders:setIamPolicy",
		"roles/pubsub.publisher", appMember)

	ok := asMember(t, s, appMember, http.MethodPost,
		"/v1/projects/"+s.cfg.ProjectID+"/topics/orders:publish",
		`{"messages":[{"data":"aGk="}]}`)
	if ok.Code != 200 {
		t.Errorf("publishing to the bound topic = %d %s", ok.Code, ok.Body)
	}

	no := asMember(t, s, appMember, http.MethodPost,
		"/v1/projects/"+s.cfg.ProjectID+"/topics/secrets:publish",
		`{"messages":[{"data":"aGk="}]}`)
	if no.Code != 403 {
		t.Errorf("publishing to the other topic = %d %s, want 403", no.Code, no.Body)
	}
}

// A binding names its members, and nobody else. The GCP equivalent of
// AWS's Principal element -- except that bindings have always carried
// theirs, which is why this cloud needed no new parsing.
func TestAResourceBindingGrantsOnlyItsMembers(t *testing.T) {
	s := newEnforcingServer(t, "")
	makeTopic(t, s, "orders")
	setResourcePolicy(t, s, "/v1/projects/"+s.cfg.ProjectID+"/topics/orders:setIamPolicy",
		"roles/pubsub.publisher", appMember)

	rec := asMember(t, s, otherMember, http.MethodPost,
		"/v1/projects/"+s.cfg.ProjectID+"/topics/orders:publish",
		`{"messages":[{"data":"aGk="}]}`)
	if rec.Code != 403 {
		t.Errorf("a member the binding does not name = %d, want 403", rec.Code)
	}
}

// A Cloud Run service's own policy, which HAS been stored since Milestone
// 10 under x-nimbus-iam and never consulted.
func TestACloudRunServiceBindingIsEnforced(t *testing.T) {
	s := newEnforcingServer(t, "")
	base := "/v2/projects/" + s.cfg.ProjectID + "/locations/us-central1/services"
	rec := asMember(t, s, "", http.MethodPost, base+"?serviceId=api",
		`{"template":{"containers":[{"image":"nginx:alpine"}]}}`)
	if rec.Code >= 300 {
		t.Fatalf("creating the service: %d %s", rec.Code, rec.Body)
	}

	if got := asMember(t, s, appMember, http.MethodGet, base+"/api", ""); got.Code != 403 {
		t.Fatalf("an unbound member reading the service = %d, want 403", got.Code)
	}
	setResourcePolicy(t, s, base+"/api:setIamPolicy", "roles/run.viewer", appMember)
	if got := asMember(t, s, appMember, http.MethodGet, base+"/api", ""); got.Code != 200 {
		t.Errorf("after the binding = %d %s, want 200", got.Code, got.Body)
	}
}

// A bucket's binding has to reach the bucket's OBJECTS, which is what
// everybody means by granting access to a bucket. bindingRule compared
// resources with exact equality, so it did not -- the same containment
// problem Azure's scopeCovers solved, and solved the same way.
func TestABucketBindingReachesItsObjects(t *testing.T) {
	s := newEnforcingServer(t, "")
	rec := asMember(t, s, "", http.MethodPost, "/storage/v1/b?project="+s.cfg.ProjectID,
		`{"name":"reports"}`)
	if rec.Code >= 300 {
		t.Fatalf("creating the bucket: %d %s", rec.Code, rec.Body)
	}
	setBucketPolicy(t, s, "reports", "roles/storage.objectViewer", appMember)

	if got := asMember(t, s, appMember, http.MethodGet,
		"/storage/v1/b/reports/o/today.txt", ""); got.Code == 403 {
		t.Errorf("reading an object in the bound bucket was refused: %s", got.Body)
	}
}

// ...and must NOT reach a differently-named bucket that merely shares a
// prefix. A bare strings.HasPrefix would grant on `reports-archive`,
// which is the trap Azure's scope matching already records.
func TestABucketBindingDoesNotReachANeighbourSharingAPrefix(t *testing.T) {
	s := newEnforcingServer(t, "")
	for _, name := range []string{"reports", "reports-archive"} {
		rec := asMember(t, s, "", http.MethodPost, "/storage/v1/b?project="+s.cfg.ProjectID,
			`{"name":"`+name+`"}`)
		if rec.Code >= 300 {
			t.Fatalf("creating %s: %d %s", name, rec.Code, rec.Body)
		}
	}
	setBucketPolicy(t, s, "reports", "roles/storage.objectViewer", appMember)

	got := asMember(t, s, appMember, http.MethodGet,
		"/storage/v1/b/reports-archive/o/today.txt", "")
	if got.Code != 403 {
		t.Errorf("reading the neighbouring bucket = %d, want 403", got.Code)
	}
}

// The explanation must name the resource's own binding, not the
// project's -- a decision explained without its source teaches nothing,
// and `nimbus can` reads the same endpoint.
func TestTheDecisionNamesTheResourcePolicy(t *testing.T) {
	s := newEnforcingServer(t, "")
	makeTopic(t, s, "orders")
	setResourcePolicy(t, s, "/v1/projects/"+s.cfg.ProjectID+"/topics/orders:setIamPolicy",
		"roles/pubsub.publisher", appMember)

	check := s.ExplainOne(appMember, "pubsub.topics.publish", "orders")
	if !check.Allowed {
		t.Fatalf("ExplainOne says no: %s", check.Reason)
	}
	if !strings.Contains(check.Rule, "orders") {
		t.Errorf("the rule is %q, which does not name the resource it is attached to", check.Rule)
	}
}

// testIamPermissions must tell the truth about a resource binding too.
// Cloud Run's echoed back whatever it was asked, which was honest while
// nothing was enforced and is a lie now -- the same fix the project
// policy's got in Milestone 14.
func TestTestIamPermissionsTellsTheTruthAboutAResourceBinding(t *testing.T) {
	s := newEnforcingServer(t, "")
	base := "/v2/projects/" + s.cfg.ProjectID + "/locations/us-central1/services"
	if rec := asMember(t, s, "", http.MethodPost, base+"?serviceId=api",
		`{"template":{"containers":[{"image":"nginx:alpine"}]}}`); rec.Code >= 300 {
		t.Fatalf("creating the service: %d %s", rec.Code, rec.Body)
	}
	setResourcePolicy(t, s, base+"/api:setIamPolicy", "roles/run.viewer", appMember)

	rec := asMember(t, s, appMember, http.MethodPost, base+"/api:testIamPermissions",
		`{"permissions":["run.services.get","run.services.delete"]}`)
	if rec.Code != 200 {
		t.Fatalf("testIamPermissions = %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Permissions []string `json:"permissions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	held := strings.Join(out.Permissions, ",")
	if !strings.Contains(held, "run.services.get") {
		t.Errorf("a permission the binding grants is missing: %v", out.Permissions)
	}
	if strings.Contains(held, "run.services.delete") {
		t.Errorf("a permission nothing grants was reported as held: %v", out.Permissions)
	}
}

// An object is named WITH ITS BUCKET, which is the one place a
// container is part of a resource's identity here. It is what lets a
// bucket's binding reach its objects, and it is also the difference
// between a refusal that says "denied on today.txt" and one that says
// which bucket. Asserted because it is a visible change to every
// object-level refusal message.
func TestAnObjectIsNamedWithItsBucket(t *testing.T) {
	cases := []struct {
		method, path       string
		permission, object string
	}{
		{"GET", "/storage/v1/b/reports/o/today.txt", "storage.objects.get", "reports/today.txt"},
		{"DELETE", "/storage/v1/b/reports/o/today.txt", "storage.objects.delete", "reports/today.txt"},
		// A key may contain slashes and is still ONE object.
		{"GET", "/storage/v1/b/reports/o/img/logo.svg", "storage.objects.get", "reports/img/logo.svg"},
		// A collection-level call is a LIST, and names the bucket --
		// composing the name must not make it look like a named object.
		{"GET", "/storage/v1/b/reports/o", "storage.objects.list", "reports"},
		{"POST", "/upload/storage/v1/b/reports/o", "storage.objects.create", "reports"},
		// The bucket itself is unchanged.
		{"GET", "/storage/v1/b/reports", "storage.buckets.get", "reports"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		permission, resource := permissionFor(r, strings.Split(strings.Trim(c.path, "/"), "/"))
		if permission != c.permission {
			t.Errorf("%s %s = %q, want %q", c.method, c.path, permission, c.permission)
		}
		if resource != c.object {
			t.Errorf("%s %s resource = %q, want %q", c.method, c.path, resource, c.object)
		}
	}
}

// coversResource is the containment rule, and the neighbour case is the
// whole reason it is not strings.HasPrefix -- Azure's scopeCovers paid
// for this lesson first.
func TestCoversResourceIsSegmentAligned(t *testing.T) {
	cases := []struct {
		scope, resource string
		want            bool
	}{
		{"", "anything", true}, // the project policy
		{"reports", "reports", true},
		{"reports", "reports/today.txt", true},
		{"reports", "reports/img/logo.svg", true},
		{"reports", "reports-archive", false},
		{"reports", "reports-archive/today.txt", false},
		{"reports", "other", false},
		{"reports/img", "reports/img/logo.svg", true},
		{"reports/img", "reports/images/logo.svg", false},
		// ARM resolves ids case-insensitively and so does this, because
		// a binding in a config need not match a path's casing.
		{"Reports", "reports/today.txt", true},
	}
	for _, c := range cases {
		if got := coversResource(c.scope, c.resource); got != c.want {
			t.Errorf("coversResource(%q, %q) = %v, want %v", c.scope, c.resource, got, c.want)
		}
	}
}
