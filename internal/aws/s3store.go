package aws

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Where S3 object bytes live.
//
// On disk under {DataDir}/s3 when --persist is on, in memory otherwise --
// the same choice the Azure blob store makes, for the same reason: a local
// emulator that forgets your uploads on restart is a worse tool, and one
// that writes to disk without being asked is a ruder one.
//
// Keys are stored in their own file with the metadata beside it, rather
// than in one index, so a key with a slash in it ("img/logo.svg", which is
// most of them) lands in a directory that matches what the client sees.

// objectInfo is everything about an object except its bytes.
type objectInfo struct {
	Key          string            `json:"key"`
	Size         int64             `json:"size"`
	ETag         string            `json:"etag"`
	ContentType  string            `json:"contentType"`
	LastModified time.Time         `json:"lastModified"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// objectStore holds every bucket's objects.
type objectStore struct {
	mu  sync.RWMutex
	dir string // empty = memory only
	mem map[string]map[string]storedObject
}

type storedObject struct {
	info objectInfo
	data []byte
}

func newObjectStore(dataDir string) *objectStore {
	s := &objectStore{mem: map[string]map[string]storedObject{}}
	if dataDir != "" {
		s.dir = filepath.Join(dataDir, "s3")
		_ = os.MkdirAll(s.dir, 0o755)
	}
	return s
}

// put stores an object and returns its ETag.
//
// The ETag is the MD5 of the content, which is what S3 returns for a
// single-part upload and what clients compare against to decide whether an
// upload needs repeating. Unlike Azure's Content-MD5 -- a content setting
// that must NOT be invented -- this one is always present and always the
// digest, so computing it is correct rather than a guess.
func (s *objectStore) put(bucket, key string, data []byte, info objectInfo) objectInfo {
	sum := md5.Sum(data)
	info.Key = key
	info.Size = int64(len(data))
	info.ETag = `"` + hex.EncodeToString(sum[:]) + `"`
	info.LastModified = time.Now().UTC().Truncate(time.Second)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mem[bucket] == nil {
		s.mem[bucket] = map[string]storedObject{}
	}
	s.mem[bucket][key] = storedObject{info: info, data: append([]byte(nil), data...)}
	s.writeThrough(bucket, key, data, info)
	return info
}

func (s *objectStore) get(bucket, key string) ([]byte, objectInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.mem[bucket][key]
	if !ok {
		return nil, objectInfo{}, false
	}
	return o.data, o.info, true
}

func (s *objectStore) delete(bucket, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.mem[bucket][key]; !ok {
		return false
	}
	delete(s.mem[bucket], key)
	if s.dir != "" {
		path := s.pathFor(bucket, key)
		_ = os.Remove(path)
		_ = os.Remove(path + ".json")
	}
	return true
}

// deleteBucket removes every object in a bucket.
func (s *objectStore) deleteBucket(bucket string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.mem, bucket)
	if s.dir != "" {
		_ = os.RemoveAll(filepath.Join(s.dir, bucket))
	}
}

// list returns the objects in a bucket whose key starts with prefix,
// sorted by key, which is the order S3 guarantees.
func (s *objectStore) list(bucket, prefix string) []objectInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []objectInfo{}
	for key, o := range s.mem[bucket] {
		if strings.HasPrefix(key, prefix) {
			out = append(out, o.info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (s *objectStore) count(bucket string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.mem[bucket])
}

// --- disk -----------------------------------------------------------------

// pathFor keeps the key's own shape on disk, so "img/logo.svg" is a file in
// a directory rather than a file with a slash in its name. A key that tries
// to climb out with ".." is refused by cleanKey before it ever gets here.
func (s *objectStore) pathFor(bucket, key string) string {
	return filepath.Join(s.dir, bucket, filepath.FromSlash(key))
}

func (s *objectStore) writeThrough(bucket, key string, data []byte, info objectInfo) {
	if s.dir == "" {
		return
	}
	path := s.pathFor(bucket, key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return
	}
	if meta, err := json.Marshal(info); err == nil {
		_ = os.WriteFile(path+".json", meta, 0o644)
	}
}

// load reads everything back off disk at startup.
func (s *objectStore) load() {
	if s.dir == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = filepath.WalkDir(s.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, ".json") {
			return nil
		}
		rel, err := filepath.Rel(s.dir, path)
		if err != nil {
			return nil
		}
		parts := strings.SplitN(filepath.ToSlash(rel), "/", 2)
		if len(parts) != 2 {
			return nil
		}
		bucket, key := parts[0], parts[1]
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		info := objectInfo{Key: key, Size: int64(len(data))}
		if meta, err := os.ReadFile(path + ".json"); err == nil {
			_ = json.Unmarshal(meta, &info)
		}
		if s.mem[bucket] == nil {
			s.mem[bucket] = map[string]storedObject{}
		}
		s.mem[bucket][key] = storedObject{info: info, data: data}
		return nil
	})
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
