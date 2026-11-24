package storage

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// These tests drive a SAS through Serve, which is the one place every storage
// request passes, so they assert the DECISION rather than the layout.
//
// They sign with the account's own key through stringToSign, which means they
// cannot catch a layout error -- deliberately. TestARealClientsSASVerifies is
// what pins the layout, against signatures az computed; these are about what
// happens once a signature is good, and building the token any other way would
// make every one of them a signature test as well.

const sasFar = "2099-01-01T00:00:00Z"

// sasQuery builds a signed SAS query string the way a client would.
func sasQuery(t *testing.T, a *Account, tok *sasToken, kind sasKind, container, blob string) string {
	t.Helper()
	if tok.version == "" {
		tok.version = "2026-04-06"
	}
	sts, _ := tok.stringToSign(kind, a.Name, ServiceBlob, container, blob)
	sig, err := Sign(a.Keys[0], sts)
	if err != nil {
		t.Fatal(err)
	}
	q := url.Values{"sv": {tok.version}, "sp": {tok.perms}, "se": {tok.expiry}, "sig": {sig}}
	for k, v := range map[string]string{
		"st": tok.start, "sip": tok.ip, "spr": tok.protocol,
		"sr": tok.resource, "ss": tok.services, "srt": tok.resTypes,
	} {
		if v != "" {
			q.Set(k, v)
		}
	}
	return q.Encode()
}

// sasFixture is an account with one container holding one blob.
func sasFixtureStore(t *testing.T) (*Handler, *Account) {
	t.Helper()
	store := NewStore()
	a := store.CreateAccount("stnimbus")
	c, ok := a.CreateContainer("docs", nil, "")
	if !ok {
		t.Fatal("could not create the container")
	}
	if _, ok := a.CreateContainer("other", nil, ""); !ok {
		t.Fatal("could not create the second container")
	}
	c.PutBlob(&Blob{Name: "hello.txt", Content: []byte("HELLO")})
	return NewHandler(store), a
}

func sasDo(h *Handler, method, path, query string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path+"?"+query, strings.NewReader(body))
	if method == http.MethodPut {
		req.Header.Set("x-ms-blob-type", "BlockBlob")
	}
	rec := httptest.NewRecorder()
	h.Serve(rec, req, Target{Account: "stnimbus", Service: ServiceBlob})
	return rec
}

// A read-only SAS reads and cannot write, and the refusal names the letter.
//
// This is the milestone's stated purpose: a SAS that grants exactly what it
// says, so it can be used to TEST that permissions are wired correctly.
func TestAReadOnlySASCannotWrite(t *testing.T) {
	h, a := sasFixtureStore(t)
	tok := &sasToken{perms: "r", expiry: sasFar, resource: "c"}
	q := sasQuery(t, a, tok, sasService, "docs", "")

	if rec := sasDo(h, http.MethodGet, "/docs/hello.txt", q, ""); rec.Code != 200 {
		t.Errorf("read with sp=r answered %d, want 200: %s", rec.Code, rec.Body.String())
	}

	rec := sasDo(h, http.MethodPut, "/docs/new.txt", q, "NOPE")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("write with sp=r answered %d, want 403: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The body is XML, so a quoted value arrives escaped -- assert the
	// substance rather than the quoting.
	for _, want := range []string{"AuthorizationPermissionMismatch", "put blob", "sp=", "w (write)", "c (create)"} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal does not mention %q: %s", want, body)
		}
	}
	// And nothing was written.
	if b := a.Container("docs").Blob("new.txt"); b != nil {
		t.Error("the refused PUT created the blob anyway")
	}
}

// An expired SAS is refused and the message says WHEN it expired.
//
// "403" on its own sends people to look at their permissions, which is the one
// thing that is not wrong.
func TestAnExpiredSASIsRefusedWithItsExpiry(t *testing.T) {
	h, a := sasFixtureStore(t)
	past := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	tok := &sasToken{perms: "r", expiry: past, resource: "c"}
	q := sasQuery(t, a, tok, sasService, "docs", "")

	rec := sasDo(h, http.MethodGet, "/docs/hello.txt", q, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expired SAS answered %d, want 403", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "expired at") || !strings.Contains(body, past[:13]) {
		t.Errorf("refusal does not quote the expiry (%s): %s", past, body)
	}
}

// A SAS whose start time has not arrived is refused too.
func TestASASIsRefusedBeforeItsStartTime(t *testing.T) {
	h, a := sasFixtureStore(t)
	future := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	tok := &sasToken{perms: "r", expiry: sasFar, start: future, resource: "c"}
	q := sasQuery(t, a, tok, sasService, "docs", "")

	rec := sasDo(h, http.MethodGet, "/docs/hello.txt", q, "")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not valid until") {
		t.Errorf("a SAS used before st answered %d: %s", rec.Code, rec.Body.String())
	}
}

// A SAS for one container cannot read another.
//
// The container is part of the signed resource, so this is enforced by the
// signature rather than by a separate comparison -- which is the stronger
// guarantee, because no amount of permission juggling can widen it.
func TestASASForOneContainerCannotReadAnother(t *testing.T) {
	h, a := sasFixtureStore(t)
	other := a.Container("other")
	other.PutBlob(&Blob{Name: "secret.txt", Content: []byte("SECRET")})

	tok := &sasToken{perms: "rl", expiry: sasFar, resource: "c"}
	q := sasQuery(t, a, tok, sasService, "docs", "")

	if rec := sasDo(h, http.MethodGet, "/docs/hello.txt", q, ""); rec.Code != 200 {
		t.Fatalf("the SAS does not work on its own container: %d %s", rec.Code, rec.Body.String())
	}
	rec := sasDo(h, http.MethodGet, "/other/secret.txt", q, "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("a SAS for container docs read container other: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Error("the refused read returned the blob's content")
	}
}

// A blob-scoped SAS cannot do container-level things, and says so as a SCOPE
// error rather than a signature one.
func TestABlobScopedSASCannotListTheContainer(t *testing.T) {
	h, a := sasFixtureStore(t)
	tok := &sasToken{perms: "rl", expiry: sasFar, resource: "b"}
	q := sasQuery(t, a, tok, sasService, "docs", "hello.txt")

	if rec := sasDo(h, http.MethodGet, "/docs/hello.txt", q, ""); rec.Code != 200 {
		t.Fatalf("sr=b could not read its own blob: %d %s", rec.Code, rec.Body.String())
	}
	rec := sasDo(h, http.MethodGet, "/docs", q+"&restype=container&comp=list", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("sr=b listed the container: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "AuthorizationResourceTypeMismatch") || !strings.Contains(body, "single blob") {
		t.Errorf("refusal blames the wrong thing; a scope error must not read as a signature error: %s", body)
	}
}

// An account SAS is checked against ss and srt.
func TestAnAccountSASIsCheckedAgainstItsServicesAndResourceTypes(t *testing.T) {
	h, a := sasFixtureStore(t)

	// ss=q covers the queue service, not blob.
	tok := &sasToken{perms: "rl", expiry: sasFar, services: "q", resTypes: "sco"}
	rec := sasDo(h, http.MethodGet, "/docs/hello.txt", sasQuery(t, a, tok, sasAccount, "", ""), "")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "AuthorizationServiceMismatch") {
		t.Errorf("an account SAS for ss=q read a blob: %d %s", rec.Code, rec.Body.String())
	}

	// srt=o is object level only, so a container listing is out of scope.
	tok = &sasToken{perms: "rl", expiry: sasFar, services: "b", resTypes: "o"}
	q := sasQuery(t, a, tok, sasAccount, "", "")
	if rec := sasDo(h, http.MethodGet, "/docs/hello.txt", q, ""); rec.Code != 200 {
		t.Errorf("srt=o could not read a blob: %d %s", rec.Code, rec.Body.String())
	}
	rec = sasDo(h, http.MethodGet, "/docs", q+"&restype=container&comp=list", "")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "AuthorizationResourceTypeMismatch") {
		t.Errorf("srt=o listed a container: %d %s", rec.Code, rec.Body.String())
	}

	// srt=sco with the right permissions does work, so the refusals above are
	// about scope and not about something else being broken.
	tok = &sasToken{perms: "rl", expiry: sasFar, services: "b", resTypes: "sco"}
	q = sasQuery(t, a, tok, sasAccount, "", "")
	if rec := sasDo(h, http.MethodGet, "/docs", q+"&restype=container&comp=list", ""); rec.Code != 200 {
		t.Errorf("a full account SAS could not list a container: %d %s", rec.Code, rec.Body.String())
	}
}

// spr and sip are enforced, because both are answerable from the request.
func TestSASProtocolAndSourceIPAreEnforced(t *testing.T) {
	h, a := sasFixtureStore(t)

	// httptest requests are not TLS, so a token demanding https is refused --
	// truthfully, which is the point. The AWS aws:SecureTransport note says
	// the same thing: answer it honestly even when the answer is awkward.
	tok := &sasToken{perms: "r", expiry: sasFar, resource: "c", protocol: "https"}
	rec := sasDo(h, http.MethodGet, "/docs/hello.txt", sasQuery(t, a, tok, sasService, "docs", ""), "")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "AuthorizationProtocolMismatch") {
		t.Errorf("spr=https was not enforced over plain HTTP: %d %s", rec.Code, rec.Body.String())
	}

	// A token allowing both protocols works.
	tok = &sasToken{perms: "r", expiry: sasFar, resource: "c", protocol: "https,http"}
	if rec := sasDo(h, http.MethodGet, "/docs/hello.txt", sasQuery(t, a, tok, sasService, "docs", ""), ""); rec.Code != 200 {
		t.Errorf("spr=https,http was refused over http: %d %s", rec.Code, rec.Body.String())
	}

	// httptest's RemoteAddr is 192.0.2.1, so a token bound elsewhere fails.
	tok = &sasToken{perms: "r", expiry: sasFar, resource: "c", ip: "203.0.113.7"}
	rec = sasDo(h, http.MethodGet, "/docs/hello.txt", sasQuery(t, a, tok, sasService, "docs", ""), "")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "AuthorizationSourceIPMismatch") {
		t.Errorf("sip was not enforced: %d %s", rec.Code, rec.Body.String())
	}

	// And a range that contains the caller is allowed.
	tok = &sasToken{perms: "r", expiry: sasFar, resource: "c", ip: "192.0.2.0-192.0.2.255"}
	if rec := sasDo(h, http.MethodGet, "/docs/hello.txt", sasQuery(t, a, tok, sasService, "docs", ""), ""); rec.Code != 200 {
		t.Errorf("a sip range containing the caller was refused: %d %s", rec.Code, rec.Body.String())
	}
}

// Shared Key is untouched: it is the control that proves a SAS regression is a
// SAS regression.
func TestSharedKeyStillWorksAlongsideSASEnforcement(t *testing.T) {
	h, a := sasFixtureStore(t)
	req := httptest.NewRequest(http.MethodGet, "/docs/hello.txt", nil)
	req.Header.Set("x-ms-date", time.Now().UTC().Format(http.TimeFormat))
	req.Header.Set("x-ms-version", "2025-05-05")
	req.Header.Set("Date", req.Header.Get("x-ms-date"))
	sig, err := Sign(a.Keys[0], StringToSign(req, a.Name, ServiceBlob, false))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "SharedKey "+a.Name+":"+sig)
	rec := httptest.NewRecorder()
	h.Serve(rec, req, Target{Account: "stnimbus", Service: ServiceBlob})
	if rec.Code != 200 || rec.Body.String() != "HELLO" {
		t.Errorf("Shared Key read answered %d %q", rec.Code, rec.Body.String())
	}
}

// Every case the blob routers handle must be classifiable for a SAS.
//
// The classifier MIRRORS serveBlob, containerOp and blobOp, and a classifier
// that disagrees with its router authorizes one thing and serves another. This
// walks the operations those switches accept and fails if any of them is
// unknown here -- so adding a case to the router without adding one here is a
// test failure rather than a refused apply.
func TestEveryBlobOperationIsClassifiableForASAS(t *testing.T) {
	cases := []struct {
		method, path, query string
		wantPerms           string
		wantLevel           byte
	}{
		{"GET", "/", "restype=service&comp=properties", "r", 's'},
		{"PUT", "/", "restype=service&comp=properties", "w", 's'},
		{"GET", "/", "comp=list", "l", 's'},
		{"PUT", "/docs", "restype=container", "c", 'c'},
		{"PUT", "/docs", "restype=container&comp=metadata", "w", 'c'},
		{"PUT", "/docs", "restype=container&comp=acl", "w", 'c'},
		{"DELETE", "/docs", "restype=container", "d", 'c'},
		{"GET", "/docs", "restype=container&comp=list", "l", 'c'},
		{"GET", "/docs", "restype=container", "r", 'c'},
		{"GET", "/docs", "restype=container&comp=metadata", "r", 'c'},
		{"GET", "/docs", "restype=container&comp=acl", "r", 'c'},
		{"PUT", "/docs/x.txt", "", "wc", 'o'},
		{"PUT", "/docs/x.txt", "comp=block&blockid=AA", "wac", 'o'},
		{"PUT", "/docs/x.txt", "comp=blocklist", "wac", 'o'},
		{"PUT", "/docs/x.txt", "comp=metadata", "w", 'o'},
		{"PUT", "/docs/x.txt", "comp=properties", "w", 'o'},
		{"GET", "/docs/x.txt", "", "r", 'o'},
		{"HEAD", "/docs/x.txt", "", "r", 'o'},
		{"GET", "/docs/x.txt", "comp=blocklist", "r", 'o'},
		{"DELETE", "/docs/x.txt", "", "d", 'o'},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path+"?"+c.query, nil)
		container, blob := splitBlobPath(req.URL.Path)
		op, ok := blobSASOperation(req, container, blob)
		if !ok {
			t.Errorf("%s %s?%s is served by the router and cannot be classified for a SAS",
				c.method, c.path, c.query)
			continue
		}
		if op.perms != c.wantPerms {
			t.Errorf("%s %s?%s needs %q, classifier says %q", c.method, c.path, c.query, c.wantPerms, op.perms)
		}
		if op.level != c.wantLevel {
			t.Errorf("%s %s?%s is %c-level, classifier says %c", c.method, c.path, c.query, c.wantLevel, op.level)
		}
	}
}

// An operation the classifier cannot place is REFUSED, not waved through.
//
// The tempting failure is to treat an unclassified request as permitted, which
// turns every gap in the classifier into a permission nobody granted -- the
// FilterExpression rule, which this project has paid for in four subsystems.
func TestAnUnclassifiableOperationIsRefused(t *testing.T) {
	h, a := sasFixtureStore(t)
	tok := &sasToken{perms: "rwdlac", expiry: sasFar, resource: "c"}
	q := sasQuery(t, a, tok, sasService, "docs", "")

	// A verb no blob router handles, with a SAS granting every letter there is.
	rec := sasDo(h, http.MethodPatch, "/docs/hello.txt", q, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an unclassifiable operation answered %d with an all-permissions SAS, want 403", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "cannot classify") || !strings.Contains(body, "PATCH") {
		t.Errorf("the refusal does not name the request it could not classify: %s", body)
	}
}

// A SAS is refused for the services whose layout has never been measured.
func TestASASIsRefusedForUnmeasuredServices(t *testing.T) {
	h, a := sasFixtureStore(t)
	tok := &sasToken{perms: "r", expiry: sasFar, services: "bqtf", resTypes: "sco"}
	q := sasQuery(t, a, tok, sasAccount, "", "")
	for _, svc := range []Service{ServiceQueue, ServiceTable, ServiceFile} {
		req := httptest.NewRequest(http.MethodGet, "/q1/messages?"+q, nil)
		rec := httptest.NewRecorder()
		h.Serve(rec, req, Target{Account: "stnimbus", Service: svc})
		if rec.Code != http.StatusForbidden {
			t.Errorf("a SAS was accepted for %s, whose layout has never been measured: %d",
				serviceLabel(svc), rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "blob service only") {
			t.Errorf("the refusal for %s does not say why: %s", serviceLabel(svc), rec.Body.String())
		}
	}
}

// --- anonymous access -------------------------------------------------------

// anonDo makes an unauthenticated request through Serve.
func anonDo(h *Handler, method, path, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path+"?"+query, nil)
	if method == http.MethodPut {
		req.Header.Set("x-ms-blob-type", "BlockBlob")
	}
	rec := httptest.NewRecorder()
	h.Serve(rec, req, Target{Account: "stnimbus", Service: ServiceBlob})
	return rec
}

// A container's public access level DECIDES, which it never did before.
//
// PublicAccess was stored, persisted, listed and echoed back as
// x-ms-blob-public-access, and read in no decision anywhere -- so every
// anonymous request to a private container was served. The Azure twin of the
// AWS bug where an unsigned request resolved to the unrestricted account root.
func TestAContainersPublicAccessLevelDecides(t *testing.T) {
	store := NewStore()
	a := store.CreateAccount("stnimbus")
	for name, access := range map[string]string{"priv": "", "blobs": "blob", "full": "container"} {
		c, ok := a.CreateContainer(name, nil, access)
		if !ok {
			t.Fatalf("could not create container %s", name)
		}
		c.PutBlob(&Blob{Name: "x.txt", Content: []byte("CONTENT-" + name)})
	}
	h := NewHandler(store)

	cases := []struct {
		name, container, method, path, query string
		wantCode                             int
		why                                  string
	}{
		{"private container, read a blob", "priv", "GET", "/priv/x.txt", "", 404,
			"a private container must not serve an anonymous read"},
		{"private container, list", "priv", "GET", "/priv", "restype=container&comp=list", 404,
			"a private container must not be listable anonymously"},
		{"blob-level access, read a blob", "blobs", "GET", "/blobs/x.txt", "", 200,
			"public access \"blob\" is exactly this"},
		{"blob-level access, list", "blobs", "GET", "/blobs", "restype=container&comp=list", 403,
			"public access \"blob\" must NOT allow listing -- that is what \"container\" adds"},
		{"container-level access, read a blob", "full", "GET", "/full/x.txt", "", 200,
			"public access \"container\" includes reads"},
		{"container-level access, list", "full", "GET", "/full", "restype=container&comp=list", 200,
			"public access \"container\" is what allows listing"},
		{"public container, write", "full", "PUT", "/full/new.txt", "", 403,
			"anonymous is read-only whatever the access level says"},
		{"public container, delete", "full", "DELETE", "/full/x.txt", "", 403,
			"anonymous is read-only whatever the access level says"},
		{"account-level list containers", "", "GET", "/", "comp=list", 403,
			"there is no container to carry an access level, so this is never anonymous"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := anonDo(h, c.method, c.path, c.query)
			if rec.Code != c.wantCode {
				t.Errorf("answered %d, want %d -- %s\n%s", rec.Code, c.wantCode, c.why, rec.Body.String())
			}
		})
	}

	// The content really came back for the cases that should serve it, so the
	// 200s above are reads and not empty successes.
	if body := anonDo(h, "GET", "/blobs/x.txt", "").Body.String(); body != "CONTENT-blobs" {
		t.Errorf("anonymous read of a public blob returned %q", body)
	}
	// And nothing was written by the refused write.
	if b := a.Container("full").Blob("new.txt"); b != nil {
		t.Error("a refused anonymous PUT created the blob anyway")
	}
}

// A private container and an absent one answer IDENTICALLY.
//
// Azure will not tell an unauthenticated caller that a container exists but is
// private, because the difference is itself information: a distinguishable
// refusal lets anyone enumerate container names. So both are 404
// ResourceNotFound and neither names the container.
func TestAPrivateContainerIsIndistinguishableFromAnAbsentOne(t *testing.T) {
	store := NewStore()
	a := store.CreateAccount("stnimbus")
	c, _ := a.CreateContainer("exists-but-private", nil, "")
	c.PutBlob(&Blob{Name: "x.txt", Content: []byte("SECRET")})
	h := NewHandler(store)

	private := anonDo(h, "GET", "/exists-but-private/x.txt", "")
	absent := anonDo(h, "GET", "/no-such-container/x.txt", "")

	if private.Code != 404 || absent.Code != 404 {
		t.Fatalf("private answered %d and absent answered %d; both must be 404", private.Code, absent.Code)
	}
	if private.Body.String() != absent.Body.String() {
		t.Errorf("the two refusals differ, which is enough to enumerate containers:\nprivate: %s\nabsent:  %s",
			private.Body.String(), absent.Body.String())
	}
	if strings.Contains(private.Body.String(), "exists-but-private") {
		t.Error("the refusal names the container it is hiding")
	}
	if strings.Contains(private.Body.String(), "SECRET") {
		t.Error("the refused read returned the content")
	}
}

// Anonymous access does not exist for queue, table or file.
func TestAnonymousIsRefusedForNonBlobServices(t *testing.T) {
	store := NewStore()
	store.CreateAccount("stnimbus")
	h := NewHandler(store)
	for _, svc := range []Service{ServiceQueue, ServiceTable, ServiceFile} {
		req := httptest.NewRequest(http.MethodGet, "/q1/messages", nil)
		rec := httptest.NewRecorder()
		h.Serve(rec, req, Target{Account: "stnimbus", Service: svc})
		if rec.Code == 200 {
			t.Errorf("%s served an anonymous request", serviceLabel(svc))
		}
	}
}

// A SAS whose signature cannot be DECODED is refused as a broken SAS, not
// demoted to anonymous.
//
// url.Values comes from URL.Query(), which discards its parse error and
// returns only the pairs it could decode -- so a sig containing a malformed
// percent-escape vanishes, and a request that plainly meant to present a
// credential presents as having none. Found by probing a real token: changing
// the last character of "...sig=abc%3D" makes "%3X", and a private blob then
// answered 404 "the specified resource does not exist", which explains nothing
// and points nowhere near the truth.
//
// So presence is read from the RAW query. Demoting a broken credential to no
// credential is the quiet kind of wrong, and here it even changed the status.
func TestABrokenSASSignatureIsNotTreatedAsAnonymous(t *testing.T) {
	h, a := sasFixtureStore(t)
	tok := &sasToken{perms: "r", expiry: sasFar, resource: "c"}
	good := sasQuery(t, a, tok, sasService, "docs", "")

	// url.Values.Encode() escapes the signature's "=" as %3D; truncating that
	// escape is what a careless edit or re-quote does.
	broken := strings.Replace(good, "%3D", "%3X", 1)
	if broken == good {
		t.Fatalf("the signature carried no percent-escape to break: %s", good)
	}

	rec := sasDo(h, http.MethodGet, "/docs/hello.txt", broken, "")
	if rec.Code == 404 {
		t.Errorf("a broken SAS was judged as anonymous and answered 404; " +
			"a request that meant to present a credential must be judged as one")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a broken SAS answered %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "could not be read") {
		t.Errorf("the refusal does not say the signature was unreadable: %s", body)
	}
}

// A signature that decodes but is wrong is refused as a bad signature, quoting
// the string-to-sign -- which is the ordinary tampering case, and must not be
// confused with the undecodable one above.
func TestATamperedButDecodableSASQuotesTheStringToSign(t *testing.T) {
	h, a := sasFixtureStore(t)
	tok := &sasToken{perms: "r", expiry: sasFar, resource: "c"}
	good := sasQuery(t, a, tok, sasService, "docs", "")

	// Flip a character inside the base64 body, leaving every escape intact.
	i := strings.Index(good, "sig=") + 10
	flipped := "A"
	if good[i] == 'A' {
		flipped = "B"
	}
	broken := good[:i] + flipped + good[i+1:]

	rec := sasDo(h, http.MethodGet, "/docs/hello.txt", broken, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a tampered SAS answered %d, want 403: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "string-to-sign") || !strings.Contains(body, "/blob/stnimbus/docs") {
		t.Errorf("the refusal does not quote the string-to-sign it used: %s", body)
	}
}
