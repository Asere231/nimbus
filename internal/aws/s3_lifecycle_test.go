package aws

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Bucket lifecycle configuration.
//
// `?lifecycle` fell through applyBucketConfig with no case: a PUT was
// accepted and discarded and the GET went on answering
// NoSuchLifecycleConfiguration. Worse than a permanent diff -- the
// provider PUTs the rules and then POLLS for them, so the apply FAILED
// after about two minutes with "couldn't find resource (21 retries)".
// Measured against aws provider 5.100.0.

const noncurrentRules = `<LifecycleConfiguration>
  <Rule>
    <ID>expire-noncurrent</ID>
    <Status>Enabled</Status>
    <Filter><Prefix>logs/</Prefix></Filter>
    <NoncurrentVersionExpiration><NoncurrentDays>7</NoncurrentDays></NoncurrentVersionExpiration>
  </Rule>
</LifecycleConfiguration>`

func putLifecycle(t *testing.T, s *Server, bucketName, body string) int {
	t.Helper()
	rec := s3req(t, s, http.MethodPut, "/"+bucketName+"?lifecycle", body)
	return rec.Code
}

// The round trip the provider depends on: it writes the rules and reads
// them back to decide whether the apply worked.
func TestALifecycleConfigurationRoundTrips(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")

	// Unset, it is NoSuchLifecycleConfiguration -- which the provider
	// tolerates, exactly like the other bucket sub-resources.
	rec := s3req(t, s, http.MethodGet, "/demo?lifecycle", "")
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "NoSuchLifecycleConfiguration") {
		t.Errorf("an unset lifecycle is %d %s", rec.Code, rec.Body)
	}

	if code := putLifecycle(t, s, "demo", noncurrentRules); code != 200 {
		t.Fatalf("PUT ?lifecycle = %d", code)
	}
	rec = s3req(t, s, http.MethodGet, "/demo?lifecycle", "")
	if rec.Code != 200 {
		t.Fatalf("GET ?lifecycle after a PUT = %d %s", rec.Code, rec.Body)
	}
	for _, want := range []string{"expire-noncurrent", "<Status>Enabled</Status>",
		"<Prefix>logs/</Prefix>", "<NoncurrentDays>7</NoncurrentDays>"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the configuration read back is missing %q:\n%s", want, rec.Body)
		}
	}

	// And it can be removed again.
	if rec := s3req(t, s, http.MethodDelete, "/demo?lifecycle", ""); rec.Code != 204 {
		t.Errorf("DELETE ?lifecycle = %d", rec.Code)
	}
	if rec := s3req(t, s, http.MethodGet, "/demo?lifecycle", ""); rec.Code != 404 {
		t.Errorf("after the delete, GET = %d", rec.Code)
	}
}

// A rule asking for something Nimbus does not apply is REFUSED, not
// stored and ignored. A lifecycle rule deletes data: one accepted but not
// applied is a backup that silently is not running, and one applied more
// broadly than written deletes what was meant to be kept.
func TestALifecycleRuleNimbusCannotApplyIsRefused(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")

	cases := map[string]string{
		"a storage-class transition": `<Rule><ID>t</ID><Status>Enabled</Status>
			<Transition><Days>30</Days></Transition></Rule>`,
		"a tag filter": `<Rule><ID>t</ID><Status>Enabled</Status>
			<Filter><Tag><Key>env</Key></Tag></Filter>
			<Expiration><Days>1</Days></Expiration></Rule>`,
		"an object-size filter": `<Rule><ID>t</ID><Status>Enabled</Status>
			<Filter><ObjectSizeGreaterThan>100</ObjectSizeGreaterThan></Filter>
			<Expiration><Days>1</Days></Expiration></Rule>`,
		"a Date rather than Days": `<Rule><ID>t</ID><Status>Enabled</Status>
			<Expiration><Date>2030-01-01T00:00:00Z</Date></Expiration></Rule>`,
		"an expired-delete-marker rule": `<Rule><ID>t</ID><Status>Enabled</Status>
			<Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration></Rule>`,
		"aborting multipart uploads": `<Rule><ID>t</ID><Status>Enabled</Status>
			<AbortIncompleteMultipartUpload><DaysAfterInitiation>7</DaysAfterInitiation></AbortIncompleteMultipartUpload></Rule>`,
		"a status that is neither": `<Rule><ID>t</ID><Status>Perhaps</Status>
			<Expiration><Days>1</Days></Expiration></Rule>`,
	}
	for name, rule := range cases {
		code := putLifecycle(t, s, "demo", "<LifecycleConfiguration>"+rule+"</LifecycleConfiguration>")
		if code != 400 {
			t.Errorf("%s was accepted with %d", name, code)
		}
	}
	// Nothing was stored by any of them.
	if rec := s3req(t, s, http.MethodGet, "/demo?lifecycle", ""); rec.Code != 404 {
		t.Errorf("a refused rule was stored anyway: %d %s", rec.Code, rec.Body)
	}
	// An empty configuration is malformed too.
	if code := putLifecycle(t, s, "demo", `<LifecycleConfiguration></LifecycleConfiguration>`); code != 400 {
		t.Errorf("an empty configuration was accepted with %d", code)
	}
}

// Noncurrent versions really expire. `now` is a parameter because S3's
// own minimum is ONE DAY, so no rule a client can write would fire while
// anybody is watching -- a test that could not ask about ten days from
// now would be asserting nothing.
func TestNoncurrentVersionsExpireOnSchedule(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	mustPutVersioning(t, s, "demo")
	if code := putLifecycle(t, s, "demo", noncurrentRules); code != 200 {
		t.Fatalf("PUT ?lifecycle = %d", code)
	}

	// Three versions under the rule's prefix, and one outside it.
	s3req(t, s, http.MethodPut, "/demo/logs/a.txt", "one")
	s3req(t, s, http.MethodPut, "/demo/logs/a.txt", "two")
	s3req(t, s, http.MethodPut, "/demo/logs/a.txt", "three")
	s3req(t, s, http.MethodPut, "/demo/keep/b.txt", "x")
	s3req(t, s, http.MethodPut, "/demo/keep/b.txt", "y")

	if n := len(mustListVersions(t, s, "/demo?versions").Versions); n != 5 {
		t.Fatalf("before the sweep: %d versions, want 5", n)
	}
	// Nothing is due yet, which is the honest answer for a rule in days.
	if removed := s.sweepLifecycle(time.Now()); removed != 0 {
		t.Errorf("the sweep removed %d versions on the day they were written", removed)
	}

	// Eight days on, the two NONCURRENT versions under logs/ are due.
	removed := s.sweepLifecycle(time.Now().Add(8 * 24 * time.Hour))
	if removed != 2 {
		t.Errorf("after eight days the sweep removed %d versions, want 2", removed)
	}
	versions := mustListVersions(t, s, "/demo?versions").Versions
	if len(versions) != 3 {
		t.Fatalf("after the sweep: %d versions, want 3", len(versions))
	}
	// The CURRENT version survives -- an Expiration rule is what removes
	// one, and this rule has none.
	rec := s3req(t, s, http.MethodGet, "/demo/logs/a.txt", "")
	if rec.Code != 200 || rec.Body.String() != "three" {
		t.Errorf("the current version was swept: %d %q", rec.Code, rec.Body)
	}
	// And the key outside the prefix is untouched, both versions of it.
	outside := 0
	for _, v := range versions {
		if strings.HasPrefix(v.Key, "keep/") {
			outside++
		}
	}
	if outside != 2 {
		t.Errorf("%d versions survive outside the rule's prefix, want 2", outside)
	}
}

// A DISABLED rule expires nothing, which is the whole point of the field.
func TestADisabledLifecycleRuleExpiresNothing(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	mustPutVersioning(t, s, "demo")
	putLifecycle(t, s, "demo", strings.Replace(noncurrentRules,
		"<Status>Enabled</Status>", "<Status>Disabled</Status>", 1))

	s3req(t, s, http.MethodPut, "/demo/logs/a.txt", "one")
	s3req(t, s, http.MethodPut, "/demo/logs/a.txt", "two")
	if removed := s.sweepLifecycle(time.Now().Add(400 * 24 * time.Hour)); removed != 0 {
		t.Errorf("a disabled rule removed %d versions", removed)
	}
}

// On an UNVERSIONED bucket an Expiration rule removes the object, because
// there is no history and a delete marker would have nothing to cover.
// On a versioned one it would mean laying a marker, which is a different
// operation and is not approximated.
func TestExpirationAppliesToAnUnversionedObject(t *testing.T) {
	rules := `<LifecycleConfiguration><Rule>
	  <ID>expire</ID><Status>Enabled</Status><Filter><Prefix></Prefix></Filter>
	  <Expiration><Days>30</Days></Expiration>
	</Rule></LifecycleConfiguration>`

	s := newTestServer(t)
	makeBucket(t, s, "plain")
	putLifecycle(t, s, "plain", rules)
	s3req(t, s, http.MethodPut, "/plain/a.txt", "x")

	if removed := s.sweepLifecycle(time.Now().Add(40 * 24 * time.Hour)); removed != 1 {
		t.Errorf("an unversioned object was not expired: removed %d", removed)
	}
	if rec := s3req(t, s, http.MethodGet, "/plain/a.txt", ""); rec.Code != 404 {
		t.Errorf("the expired object still reads: %d", rec.Code)
	}

	// The same rule on a VERSIONED bucket leaves the current version
	// alone: expiring it means a delete marker, which this does not do.
	v := newTestServer(t)
	makeBucket(t, v, "versioned")
	mustPutVersioning(t, v, "versioned")
	putLifecycle(t, v, "versioned", rules)
	s3req(t, v, http.MethodPut, "/versioned/a.txt", "x")
	if removed := v.sweepLifecycle(time.Now().Add(40 * 24 * time.Hour)); removed != 0 {
		t.Errorf("a current version on a versioned bucket was removed: %d", removed)
	}
}
