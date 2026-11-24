package keyvault

import "time"

// What the visualizer is allowed to see.
//
// Names, versions and timestamps are listed freely; a value is only ever
// returned by ReadSecret, which the inspector calls when somebody presses
// reveal. Keeping the two apart is the whole point: the list is drawn every
// time a vault is selected, so anything in it is on screen -- and in any
// screenshot -- without being asked for.
//
// A KEY has nothing to reveal, which is the service's whole point, and a
// CERTIFICATE's public half is not a secret either. Only a secret gets a
// reveal button, and only a secret has an endpoint that could serve one.

// ObjectSummary is one vault object's metadata, without any value.
type ObjectSummary struct {
	// Kind is "secret", "key" or "certificate". The inspector needs it to
	// decide what may be revealed -- and before this existed, a vault holding
	// only keys reported itself EMPTY.
	Kind        string
	Name        string
	Version     string
	Versions    int
	ContentType string
	Enabled     bool
	Updated     time.Time
	// Deleted marks an object in the vault's recycle bin, which is worth
	// showing: it still holds its name.
	Deleted bool
}

// ListVaultObjects returns a vault's secrets, keys and certificates, live ones
// first and then the recycle bin. Nil if there is no such vault.
//
// Named for what it does. It used to be ListSecrets and used to list only
// secrets; keeping that name once keys and certificates existed would have
// been exactly the kind of claim this project keeps finding stale.
func (h *Handler) ListVaultObjects(vault string) []ObjectSummary {
	v := h.store.Vault(vault)
	if v == nil {
		return nil
	}
	out := []ObjectSummary{}
	for _, sec := range v.List() {
		out = append(out, summariseSecret(sec, false))
	}
	for _, k := range v.ListKeys() {
		out = append(out, summariseKey(k, false))
	}
	for _, c := range v.ListCertificates() {
		out = append(out, summariseCert(c, false))
	}
	for _, sec := range v.ListDeleted() {
		out = append(out, summariseSecret(sec, true))
	}
	for _, k := range v.ListDeletedKeys() {
		out = append(out, summariseKey(k, true))
	}
	for _, c := range v.ListDeletedCertificates() {
		out = append(out, summariseCert(c, true))
	}
	return out
}

func summariseSecret(sec *Secret, deleted bool) ObjectSummary {
	s := ObjectSummary{Kind: "secret", Name: sec.Name, Versions: len(sec.Versions), Deleted: deleted}
	if ver := sec.Latest(); ver != nil {
		s.Version, s.ContentType, s.Enabled, s.Updated = ver.Version, ver.ContentType, ver.Enabled, ver.Updated
	}
	return s
}

func summariseKey(k *Key, deleted bool) ObjectSummary {
	s := ObjectSummary{Kind: "key", Name: k.Name, Versions: len(k.Versions), Deleted: deleted}
	if ver := k.Latest(); ver != nil {
		// The key TYPE stands in for a content type: it is the one thing
		// about a key worth seeing at a glance, and it is not private.
		s.ContentType = ver.Kty
		if ver.Curve != "" {
			s.ContentType += " " + ver.Curve
		}
		s.Version, s.Enabled, s.Updated = ver.Version, ver.Enabled, ver.Updated
	}
	return s
}

func summariseCert(c *Certificate, deleted bool) ObjectSummary {
	s := ObjectSummary{Kind: "certificate", Name: c.Name, Versions: len(c.Versions), Deleted: deleted}
	if ver := c.Latest(); ver != nil {
		s.Version, s.Enabled, s.Updated = ver.Version, ver.Enabled, ver.Updated
	}
	return s
}

// ReadSecret returns the current value of one secret.
func (h *Handler) ReadSecret(vault, name string) (string, bool) {
	v := h.store.Vault(vault)
	if v == nil {
		return "", false
	}
	sec := v.Secret(name)
	if sec == nil {
		sec = v.DeletedSecret(name)
	}
	if sec == nil {
		return "", false
	}
	ver := sec.Latest()
	if ver == nil {
		return "", false
	}
	return ver.Value, true
}
