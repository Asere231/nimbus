package gcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Asere231/nimbus/internal/authz"
	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
)

// --- which permission a request needs -------------------------------------

func TestThePermissionARequestNeedsIsDerivedFromItsPath(t *testing.T) {
	cases := []struct {
		method, path string
		want         string
	}{
		{"POST", "/compute/v1/projects/p/global/networks", "compute.networks.create"},
		{"GET", "/compute/v1/projects/p/global/networks", "compute.networks.list"},
		{"GET", "/compute/v1/projects/p/global/networks/n", "compute.networks.get"},
		{"DELETE", "/compute/v1/projects/p/global/networks/n", "compute.networks.delete"},
		{"PATCH", "/compute/v1/projects/p/global/networks/n", "compute.networks.update"},
		{"POST", "/compute/v1/projects/p/regions/r/subnetworks", "compute.subnetworks.create"},
		{"GET", "/compute/v1/projects/p/zones/z/instances/i", "compute.instances.get"},
		{"POST", "/compute/v1/projects/p/global/firewalls", "compute.firewalls.create"},

		// Cloud Storage abbreviates its collections, which is the one part
		// of this that cannot be derived: /b/{b}/o is buckets and objects.
		{"POST", "/storage/v1/b", "storage.buckets.create"},
		{"GET", "/storage/v1/b/demo", "storage.buckets.get"},
		{"DELETE", "/storage/v1/b/demo", "storage.buckets.delete"},
		{"GET", "/storage/v1/b/demo/o", "storage.objects.list"},
		{"GET", "/storage/v1/b/demo/o/thing.txt", "storage.objects.get"},
		{"DELETE", "/storage/v1/b/demo/o/thing.txt", "storage.objects.delete"},
		{"POST", "/upload/storage/v1/b/demo/o", "storage.objects.create"},

		{"GET", "/v1/projects/nimbus-local", "resourcemanager.projects.get"},
		{"POST", "/v1/projects/nimbus-local:getIamPolicy", "resourcemanager.projects.getIamPolicy"},
		{"POST", "/v1/projects/nimbus-local:setIamPolicy", "resourcemanager.projects.setIamPolicy"},

		{"POST", "/v1/projects/p/serviceAccounts", "iam.serviceAccounts.create"},
		{"GET", "/v1/projects/p/serviceAccounts/a@b.com", "iam.serviceAccounts.get"},

		// GKE's permissions are spelled container.*, after the product's
		// old name -- not gke.*, which is only what the router calls it.
		{"POST", "/v1/projects/p/locations/l/clusters", "container.clusters.create"},
		{"GET", "/v1/projects/p/locations/l/clusters/c", "container.clusters.get"},

		// A colon verb is not a path segment, unlike Compute's trailing
		// lifecycle verbs. Both shapes exist in this one cloud.
		{"PUT", "/v1/projects/p/topics/t", "pubsub.topics.create"},
		{"POST", "/v1/projects/p/topics/t:publish", "pubsub.topics.publish"},
		{"POST", "/v1/projects/p/locations/l/clusters/c/nodePools/np:setSize",
			"container.nodePools.setSize"},

		{"POST", "/v2/projects/p/locations/l/services", "run.services.create"},
		{"GET", "/v2/projects/p/locations/l/services/s", "run.services.get"},
	}

	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		got, _ := permissionFor(r, pathParts(c.path))
		if got != c.want {
			t.Errorf("%s %s = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

// --- the role catalogue ---------------------------------------------------

func TestTheBundledRolesGrantWhatTheyShould(t *testing.T) {
	cases := []struct {
		role, permission string
		want             bool
	}{
		{"roles/owner", "storage.buckets.delete", true},
		{"roles/owner", "resourcemanager.projects.setIamPolicy", true},

		// Editor is the one role here that cannot be a plain allow-list,
		// exactly as Azure's Contributor is: everything except handing
		// out access.
		{"roles/editor", "storage.buckets.delete", true},
		{"roles/editor", "resourcemanager.projects.setIamPolicy", false},
		{"roles/editor", "resourcemanager.projects.getIamPolicy", true},

		{"roles/viewer", "storage.buckets.get", true},
		{"roles/viewer", "storage.buckets.list", true},
		{"roles/viewer", "storage.buckets.delete", false},
		{"roles/viewer", "storage.objects.create", false},

		{"roles/storage.admin", "storage.buckets.delete", true},
		{"roles/storage.admin", "compute.networks.create", false},
		{"roles/storage.objectViewer", "storage.objects.get", true},
		{"roles/storage.objectViewer", "storage.objects.delete", false},
		{"roles/storage.objectViewer", "storage.buckets.delete", false},

		{"roles/compute.networkAdmin", "compute.networks.create", true},
		{"roles/compute.networkAdmin", "compute.instances.create", false},

		{"roles/pubsub.publisher", "pubsub.topics.publish", true},
		{"roles/pubsub.publisher", "pubsub.topics.delete", false},
	}

	for _, c := range cases {
		rule := compileBinding("test", c.role, "")
		got, err := rule.Applies(request(c.permission, ""))
		if err != nil {
			t.Errorf("%s / %s: %v", c.role, c.permission, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s grants %s = %v, want %v", c.role, c.permission, got, c.want)
		}
	}
}

// A role Nimbus does not bundle REFUSES and names itself, which is neither
// "grants nothing" (indistinguishable from a role that does not cover the
// permission) nor "grants everything".
func TestAnUnbundledRoleRefusesAndNamesItself(t *testing.T) {
	rule := compileBinding("test", "roles/invented.admin", "")
	_, err := rule.Applies(request("storage.buckets.get", ""))
	if err == nil {
		t.Fatal("an unbundled role did not refuse")
	}
	if !strings.Contains(err.Error(), "roles/invented.admin") {
		t.Errorf("the refusal does not name the role: %v", err)
	}
}

// --- through the whole stack ----------------------------------------------

const appMember = "serviceAccount:app@nimbus-local.iam.gserviceaccount.com"

// asMember replays a request whose bearer token claims an identity. The
// token IS the member string a policy binding names -- no mapping at all,
// which is what made this the cheapest of the three clouds.
func asMember(t *testing.T, s *Server, member, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if member != "" {
		r.Header.Set("Authorization", "Bearer "+member)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	return rec
}

// setBinding grants a role to a member in the project policy, the way the
// provider's google_project_iam_policy does.
func setBinding(t *testing.T, s *Server, role, member string) {
	t.Helper()
	body := `{"policy":{"bindings":[{"role":"` + role + `","members":["` + member + `"]}]}}`
	rec := asMember(t, s, "", http.MethodPost,
		"/v1/projects/"+s.cfg.ProjectID+":setIamPolicy", body)
	if rec.Code != 200 {
		t.Fatalf("setting the policy: %d %s", rec.Code, rec.Body)
	}
}

func TestWithEnforcementOffNoRequestIsRefused(t *testing.T) {
	s := newTestServer(t)
	rec := asMember(t, s, appMember, http.MethodPost,
		"/compute/v1/projects/"+s.cfg.ProjectID+"/global/networks",
		`{"name":"n","autoCreateSubnetworks":false}`)
	if rec.Code >= 400 {
		t.Fatalf("a create was refused with enforcement off: %d %s", rec.Code, rec.Body)
	}
}

// A token that is not member-shaped -- `nimbus`, which every existing
// example uses -- is the default identity and unrestricted. That is what
// keeps turning the flag on free.
func TestANonMemberTokenIsUnrestricted(t *testing.T) {
	s := newEnforcingServer(t, "")
	rec := asMember(t, s, "nimbus", http.MethodPost,
		"/compute/v1/projects/"+s.cfg.ProjectID+"/global/networks",
		`{"name":"n","autoCreateSubnetworks":false}`)
	if rec.Code >= 400 {
		t.Fatalf("the default identity was refused: %d %s", rec.Code, rec.Body)
	}
}

func TestAMemberIsRefusedWhatItsBindingsDoNotGrant(t *testing.T) {
	s := newEnforcingServer(t, "")
	setBinding(t, s, "roles/viewer", appMember)

	// Viewer may list...
	if rec := asMember(t, s, appMember, http.MethodGet,
		"/compute/v1/projects/"+s.cfg.ProjectID+"/global/networks", ""); rec.Code != 200 {
		t.Errorf("a granted list = %d %s", rec.Code, rec.Body)
	}

	// ...and may not create. 403 with PERMISSION_DENIED, which is what a
	// client matches on.
	rec := asMember(t, s, appMember, http.MethodPost,
		"/compute/v1/projects/"+s.cfg.ProjectID+"/global/networks",
		`{"name":"n","autoCreateSubnetworks":false}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an ungranted create = %d, want 403\n%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{"PERMISSION_DENIED", "compute.networks.create", appMember} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not mention %q: %s", want, body)
		}
	}
}

// Every denial reaches the activity feed, naming the member, the
// permission and the reason.
func TestAGCPDenialIsOnTheBus(t *testing.T) {
	s, bus := newEnforcingServerWithBus(t, "")
	setBinding(t, s, "roles/viewer", appMember)
	asMember(t, s, appMember, http.MethodPost,
		"/compute/v1/projects/"+s.cfg.ProjectID+"/global/networks",
		`{"name":"n","autoCreateSubnetworks":false}`)

	for _, e := range bus.RecentEvents(100) {
		if strings.HasPrefix(e.Note, "denied:") {
			for _, want := range []string{"compute.networks.create", appMember} {
				if !strings.Contains(e.Note, want) {
					t.Errorf("the note does not mention %q: %s", want, e.Note)
				}
			}
			if e.Status != http.StatusForbidden {
				t.Errorf("recorded status %d, want 403", e.Status)
			}
			return
		}
	}
	t.Fatal("no denial reached the bus")
}

// testIamPermissions has to tell the TRUTH once there is a truth to tell.
// It used to answer with whatever was asked about, because nothing was
// enforced; a client that trusts it and then gets a 403 has been lied to.
func TestTestIamPermissionsAnswersHonestly(t *testing.T) {
	s := newEnforcingServer(t, "")
	setBinding(t, s, "roles/storage.objectViewer", appMember)

	rec := asMember(t, s, appMember, http.MethodPost,
		"/v1/projects/"+s.cfg.ProjectID+":testIamPermissions",
		`{"permissions":["storage.objects.get","storage.objects.delete"]}`)
	if rec.Code != 200 {
		t.Fatalf("testIamPermissions = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "storage.objects.get") {
		t.Errorf("a granted permission is missing: %s", body)
	}
	if strings.Contains(body, "storage.objects.delete") {
		t.Errorf("an ungranted permission was reported as held: %s", body)
	}
}

// newEnforcingServer is newTestServer with IAM evaluation on.
func newEnforcingServer(t *testing.T, principal string) *Server {
	t.Helper()
	s, _ := newEnforcingServerWithBus(t, principal)
	return s
}

func newEnforcingServerWithBus(t *testing.T, principal string) (*Server, *core.Bus) {
	t.Helper()
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{PublicURL: base, EnforceIAM: true, Principal: principal}, store, bus)
	f := docker.NewFake()
	f.Unavailable = true
	s.UseDocker(f)
	return s, bus
}

// request builds an authz.Request, so a rule can be exercised without a
// server.
func request(permission, resource string) authz.Request {
	return authz.Request{
		Principal: authz.Principal{ID: appMember, Name: "app", Kind: "serviceAccount"},
		Action:    permission,
		Resource:  resource,
	}
}

// --- explaining -----------------------------------------------------------

// The inspector and `nimbus can` both read this, and it must agree with
// what a request actually gets. One engine, asked two ways.
func TestExplainAgreesWithWhatARequestGets(t *testing.T) {
	s := newEnforcingServer(t, "")
	setBinding(t, s, "roles/storage.objectViewer", appMember)

	cases := []struct {
		permission string
		want       bool
	}{
		{"storage.objects.get", true},
		{"storage.objects.list", true},
		{"storage.objects.create", false},
		{"storage.buckets.delete", false},
	}
	for _, c := range cases {
		check := s.ExplainOne(appMember, c.permission, "nimbus-iam-enforced")
		if check.Allowed != c.want {
			t.Errorf("ExplainOne(%s) = %v, want %v (%s)", c.permission, check.Allowed, c.want, check.Reason)
		}
		if check.Reason == "" {
			t.Errorf("%s has no reason", c.permission)
		}
		if c.want && check.Rule == "" {
			t.Errorf("%s is allowed by nothing named", c.permission)
		}
	}
}

func TestExplainAccessDerivesItsActionsFromTheType(t *testing.T) {
	s := newEnforcingServer(t, "")
	setBinding(t, s, "roles/compute.networkAdmin", appMember)

	report := s.ExplainAccess(appMember, "projects/p/global/networks/n", "compute.googleapis.com/Network")
	if len(report.Checks) == 0 {
		t.Fatal("no checks for a network")
	}
	if !report.Enforcing {
		t.Error("the report does not say enforcement is on")
	}
	seen := map[string]bool{}
	for _, c := range report.Checks {
		seen[c.Action] = c.Allowed
	}
	// compute.googleapis.com/Network -> compute.networks.*, which is the
	// same derivation the enforcement point uses.
	if !seen["compute.networks.create"] {
		t.Errorf("networkAdmin cannot create a network: %+v", report.Checks)
	}
	if seen["compute.networks.setIamPolicy"] {
		t.Error("networkAdmin was reported as able to set an IAM policy")
	}
}

func TestExplainSaysWhenAPrincipalIsUnrestricted(t *testing.T) {
	s := newEnforcingServer(t, "")
	report := s.ExplainAccess("", "projects/p/global/networks/n", "compute.googleapis.com/Network")
	if !report.Unrestricted {
		t.Error("the default identity is not reported as unrestricted")
	}
	for _, c := range report.Checks {
		if !c.Allowed {
			t.Errorf("an unrestricted principal was denied %s", c.Action)
		}
	}
}

// The same agreement criterion, on GCP. See the AWS version for why the
// action and resource are written by hand.
func TestNimbusCanAgreesWithTheRequestPath(t *testing.T) {
	s := newEnforcingServer(t, "")
	setBinding(t, s, "roles/compute.networkAdmin", appMember)
	project := s.cfg.ProjectID

	cases := []struct {
		method, path     string
		action, resource string
	}{
		{http.MethodGet, "/compute/v1/projects/" + project + "/global/networks",
			"compute.networks.list", "the project"},
		{http.MethodPost, "/compute/v1/projects/" + project + "/global/networks",
			"compute.networks.create", "the project"},
		{http.MethodGet, "/compute/v1/projects/" + project + "/zones/us-central1-a/instances",
			"compute.instances.list", "the project"},
		{http.MethodPost, "/v1/projects/" + project + "/serviceAccounts",
			"iam.serviceAccounts.create", "the project"},
	}
	for _, c := range cases {
		rec := asMember(t, s, appMember, c.method, c.path, `{"name":"agree"}`)
		requestAllowed := rec.Code != http.StatusForbidden

		check := s.ExplainOne(appMember, c.action, c.resource)
		if check.Allowed != requestAllowed {
			t.Errorf("%s %s: the request says allowed=%v but `nimbus can %s` says %v (%s)",
				c.method, c.path, requestAllowed, c.action, check.Allowed, check.Reason)
		}
	}
}
