package gcp

import "strings"

// Which API a path belongs to.
//
// This started as two `if`s in server.go and is now a table, because the
// `/v1` prefix has stopped meaning one thing. Measured against the google
// provider 6.50.0 and gcloud 592.0.0, FOUR services answer under it:
//
//	/v1/projects/{p}                                    Resource Manager
//	/v1/projects/{p}/serviceAccounts/{sa}               IAM
//	/v1/projects/{p}/topics/{t}                         Pub/Sub
//	/v1/projects/{p}/locations/{loc}/clusters/{c}       GKE
//
// They are told apart by the segment after the project -- there is nothing
// in the prefix to go on. That is worth a table and a test rather than a
// growing chain of conditions: a path routed to the wrong service answers
// a 404 that names the wrong API, which sends the next person reading the
// activity feed in entirely the wrong direction.
//
// Two notes on how clients build these URLs, both measured and both
// surprising:
//
//   - `container_custom_endpoint` (and gcloud's
//     api_endpoint_overrides/container) must be the BARE ORIGIN. The
//     provider and gcloud both append `v1/` themselves, so an endpoint
//     ending in `/v1/` produces `/v1/v1/projects/...`. This is exactly the
//     trap `iam_custom_endpoint` set in Milestone 9, one service later --
//     the version belongs to the endpoint for some services and to the
//     client for others, and nothing announces which.
//   - Cloud Run's Admin API is `/v2`, and `gcloud run` does not use it at
//     all; see run.go.
const (
	apiCompute         = "compute"
	apiStorage         = "storage"
	apiIAM             = "iam"
	apiResourceManager = "resourcemanager"
	apiGKE             = "gke"
	apiPubSub          = "pubsub"
	apiRun             = "run"
	apiKnative         = "knative"
)

// pubsubCollections are the segments that mean Pub/Sub when they follow a
// project. Snapshots and schemas are here so they route to the service
// that will answer "not implemented" for them, rather than to Resource
// Manager, which would answer "no such project field".
var pubsubCollections = []string{"topics", "subscriptions", "snapshots", "schemas"}

// apiFor returns which service a path's segments belong to, or "" for a
// path nothing serves. It is a pure function of the path so it can be
// tested exhaustively, which router_test.go does.
func apiFor(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	switch parts[0] {
	case "compute":
		return apiCompute
	case "storage":
		return apiStorage
	case "upload":
		// Uploads live under their own prefix: /upload/storage/v1/b/{b}/o.
		if len(parts) > 1 && parts[1] == "storage" {
			return apiStorage
		}
		return ""
	case "iam":
		// An iam_custom_endpoint that DID carry its version.
		return apiIAM
	case "apis":
		// The Knative serving API, which is what `gcloud run` speaks.
		if len(parts) > 1 && parts[1] == "serving.knative.dev" {
			return apiKnative
		}
		return ""
	case "v2":
		return apiRun
	case "v3":
		return apiResourceManager
	case "v1", "v1beta1":
		return apiForV1(parts)
	}
	return ""
}

// apiForV1 disambiguates the crowded prefix.
func apiForV1(parts []string) string {
	// Everything here is /v1/projects/{project}/...; a shorter path is
	// Resource Manager's own (/v1/projects, a list).
	if len(parts) < 4 || parts[1] != "projects" {
		return apiResourceManager
	}
	switch rest := parts[3]; {
	case rest == "serviceAccounts":
		return apiIAM
	case contains(pubsubCollections, rest):
		return apiPubSub
	case rest == "locations" || rest == "zones":
		// GKE is the only /v1 service with a location level. Its
		// operations live beside its clusters, which is why the check is
		// on the level and not on the collection name.
		return apiGKE
	}
	return apiResourceManager
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// trimVerb splits Google's `:verb` suffix off the last path segment:
// `topics/t:publish` is the topic `t` and the verb `publish`. The verb is
// part of the path rather than a parameter, so a client's URL carries it
// on the resource name itself.
func trimVerb(segment string) (name, verb string) {
	if i := strings.Index(segment, ":"); i >= 0 {
		return segment[:i], segment[i+1:]
	}
	return segment, ""
}
