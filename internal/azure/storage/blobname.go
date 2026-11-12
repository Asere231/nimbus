package storage

import "strings"

// Whether a blob or container name may be turned into a path.
//
// persist.go writes a blob's bytes to
// filepath.Join(acctDir, "blobs", container, name), so a name carrying ".."
// escapes its container: measured on Windows, a blob PUT as
// "../../nimbus-traversal-probe.txt" landed in the account directory, next to
// the account.json manifest that a blob of that name would overwrite. The
// container listed it afterwards under its original name, so nothing about
// the response said anything had gone wrong.
//
// S3's cleanKey has refused this since Milestone 8 and the blob store never
// learned it. This is that rule, in Azure's vocabulary.
//
// The name is judged RAW and slash-separated rather than handed to filepath,
// which is the shape every platform fix in this project has taken. The two
// hosts disagree in both directions: a backslash is a separator on Windows
// only, so `a\..\..\x` is one harmless segment to a "/"-split check and three
// levels up to filepath.Join there; and "C:/x" is an ordinary name on Unix
// and absolute on Windows, where filepath.IsAbs is false for "/absolute.txt".
// Judging the name itself refuses the same request on every machine, which is
// what a client's name deserves.
//
// Deliberately NARROW. Only a whole ".." segment is a traversal: a dot is an
// ordinary character in a blob name, "..hidden" and "file..name.txt" are
// names somebody meant, and a colon is how every timestamp is spelled, so
// refusing those would be a worse bug than the one this fixes. Real Azure
// agrees -- its own guidance is to avoid a name ENDING in a dot or a slash,
// not to refuse dots.
//
// A backslash is refused outright, as S3 does, because there is no way to
// store one safely on Windows and a blob name containing one is vanishingly
// rare next to the harm.
func blobNameIsSafe(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) {
		return false
	}
	// A single letter then a colon, at the end of the first segment: a
	// Windows drive. Narrow on purpose -- a colon anywhere else is legal and
	// ordinary, which is why this looks at the position rather than the
	// character.
	if len(name) >= 2 && name[1] == ':' && isASCIILetter(name[0]) &&
		(len(name) == 2 || name[2] == '/') {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
