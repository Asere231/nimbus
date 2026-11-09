package funcs

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Unpacking a deployment package.
//
// A zip arrives base64-encoded in the create call, and its entries are
// NAMES, not paths that can be trusted: "../../etc/passwd" is a legal entry
// and extracting it with filepath.Join leaves the directory entirely. This
// is the same trap as an S3 key, which is also not a path, and it is
// checked the same way -- by refusing rather than by sanitising, because a
// package containing such an entry is not one anybody meant to deploy.

// Unpack writes a zip's contents into dir, which is emptied first so an
// update cannot leave a deleted file behind.
func Unpack(archive []byte, dir string) error {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return fmt.Errorf("the deployment package is not a readable zip: %w", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	for _, f := range zr.File {
		target, err := safeJoin(dir, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := copyEntry(f, target); err != nil {
			return err
		}
	}
	return nil
}

// copyEntry writes one file, keeping the executable bit: a Go or custom
// runtime's bootstrap has to stay runnable, and a zip that is unpacked with
// everything at 0644 fails with "exec format error" or a permission denied
// that names nothing useful.
func copyEntry(f *zip.File, target string) error {
	src, err := f.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	mode := f.Mode().Perm()
	if mode == 0 {
		mode = 0o644
	}
	if mode&0o111 != 0 {
		mode |= 0o755
	}
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer dst.Close()
	// Bounded: a zip can claim a far larger uncompressed size than it has
	// any business having, and filling the disk is a worse failure than a
	// refused deploy.
	if _, err := io.Copy(dst, io.LimitReader(src, 512<<20)); err != nil {
		return err
	}
	return nil
}

// safeJoin refuses an entry that would land outside dir.
//
// The decision is made on the RAW zip name, by zip's own rules -- entries
// are always forward-slash separated, whatever wrote them -- and not on a
// platform-cleaned path, because the two host platforms disagree about the
// same name in both directions:
//
//   - "/absolute.py" is absolute on Unix, and filepath.IsAbs is FALSE for it
//     on Windows, where a path needs a volume name to be absolute. So the
//     old check let it through on Windows (CI caught this, with the entry
//     landing inside dir rather than escaping -- harmless there, and
//     accepted when it should never be).
//   - "C:/x" is the mirror image: absolute on Windows, an ordinary relative
//     name containing a colon on Unix.
//
// Judging the name itself makes a package that is refused on one machine
// refused on every machine, which matters more than either platform's rules:
// the same zip must not deploy in CI and fail on a laptop.
func safeJoin(dir, name string) (string, error) {
	bad := func(why string) error {
		return fmt.Errorf("the deployment package contains the entry %q, "+
			"which %s", name, why)
	}
	if name == "" {
		return "", bad("has no name")
	}
	// A backslash is not a separator in a zip name, so a name containing one
	// means either a non-conformant writer or an attempt to be read as a path
	// on Windows only. Either way it is not a name to guess about.
	if strings.Contains(name, `\`) {
		return "", bad("contains a backslash, which is not a zip path separator")
	}
	if strings.HasPrefix(name, "/") {
		return "", bad("is an absolute path")
	}
	// A drive letter, which only the leading segment can carry: "C:/x" is
	// absolute on Windows and an ordinary name with a colon in it on Unix.
	// The rule is a single letter, a colon, then the end of the segment --
	// narrow on purpose, because a colon is a legal character in a zip name
	// and refusing "a:b.py" would reject a package that works everywhere.
	if len(name) >= 2 && name[1] == ':' && isLetter(name[0]) &&
		(len(name) == 2 || name[2] == '/') {
		return "", bad("begins with a drive letter")
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return "", bad("would be written outside the function's directory")
		}
	}

	target := filepath.Join(dir, filepath.FromSlash(name))
	// Belt and braces: whatever the rules above missed, the result still has
	// to be inside dir.
	if rel, err := filepath.Rel(dir, target); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("the deployment package contains the unusable entry %q", name)
	}
	return target, nil
}

func isLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// hashOf is a short stable digest, for container and directory names.
func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}
