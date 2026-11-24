package keyvault

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"fmt"
	"sort"
	"strings"
	"time"
)

// A vault's KEYS, kept apart from store.go because the two together ran past
// this project's 500-line limit -- and because a key is a different kind of
// thing from a secret despite sharing its shape: a secret is a value you get
// back, a key is material that stays here and is USED on your behalf.

// Key is a vault key, and the shape mirrors Secret deliberately: Key Vault
// never overwrites either of them, so both are a name with an ordered list of
// versions and the same soft-delete fields. Reusing the shape means the
// recycle-bin machinery proven for secrets works here unchanged.
type Key struct {
	Name     string
	Versions []*KeyVersion
	// Deleted is set when the key is in the vault's recycle bin.
	Deleted      time.Time
	RecoveryID   string
	ScheduledFor time.Time
}

// KeyVersion is one version of a key.
//
// The PRIVATE half lives here and is the one thing in this package that must
// never reach a response. It is held as a crypto key rather than bytes so that
// nothing has to re-parse it per operation, and so that the type system makes
// the RSA and EC cases distinct instead of leaving a tagged blob to be
// interpreted.
type KeyVersion struct {
	Version string
	Kty     string   // "RSA" or "EC"
	Curve   string   // EC only: "P-256", "P-384", "P-521"
	Ops     []string // key_ops, as the client declared them

	RSA *rsa.PrivateKey
	EC  *ecdsa.PrivateKey

	Tags      map[string]string
	Enabled   bool
	NotBefore *time.Time
	Expires   *time.Time
	Created   time.Time
	Updated   time.Time
}

// Public returns the public half, which is all a response may carry.
func (k *KeyVersion) Public() crypto.PublicKey {
	switch {
	case k.RSA != nil:
		return &k.RSA.PublicKey
	case k.EC != nil:
		return &k.EC.PublicKey
	}
	return nil
}

// Latest returns the newest version of a key, or nil.
func (k *Key) Latest() *KeyVersion {
	if len(k.Versions) == 0 {
		return nil
	}
	return k.Versions[len(k.Versions)-1]
}

// Version returns one version of a key by id, or nil.
func (k *Key) Version(id string) *KeyVersion {
	if id == "" {
		return k.Latest()
	}
	for _, v := range k.Versions {
		if strings.EqualFold(v.Version, id) {
			return v
		}
	}
	return nil
}

// CreateKey generates a new version of a key.
//
// The key material is GENERATED here and stored, never recomputed. That is
// not a breach of "derive, do not generate": that rule is about a value a
// client COMPARES between reads, and a stored key agrees with itself. What
// matters is that the JWK comes back byte-identical every time, which it does
// because the same key is serialised each read -- regenerating per read would
// make every plan want a change.
func (v *Vault) CreateKey(name, kty, curve string, size int, ops []string, tags map[string]string, attrs Attributes) (*KeyVersion, error) {
	ver := &KeyVersion{
		Version: newVersion(),
		Kty:     kty,
		Curve:   curve,
		Ops:     ops,
		Tags:    tags,
		Enabled: true,
		Created: time.Now().UTC(),
		Updated: time.Now().UTC(),
	}
	switch kty {
	case "RSA":
		k, err := rsa.GenerateKey(rand.Reader, size)
		if err != nil {
			return nil, err
		}
		ver.RSA = k
	case "EC":
		c, err := curveFor(curve)
		if err != nil {
			return nil, err
		}
		k, err := ecdsa.GenerateKey(c, rand.Reader)
		if err != nil {
			return nil, err
		}
		ver.EC = k
	}
	applyAttrs(ver, attrs)

	v.mu.Lock()
	defer v.mu.Unlock()
	k := v.Keys[strings.ToLower(name)]
	if k == nil {
		k = &Key{Name: name}
		v.Keys[strings.ToLower(name)] = k
	}
	k.Versions = append(k.Versions, ver)
	return ver, nil
}

// AddKeyVersion stores a version whose material was made elsewhere -- the
// path a CERTIFICATE uses, since a certificate's key is generated alongside
// its x509 material rather than on its own.
func (v *Vault) AddKeyVersion(name string, ver *KeyVersion) {
	v.mu.Lock()
	defer v.mu.Unlock()
	k := v.Keys[strings.ToLower(name)]
	if k == nil {
		k = &Key{Name: name}
		v.Keys[strings.ToLower(name)] = k
	}
	k.Versions = append(k.Versions, ver)
}

func applyAttrs(ver *KeyVersion, attrs Attributes) {
	if attrs.Enabled != nil {
		ver.Enabled = *attrs.Enabled
	}
	ver.NotBefore, ver.Expires = attrs.NotBefore, attrs.Expires
}

// Key returns a live key, or nil.
func (v *Vault) Key(name string) *Key {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.Keys[strings.ToLower(name)]
}

// DeletedKey returns a key from the recycle bin, or nil.
func (v *Vault) DeletedKey(name string) *Key {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.DeletedKeys[strings.ToLower(name)]
}

// ListKeys returns the live keys, by name.
func (v *Vault) ListKeys() []*Key {
	v.mu.Lock()
	defer v.mu.Unlock()
	return sortedKeys(v.Keys)
}

// ListDeletedKeys returns the recycle bin, by name.
func (v *Vault) ListDeletedKeys() []*Key {
	v.mu.Lock()
	defer v.mu.Unlock()
	return sortedKeys(v.DeletedKeys)
}

func sortedKeys(m map[string]*Key) []*Key {
	out := make([]*Key, 0, len(m))
	for _, k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// DeleteKey moves a key to the recycle bin and returns it, or nil if there
// was no such key.
func (v *Vault) DeleteKey(name string, retention time.Duration) *Key {
	v.mu.Lock()
	defer v.mu.Unlock()
	k := v.Keys[strings.ToLower(name)]
	if k == nil {
		return nil
	}
	delete(v.Keys, strings.ToLower(name))
	k.Deleted = time.Now().UTC()
	k.ScheduledFor = k.Deleted.Add(retention)
	v.DeletedKeys[strings.ToLower(name)] = k
	return k
}

// RecoverKey brings a key back out of the recycle bin.
func (v *Vault) RecoverKey(name string) *Key {
	v.mu.Lock()
	defer v.mu.Unlock()
	k := v.DeletedKeys[strings.ToLower(name)]
	if k == nil {
		return nil
	}
	delete(v.DeletedKeys, strings.ToLower(name))
	k.Deleted = time.Time{}
	k.ScheduledFor = time.Time{}
	v.Keys[strings.ToLower(name)] = k
	return k
}

// PurgeKey removes a key from the recycle bin for good.
func (v *Vault) PurgeKey(name string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.DeletedKeys[strings.ToLower(name)]; !ok {
		return false
	}
	delete(v.DeletedKeys, strings.ToLower(name))
	return true
}

// UpdateKey changes a version's attributes, ops or tags in place.
func (v *Vault) UpdateKey(name, version string, ops []string, tags map[string]string, attrs Attributes) *KeyVersion {
	v.mu.Lock()
	defer v.mu.Unlock()
	k := v.Keys[strings.ToLower(name)]
	if k == nil {
		return nil
	}
	ver := k.Version(version)
	if ver == nil {
		return nil
	}
	if ops != nil {
		ver.Ops = ops
	}
	if tags != nil {
		ver.Tags = tags
	}
	applyAttrs(ver, attrs)
	ver.Updated = time.Now().UTC()
	return ver
}

// curveFor maps Azure's curve names to the stdlib's curves.
//
// P-256K (secp256k1) is deliberately absent: it is a real Azure curve and the
// Go standard library does not implement it, and the stdlib-only rule is not
// negotiable. Refused by name rather than silently substituted with P-256,
// which would produce signatures nothing could verify.
func curveFor(name string) (elliptic.Curve, error) {
	switch name {
	case "P-256":
		return elliptic.P256(), nil
	case "P-384":
		return elliptic.P384(), nil
	case "P-521":
		return elliptic.P521(), nil
	case "P-256K", "SECP256K1":
		return nil, fmt.Errorf("curve %s is not implemented: the Go standard library has no secp256k1, "+
			"and Nimbus does not add cryptographic dependencies", name)
	}
	return nil, fmt.Errorf("unknown curve %q", name)
}

// persistKey and restoreKey convert a key to and from its on-disk form.
//
// The private half travels as PKCS#8 DER, which crypto/x509 can write and
// read for both RSA and ECDSA -- so one field covers both key types and
// nothing has to branch on kty to serialise. A version whose material cannot
// be parsed back is DROPPED and counted as ignored rather than restored
// without a private half: a key that exists and cannot sign is worse than one
// that is reported missing, because the first fails at the moment it is used
// and the second fails when it is looked for.
func persistKey(k *Key) persistedKey {
	out := persistedKey{
		Name: k.Name, Deleted: k.Deleted,
		RecoveryID: k.RecoveryID, ScheduledFor: k.ScheduledFor,
	}
	for _, ver := range k.Versions {
		var priv any
		switch {
		case ver.RSA != nil:
			priv = ver.RSA
		case ver.EC != nil:
			priv = ver.EC
		default:
			continue
		}
		der, err := x509.MarshalPKCS8PrivateKey(priv)
		if err != nil {
			continue
		}
		out.Versions = append(out.Versions, persistedKeyVersion{
			Version: ver.Version, Kty: ver.Kty, Curve: ver.Curve, Ops: ver.Ops,
			PKCS8: der, Tags: ver.Tags, Enabled: ver.Enabled,
			NotBefore: ver.NotBefore, Expires: ver.Expires,
			Created: ver.Created, Updated: ver.Updated,
		})
	}
	return out
}

func restoreKey(p persistedKey) *Key {
	k := &Key{
		Name: p.Name, Deleted: p.Deleted,
		RecoveryID: p.RecoveryID, ScheduledFor: p.ScheduledFor,
	}
	for _, pv := range p.Versions {
		priv, err := x509.ParsePKCS8PrivateKey(pv.PKCS8)
		if err != nil {
			continue
		}
		ver := &KeyVersion{
			Version: pv.Version, Kty: pv.Kty, Curve: pv.Curve, Ops: pv.Ops,
			Tags: pv.Tags, Enabled: pv.Enabled,
			NotBefore: pv.NotBefore, Expires: pv.Expires,
			Created: pv.Created, Updated: pv.Updated,
		}
		switch t := priv.(type) {
		case *rsa.PrivateKey:
			ver.RSA = t
		case *ecdsa.PrivateKey:
			ver.EC = t
		default:
			continue
		}
		k.Versions = append(k.Versions, ver)
	}
	if len(k.Versions) == 0 {
		return nil
	}
	return k
}
