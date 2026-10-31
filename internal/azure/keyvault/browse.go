package keyvault

import "time"

// What the visualizer is allowed to see.
//
// Names, versions and timestamps are listed freely; a value is only ever
// returned by ReadSecret, which the inspector calls when somebody presses
// reveal. Keeping the two apart is the whole point: the list is drawn every
// time a vault is selected, so anything in it is on screen -- and in any
// screenshot -- without being asked for.

// SecretSummary is one secret's metadata, without its value.
type SecretSummary struct {
	Name        string
	Version     string
	Versions    int
	ContentType string
	Enabled     bool
	Updated     time.Time
	// Deleted marks a secret in the vault's recycle bin, which is worth
	// showing: it still holds its name.
	Deleted bool
}

// ListSecrets returns a vault's secrets, live ones first and then the recycle
// bin. Nil if there is no such vault.
func (h *Handler) ListSecrets(vault string) []SecretSummary {
	v := h.store.Vault(vault)
	if v == nil {
		return nil
	}
	out := []SecretSummary{}
	for _, sec := range v.List() {
		out = append(out, summarise(sec, false))
	}
	for _, sec := range v.ListDeleted() {
		out = append(out, summarise(sec, true))
	}
	return out
}

func summarise(sec *Secret, deleted bool) SecretSummary {
	s := SecretSummary{Name: sec.Name, Versions: len(sec.Versions), Deleted: deleted}
	if ver := sec.Latest(); ver != nil {
		s.Version, s.ContentType, s.Enabled, s.Updated = ver.Version, ver.ContentType, ver.Enabled, ver.Updated
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
