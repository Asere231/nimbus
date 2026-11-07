package gcp

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Where object bytes live.
//
// On disk under {DataDir}/gcs when --persist is on, in memory otherwise --
// the third time this choice has been made, after Azure's blobs and S3's
// objects, and for the same reason both times before: an emulator that
// forgets your uploads on restart is a worse tool, and one that writes to
// disk unasked is a ruder one.
//
// A name is NOT a path. "img/logo.svg" is one flat key that happens to
// contain a slash, and "../../etc/passwd" is a legal key too -- so a name
// is hashed into a filename rather than joined onto the directory. S3
// learned this with cleanKey; hashing is the simpler answer and also deals
// with the case sensitivity that a file system may not have.

// maxObject bounds a single upload. Large enough for anything a local
// example does, small enough that a runaway client cannot fill the disk.
const maxObject = 512 << 20

type objectStore struct {
	mu  sync.RWMutex
	dir string // empty = memory only
	// bucket -> name -> object
	data map[string]map[string]storedObject
	// resumable upload sessions, by upload id
	uploads map[string]*uploadSession
}

type storedObject struct {
	meta map[string]any
	data []byte
}

type uploadSession struct {
	bucket string
	name   string
	meta   map[string]any
	data   []byte
}

func newObjectStore(dataDir string) *objectStore {
	s := &objectStore{
		data:    map[string]map[string]storedObject{},
		uploads: map[string]*uploadSession{},
	}
	if dataDir != "" {
		s.dir = filepath.Join(dataDir, "gcs")
		_ = os.MkdirAll(s.dir, 0o755)
	}
	return s
}

// put stores an object and returns the metadata document.
func (s *objectStore) put(srv *Server, bucket, name string, data []byte, meta map[string]any) (map[string]any, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data[bucket] == nil {
		s.data[bucket] = map[string]storedObject{}
	}

	doc := map[string]any{}
	for k, v := range meta {
		doc[k] = v
	}
	sum := md5.Sum(data)
	// A generation is a microsecond timestamp in real GCS and must
	// INCREASE with each write of the same name, because a client uses it
	// for if-generation-match. Reusing the previous one plus a step keeps
	// that true without depending on the clock's resolution -- Windows's
	// is a millisecond or worse, which this project has been bitten by
	// before.
	generation := int64(1)
	if prev, ok := s.data[bucket][name]; ok {
		if n, err := strconv.ParseInt(str(prev.meta["generation"]), 10, 64); err == nil {
			generation = n + 1
		}
	}

	doc["kind"] = "storage#object"
	doc["name"] = name
	doc["bucket"] = bucket
	doc["id"] = bucket + "/" + name + "/" + strconv.FormatInt(generation, 10)
	doc["selfLink"] = srv.bucketLink(bucket) + "/o/" + name
	doc["mediaLink"] = srv.bucketLink(bucket) + "/o/" + name + "?alt=media"
	doc["generation"] = strconv.FormatInt(generation, 10)
	doc["metageneration"] = "1"
	doc["size"] = strconv.Itoa(len(data))
	doc["md5Hash"] = base64MD5(sum[:])
	doc["crc32c"] = crc32cBase64(data)
	doc["etag"] = etagOf(bucket + "/" + name + "/" + strconv.FormatInt(generation, 10))
	doc["storageClass"] = orDefaultString(str(doc["storageClass"]), "STANDARD")
	doc["timeCreated"] = timestamp()
	doc["updated"] = timestamp()
	if str(doc["contentType"]) == "" {
		doc["contentType"] = "application/octet-stream"
	}

	s.data[bucket][name] = storedObject{meta: doc, data: data}
	s.flush(bucket, name)
	return doc, nil
}

func (s *objectStore) get(bucket, name string) ([]byte, map[string]any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.data[bucket][name]
	if !ok {
		return nil, nil, false
	}
	return o.data, o.meta, true
}

func (s *objectStore) meta(bucket, name string) (map[string]any, bool) {
	_, doc, ok := s.get(bucket, name)
	return doc, ok
}

func (s *objectStore) patchMeta(bucket, name string, patch map[string]any) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.data[bucket][name]
	if !ok {
		return nil, false
	}
	for field, value := range patch {
		switch field {
		case "name", "bucket", "id", "selfLink", "generation", "md5Hash", "crc32c", "size":
			continue // identity and digests are not patchable
		}
		if value == nil {
			delete(o.meta, field)
			continue
		}
		o.meta[field] = value
	}
	o.meta["updated"] = timestamp()
	o.meta["metageneration"] = bumpString(o.meta["metageneration"])
	s.data[bucket][name] = o
	s.flush(bucket, name)
	return o.meta, true
}

func (s *objectStore) delete(bucket, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data[bucket], name)
	if s.dir != "" {
		_ = os.Remove(s.pathFor(bucket, name))
		_ = os.Remove(s.pathFor(bucket, name) + ".json")
	}
}

func (s *objectStore) names(bucket string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.data[bucket]))
	for name := range s.data[bucket] {
		out = append(out, name)
	}
	return out
}

func (s *objectStore) count(bucket string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data[bucket])
}

// --- resumable sessions ----------------------------------------------------

func (s *objectStore) newUpload(bucket, name string, meta map[string]any) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := hexID(32)
	s.uploads[id] = &uploadSession{bucket: bucket, name: name, meta: meta}
	return id
}

func (s *objectStore) upload(id string) *uploadSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.uploads[id]
}

func (s *objectStore) finishUpload(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.uploads, id)
}

// --- disk ------------------------------------------------------------------

// pathFor hashes the object's name into a filename. The name is a key, not
// a path: hashing means a slash, a "..", or two names differing only in
// case can never collide or escape the directory.
func (s *objectStore) pathFor(bucket, name string) string {
	return filepath.Join(s.dir, sanitise(bucket), numericID(name)+".obj")
}

func sanitise(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "bucket"
	}
	return b.String()
}

// flush writes one object out. Called with the lock held.
func (s *objectStore) flush(bucket, name string) {
	if s.dir == "" {
		return
	}
	o := s.data[bucket][name]
	path := s.pathFor(bucket, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, o.data, 0o644)
	if meta, err := json.Marshal(o.meta); err == nil {
		_ = os.WriteFile(path+".json", meta, 0o644)
	}
}

// load reads every object back, so a restart with --persist still has them.
func (s *objectStore) load() {
	if s.dir == "" {
		return
	}
	buckets, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range buckets {
		if !b.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(s.dir, b.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			metaPath := filepath.Join(s.dir, b.Name(), f.Name())
			raw, err := os.ReadFile(metaPath)
			if err != nil {
				continue
			}
			var meta map[string]any
			if json.Unmarshal(raw, &meta) != nil {
				continue
			}
			data, err := os.ReadFile(strings.TrimSuffix(metaPath, ".json"))
			if err != nil {
				continue
			}
			// The bucket comes from the METADATA, not from the directory
			// name: the directory is a sanitised version and cannot be
			// turned back into the real name.
			bucket, name := str(meta["bucket"]), str(meta["name"])
			if bucket == "" || name == "" {
				continue
			}
			if s.data[bucket] == nil {
				s.data[bucket] = map[string]storedObject{}
			}
			s.data[bucket][name] = storedObject{meta: meta, data: data}
		}
	}
}

// crc32cBase64 is the other digest GCS reports: CRC-32C (Castagnoli, not
// the CRC-32 everything else uses), big-endian, base64. A client that
// checks it against its own will catch a corrupted upload; one given the
// wrong polynomial's answer rejects a perfectly good one.
func crc32cBase64(data []byte) string {
	table := crc32.MakeTable(crc32.Castagnoli)
	sum := crc32.Checksum(data, table)
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], sum)
	return base64.StdEncoding.EncodeToString(b[:])
}
