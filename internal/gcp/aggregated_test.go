package gcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// `gcloud compute networks subnets list` reads the AGGREGATED collection.
//
// Milestone 10 found that gcloud and the provider use different paths for
// the same thing, and handled it for instances: the provider reads the zonal
// collection while gcloud reads `aggregated/instances`, a MAP keyed by scope
// rather than a list. What it did not do was generalise, so every other
// aggregated collection fell through to "Nimbus does not implement the
// Compute collection \"aggregated\" yet" -- naming `aggregated` as though it
// were the collection, which is itself a clue that the path was never read
// properly.
//
// Found on Windows with gcloud 593.0.0, and it is NOT a Windows bug: the
// path has nothing platform-specific in it. The roadmap claimed `networks
// subnets list` had been verified, which is the kind of claim this project
// keeps learning to distrust -- either gcloud changed under it or it was
// never true.
func TestGcloudListsSubnetworksThroughTheAggregatedCollection(t *testing.T) {
	s := newTestServer(t)

	// A network and a subnetwork, created the way the provider does.
	must(t, s, http.MethodPost, "/compute/v1/projects/nimbus-local/global/networks", `{
		"name": "nimbus-net", "autoCreateSubnetworks": false}`)
	must(t, s, http.MethodPost, "/compute/v1/projects/nimbus-local/regions/us-central1/subnetworks", `{
		"name": "app", "ipCidrRange": "10.0.1.0/24",
		"network": "projects/nimbus-local/global/networks/nimbus-net"}`)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/compute/v1/projects/nimbus-local/aggregated/subnetworks", nil)
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("aggregated/subnetworks answered %d: %s", rec.Code, rec.Body)
	}

	var doc struct {
		Kind  string `json:"kind"`
		Items map[string]struct {
			Subnetworks []map[string]any `json:"subnetworks"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decoding the response: %v\n%s", err, rec.Body)
	}

	// The kind is per collection, and a client parsing with the wrong one
	// sees an empty document rather than an error.
	if doc.Kind != "compute#subnetworkAggregatedList" {
		t.Errorf("kind = %q, want compute#subnetworkAggregatedList", doc.Kind)
	}
	// Keyed by SCOPE, and a subnetwork is regional -- an instance's scope is
	// a zone, so a single hard-coded prefix would be wrong for one of them.
	scope, ok := doc.Items["regions/us-central1"]
	if !ok {
		t.Fatalf("no regions/us-central1 scope in items: %v", doc.Items)
	}
	if len(scope.Subnetworks) != 1 {
		t.Fatalf("scope holds %d subnetworks, want 1", len(scope.Subnetworks))
	}
	if name := scope.Subnetworks[0]["name"]; name != "app" {
		t.Errorf("subnetwork name = %v, want app", name)
	}
}

// An empty aggregated collection answers a warning rather than an empty map,
// which is what real Compute does and what gcloud prints as "Listed 0 items".
// An empty `items` would have gcloud report nothing at all, which is the same
// output for "none exist" and "this is not implemented".
func TestAnEmptyAggregatedCollectionCarriesAWarning(t *testing.T) {
	s := newTestServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/compute/v1/projects/nimbus-local/aggregated/subnetworks", nil)
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", rec.Code, rec.Body)
	}
	var doc struct {
		Items map[string]struct {
			Warning map[string]any `json:"warning"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Items) == 0 {
		t.Fatal("items is empty; want one scope carrying a warning")
	}
	for scope, entry := range doc.Items {
		if entry.Warning["code"] != "NO_RESULTS_ON_PAGE" {
			t.Errorf("scope %q has no NO_RESULTS_ON_PAGE warning: %v", scope, entry.Warning)
		}
	}
}
