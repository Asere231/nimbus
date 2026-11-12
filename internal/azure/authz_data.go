package azure

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/Asere231/nimbus/internal/authz"
	"github.com/Asere231/nimbus/internal/azure/keyvault"
	"github.com/Asere231/nimbus/internal/core"
)

// Azure's other action axis.
//
// `dataActions` are not a policy attached to a resource -- they hang off
// the same caller-side role assignments everything else here does. They
// belong to this milestone anyway, because they are the reason an Azure
// learner cannot otherwise see why reading a SECRET is a different
// permission from reading the vault that holds it. Owner and Contributor
// carry `Actions: ["*"]` and no data actions at all, so a Contributor
// can delete a key vault and cannot read a secret out of it. Almost
// nobody expects that the first time.
//
// Only KEY VAULT's data plane is enforced, and the reason is a fact
// about credentials rather than a shortcut. A data action is evaluated
// against a principal, and Azure Storage's Shared Key carries none at
// all -- there is nothing in the request that says who is asking.
// Enforcing blob data actions would therefore mean requiring OAuth on
// the storage data plane, which would break `examples/terraform/
// azure-storage` and the Python SDK harness for a model nobody asked
// for. Key Vault's data plane uses a bearer token, after its own 401
// challenge, and that token already carries `oid`. docs/ROADMAP.md says
// so rather than leaving it to be discovered.

// authorizeDataAction is the hook the Key Vault data plane calls. The
// action it is given was worked out by the keyvault package, beside the
// routing it mirrors.
func (s *Server) authorizeDataAction(r *http.Request, vault, action string) *keyvault.Denial {
	if !s.cfg.EnforceIAM {
		return nil
	}

	p := s.resolvePrincipal(r)
	if p.Unrestricted {
		return nil
	}

	scope := s.vaultScope(vault)
	d := authz.Evaluate(authz.Request{
		Principal: p,
		Action:    action,
		Resource:  scope,
		// Which axis to match against. Without this a Contributor's
		// `Actions: ["*"]` would grant every data action, which is the
		// exact confusion this enforces away.
		Context: map[string]string{PlaneKey: DataPlane},
	}, s.rulesFor(p.ID))
	if d.Allowed {
		return nil
	}

	return &keyvault.Denial{
		// Key Vault's own refusal: 403 Forbidden, in its error
		// document. Not AuthorizationFailed -- that is ARM's, and a
		// data-plane client does not read ARM's shape.
		Status: http.StatusForbidden,
		Code:   "Forbidden",
		Message: fmt.Sprintf(
			"The user, group or application 'oid=%s' does not have permission to perform "+
				"action '%s' on scope '%s'. %s", p.ID, action, scope, d.Reason),
		Note: "denied: " + action + " for " + p.ID + " -- " + d.Reason,
	}
}

// vaultScope is the ARM id a vault's data actions are evaluated at, so
// that an assignment made at the subscription or the resource group
// inherits down to the data plane exactly as it does to the control
// plane.
//
// Found by NAME, because that is all the data plane has: a request
// arrives on `{vault}.{suffix}` and carries no subscription or resource
// group anywhere. A vault that ARM does not know about gets a scope that
// matches nothing, which refuses -- the right answer, since the data
// plane would 404 a moment later anyway.
func (s *Server) vaultScope(vault string) string {
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return eq(r.Type, typeKeyVault) && eq(r.Name, vault)
	}) {
		return res.ID
	}
	return "/vaults/" + strings.ToLower(vault)
}
