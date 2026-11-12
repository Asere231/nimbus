package fsname

import (
	"strings"
	"testing"
)

// The names every example actually uses must be untouched.
//
// This is the whole reason for percent-encoding rather than hashing: a
// persisted blob or object should still be something you can find with `ls`
// and read with `cat`, which internal/azure/storage/persist.go calls most of
// the point of a local emulator. It is also what makes the change backwards
// compatible -- a key that encoded to itself yesterday still finds its file
// today, because the encoding is the identity for these.
func TestOrdinaryNamesAreUnchanged(t *testing.T) {
	for _, name := range []string{
		"readme.txt",
		"img/logo.svg",
		"docs/hello.txt",
		"deep/nested/path/to/file.bin",
		"data-2026-01-01.json",
		"a_b-c.d",
		"00000000",
	} {
		if got := Encode(name); got != name {
			t.Errorf("Encode(%q) = %q, want it unchanged", name, got)
		}
	}
}

// Everything must come back exactly, because S3's objectStore.load walks the
// directory and rebuilds each key from the path it found. An encoding that
// does not round-trip silently renames every object on restart.
func TestEncodeRoundTrips(t *testing.T) {
	names := []string{
		"readme.txt",
		"Readme.txt",
		"README.TXT",
		"img/logo.svg",
		"MyDocs/Report.PDF",
		"a:b",
		"q?x",
		"star*x",
		"pipe|x",
		`back\slash`,
		`quote"x`,
		"lt<gt>x",
		"trail.",
		"trail ",
		" lead",
		"con",
		"CON",
		"com1",
		"nul.txt",
		"100% sure.txt",
		"%2e%2e",
		"..",
		".",
		"a/../b",
		"a//b",
		"///",
		"ünïcôde.txt",
		"emoji-\U0001F600.txt",
		"tab\tchar",
		"newline\nchar",
		strings.Repeat("x", 300),
		"x",
	}
	for _, name := range names {
		enc := Encode(name)
		back, ok := Decode(enc)
		if !ok {
			t.Errorf("Decode(Encode(%q)) = %q, not decodable", name, enc)
			continue
		}
		if back != name {
			t.Errorf("round trip failed: %q -> %q -> %q", name, enc, back)
		}
	}
}

// The property the whole exercise exists for.
//
// NTFS and APFS are both case-insensitive by default, so two keys differing
// only in case landed on one file: measured on Windows, S3 keys "Readme.txt"
// and "readme.txt" left ONE file carrying the first name and the second
// one's bytes, and after a restart the listing advertised a key that 404'd
// while the key that worked was not listed at all. Azure's manifest-based
// loader failed differently and just as badly -- both names came back with
// the same content.
//
// An encoded name contains no uppercase at all, so comparing case
// insensitively is the same as comparing exactly, and percent-encoding is
// injective. That is the entire proof, and it is why uppercase is encoded
// even though uppercase is legal in a filename.
func TestEncodedNamesNeverCollideCaseInsensitively(t *testing.T) {
	names := []string{
		"Readme.txt", "readme.txt", "README.txt", "ReadMe.TXT",
		"a:b", "a%3ab", "A:B",
		"con", "CON", "Con",
		"img/logo.svg", "IMG/LOGO.SVG", "Img/Logo.svg",
		"trail.", "trail", "TRAIL.",
	}
	seen := map[string]string{}
	for _, name := range names {
		key := strings.ToLower(Encode(name))
		if other, clash := seen[key]; clash {
			t.Errorf("%q and %q both encode to %q case-insensitively",
				other, name, key)
			continue
		}
		seen[key] = name
	}
}

// Windows refuses these outright, or silently rewrites them. Measured: a
// trailing dot was stripped, so the key "trail." was written to a file named
// "trail" and came back renamed; ":" opened an NTFS alternate data stream, so
// the bytes went into a hidden stream of a zero-length file; and "?", "*" and
// "|" failed the write entirely, which writeThrough swallowed, so the object
// served fine until a restart and then was simply gone.
func TestNamesWindowsCannotStoreAreEncodedAway(t *testing.T) {
	for _, name := range []string{
		"a:b", "q?x", "star*x", `pipe|x`, `back\slash`,
		`quote"x`, "lt<gt>x", "trail.", "trail ", "tab\tchar",
	} {
		enc := Encode(name)
		for _, bad := range []string{":", "?", "*", "|", `\`, `"`, "<", ">", "\t"} {
			if strings.Contains(enc, bad) {
				t.Errorf("Encode(%q) = %q still contains %q", name, enc, bad)
			}
		}
		for _, seg := range strings.Split(enc, "/") {
			if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
				t.Errorf("Encode(%q) = %q has a segment ending in a dot or space", name, enc)
			}
		}
	}
}

// A reserved device name is reserved per path SEGMENT, and only matters when
// the name would otherwise come out as one: "CON" encodes to "%43%4f%4e" on
// the uppercase rule alone and is already safe, so it is the lowercase "con"
// that needs the guard.
func TestReservedDeviceNamesAreEscaped(t *testing.T) {
	for _, name := range []string{"con", "nul", "aux", "prn", "com1", "lpt9", "con/x", "x/con"} {
		enc := Encode(name)
		for _, seg := range strings.Split(enc, "/") {
			stem := seg
			if i := strings.Index(stem, "."); i >= 0 {
				stem = stem[:i]
			}
			if isReserved(stem) {
				t.Errorf("Encode(%q) = %q leaves reserved segment %q", name, enc, stem)
			}
		}
		if back, ok := Decode(enc); !ok || back != name {
			t.Errorf("reserved name %q did not round trip: %q -> %q", name, enc, back)
		}
	}
}

// The directory layout is what makes a persisted store browsable, so a key
// with slashes has to stay a path -- but an empty segment ("a//b" is a legal
// S3 key) cannot be a directory, and filepath.Join would quietly collapse it.
// Such a name falls back to being one flat segment, which costs the layout in
// a case nobody has and keeps the round trip total.
func TestSlashesStayDirectoriesUnlessASegmentIsEmpty(t *testing.T) {
	if got := Encode("img/logo.svg"); got != "img/logo.svg" {
		t.Errorf("Encode kept no directory structure: %q", got)
	}
	flat := Encode("a//b")
	if strings.Contains(flat, "/") {
		t.Errorf("Encode(%q) = %q, want one flat segment", "a//b", flat)
	}
	if back, ok := Decode(flat); !ok || back != "a//b" {
		t.Errorf("empty-segment name did not round trip: %q -> %q", flat, back)
	}
}

// Decode must refuse what it cannot read rather than inventing a name. These
// are not things Encode produces; they are what a stray file in the data
// directory looks like.
func TestDecodeRefusesMalformedInput(t *testing.T) {
	for _, bad := range []string{"%", "%2", "%zz", "a%g0b", "%2"} {
		if got, ok := Decode(bad); ok {
			t.Errorf("Decode(%q) = %q, want refusal", bad, got)
		}
	}
}
