package gcp

import (
	"sort"
	"strings"
	"testing"

	"github.com/Asere231/nimbus/internal/authz"
)

// No bundled role may grant setIamPolicy by ACCIDENT.
//
// The catalogue expresses each predefined role as the wildcard patterns it
// amounts to, and a wildcard grants more than the role it approximates. That
// is tolerable for most permissions -- a role granting one `get` too many is
// a divergence nobody is hurt by -- and it is NOT tolerable for
// `setIamPolicy`, which is the permission that hands out every other
// permission. An approximation loose about that one is a privilege escalation
// the emulator teaches as normal.
//
// `roles/compute.networkAdmin` was granting it until a test caught it, and
// docs/ROADMAP.md said the quiet part out loud: "the exclusions are added
// case by case as they are noticed, which means the ones nobody has noticed
// are still there." This replaces noticing with declaring. Every role is
// listed below as granting IAM or not, taken from Google's published
// definitions, and a role that is not listed must not grant it -- so a role
// added later is covered before anyone thinks about it.
//
// It asks through compileBinding and Applies, which is the path a REQUEST
// takes, and not through grantsPermission directly. That is not fastidiousness:
// compileBinding lowercases the patterns AND the exclusions, and
// `setIamPolicy` has capitals in it -- so reading the raw table and calling
// the matcher reports every exclusion as inert and every role as escalating.
// The first version of this test did exactly that and accused `roles/editor`
// of handing out access, which it does not. Asking the way the server asks is
// the only form that means anything.
//
// The test is two-sided on purpose. A role that grants IAM and should not is
// an escalation; a role that does not and should is a role that cannot do its
// job, which is the fault Key Vault Reader had.
var rolesThatGrantIAM = map[string]bool{
	// The basic roles. Editor is the interesting one: it is everything
	// EXCEPT handing out access, which is why it cannot be a plain
	// allow-list -- the same shape as Azure's Contributor.
	"roles/owner":  true,
	"roles/editor": false,
	"roles/viewer": false,

	// An admin of a service administers that service's IAM too.
	"roles/storage.admin":           true,
	"roles/storage.objectAdmin":     true,
	"roles/compute.admin":           true,
	"roles/pubsub.admin":            true,
	"roles/container.admin":         true,
	"roles/iam.serviceAccountAdmin": true,
	"roles/run.admin":               true,
	"roles/cloudfunctions.admin":    true,

	// And the narrower roles do not. `cloudfunctions.developer` is the one
	// to watch: it deploys and invokes and does NOT manage access, so
	// giving it the same `cloudfunctions.*` wildcard as the admin role --
	// which is exactly how it was first written -- made a deployer able to
	// hand out every permission on the function.
	"roles/storage.objectViewer":     false,
	"roles/storage.objectCreator":    false,
	"roles/compute.networkAdmin":     false,
	"roles/compute.viewer":           false,
	"roles/pubsub.publisher":         false,
	"roles/container.viewer":         false,
	"roles/run.viewer":               false,
	"roles/cloudfunctions.developer": false,
	"roles/cloudfunctions.viewer":    false,
}

// iamProbes are setIamPolicy permissions across every service in the
// catalogue. A role is asked about all of them, because a wildcard scoped to
// one service only over-grants within it.
var iamProbes = []string{
	"storage.buckets.setIamPolicy",
	"storage.objects.setIamPolicy",
	"compute.instances.setIamPolicy",
	"compute.subnetworks.setIamPolicy",
	"pubsub.topics.setIamPolicy",
	"pubsub.subscriptions.setIamPolicy",
	"container.clusters.setIamPolicy",
	"iam.serviceAccounts.setIamPolicy",
	"run.services.setIamPolicy",
	"cloudfunctions.functions.setIamPolicy",
}

func TestNoBundledRoleGrantsIAMByAccident(t *testing.T) {
	// Every role in the catalogue must have a declared expectation, or a
	// role added later would be silently unexamined -- which is the whole
	// failure this test exists to end.
	var undeclared []string
	for name := range gcpRoles {
		if _, ok := rolesThatGrantIAM[name]; !ok {
			undeclared = append(undeclared, name)
		}
	}
	sort.Strings(undeclared)
	if len(undeclared) > 0 {
		t.Errorf("these roles have no declared IAM expectation:\n  %s\n\n"+
			"add each to rolesThatGrantIAM, from Google's published definition -- a role nobody "+
			"declared is a role nobody checked", strings.Join(undeclared, "\n  "))
	}

	for name, role := range gcpRoles {
		want, declared := rolesThatGrantIAM[name]
		if !declared {
			continue
		}
		for _, probe := range iamProbes {
			got := roleGrants(name, probe)
			// A role scoped to one collection is only expected to grant IAM
			// within it; the probes outside it must always refuse.
			expected := want && roleReaches(name, probe)
			if got && !expected {
				t.Errorf("%s grants %s and should not -- setIamPolicy is the permission that "+
					"hands out every other permission, so an approximation loose about it is a "+
					"privilege escalation\n  patterns: %v\n  excluded: %v",
					name, probe, role.Permissions, role.Excluded)
			}
			if !got && expected {
				t.Errorf("%s does NOT grant %s and should; a role that cannot do its own job is "+
					"the other way for a curation to be wrong", name, probe)
			}
		}
	}
}

// roleGrants asks the question the way a request does: compile the binding,
// then ask the compiled rule.
func roleGrants(role, permission string) bool {
	rule := compileBinding("test", role, "")
	ok, err := rule.Applies(authz.Request{Action: permission, Resource: "the project"})
	if err != nil {
		return false
	}
	return ok
}

// roleReaches reports whether a role's patterns reach the probe's own
// COLLECTION, so `storage.objectAdmin` is not faulted for declining IAM on a
// bucket -- it administers objects, and bucket policy is not its business.
//
// Asked with harmless reads rather than with setIamPolicy, or this would
// restate the question it exists to scope.
func roleReaches(role, probe string) bool {
	prefix := probe[:strings.LastIndex(probe, ".")] // e.g. storage.objects
	for _, verb := range []string{"get", "list", "create", "update", "delete"} {
		if roleGrants(role, prefix+"."+verb) {
			return true
		}
	}
	return false
}
