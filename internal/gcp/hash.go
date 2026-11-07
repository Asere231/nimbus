package gcp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"math/big"
	"strings"
)

// Derived values.
//
// Everything a client reads back twice is computed from something stable,
// never generated fresh: an etag from the resource's own id, a numeric id
// from its self link. This is the rule that each of the three clouds has
// now taught in its own accent -- Azure's Content-MD5 and etags, AWS's
// CodeSha256 -- and the symptom of breaking it is always the same, a plan
// that never settles and nothing to point at.
//
// The exception is an id decided at CREATE time, like a random operation
// name: it is stored, and a stored value agrees with itself.

// numericID is the decimal string GCP uses for a resource's `id`. Derived
// from the self link, so two reads of the same resource agree -- and 19
// digits, because that is what clients expect to parse as an int64.
func numericID(seed string) numberString {
	sum := sha256.Sum256([]byte("nimbus/gcp/" + seed))
	n := new(big.Int).SetBytes(sum[:8])
	// Keep it comfortably inside int64 and away from zero.
	n.Mod(n, big.NewInt(8_000_000_000_000_000_000))
	n.Add(n, big.NewInt(1_000_000_000_000_000_000))
	return n.String()
}

// etagOf is a stable etag. Nothing Nimbus serves does optimistic
// concurrency, so there is nothing to lose by deriving it and a stable read
// to gain -- the same decision ARM etags got in Milestone 5.
func etagOf(seed string) string {
	sum := sha256.Sum256([]byte("nimbus/gcp/etag/" + seed))
	return base64.StdEncoding.EncodeToString(sum[:8])
}

// hexID is a fresh random identifier, for the things that are decided once
// at create and then stored.
func hexID(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n)
	}
	return hex.EncodeToString(b)[:n]
}
