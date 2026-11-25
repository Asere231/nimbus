package ui

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// unmappedOnPurpose is every resource type the examples use that has NO
// Nimbus resource behind it, with the reason.
//
// Two kinds, and the distinction is the point:
//
//   - A SUB-RESOURCE Terraform manages separately but Nimbus stores as a
//     field of its parent. `aws_s3_bucket_versioning` is a setting on a
//     bucket; there is no versioning resource to draw, and inventing one
//     would put a box on the graph that no API ever returns.
//   - A resource belonging to ANOTHER PROVIDER entirely. The kubernetes
//     provider's objects reach the graph through the k3s watcher rather
//     than through a plan, and `local_file` and `terraform_data` are not
//     cloud resources at all.
//
// A type in neither this list nor a mapping table is a gap somebody has to
// decide about, which is what the test below is for.
var unmappedOnPurpose = map[string]string{
	// Fields of their parent in Nimbus.
	"aws_iam_role_policy":                               "an inline policy, stored on the role",
	"aws_iam_role_policy_attachment":                    "an attachment, stored on the role",
	"aws_route_table_association":                       "an association, stored on the route table",
	"aws_s3_bucket_policy":                              "a bucket's ?policy sub-resource",
	"aws_s3_bucket_public_access_block":                 "a bucket's ?publicAccessBlock sub-resource",
	"aws_s3_bucket_versioning":                          "a bucket's ?versioning sub-resource",
	"aws_s3_object":                                     "object bytes, not a control-plane resource",
	"azurerm_subnet_network_security_group_association": "a reference stored on the subnet",
	"google_project_iam_member":                         "a binding, stored on the project's IAM policy",
	"google_storage_bucket_iam_member":                  "a binding, stored on the bucket's IAM policy",
	"google_cloud_run_v2_service_iam_member":            "a binding, stored on the service's IAM policy",
	"google_storage_bucket_object":                      "object bytes, not a control-plane resource",

	// Other providers entirely.
	"kubernetes_deployment": "the kubernetes provider; pods reach the graph through the k3s watcher",
	"kubernetes_secret":     "the kubernetes provider; not an ARM or AWS resource",
	"local_file":            "a file on disk, not a cloud resource",
	"terraform_data":        "Terraform's own resource, with nothing behind it",
}

var resourceDecl = regexp.MustCompile(`(?m)^resource\s+"([a-z0-9_]+)"`)

// Every resource type the repo's own examples use is either MAPPED in the
// plan overlay or listed above with a reason.
//
// This exists because the tables' comment said they were "deliberately
// partial: add a line when an example needs one", and that is how a table
// drifts rather than how it stays complete. Measured when this test was
// written: every one of the fourteen google_* types was missing, so the
// overlay had never placed a change on a GCP graph at all, and eight AWS
// and six Azure types the examples use had no line either. An unmapped
// change still DRAWS, labelled with its Terraform address -- which is why
// nothing failed and nobody noticed.
//
// Reading the examples' own source in a test is what
// TestEveryModuleImportResolves and TestEverySpecTypeHasABadge already do,
// for the same reason: a claim with no test behind it will drift.
func TestEveryExampleTypeIsAccountedForInThePlanOverlay(t *testing.T) {
	types := exampleResourceTypes(t)
	if len(types) < 20 {
		// The glob found nothing useful -- a moved directory would
		// otherwise make this test pass by asserting about an empty set.
		t.Fatalf("only found %d resource types in the examples; the scan is broken", len(types))
	}

	var missing []string
	for _, typ := range types {
		if armType(typ) != "" {
			continue
		}
		if _, deliberate := unmappedOnPurpose[typ]; deliberate {
			continue
		}
		missing = append(missing, typ)
	}
	sort.Strings(missing)
	for _, typ := range missing {
		t.Errorf("%s is used by an example but the plan overlay cannot place it: "+
			"add it to armTypes/awsTypes/googleTypes in plan.go, or to "+
			"unmappedOnPurpose in this test with the reason", typ)
	}
}

// And the other direction: a reason listed for a type no example uses any
// more is a stale note, which is the same defect from the other side.
func TestNoStaleReasonsInTheUnmappedList(t *testing.T) {
	used := map[string]bool{}
	for _, typ := range exampleResourceTypes(t) {
		used[typ] = true
	}
	for typ := range unmappedOnPurpose {
		if !used[typ] {
			t.Errorf("unmappedOnPurpose lists %s, which no example uses any more", typ)
		}
	}
}

// A type cannot be in both a mapping table and the deliberate-omission
// list: one of the two would be a lie about what the graph holds.
func TestNoTypeIsBothMappedAndOmitted(t *testing.T) {
	for typ := range unmappedOnPurpose {
		if got := armType(typ); got != "" {
			t.Errorf("%s is listed as having no Nimbus resource but maps to %q", typ, got)
		}
	}
}

func exampleResourceTypes(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..", "examples", "terraform")
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".tf") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, m := range resourceDecl.FindAllStringSubmatch(string(body), -1) {
			seen[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	out := make([]string, 0, len(seen))
	for typ := range seen {
		out = append(out, typ)
	}
	sort.Strings(out)
	return out
}
