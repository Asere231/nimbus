package aws

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Asere231/nimbus/internal/datadir"
	"github.com/Asere231/nimbus/internal/fsname"
)

// Getting an object's HISTORY onto a disk, and back off it.
//
// The layout is deliberately conservative: the current version keeps the
// plain "{bucket}/{key}" path it has always had, and noncurrent versions
// sit beside it as "{key}.v0%", "{key}.v1%", in history order. So a data
// directory written before versioning existed loads unchanged, and a
// bucket with one version of everything -- which is every bucket until
// somebody turns versioning on -- is byte-for-byte what it was.
//
// A whole key is rewritten on every change rather than appended to. That
// is more work than it needs to be and it is the only way to keep the
// ordinals meaningful after a version in the middle is deleted, which is
// what a client emptying a versioned bucket does over and over.

// pathFor keeps the key's own shape on disk, so "img/logo.svg" is a file in
// a directory rather than a file with a slash in its name. A key that tries
// to climb out with ".." is refused by cleanKey before it ever gets here.
//
// The name goes through fsname.Encode, which is the identity for an ordinary
// lowercase key and escapes what a filesystem cannot hold. Without it, NTFS
// and APFS -- both case-insensitive by default -- merged two keys differing
// only in case onto one file, ":" opened an alternate data stream, and "?",
// "*" and "|" failed the write entirely. See internal/fsname.
func (s *objectStore) pathFor(bucket, key string) string {
	return filepath.Join(s.dir, fsname.Encode(bucket), filepath.FromSlash(fsname.Encode(key)))
}

// writeKey persists a key's whole version history, current version last.
// Called with the store's lock held.
func (s *objectStore) writeKey(bucket, key string, versions []storedObject) {
	if s.dir == "" {
		return
	}
	path := s.pathFor(bucket, key)

	// Clear what was there. Globbing is safe because fsname.Encode emits
	// only lowercase letters, digits, "-", "_", "." and "%XX" -- none of
	// which is a glob metacharacter, so no key can smuggle one in.
	_ = os.Remove(path)
	_ = os.Remove(path + metaSuffix)
	stale, _ := filepath.Glob(path + versionPrefix + "*%")
	for _, p := range stale {
		_ = os.Remove(p)
	}
	if len(versions) == 0 {
		return
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("nimbus: could not persist s3://%s/%s: %v", bucket, key, err)
		return
	}
	for i, o := range versions {
		p := path
		if i < len(versions)-1 {
			p = path + versionPrefix + strconv.Itoa(i) + "%"
		}
		s.writeVersion(bucket, key, p, o)
	}
}

// writeVersion writes one version's bytes and its metadata sidecar.
//
// A failed write is REPORTED, not swallowed. It used to return quietly, so
// an object whose name the filesystem refused served perfectly from memory
// and then was simply gone after a restart, with nothing anywhere saying
// why. fsname.Encode should mean this no longer happens for a name's sake,
// which is exactly why a failure now means something real -- a full disk, a
// permission -- and is worth saying out loud.
func (s *objectStore) writeVersion(bucket, key, path string, o storedObject) {
	// A delete marker has no bytes, and its file is empty. The sidecar is
	// what says it is a marker, which is why a version with no readable
	// sidecar is not loaded as one: see load.
	if err := os.WriteFile(path, o.data, 0o644); err != nil {
		log.Printf("nimbus: could not persist s3://%s/%s: %v", bucket, key, err)
		return
	}
	meta, err := json.Marshal(o.info)
	if err != nil {
		return
	}
	if err := os.WriteFile(path+metaSuffix, meta, 0o644); err != nil {
		log.Printf("nimbus: could not persist metadata for s3://%s/%s: %v", bucket, key, err)
	}
}

// loadedVersion is one file found on disk, with where it belongs in the
// history. ordinal is -1 for the current version, whose file has no suffix.
type loadedVersion struct {
	ordinal int
	object  storedObject
}

// load reads everything back off disk at startup.
func (s *objectStore) load() {
	if s.dir == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	ignored := 0
	found := map[string]map[string][]loadedVersion{}
	_ = filepath.WalkDir(s.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, metaSuffix) {
			return nil
		}
		rel, err := filepath.Rel(s.dir, path)
		if err != nil {
			return nil
		}
		parts := strings.SplitN(filepath.ToSlash(rel), "/", 2)
		if len(parts) != 2 {
			ignored++
			return nil
		}
		// The path is an ENCODED name, so the key is what it decodes back
		// to. A file that does not decode is not one we wrote -- something
		// a person dropped in the data directory -- and is skipped rather
		// than loaded under an invented name.
		bucket, ok := fsname.Decode(parts[0])
		if !ok {
			ignored++
			return nil
		}
		encoded, ordinal := splitVersionSuffix(parts[1])
		key, ok := fsname.Decode(encoded)
		if !ok {
			ignored++
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			ignored++
			return nil
		}
		info := objectInfo{Key: key, Size: int64(len(data)), VersionID: nullVersionID}
		if meta, err := os.ReadFile(path + metaSuffix); err == nil {
			_ = json.Unmarshal(meta, &info)
		}
		if found[bucket] == nil {
			found[bucket] = map[string][]loadedVersion{}
		}
		found[bucket][key] = append(found[bucket][key],
			loadedVersion{ordinal: ordinal, object: storedObject{info: info, data: data}})
		return nil
	})

	for bucket, keys := range found {
		s.mem[bucket] = map[string][]storedObject{}
		for key, vs := range keys {
			// Oldest first, with the unsuffixed file -- the current
			// version -- last. Sorting on the ordinal rather than on
			// LastModified for the reason the slice exists at all: two
			// versions of one key can share a second.
			sort.SliceStable(vs, func(i, j int) bool {
				if vs[i].ordinal == -1 {
					return false
				}
				if vs[j].ordinal == -1 {
					return true
				}
				return vs[i].ordinal < vs[j].ordinal
			})
			out := make([]storedObject, 0, len(vs))
			for _, v := range vs {
				out = append(out, v.object)
			}
			s.mem[bucket][key] = out
		}
	}
	datadir.ReportIgnored("s3", s.dir, ignored)
}

// splitVersionSuffix separates an encoded key from a ".v{n}%" suffix,
// returning -1 for a path that has none -- the current version.
//
// Safe because an encoded name never ENDS in "%": fsname.Encode only ever
// emits "%" followed by two hex digits. So a trailing "%" was appended
// here, and nothing a client can name will be mistaken for one.
func splitVersionSuffix(encoded string) (string, int) {
	if !strings.HasSuffix(encoded, "%") {
		return encoded, -1
	}
	trimmed := strings.TrimSuffix(encoded, "%")
	i := strings.LastIndex(trimmed, versionPrefix)
	if i < 0 {
		return encoded, -1
	}
	n, err := strconv.Atoi(trimmed[i+len(versionPrefix):])
	if err != nil {
		return encoded, -1
	}
	return trimmed[:i], n
}

// cleanKey refuses a key that would escape the bucket's directory.
//
// Nothing a client sends is trusted with a path. "../../etc/passwd" is a
// legal S3 key -- S3 stores it flat and it means nothing -- but written
// through filepath.Join it would leave the data directory entirely.
// A key knows only "/", so anything the HOST filesystem would read as a
// separator has to be refused here, while it is still just a character.
// `a\..\..\x` is one harmless segment to a "/"-split check and three
// levels up to filepath.Join on Windows; "C:/x" is an ordinary name on Unix
// and absolute on Windows. Judging the key by S3's own rules makes the same
// request refused on every machine, which is what a client's key deserves.
func cleanKey(key string) (string, bool) {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, `\`) {
		return "", false
	}
	// A single letter then a colon, at the end of the first segment: a
	// Windows drive. Narrow, because a colon elsewhere is a legal character
	// in a key -- a timestamp has two of them.
	if len(key) >= 2 && key[1] == ':' && isASCIILetter(key[0]) &&
		(len(key) == 2 || key[2] == '/') {
		return "", false
	}
	for _, part := range strings.Split(key, "/") {
		if part == ".." {
			return "", false
		}
	}
	return key, true
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
