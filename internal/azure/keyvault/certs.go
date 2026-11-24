package keyvault

import (
	"net/http"
	"strings"
)

// Certificates. SCAFFOLD -- see docs/ROADMAP.md.
//
// A certificate is a three-part object: a certificate, a key and a secret,
// all under one name and all addressable separately. Only the not-found path
// exists so far, which is what lets azurerm get as far as sending its create.
func (h *Handler) serveCertificates(w http.ResponseWriter, r *http.Request, v *Vault, seg []string) string {
	name := ""
	if len(seg) > 0 {
		name = seg[0]
	}
	if r.Method == http.MethodGet && name != "" {
		WriteError(w, 404, "CertificateNotFound",
			"A certificate with (name/id) %s was not found in this key vault.", name)
		return ""
	}
	WriteError(w, 501, "NotSupported",
		"Nimbus does not implement this certificate operation yet: %s /certificates/%s",
		r.Method, strings.Join(seg, "/"))
	return "unimplemented certificate operation: " + r.Method
}
