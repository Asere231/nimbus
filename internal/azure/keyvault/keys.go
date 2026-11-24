package keyvault

import (
	"net/http"
	"strings"
)

// Keys, which are the half of Key Vault that is about CRYPTOGRAPHY rather
// than storage.
//
// A secret is a string you put in and get back. A KEY is the opposite: the
// private half never leaves, and the service performs operations with it. That
// distinction is the entire reason Key Vault and KMS exist, so the property
// worth protecting here is not confidentiality of a stored value but the fact
// that no response ever carries private material -- asserted directly by a
// test rather than left to inspection.

// serveKeys handles everything addressed at "/keys/...".
//
// NOTE: this is the measurement scaffold. azurerm checks for the presence of
// an existing key BEFORE it creates one, and aborted on the 501 that used to
// be here -- so the create was never sent and its body could not be read. A
// 404 for a missing key is what lets the provider get as far as the PUT.
func (h *Handler) serveKeys(w http.ResponseWriter, r *http.Request, v *Vault, seg []string) string {
	name := ""
	if len(seg) > 0 {
		name = seg[0]
	}

	if r.Method == http.MethodGet && name != "" && !strings.EqualFold(name, "") {
		if len(seg) > 1 && strings.EqualFold(seg[1], "versions") {
			WriteError(w, 501, "NotSupported", "Nimbus does not implement key versions yet.")
			return "unimplemented: key versions"
		}
		return h.keyNotFound(w, v, name)
	}

	WriteError(w, 501, "NotSupported", "Nimbus does not implement this key operation yet: %s /keys/%s",
		r.Method, strings.Join(seg, "/"))
	return "unimplemented key operation: " + r.Method
}

// keyNotFound is what Key Vault answers for a key that is not there.
func (h *Handler) keyNotFound(w http.ResponseWriter, v *Vault, name string) string {
	WriteError(w, 404, "KeyNotFound",
		"A key with (name/id) %s was not found in this key vault.", name)
	return ""
}

// serveCertificates handles everything addressed at "/certificates/..." other
// than the contacts list, which handler.go routes first.
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
