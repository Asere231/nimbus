package aws

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Published versions and aliases, which Lambda did not have: a config with
// `publish = true` or an `aws_lambda_alias` could not apply at all.

func publish(t *testing.T, s *Server, name, body string) map[string]any {
	t.Helper()
	rec, doc := lambdaCall(t, s, http.MethodPost,
		"/2015-03-31/functions/"+name+"/versions", body)
	if rec.Code != 201 {
		t.Fatalf("publishing %s: %d %s", name, rec.Code, rec.Body)
	}
	return doc
}

// A published version is an immutable SNAPSHOT of the configuration, and
// $LATEST goes on changing underneath it. A version that tracked the live
// configuration would be a label rather than a snapshot.
func TestAPublishedVersionIsASnapshot(t *testing.T) {
	s, _ := newLambdaTestServer(t)
	createWriter(t, s)

	v1 := publish(t, s, "writer", `{"Description":"first cut"}`)
	if got := str(v1["Version"]); got != "1" {
		t.Errorf("the first version is %q, want \"1\"", got)
	}
	if got := str(v1["FunctionArn"]); !strings.HasSuffix(got, ":writer:1") {
		t.Errorf("the version's ARN is %q, want one ending :writer:1", got)
	}
	if got := str(v1["State"]); got != "Active" {
		t.Errorf("a published version's State is %q, want Active", got)
	}

	// Change the live function. The version must not follow.
	rec, _ := lambdaCall(t, s, http.MethodPut,
		"/2015-03-31/functions/writer/configuration", `{"Timeout": 99, "MemorySize": 1024}`)
	if rec.Code != 200 {
		t.Fatalf("updating the configuration: %d %s", rec.Code, rec.Body)
	}

	_, read := lambdaCall(t, s, http.MethodGet,
		"/2015-03-31/functions/writer/configuration?Qualifier=1", "")
	if got := read["Timeout"]; got != float64(10) {
		t.Errorf("version 1's Timeout is %v, want the 10 it was published with", got)
	}
	_, live := lambdaCall(t, s, http.MethodGet,
		"/2015-03-31/functions/writer/configuration", "")
	if got := live["Timeout"]; got != float64(99) {
		t.Errorf("$LATEST's Timeout is %v, want 99", got)
	}

	// A second publish takes the CURRENT configuration and numbers 2.
	v2 := publish(t, s, "writer", `{}`)
	if got := str(v2["Version"]); got != "2" {
		t.Fatalf("the second version is %q, want \"2\"", got)
	}
	if got := v2["Timeout"]; got != float64(99) {
		t.Errorf("version 2's Timeout is %v, want the 99 that was live", got)
	}

	// The listing is $LATEST first, then the versions in NUMERIC order --
	// not the lexical order a store's ids would give, where 10 precedes 2.
	_, listed := lambdaCall(t, s, http.MethodGet, "/2015-03-31/functions/writer/versions", "")
	versions, _ := listed["Versions"].([]any)
	var got []string
	for _, v := range versions {
		m, _ := v.(map[string]any)
		got = append(got, str(m["Version"]))
	}
	want := []string{"$LATEST", "1", "2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the version listing is %v, want %v", got, want)
	}
}

// The CODE is copied at publish, which is what makes a version a snapshot
// rather than a label. Without it, invoking version 1 would run whatever
// $LATEST became -- the silent-wrong-answer shape.
func TestPublishingCopiesTheCode(t *testing.T) {
	s, _ := newLambdaTestServer(t)
	createWriter(t, s)
	v1 := publish(t, s, "writer", `{}`)

	live := s.lambda.CodeDir(s.lambdaARN("writer"))
	snapshot := s.lambda.CodeDir(str(v1["FunctionArn"]))
	if live == snapshot {
		t.Fatalf("the version shares the live code directory: %s", snapshot)
	}
	original, err := os.ReadFile(filepath.Join(snapshot, "app.py"))
	if err != nil {
		t.Fatalf("the version's code was not copied: %v", err)
	}

	// Replace the live code. The snapshot must not change.
	rec, _ := lambdaCall(t, s, http.MethodPut, "/2015-03-31/functions/writer/code",
		`{"ZipFile": "`+pyZipSaying(t, "second")+`"}`)
	if rec.Code != 200 {
		t.Fatalf("updating the code: %d %s", rec.Code, rec.Body)
	}
	// The code update replaces the container in a goroutine, so the
	// unpack has not necessarily happened yet -- "put the system in the
	// state, do not arrive before a goroutine changes it".
	waitSettled(t, s, "writer")

	after, err := os.ReadFile(filepath.Join(snapshot, "app.py"))
	if err != nil {
		t.Fatalf("the version's code disappeared: %v", err)
	}
	if string(after) != string(original) {
		t.Errorf("the version's code changed with the live function:\n%s", after)
	}
	if nowLive, err := os.ReadFile(filepath.Join(live, "app.py")); err == nil {
		if !strings.Contains(string(nowLive), "second") {
			t.Errorf("the live code did not change: %s", nowLive)
		}
	}
}

// An alias is a stable name pointing into the history, and it must follow
// what it points at rather than snapshotting anything itself.
func TestAnAliasPointsAtAVersion(t *testing.T) {
	s, _ := newLambdaTestServer(t)
	createWriter(t, s)
	publish(t, s, "writer", `{}`)
	publish(t, s, "writer", `{}`)

	rec, alias := lambdaCall(t, s, http.MethodPost, "/2015-03-31/functions/writer/aliases",
		`{"Name": "prod", "FunctionVersion": "1", "Description": "the live one"}`)
	if rec.Code != 201 {
		t.Fatalf("creating the alias: %d %s", rec.Code, rec.Body)
	}
	if got := str(alias["AliasArn"]); !strings.HasSuffix(got, ":writer:prod") {
		t.Errorf("the alias ARN is %q", got)
	}

	// Reading through the alias gives the version behind it.
	_, read := lambdaCall(t, s, http.MethodGet,
		"/2015-03-31/functions/writer/configuration?Qualifier=prod", "")
	if got := str(read["Version"]); got != "1" {
		t.Errorf("reading through the alias gives version %q, want \"1\"", got)
	}

	// Moving the alias is a PUT, and a field left out keeps its value.
	rec, moved := lambdaCall(t, s, http.MethodPut,
		"/2015-03-31/functions/writer/aliases/prod", `{"FunctionVersion": "2"}`)
	if rec.Code != 200 {
		t.Fatalf("moving the alias: %d %s", rec.Code, rec.Body)
	}
	if got := str(moved["FunctionVersion"]); got != "2" {
		t.Errorf("the alias points at %q, want \"2\"", got)
	}
	if got := str(moved["Description"]); got != "the live one" {
		t.Errorf("a partial update cleared the description: %q", got)
	}

	// Listing, and narrowing by the version an alias points at.
	_, listed := lambdaCall(t, s, http.MethodGet, "/2015-03-31/functions/writer/aliases", "")
	if aliases, _ := listed["Aliases"].([]any); len(aliases) != 1 {
		t.Errorf("the alias listing has %d entries, want 1", len(aliases))
	}
	_, narrowed := lambdaCall(t, s, http.MethodGet,
		"/2015-03-31/functions/writer/aliases?FunctionVersion=1", "")
	if aliases, _ := narrowed["Aliases"].([]any); len(aliases) != 0 {
		t.Errorf("narrowing to version 1 found %d aliases, want 0 after the move", len(aliases))
	}

	// And it goes.
	if rec, _ := lambdaCall(t, s, http.MethodDelete,
		"/2015-03-31/functions/writer/aliases/prod", ""); rec.Code != 204 {
		t.Errorf("deleting the alias = %d, want 204", rec.Code)
	}
	if rec, _ := lambdaCall(t, s, http.MethodGet,
		"/2015-03-31/functions/writer/aliases/prod", ""); rec.Code != 404 {
		t.Errorf("the deleted alias still reads: %d", rec.Code)
	}
	// Deleting it again succeeds, which is what makes a destroy idempotent.
	if rec, _ := lambdaCall(t, s, http.MethodDelete,
		"/2015-03-31/functions/writer/aliases/prod", ""); rec.Code != 204 {
		t.Errorf("deleting an absent alias = %d, want 204", rec.Code)
	}
}

// A qualifier is EITHER a version number or an alias name, and both make
// the same qualified ARN -- so a numeric alias would be a second resource
// with the first one's id. Real AWS forbids it; so does this.
func TestAnAliasMayNotLookLikeAVersion(t *testing.T) {
	s, _ := newLambdaTestServer(t)
	createWriter(t, s)
	publish(t, s, "writer", `{}`)

	for _, name := range []string{"1", "2", "$LATEST"} {
		rec, doc := lambdaCall(t, s, http.MethodPost, "/2015-03-31/functions/writer/aliases",
			`{"Name": "`+name+`", "FunctionVersion": "1"}`)
		if rec.Code != 400 {
			t.Errorf("an alias called %q was accepted with %d", name, rec.Code)
		}
		if !strings.Contains(str(doc["message"]), name) {
			t.Errorf("the refusal for %q does not name it: %v", name, doc["message"])
		}
	}
}

// An alias pointing at a version nobody published is refused at creation,
// not left to fail at invoke time far from the mistake.
func TestAnAliasCannotPointAtAMissingVersion(t *testing.T) {
	s, _ := newLambdaTestServer(t)
	createWriter(t, s)

	rec, _ := lambdaCall(t, s, http.MethodPost, "/2015-03-31/functions/writer/aliases",
		`{"Name": "prod", "FunctionVersion": "7"}`)
	if rec.Code != 404 {
		t.Errorf("an alias pointing at version 7 was accepted with %d", rec.Code)
	}
	// $LATEST is a legal target, which real Lambda allows.
	if rec, _ := lambdaCall(t, s, http.MethodPost, "/2015-03-31/functions/writer/aliases",
		`{"Name": "edge", "FunctionVersion": "$LATEST"}`); rec.Code != 201 {
		t.Errorf("an alias on $LATEST was refused with %d", rec.Code)
	}
}

// Publishing with a CodeSha256 that does not match is refused -- it is how
// a client makes sure it is publishing the code it just uploaded.
func TestPublishingChecksTheCodeDigest(t *testing.T) {
	s, _ := newLambdaTestServer(t)
	createWriter(t, s)

	rec, doc := lambdaCall(t, s, http.MethodPost, "/2015-03-31/functions/writer/versions",
		`{"CodeSha256": "not-the-deployed-digest"}`)
	if rec.Code != 400 {
		t.Fatalf("a mismatched CodeSha256 was accepted with %d", rec.Code)
	}
	if !strings.Contains(str(doc["message"]), "not-the-deployed-digest") {
		t.Errorf("the refusal does not quote what it was given: %v", doc["message"])
	}
	// The real digest is accepted.
	_, live := lambdaCall(t, s, http.MethodGet, "/2015-03-31/functions/writer/configuration", "")
	publish(t, s, "writer", `{"CodeSha256": "`+str(live["CodeSha256"])+`"}`)
}

// Deleting the function takes its versions and aliases with it, and
// removes their code -- a container and a code directory are not store
// resources, so the cascade that removes their records cannot reach them.
func TestDeletingAFunctionTakesItsVersionsAndAliases(t *testing.T) {
	s, _ := newLambdaTestServer(t)
	createWriter(t, s)
	v1 := publish(t, s, "writer", `{}`)
	lambdaCall(t, s, http.MethodPost, "/2015-03-31/functions/writer/aliases",
		`{"Name": "prod", "FunctionVersion": "1"}`)

	snapshot := s.lambda.CodeDir(str(v1["FunctionArn"]))
	if _, err := os.Stat(snapshot); err != nil {
		t.Fatalf("the version's code is not there to begin with: %v", err)
	}

	if rec, _ := lambdaCall(t, s, http.MethodDelete, "/2015-03-31/functions/writer", ""); rec.Code != 204 {
		t.Fatalf("deleting the function: %d", rec.Code)
	}
	if s.store.Exists(str(v1["FunctionArn"])) {
		t.Error("the version outlived the function")
	}
	if s.store.Exists(s.lambdaARN("writer") + ":prod") {
		t.Error("the alias outlived the function")
	}
	if _, err := os.Stat(snapshot); err == nil {
		t.Errorf("the version's code directory was left behind: %s", snapshot)
	}
}

// A delete naming a QUALIFIER prunes one version and leaves the function.
// $LATEST is refused, because deleting it would mean deleting the function
// by another name.
func TestDeletingOneVersionLeavesTheFunction(t *testing.T) {
	s, _ := newLambdaTestServer(t)
	createWriter(t, s)
	v1 := publish(t, s, "writer", `{}`)
	v2 := publish(t, s, "writer", `{}`)

	if rec, _ := lambdaCall(t, s, http.MethodDelete,
		"/2015-03-31/functions/writer?Qualifier=1", ""); rec.Code != 204 {
		t.Fatalf("deleting version 1: %d", rec.Code)
	}
	if s.store.Exists(str(v1["FunctionArn"])) {
		t.Error("version 1 survived its delete")
	}
	if !s.store.Exists(str(v2["FunctionArn"])) {
		t.Error("deleting version 1 took version 2 as well")
	}
	if _, aerr := s.loadFunction("writer"); aerr != nil {
		t.Error("deleting a version took the function")
	}

	rec, _ := lambdaCall(t, s, http.MethodDelete,
		"/2015-03-31/functions/writer?Qualifier=$LATEST", "")
	if rec.Code != 400 {
		t.Errorf("deleting $LATEST on its own = %d, want 400", rec.Code)
	}
}

// A qualified ARN is the RESOURCE a policy names, which is what makes
// "may invoke the prod alias but not $LATEST" something a person can
// write -- the whole reason aliases exist.
func TestAQualifiedInvokeIsADifferentResource(t *testing.T) {
	s, _ := newLambdaTestServer(t)
	createWriter(t, s)

	cases := []struct{ path, want string }{
		{"/2015-03-31/functions/writer/invocations", "function:writer"},
		{"/2015-03-31/functions/writer/invocations?Qualifier=1", "function:writer:1"},
		{"/2015-03-31/functions/writer/invocations?Qualifier=prod", "function:writer:prod"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("{}"))
		got, ok := s.lambdaTarget(r)
		if !ok {
			t.Errorf("%s was not classified at all", tc.path)
			continue
		}
		if !strings.HasSuffix(got.Resource, tc.want) {
			t.Errorf("%s names resource %q, want one ending %q", tc.path, got.Resource, tc.want)
		}
		if got.Action != "lambda:InvokeFunction" {
			t.Errorf("%s is action %q", tc.path, got.Action)
		}
	}
}

// pyZipSaying is a handler whose body contains a marker, so a test can
// tell one unpacked copy of the code from another.
func pyZipSaying(t *testing.T, marker string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("app.py")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("def handler(event, context):\n    return {\"said\": \"" + marker + "\"}\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// A weighted alias really splits, and the split is a pure function of the
// config and one number -- which is the only way to test it without
// counting thousands of invocations. An ignored weight would send every
// request one way while the configuration said otherwise, which a client
// reads as an answer about its canary.
func TestAWeightedAliasSplitsTraffic(t *testing.T) {
	alias := &lambdaAlias{
		FunctionVersion: "1",
		RoutingConfig: map[string]any{
			// Weights that sum EXACTLY in binary, so the boundaries are
			// real assertions rather than a test of IEEE 754: 0.1 + 0.2
			// is 0.30000000000000004, and a case sitting on that edge
			// tests floating-point addition instead of the split.
			"AdditionalVersionWeights": map[string]any{"2": 0.125, "3": 0.25},
		},
	}
	cases := []struct {
		roll float64
		want string
	}{
		{0.000, "2"}, // the first eighth goes to version 2
		{0.124, "2"},
		{0.125, "3"}, // the next quarter to version 3
		{0.374, "3"},
		{0.375, "1"}, // and the rest to the version the alias names
		{0.999, "1"},
	}
	for _, tc := range cases {
		if got := pickAliasVersion(alias, tc.roll); got != tc.want {
			t.Errorf("a roll of %.2f picked version %q, want %q", tc.roll, got, tc.want)
		}
	}

	// No routing config at all means the named version, every time.
	plain := &lambdaAlias{FunctionVersion: "4"}
	for _, roll := range []float64{0, 0.5, 0.999} {
		if got := pickAliasVersion(plain, roll); got != "4" {
			t.Errorf("an unweighted alias picked %q at roll %.3f", got, roll)
		}
	}

	// A weight that is not a number, or is zero, is skipped rather than
	// swallowing the traffic.
	odd := &lambdaAlias{
		FunctionVersion: "1",
		RoutingConfig: map[string]any{
			"AdditionalVersionWeights": map[string]any{"2": "a tenth", "3": 0.0},
		},
	}
	if got := pickAliasVersion(odd, 0.0); got != "1" {
		t.Errorf("an unreadable weight captured the traffic: %q", got)
	}

	// A NEGATIVE weight is where skipping matters rather than being
	// defensive: accumulating it would move the running total BACKWARDS,
	// so a later version's slice would start below where the previous one
	// ended and claim traffic that is not its own.
	negative := &lambdaAlias{
		FunctionVersion: "1",
		RoutingConfig: map[string]any{
			"AdditionalVersionWeights": map[string]any{"2": -0.5, "3": 0.25},
		},
	}
	if got := pickAliasVersion(negative, 0.0); got != "3" {
		t.Errorf("with a negative weight on version 2, a roll of 0 picked %q, want \"3\"", got)
	}
	if got := pickAliasVersion(negative, 0.5); got != "1" {
		t.Errorf("with a negative weight on version 2, a roll of 0.5 picked %q, want \"1\"", got)
	}
}
