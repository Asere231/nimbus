// Package fsname turns a client's object name into a path a filesystem can
// hold, reversibly and identically on every machine.
//
// An object name is a flat key. S3, Azure Blob and Cloud Storage all let it
// contain very nearly anything, and all three of Nimbus's stores want to put
// it on disk so that a persisted upload is something you can find with `ls`
// and read with `cat` -- which persist.go calls most of the point of a local
// emulator. Two of them did that by joining the name straight onto a
// directory, and on Windows that was measured doing three different wrong
// things:
//
//   - Two keys differing only in CASE became one file. NTFS kept the first
//     name and the second one's bytes; after a restart S3 listed a key that
//     404'd while the key that worked was not listed, and Azure returned the
//     same content under both names. APFS is case-insensitive by default too,
//     so this was never Windows-only -- only the Linux CI leg was immune,
//     which is why nothing ever said so.
//   - ":" opened an NTFS ALTERNATE DATA STREAM. The bytes went into a hidden
//     stream of a zero-length file, and the object came back empty under a
//     truncated name.
//   - "?", "*" and "|" failed the write outright, which writeThrough
//     swallowed, so the object served perfectly until a restart and then was
//     simply gone. A trailing dot was silently stripped and the object came
//     back renamed.
//
// # Why percent-encoding, and why uppercase is encoded
//
// The rule is: a byte is kept only if it is a lowercase letter, a digit, "-",
// "_" or "."; everything else becomes %XX. Slashes stay slashes, so a key's
// directory layout survives.
//
// Encoding uppercase looks excessive until you try to prove the alternative
// correct. Because an encoded name contains NO uppercase, comparing two
// encoded names case-insensitively is the same as comparing them exactly, and
// percent-encoding is injective -- so distinct keys cannot collide on a
// case-insensitive filesystem. That is the whole proof, in one line. The
// readable alternative, keeping the name and appending a disambiguating
// suffix only when needed, needs a four-case argument and a marker character
// that itself has to be escaped, in a path where a mistake renames data.
//
// The cost is paid only by names with capitals in them, and not at all by the
// ones that actually turn up: every key in every example here is lowercase,
// so Encode is the IDENTITY for them. That is also what makes this change
// backwards compatible -- a key whose file is already at its own name still
// finds it.
//
// # What this is not
//
// It is not a safety check. A name that climbs out with ".." is refused
// before it gets here, by S3's cleanKey and the blob service's
// blobNameIsSafe, because a client that sends one deserves an error rather
// than a quietly renamed object. Encode is total and would neutralise it
// anyway -- ".." encodes to "%2e%2e" -- but defence in depth is not a reason
// to stop refusing.
//
// GCP's objectstore hashes instead, which is immune to all of this and gives
// up the browsability. That choice is left alone; this is the same problem
// answered the other way, for the two stores whose whole point is that you
// can look at what they wrote.
package fsname

import "strings"

// safe is every byte kept as itself. Deliberately small: widening it later is
// easy and narrowing it would rename everything already on disk. Note what is
// absent -- uppercase (the case-collision proof above), space (a trailing one
// is stripped by Windows) and every shell or path metacharacter.
const safe = "abcdefghijklmnopqrstuvwxyz0123456789-_."

const hexDigits = "0123456789abcdef"

// Encode turns an object name into a slash-separated relative path. The
// caller applies filepath.FromSlash; keeping slashes here means the result is
// the same text on every OS, which is what makes a persisted directory
// portable between machines.
func Encode(name string) string {
	// An empty segment cannot be a directory -- "a//b" is a legal S3 key --
	// and filepath.Join would quietly collapse it, which breaks the round
	// trip. Such a name becomes ONE flat segment instead, losing the layout
	// in a case nobody has rather than losing the name in every case.
	segments := strings.Split(name, "/")
	flat := name == ""
	for _, s := range segments {
		if s == "" {
			flat = true
			break
		}
	}
	if flat {
		return encodeSegment(name)
	}

	out := make([]string, len(segments))
	for i, s := range segments {
		out[i] = encodeSegment(s)
	}
	return strings.Join(out, "/")
}

// encodeSegment handles one path component, including the two rules that are
// about the segment as a whole rather than about any single byte.
func encodeSegment(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(safe, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	out := b.String()

	// Windows strips a trailing dot, so a key ending in one came back
	// renamed. Only the final byte needs escaping; dots elsewhere are
	// ordinary, and "file..name.txt" is a name somebody meant.
	if strings.HasSuffix(out, ".") {
		out = out[:len(out)-1] + "%2e"
	}

	// A reserved device name is reserved per segment. Only a name that is
	// already lowercase can come out as one -- "CON" encodes to "%43%4f%4e"
	// on the uppercase rule alone -- so this guard fires rarely, and escaping
	// the first byte is enough to stop the match.
	stem := out
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	if isReserved(stem) {
		out = "%" + string(hexDigits[out[0]>>4]) + string(hexDigits[out[0]&0x0f]) + out[1:]
	}
	return out
}

// Decode reverses Encode. It reports false for anything it cannot read, so a
// stray file in the data directory is skipped rather than loaded under an
// invented name.
func Decode(path string) (string, bool) {
	var b strings.Builder
	b.Grow(len(path))
	for i := 0; i < len(path); i++ {
		if path[i] != '%' {
			b.WriteByte(path[i])
			continue
		}
		if i+2 >= len(path) {
			return "", false
		}
		hi, ok1 := unhex(path[i+1])
		lo, ok2 := unhex(path[i+2])
		if !ok1 || !ok2 {
			return "", false
		}
		b.WriteByte(hi<<4 | lo)
		i += 2
	}
	return b.String(), true
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// isReserved reports whether a segment stem is an MS-DOS device name. These
// are still special in the Win32 API: an open of "con" in any directory talks
// to the console rather than creating a file. Measured on Windows 11, a file
// called CON in a subdirectory did get created -- the rule has been relaxed
// -- but the guard costs one comparison and older Windows and many libraries
// still enforce it.
func isReserved(stem string) bool {
	switch stem {
	case "con", "prn", "aux", "nul":
		return true
	}
	if len(stem) == 4 && stem[3] >= '1' && stem[3] <= '9' {
		switch stem[:3] {
		case "com", "lpt":
			return true
		}
	}
	return false
}
