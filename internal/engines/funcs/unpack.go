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
func safeJoin(dir, name string) (string, error) {
	// Zip entries always use forward slashes, whatever wrote them.
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("the deployment package contains the entry %q, "+
			"which would be written outside the function's directory", name)
	}
	target := filepath.Join(dir, clean)
	// Belt and braces, for a name that cleans to something unexpected on
	// one platform's path rules and not another's.
	if rel, err := filepath.Rel(dir, target); err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("the deployment package contains the unusable entry %q", name)
	}
	return target, nil
}

// hashOf is a short stable digest, for container and directory names.
func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}
