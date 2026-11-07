package gcp

import "strings"

// Identity in GCP is a self link: the full URL the resource answers on.
//
// That is what goes in the store as Resource.ID, which is why every link
// here is built from Config.PublicURL -- follow one and you come back to
// this Nimbus. Two consequences worth stating:
//
//   - A self link embeds the port, so the ids in a persisted state file are
//     only valid for the port that wrote them. The same is true of Azure's
//     storage suffix and ACR's loginServer; it is the price of being
//     addressable.
//   - The store compares ids case-insensitively. Every GCP name here is
//     lower-case by the API's own validation, so nothing collides -- but an
//     object NAME is case-sensitive, which is one reason objects are not
//     resources in the store (the other being that there can be thousands).
//
// The TYPE strings follow the shape the other clouds use: a provider and a
// path. GCP has no canonical type name of its own, so Nimbus uses the one
// in Google's own asset inventory: compute.googleapis.com/Network.

const (
	TypeProject        = "cloudresourcemanager.googleapis.com/Project"
	TypeNetwork        = "compute.googleapis.com/Network"
	TypeSubnetwork     = "compute.googleapis.com/Subnetwork"
	TypeFirewall       = "compute.googleapis.com/Firewall"
	TypeRoute          = "compute.googleapis.com/Route"
	TypeRouter         = "compute.googleapis.com/Router"
	TypeServiceAccount = "iam.googleapis.com/ServiceAccount"
	TypeBucket         = "storage.googleapis.com/Bucket"
)

// base is PublicURL without a trailing slash, so joining never doubles one.
// Azure learned this the hard way: a `//` in resourceManager made the SDK's
// pollers compute the wrong path and delete polling never ended.
func (s *Server) base() string {
	return strings.TrimRight(s.cfg.PublicURL, "/")
}

// computeLink builds a Compute self link: the API has exactly three scopes
// -- global, a region, or a zone -- and everything lives in one of them.
func (s *Server) computeLink(scope, kind, name string) string {
	return s.base() + "/compute/v1/projects/" + s.cfg.ProjectID + "/" + scope + "/" + kind + "/" + name
}

func (s *Server) globalLink(kind, name string) string { return s.computeLink("global", kind, name) }

func (s *Server) regionLink(region, kind, name string) string {
	return s.computeLink("regions/"+region, kind, name)
}

// projectSelfLink is the project's own id in the store, and the parent of
// everything else in this cloud.
func (s *Server) projectSelfLink() string {
	return s.base() + "/v1/projects/" + s.cfg.ProjectID
}

// computeProjectLink is the project as COMPUTE sees it, which is a
// different URL from the Resource Manager one for the same thing. The
// provider fetches it to turn a project number back into a project id.
func (s *Server) computeProjectLink() string {
	return s.base() + "/compute/v1/projects/" + s.cfg.ProjectID
}

func (s *Server) bucketLink(name string) string {
	return s.base() + "/storage/v1/b/" + name
}

// serviceAccountName is the "resource name" IAM uses, which is a path
// rather than a URL -- IAM is a newer API and does not use self links.
func (s *Server) serviceAccountName(email string) string {
	return "projects/" + s.cfg.ProjectID + "/serviceAccounts/" + email
}

// serviceAccountEmail is DERIVED from the account id and the project, never
// generated. The provider reads it back on every refresh and uses it as the
// resource's identity; a value that differed between reads would be a
// permanent diff -- the rule that has cost this project a day per cloud.
func (s *Server) serviceAccountEmail(accountID string) string {
	return accountID + "@" + s.cfg.ProjectID + ".iam.gserviceaccount.com"
}

// relativeName turns a self link or a partial URL into the
// `projects/p/global/networks/n` form the provider sends in request bodies
// and compares against. A client may send any of three spellings for the
// same thing -- the bare name, this relative form, or the full URL -- so
// anything that matches them has to reduce them first.
func relativeName(ref string) string {
	if i := strings.Index(ref, "/projects/"); i >= 0 {
		return strings.TrimPrefix(ref[i:], "/")
	}
	return strings.TrimPrefix(ref, "/")
}
