package azure

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
)

// --- the action an ARM request needs --------------------------------------

// Azure's action names are DERIVABLE, which is the thing that makes this
// cloud cheap compared with AWS. S3 needed a table because AWS's own names
// are inconsistent (`s3:GetBucketVersioning` but
// `s3:GetLifecycleConfiguration`); ARM's are mechanical:
// `{type}/{read|write|delete}`, and `{type}/{verb}/action` for a POST.
func TestTheActionAnARMRequestNeedsIsDerivedFromItsPath(t *testing.T) {
	cases := []struct {
		method, path  string
		action, scope string
	}{
		{"GET", "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet",
			"Microsoft.Network/virtualNetworks/read",
			"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet"},
		{"PUT", "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet",
			"Microsoft.Network/virtualNetworks/write",
			"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet"},
		{"PATCH", "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet",
			"Microsoft.Network/virtualNetworks/write", ""},
		{"DELETE", "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet",
			"Microsoft.Network/virtualNetworks/delete", ""},
		{"HEAD", "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet",
			"Microsoft.Network/virtualNetworks/read", ""},

		// A nested type keeps its whole path, which is what makes
		// `Microsoft.Network/*` cover a subnet as well as its VNet.
		{"PUT", "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet/subnets/internal",
			"Microsoft.Network/virtualNetworks/subnets/write", ""},

		// A collection read.
		{"GET", "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks",
			"Microsoft.Network/virtualNetworks/read", "/subscriptions/sub/resourceGroups/rg"},

		// A POST action. parseTarget puts the verb on the end of Type, so
		// the action name is that plus "/action" -- which is exactly how
		// Azure spells it.
		{"POST", "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/c/listClusterUserCredential",
			"Microsoft.ContainerService/managedClusters/listClusterUserCredential/action",
			"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/c"},

		// Resource groups and subscriptions are not provider types, and
		// their real action names put `subscriptions/` in the middle.
		{"PUT", "/subscriptions/sub/resourceGroups/rg",
			"Microsoft.Resources/subscriptions/resourceGroups/write",
			"/subscriptions/sub/resourceGroups/rg"},
		{"DELETE", "/subscriptions/sub/resourceGroups/rg",
			"Microsoft.Resources/subscriptions/resourceGroups/delete", ""},
		{"GET", "/subscriptions/sub/resourceGroups",
			"Microsoft.Resources/subscriptions/resourceGroups/read", "/subscriptions/sub"},
		{"GET", "/subscriptions/sub", "Microsoft.Resources/subscriptions/read", "/subscriptions/sub"},
		{"GET", "/subscriptions", "Microsoft.Resources/subscriptions/read", ""},
	}

	for _, c := range cases {
		tgt, ok := parseTarget(c.path)
		if !ok {
			t.Errorf("%s %s: parseTarget said no", c.method, c.path)
			continue
		}
		action, scope := armAction(tgt, c.method)
		if action != c.action {
			t.Errorf("%s %s = %q, want %q", c.method, c.path, action, c.action)
		}
		if c.scope != "" && scope != c.scope {
			t.Errorf("%s %s scope = %q, want %q", c.method, c.path, scope, c.scope)
		}
	}
}

// --- scope inheritance ----------------------------------------------------

// The part of RBAC a learner most needs to see working: an assignment made
// at a resource group governs everything inside it. It is ID-prefix
// matching, aligned to segments -- and case-insensitively, because ARM
// resolves IDs that way and answers with the casing used at creation.
func TestAScopeCoversWhatIsBeneathIt(t *testing.T) {
	const rg = "/subscriptions/sub/resourceGroups/rg"
	cases := []struct {
		scope, resource string
		want            bool
	}{
		{rg, rg, true},
		{rg, rg + "/providers/Microsoft.Network/virtualNetworks/vnet", true},
		{rg, rg + "/providers/Microsoft.Network/virtualNetworks/vnet/subnets/a", true},
		{"/subscriptions/sub", rg + "/providers/Microsoft.Network/virtualNetworks/vnet", true},
		{"/", rg, true},

		// A different resource group, and the trap: one whose name merely
		// STARTS with the scope's name. A plain strings.HasPrefix says yes
		// to this, which would silently widen every assignment.
		{rg, "/subscriptions/sub/resourceGroups/other/providers/Microsoft.Network/virtualNetworks/v", false},
		{rg, "/subscriptions/sub/resourceGroups/rg-two/providers/Microsoft.Network/virtualNetworks/v", false},
		{rg + "/providers/Microsoft.Network/virtualNetworks/vnet", rg, false},

		// ARM is case-insensitive about IDs, so an assignment written with
		// different casing still governs.
		{"/SUBSCRIPTIONS/SUB/RESOURCEGROUPS/RG", rg + "/providers/Microsoft.Network/virtualNetworks/v", true},
	}
	for _, c := range cases {
		if got := scopeCovers(c.scope, c.resource); got != c.want {
			t.Errorf("scopeCovers(%q, %q) = %v, want %v", c.scope, c.resource, got, c.want)
		}
	}
}

// --- evaluation through the whole stack -----------------------------------

// asPrincipal is `do` with a bearer token naming who is asking. Identity
// here is the token's `oid` claim, which Nimbus issues for whatever
// client_id was requested and never verifies -- see principal.go.
func asPrincipal(t *testing.T, srv *httptest.Server, oid, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+path+"?api-version=2023-09-01", rdr)
	req.Header.Set("Content-Type", "application/json")
	if oid != "" {
		req.Header.Set("Authorization", "Bearer "+fakeJWT(map[string]any{
			"oid": oid, "tid": DefaultTenantID,
		}))
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return res.StatusCode, out
}

func errorCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	return str(e["code"])
}

func errorMessage(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	return str(e["message"])
}

func TestWithEnforcementOffNoARMRequestIsRefused(t *testing.T) {
	srv, _ := newTestServer(t)
	code, body := asPrincipal(t, srv, "11111111-1111-1111-1111-111111111111",
		http.MethodPut, "/subscriptions/"+DefaultSubscriptionID+"/resourceGroups/rg",
		map[string]any{"location": "eastus"})
	if code >= 400 {
		t.Fatalf("a create was refused with enforcement off: %d %v", code, body)
	}
}

func TestAnUnidentifiedCallerIsUnrestricted(t *testing.T) {
	srv, _ := newEnforcingTestServer(t, "")
	// No bearer token at all. The caller carries no assignments and is
	// treated as unrestricted -- the same decision AWS's account root is,
	// and what keeps every existing example applying with the flag on.
	code, body := asPrincipal(t, srv, "", http.MethodPut,
		"/subscriptions/"+DefaultSubscriptionID+"/resourceGroups/rg",
		map[string]any{"location": "eastus"})
	if code >= 400 {
		t.Fatalf("an unidentified caller was refused: %d %v", code, body)
	}
}

// newEnforcingTestServer is newTestServer with RBAC evaluation on.
func newEnforcingTestServer(t *testing.T, principal string) (*httptest.Server, *core.Store) {
	t.Helper()
	bus := core.NewBus(100)
	store, _ := core.NewStore(bus, "")
	var az *Server
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { az.ServeHTTP(w, r) }))
	az = New(Config{PublicURL: srv.URL, EnforceIAM: true, Principal: principal}, store, bus)
	az.useDocker(docker.NewFake())
	testServers[srv] = az
	t.Cleanup(func() { delete(testServers, srv); srv.Close() })
	return srv, store
}

const (
	appPrincipal = "11111111-1111-1111-1111-111111111111"
	roleReader   = "acdd72a7-3385-48ef-bd42-f606fba81ae7"
	roleContrib  = "b24988ac-6180-42a0-ab88-20f7382dd24c"
	roleNetwork  = "4d97b98b-1d4f-4787-a291-c67834d212e7"
)

// assign makes a role assignment at a scope, as azurerm does: a PUT of an
// extension resource whose parent IS the scope it grants over.
func assign(t *testing.T, srv *httptest.Server, scope, roleGUID, principal string) {
	t.Helper()
	name := "assignment-" + roleGUID[:8] + "-" + principal[:8]
	path := scope + "/providers/Microsoft.Authorization/roleAssignments/" + name
	code, body := do(t, srv, http.MethodPut, path, map[string]any{
		"properties": map[string]any{
			"roleDefinitionId": "/subscriptions/" + DefaultSubscriptionID +
				"/providers/Microsoft.Authorization/roleDefinitions/" + roleGUID,
			"principalId": principal,
		},
	})
	if code >= 300 {
		t.Fatalf("creating the assignment: %d %v", code, body)
	}
}

func rgPath(name string) string {
	return "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + name
}

// The done-criterion the prompt singles out: an assignment made at a
// RESOURCE GROUP governs a resource created inside it.
func TestAnAssignmentAtAResourceGroupGovernsWhatIsInside(t *testing.T) {
	srv, _ := newEnforcingTestServer(t, "")

	// The default identity is unrestricted, so it sets the scene.
	if code, body := do(t, srv, http.MethodPut, rgPath("rg"),
		map[string]any{"location": "eastus"}); code >= 300 {
		t.Fatalf("creating the resource group: %d %v", code, body)
	}
	assign(t, srv, rgPath("rg"), roleNetwork, appPrincipal)

	vnet := rgPath("rg") + "/providers/Microsoft.Network/virtualNetworks/vnet"
	body := vnetBody()

	// Network Contributor at the resource group covers a VNet inside it,
	// which nothing in the assignment mentions by name.
	code, out := asPrincipal(t, srv, appPrincipal, http.MethodPut, vnet, body)
	if code >= 300 {
		t.Fatalf("a granted write inside the scope was refused: %d %v", code, out)
	}

	// ...and a subnet beneath THAT, two levels down.
	subnet := vnet + "/subnets/internal"
	code, out = asPrincipal(t, srv, appPrincipal, http.MethodPut, subnet,
		map[string]any{"properties": map[string]any{"addressPrefix": "10.0.1.0/24"}})
	if code >= 300 {
		t.Fatalf("a write two levels inside the scope was refused: %d %v", code, out)
	}

	// A resource group of its own is NOT covered: Network Contributor
	// grants Microsoft.Network/*, and a resource group is not that.
	code, out = asPrincipal(t, srv, appPrincipal, http.MethodPut, rgPath("other"),
		map[string]any{"location": "eastus"})
	if code != http.StatusForbidden {
		t.Fatalf("creating a resource group = %d, want 403\n%v", code, out)
	}
	if errorCode(out) != "AuthorizationFailed" {
		t.Errorf("refusal code = %q, want AuthorizationFailed", errorCode(out))
	}
	for _, want := range []string{appPrincipal, "Microsoft.Resources/subscriptions/resourceGroups/write"} {
		if !strings.Contains(errorMessage(out), want) {
			t.Errorf("the refusal does not mention %q: %s", want, errorMessage(out))
		}
	}
}

// An assignment in a DIFFERENT resource group governs nothing here, which
// is the half of inheritance that a prefix check gets wrong.
func TestAnAssignmentInAnotherScopeDoesNotGovern(t *testing.T) {
	srv, _ := newEnforcingTestServer(t, "")
	do(t, srv, http.MethodPut, rgPath("rg"), map[string]any{"location": "eastus"})
	do(t, srv, http.MethodPut, rgPath("rg-two"), map[string]any{"location": "eastus"})
	assign(t, srv, rgPath("rg"), roleContrib, appPrincipal)

	// rg-two starts with rg: a plain prefix check would allow this.
	path := rgPath("rg-two") + "/providers/Microsoft.Network/virtualNetworks/v"
	code, out := asPrincipal(t, srv, appPrincipal, http.MethodPut, path,
		map[string]any{"location": "eastus"})
	if code != http.StatusForbidden {
		t.Fatalf("a write in another resource group = %d, want 403\n%v", code, out)
	}
}

// Reader may read and not write, which is the smallest thing RBAC has to
// get right.
func TestReaderMayReadAndNotWrite(t *testing.T) {
	srv, _ := newEnforcingTestServer(t, "")
	do(t, srv, http.MethodPut, rgPath("rg"), map[string]any{"location": "eastus"})
	assign(t, srv, "/subscriptions/"+DefaultSubscriptionID, roleReader, appPrincipal)

	if code, out := asPrincipal(t, srv, appPrincipal, http.MethodGet, rgPath("rg"), nil); code != 200 {
		t.Errorf("a granted read = %d %v", code, out)
	}
	code, out := asPrincipal(t, srv, appPrincipal, http.MethodPut, rgPath("rg"),
		map[string]any{"location": "eastus"})
	if code != http.StatusForbidden {
		t.Errorf("an ungranted write = %d, want 403\n%v", code, out)
	}
}

// notActions are SUBTRACTED, and Contributor's are the whole difference
// from Owner: it can do everything except hand out access.
func TestContributorCannotAssignRoles(t *testing.T) {
	srv, _ := newEnforcingTestServer(t, "")
	do(t, srv, http.MethodPut, rgPath("rg"), map[string]any{"location": "eastus"})
	assign(t, srv, "/subscriptions/"+DefaultSubscriptionID, roleContrib, appPrincipal)

	// It can write an ordinary resource.
	vnet := rgPath("rg") + "/providers/Microsoft.Network/virtualNetworks/v"
	if code, out := asPrincipal(t, srv, appPrincipal, http.MethodPut, vnet, vnetBody()); code >= 300 {
		t.Fatalf("Contributor could not write a VNet: %d %v", code, out)
	}

	// It cannot write a role assignment, because of the notAction.
	path := rgPath("rg") + "/providers/Microsoft.Authorization/roleAssignments/nope"
	code, out := asPrincipal(t, srv, appPrincipal, http.MethodPut, path, map[string]any{
		"properties": map[string]any{"roleDefinitionId": "x", "principalId": appPrincipal},
	})
	if code != http.StatusForbidden {
		t.Fatalf("Contributor assigned a role: %d %v", code, out)
	}
}

// A role Nimbus does not know REFUSES and names itself -- never grants
// nothing, which looks identical to a role that does not cover the action.
func TestAnUnknownRoleDefinitionRefusesAndNamesItself(t *testing.T) {
	srv, _ := newEnforcingTestServer(t, "")
	do(t, srv, http.MethodPut, rgPath("rg"), map[string]any{"location": "eastus"})

	const madeUp = "99999999-9999-9999-9999-999999999999"
	assign(t, srv, rgPath("rg"), madeUp, appPrincipal)

	path := rgPath("rg") + "/providers/Microsoft.Network/virtualNetworks/v"
	code, out := asPrincipal(t, srv, appPrincipal, http.MethodPut, path,
		map[string]any{"location": "eastus"})
	if code != http.StatusForbidden {
		t.Fatalf("an unknown role allowed a write: %d %v", code, out)
	}
	if !strings.Contains(errorMessage(out), madeUp) {
		t.Errorf("the refusal does not name the role: %s", errorMessage(out))
	}
}

// Every denial reaches the activity feed, naming the principal, the action
// and the rule.
func TestAnARMDenialIsOnTheBus(t *testing.T) {
	bus := core.NewBus(100)
	store, _ := core.NewStore(bus, "")
	var az *Server
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { az.ServeHTTP(w, r) }))
	az = New(Config{PublicURL: srv.URL, EnforceIAM: true}, store, bus)
	az.useDocker(docker.NewFake())
	testServers[srv] = az
	t.Cleanup(func() { delete(testServers, srv); srv.Close() })

	asPrincipal(t, srv, appPrincipal, http.MethodPut, rgPath("rg"),
		map[string]any{"location": "eastus"})

	for _, e := range bus.RecentEvents(100) {
		if strings.HasPrefix(e.Note, "denied:") {
			if !strings.Contains(e.Note, appPrincipal) {
				t.Errorf("the note does not name the principal: %s", e.Note)
			}
			if e.Status != http.StatusForbidden {
				t.Errorf("recorded status %d, want 403", e.Status)
			}
			return
		}
	}
	t.Fatal("no denial reached the bus")
}

// vnetBody is the minimum a VNet write needs; network.go refuses one
// without an address space, which is a validation error rather than an
// authorization one and would mask what these tests are about.
func vnetBody() map[string]any {
	return map[string]any{
		"location": "eastus",
		"properties": map[string]any{
			"addressSpace": map[string]any{"addressPrefixes": []string{"10.0.0.0/16"}},
		},
	}
}

// Turning the flag on must cost nothing for a configuration that says
// nothing about RBAC. Every Azure example sets `client_id` to
// DefaultClientID, so the token's oid is derived from that -- and checking
// only DefaultObjectID refused all of azure-basic the moment the flag was
// on. This pins the fix.
func TestTheDefaultClientIDIsUnrestricted(t *testing.T) {
	srv, _ := newEnforcingTestServer(t, "")
	oid := principalFromClientID(DefaultClientID)
	if oid == DefaultObjectID {
		t.Fatal("this test proves nothing if the two ids coincide")
	}

	code, out := asPrincipal(t, srv, oid, http.MethodPut, rgPath("rg"),
		map[string]any{"location": "eastus"})
	if code >= 300 {
		t.Fatalf("the default client_id was refused: %d %v", code, out)
	}
}

// The same agreement criterion, on Azure. See the AWS version for why the
// action and scope are written by hand.
func TestNimbusCanAgreesWithTheRequestPath(t *testing.T) {
	srv, _ := newEnforcingTestServer(t, "")
	do(t, srv, http.MethodPut, rgPath("rg"), map[string]any{"location": "eastus"})
	assign(t, srv, rgPath("rg"), roleNetwork, appPrincipal)
	s := serverOf(t, srv)

	vnet := rgPath("rg") + "/providers/Microsoft.Network/virtualNetworks/vnet"
	cases := []struct {
		method, path  string
		action, scope string
		body          map[string]any
	}{
		{http.MethodPut, vnet, "Microsoft.Network/virtualNetworks/write", vnet, vnetBody()},
		{http.MethodGet, vnet, "Microsoft.Network/virtualNetworks/read", vnet, nil},
		{http.MethodPut, rgPath("other"),
			"Microsoft.Resources/subscriptions/resourceGroups/write", rgPath("other"),
			map[string]any{"location": "eastus"}},
	}
	for _, c := range cases {
		code, _ := asPrincipal(t, srv, appPrincipal, c.method, c.path, c.body)
		requestAllowed := code != http.StatusForbidden

		check := s.ExplainOne(appPrincipal, c.action, c.scope)
		if check.Allowed != requestAllowed {
			t.Errorf("%s %s: the request says allowed=%v but `nimbus can %s` says %v (%s)",
				c.method, c.path, requestAllowed, c.action, check.Allowed, check.Reason)
		}
	}
}
