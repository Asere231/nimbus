package keyvault

import (
	"net/http"
	"strings"
)

// Which DATA ACTION a request needs, derived beside the routing it mirrors.
//
// Separate from handler.go because the two together ran past this project's
// 500-line limit once keys and certificates were added -- and because this is
// the one function in the package whose job is to agree with another: a
// classifier that disagrees with its router authorizes one thing and serves
// another, which is the rule the AWS authorization work wrote down.

// DataActionFor names the Azure RBAC data action a request needs, or ""
// for a path that grants nothing and can be left to answer for itself.
//
// It lives HERE, beside the routing it mirrors, for the reason AWS's
// classifier records: if the two disagree, a request is authorized as
// one thing and served as another. Every path Serve dispatches has an
// action; the 501 paths do not, because refusing a path that does
// nothing with a 403 tells a client less than the 501 does.
//
// The distinction the whole thing exists to draw is `getSecret` versus
// `readMetadata`. Key Vault Reader holds the second and not the first,
// which is what lets a role see that a secret called `db-password`
// exists without seeing what it is.
func DataActionFor(r *http.Request, path string) string {
	seg := splitPath(path)
	if len(seg) == 0 {
		return ""
	}
	const prefix = "Microsoft.KeyVault/vaults/"
	named := len(seg) > 1 && seg[1] != ""

	switch strings.ToLower(seg[0]) {
	case "secrets":
		switch r.Method {
		case http.MethodGet:
			// A list, or a secret's versions, is metadata. Only
			// fetching one secret is getSecret.
			if !named || (len(seg) > 2 && strings.EqualFold(seg[2], "versions")) {
				return prefix + "secrets/readMetadata/action"
			}
			return prefix + "secrets/getSecret/action"
		case http.MethodPut, http.MethodPatch:
			return prefix + "secrets/setSecret/action"
		case http.MethodDelete:
			return prefix + "secrets/delete"
		case http.MethodPost:
			if len(seg) > 2 && strings.EqualFold(seg[2], "backup") {
				return prefix + "secrets/backup/action"
			}
			return prefix + "secrets/restore/action"
		}
	case "deletedsecrets":
		switch {
		case r.Method == http.MethodDelete:
			return prefix + "secrets/purge"
		case r.Method == http.MethodPost:
			return prefix + "secrets/recover/action"
		case r.Method == http.MethodGet:
			return prefix + "secrets/readMetadata/action"
		}
	case "keys":
		// A key needs no readMetadata/getSecret split, and the reason is
		// the service's whole point: there is no "value" to withhold,
		// because the private half is never returned to anybody. So a
		// read is a read, and Key Vault Reader's `vaults/*/read` covers
		// listing keys with no entry of its own -- unlike secrets, where
		// the split exists precisely because a secret's VALUE is
		// readable.
		switch r.Method {
		case http.MethodGet:
			return prefix + "keys/read"
		case http.MethodPut, http.MethodPatch:
			return prefix + "keys/update/action"
		case http.MethodDelete:
			return prefix + "keys/delete"
		case http.MethodPost:
			if len(seg) > 2 {
				switch strings.ToLower(seg[len(seg)-1]) {
				case "create":
					return prefix + "keys/create"
				case "sign":
					return prefix + "keys/sign/action"
				case "verify":
					return prefix + "keys/verify/action"
				case "encrypt":
					return prefix + "keys/encrypt/action"
				case "decrypt":
					return prefix + "keys/decrypt/action"
				case "wrapkey":
					return prefix + "keys/wrap/action"
				case "unwrapkey":
					return prefix + "keys/unwrap/action"
				case "backup":
					return prefix + "keys/backup/action"
				}
			}
			return prefix + "keys/create"
		}
	case "deletedkeys":
		switch r.Method {
		case http.MethodDelete:
			return prefix + "keys/purge"
		case http.MethodPost:
			return prefix + "keys/recover/action"
		case http.MethodGet:
			return prefix + "keys/read"
		}
	case "certificates":
		// CONTACTS is the one certificate path that must stay permissive.
		// azurerm reads it straight after every read of the vault
		// RESOURCE, and a 403 there crashes the provider's Key Vault
		// client -- so it is a plain read action, deliberately no
		// stricter than the vault itself.
		//
		// Everything else under /certificates is a real certificate
		// operation and must NOT be authorized as a contacts read. That
		// was the behaviour until certificates existed: this case
		// returned certificatecontacts/read for every path beneath it, so
		// the moment a create was implemented it would have been
		// permitted by whatever could read a contact list. Latent for
		// four milestones because nothing else was routed here.
		if named && strings.EqualFold(seg[1], "contacts") {
			return prefix + "certificatecontacts/read"
		}
		if !named {
			if r.Method == http.MethodGet {
				return prefix + "certificates/read"
			}
			return prefix + "certificates/create"
		}
		switch r.Method {
		case http.MethodGet:
			return prefix + "certificates/read"
		case http.MethodPatch, http.MethodPut:
			return prefix + "certificates/update/action"
		case http.MethodDelete:
			return prefix + "certificates/delete"
		case http.MethodPost:
			return prefix + "certificates/create"
		}
	case "deletedcertificates":
		switch r.Method {
		case http.MethodDelete:
			return prefix + "certificates/purge"
		case http.MethodPost:
			return prefix + "certificates/recover/action"
		case http.MethodGet:
			return prefix + "certificates/read"
		}
	}
	return ""
}

// DataActions is every data action this data plane can require.
//
// Exported so that the explainer can tell a data action from a
// control-plane one EXACTLY, rather than by guessing from its shape --
// `Microsoft.KeyVault/vaults/secrets/delete` is a data action while
// `Microsoft.KeyVault/vaults/delete` is not, and no pattern separates
// those two reliably. Deriving the set from DataActionFor, the same
// function the enforcement point uses, means the explanation cannot
// classify an action differently from the way it is enforced.
func DataActions() []string {
	seen := map[string]bool{}
	var out []string
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/secrets"},
		{http.MethodGet, "/secrets/x"},
		{http.MethodPut, "/secrets/x"},
		{http.MethodDelete, "/secrets/x"},
		{http.MethodPost, "/secrets/x/backup"},
		{http.MethodPost, "/secrets/x/restore"},
		{http.MethodDelete, "/deletedsecrets/x"},
		{http.MethodPost, "/deletedsecrets/x/recover"},
		{http.MethodGet, "/certificates/contacts"},
	} {
		r, _ := http.NewRequest(probe.method, probe.path, nil)
		if a := DataActionFor(r, probe.path); a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}
