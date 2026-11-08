package gcp

import "testing"

// The paths in this table were all captured from a real client -- the
// google provider 6.50.0 or gcloud 592.0.0 -- against a logging server,
// not written from the documentation. Four services answer under /v1 and
// the only thing telling them apart is the segment after the project, so
// this is the test that stops one of them quietly swallowing another's
// paths.
func TestApiForRoutesEveryMeasuredPath(t *testing.T) {
	cases := []struct {
		path string
		want string
		why  string
	}{
		// Resource Manager
		{"/v1/projects/nimbus-local", apiResourceManager, "a project read"},
		{"/v1/projects", apiResourceManager, "the project list az login makes"},
		{"/v3/projects/nimbus-local", apiResourceManager, "the newer Resource Manager"},
		{"/v1/projects/nimbus-local:getIamPolicy", apiResourceManager, "project IAM"},

		// IAM
		{"/v1/projects/nimbus-local/serviceAccounts", apiIAM, "service account list"},
		{"/v1/projects/nimbus-local/serviceAccounts/a@b.iam.gserviceaccount.com", apiIAM, "one account"},
		{"/iam/v1/projects/nimbus-local/serviceAccounts", apiIAM, "an endpoint carrying its version"},

		// Pub/Sub -- measured: `gcloud pubsub topics publish` and the
		// provider both post the verb on the resource name.
		{"/v1/projects/nimbus-local/topics", apiPubSub, "topic list"},
		{"/v1/projects/nimbus-local/topics/t1", apiPubSub, "topic create (PUT)"},
		{"/v1/projects/nimbus-local/topics/t1:publish", apiPubSub, "publish"},
		{"/v1/projects/nimbus-local/subscriptions/s1:pull", apiPubSub, "pull"},
		{"/v1/projects/nimbus-local/subscriptions/s1:acknowledge", apiPubSub, "acknowledge"},
		{"/v1/projects/nimbus-local/snapshots", apiPubSub, "a collection nobody has built"},

		// GKE -- the location level is what identifies it.
		{"/v1/projects/nimbus-local/locations/us-central1-a/clusters", apiGKE, "cluster create"},
		{"/v1/projects/nimbus-local/locations/us-central1-a/clusters/c1", apiGKE, "cluster read"},
		{"/v1/projects/nimbus-local/locations/us-central1-a/clusters/c1/nodePools/np", apiGKE, "node pool"},
		{"/v1/projects/nimbus-local/locations/us-central1-a/operations/op-1", apiGKE, "a GKE operation"},
		{"/v1/projects/nimbus-local/zones/us-central1-a/clusters", apiGKE, "the legacy zonal spelling"},

		// Cloud Run's Admin API, and the Knative API gcloud uses instead.
		{"/v2/projects/nimbus-local/locations/us-central1/services", apiRun, "service create"},
		{"/v2/projects/nimbus-local/locations/us-central1/operations/op-1", apiRun, "a Run operation"},
		{"/apis/serving.knative.dev/v1/namespaces/nimbus-local/services", apiKnative, "gcloud run services list"},

		// The two from Milestone 9 that must not have moved.
		{"/compute/v1/projects/nimbus-local/global/networks/n1", apiCompute, "a network"},
		{"/storage/v1/b/bucket", apiStorage, "a bucket"},
		{"/upload/storage/v1/b/bucket/o", apiStorage, "an object upload"},

		// Nothing at all.
		{"/", "", "the empty path"},
		{"/nope/v1/projects/p", "", "an API nobody serves"},
	}

	for _, c := range cases {
		if got := apiFor(pathParts(c.path)); got != c.want {
			t.Errorf("apiFor(%q) = %q, want %q (%s)", c.path, got, c.want, c.why)
		}
	}
}

func TestTrimVerbSplitsGooglesColonSuffix(t *testing.T) {
	for _, c := range []struct{ in, name, verb string }{
		{"t1:publish", "t1", "publish"},
		{"s1:modifyAckDeadline", "s1", "modifyAckDeadline"},
		{"plain", "plain", ""},
	} {
		name, verb := trimVerb(c.in)
		if name != c.name || verb != c.verb {
			t.Errorf("trimVerb(%q) = %q, %q; want %q, %q", c.in, name, verb, c.name, c.verb)
		}
	}
}
