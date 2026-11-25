package gcp

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Asere231/nimbus/internal/datadir"
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

// objectStore holds every bucket's objects, each as its GENERATIONS.
//
// The value is a slice, OLDEST FIRST. At most one generation is LIVE --
// the one with no "timeDeleted" -- and it is the last if it is there at
// all. GCS has no delete marker: a delete against a versioned bucket
// ARCHIVES the live generation by stamping timeDeleted on it, and the
// object then reads 404 while its history is still listable with
// ?versions=true. That is a genuinely different model from S3's, which
// lays a marker over the history as a new version, and it is the kind of
// difference this project exists to show.
type objectStore struct {
	mu  sync.RWMutex
	dir string // empty = memory only
	// bucket -> name -> generations, oldest first
	data map[string]map[string][]storedObject
	// resumable upload sessions, by upload id
	uploads map[string]*uploadSession

	// lastGen is the highest generation this store has ever ISSUED, which
	// is not the same as the highest it still holds: deleting the newest
	// generation must not let the next write reuse its number, because a
	// client uses the generation for if-generation-match and two different
	// objects comparing equal is the one thing it cannot survive. Seeded
	// from disk on load, so a restart cannot reuse one either. Found by a
	// test, after a comment claiming the opposite.
	lastGen int64
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
		data:    map[string]map[string][]storedObject{},
		uploads: map[string]*uploadSession{},
	}
	if dataDir != "" {
		s.dir = filepath.Join(dataDir, "gcs")
		_ = os.MkdirAll(s.dir, 0o755)
	}
	return s
}

// put stores a new generation of an object and returns its metadata.
//
// With versioning on, the generation it replaces is ARCHIVED rather than
// discarded. With versioning off, the live generation is discarded as it
// always was -- and any archived generations from before versioning was
// turned off stay, which is what real GCS does too.
func (s *objectStore) put(srv *Server, bucket, name string, data []byte, meta map[string]any, versioned bool) (map[string]any, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data[bucket] == nil {
		s.data[bucket] = map[string][]storedObject{}
	}

	doc := map[string]any{}
	for k, v := range meta {
		doc[k] = v
	}
	sum := md5.Sum(data)
	// A generation is a microsecond timestamp in real GCS and must
	// INCREASE with each write of the same name, because a client uses it
	// for if-generation-match. Counting up from the HIGHEST generation
	// seen keeps that true without depending on the clock's resolution --
	// Windows's is a millisecond or worse, which this project has been
	// bitten by before. The counter is per STORE rather than per object,
	// which is also what real GCS does -- a generation there is a
	// microsecond timestamp, so it rises across the whole bucket and is
	// never reused.
	generation := s.nextGeneration(s.data[bucket][name])

	gens := s.data[bucket][name]
	if i := liveIndex(gens); i >= 0 {
		if versioned {
			gens[i].meta["timeDeleted"] = timestamp()
		} else {
			gens = append(gens[:i:i], gens[i+1:]...)
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

	s.data[bucket][name] = append(gens, storedObject{meta: doc, data: data})
	s.flush(bucket, name)
	return doc, nil
}

// liveIndex is where the live generation sits, or -1 if the object has
// been deleted and only archived generations remain. At most one is ever
// live, which is what makes "the object" a well-defined thing to GET.
func liveIndex(gens []storedObject) int {
	for i := len(gens) - 1; i >= 0; i-- {
		if _, archived := gens[i].meta["timeDeleted"]; !archived {
			return i
		}
	}
	return -1
}

// nextGeneration issues a generation nothing has had. Called with the lock
// held.
func (s *objectStore) nextGeneration(gens []storedObject) int64 {
	next := s.lastGen
	if h := highestGeneration(gens); h > next {
		next = h
	}
	next++
	s.lastGen = next
	return next
}

func highestGeneration(gens []storedObject) int64 {
	var highest int64
	for _, o := range gens {
		if n, err := strconv.ParseInt(str(o.meta["generation"]), 10, 64); err == nil && n > highest {
			highest = n
		}
	}
	return highest
}

// get is the live generation, which is what a request naming no generation
// asks for. An object whose generations are all archived is NOT found,
// which is how a deleted object reads while its history is still listable.
func (s *objectStore) get(bucket, name string) ([]byte, map[string]any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	gens := s.data[bucket][name]
	i := liveIndex(gens)
	if i < 0 {
		return nil, nil, false
	}
	return gens[i].data, gens[i].meta, true
}

// getGeneration is one named generation, live or archived.
func (s *objectStore) getGeneration(bucket, name, generation string) ([]byte, map[string]any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, o := range s.data[bucket][name] {
		if str(o.meta["generation"]) == generation {
			return o.data, o.meta, true
		}
	}
	return nil, nil, false
}

func (s *objectStore) meta(bucket, name string) (map[string]any, bool) {
	_, doc, ok := s.get(bucket, name)
	return doc, ok
}

// generations is every generation of every object in a bucket, objects in
// name order and generations oldest first -- which is the order GCS lists
// them in with versions=true.
func (s *objectStore) generations(bucket string) []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []map[string]any{}
	for _, name := range sortedNames(s.namesLocked(bucket)) {
		for _, o := range s.data[bucket][name] {
			out = append(out, o.meta)
		}
	}
	return out
}

func (s *objectStore) patchMeta(bucket, name string, patch map[string]any) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gens := s.data[bucket][name]
	i := liveIndex(gens)
	if i < 0 {
		return nil, false
	}
	o := gens[i]
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
	gens[i] = o
	s.flush(bucket, name)
	return o.meta, true
}

// delete removes the live generation, or ARCHIVES it when the bucket is
// versioned. Deleting an object that is not live succeeds either way,
// which is what makes a destroy idempotent.
func (s *objectStore) delete(bucket, name string, versioned bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gens := s.data[bucket][name]
	i := liveIndex(gens)
	if i < 0 {
		return
	}
	if versioned {
		gens[i].meta["timeDeleted"] = timestamp()
	} else {
		gens = append(gens[:i:i], gens[i+1:]...)
		if len(gens) == 0 {
			delete(s.data[bucket], name)
		} else {
			s.data[bucket][name] = gens
		}
	}
	s.flush(bucket, name)
}

// deleteGeneration removes one generation PERMANENTLY, which is how a
// client empties a versioned bucket.
func (s *objectStore) deleteGeneration(bucket, name, generation string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	gens := s.data[bucket][name]
	for i, o := range gens {
		if str(o.meta["generation"]) != generation {
			continue
		}
		gens = append(gens[:i:i], gens[i+1:]...)
		if len(gens) == 0 {
			delete(s.data[bucket], name)
		} else {
			s.data[bucket][name] = gens
		}
		s.flush(bucket, name)
		return true
	}
	return false
}

// names is the LIVE objects, which is what an ordinary listing shows. An
// object with nothing but archived generations is absent from it, exactly
// as a deleted one should be.
func (s *objectStore) names(bucket string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.data[bucket]))
	for name, gens := range s.data[bucket] {
		if liveIndex(gens) >= 0 {
			out = append(out, name)
		}
	}
	return out
}

// namesLocked is every name with any generation at all. Called with the
// lock held.
func (s *objectStore) namesLocked(bucket string) []string {
	out := make([]string, 0, len(s.data[bucket]))
	for name := range s.data[bucket] {
		out = append(out, name)
	}
	return out
}

// count is every generation in the bucket, archived ones included.
//
// Archived generations count because GCS refuses to delete a bucket that
// still holds any, and force_destroy depends on being told so: it lists
// with versions=true and deletes each generation by number, so reporting
// the bucket as empty after the live objects were deleted would stop it
// one pass early and the bucket delete would then fail with nothing
// connecting the two.
func (s *objectStore) count(bucket string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, gens := range s.data[bucket] {
		n += len(gens)
	}
	return n
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
//
// A generation gets its own file, suffixed "-g{n}". The LIVE generation
// keeps the unsuffixed name it has always had -- which is why a data
// directory written before versioning existed loads unchanged. Nothing
// parses these names back: load reads the bucket, the name and the
// generation out of the metadata, so the filename only has to be unique.
func (s *objectStore) pathFor(bucket, name string) string {
	return filepath.Join(s.dir, sanitise(bucket), numericID(name)+".obj")
}

func (s *objectStore) pathForGeneration(bucket, name, generation string) string {
	return filepath.Join(s.dir, sanitise(bucket),
		numericID(name)+"-g"+generation+".obj")
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

// flush writes an object's whole generation history out, live generation
// in the unsuffixed file. Called with the lock held.
//
// The key is rewritten rather than appended to, because the live
// generation moves: the file that held it a moment ago now holds an
// archived one, and leaving both would load the same generation twice.
func (s *objectStore) flush(bucket, name string) {
	if s.dir == "" {
		return
	}
	base := s.pathFor(bucket, name)
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		return
	}
	// Clear what was there. Globbing is safe because numericID is decimal
	// digits and the bucket directory is sanitised to letters, digits, "-"
	// and "_" -- so no name a client chooses can smuggle in a glob
	// metacharacter.
	for _, p := range globOf(base, strings.TrimSuffix(base, ".obj")+"-g*.obj") {
		_ = os.Remove(p)
		_ = os.Remove(p + ".json")
	}
	gens := s.data[bucket][name]
	live := liveIndex(gens)
	for i, o := range gens {
		path := base
		if i != live {
			path = s.pathForGeneration(bucket, name, str(o.meta["generation"]))
		}
		_ = os.WriteFile(path, o.data, 0o644)
		if meta, err := json.Marshal(o.meta); err == nil {
			_ = os.WriteFile(path+".json", meta, 0o644)
		}
	}
}

// globOf is the files a key owns: the unsuffixed one and every "-g{n}" one.
func globOf(base, pattern string) []string {
	out := []string{base}
	matches, _ := filepath.Glob(pattern)
	return append(out, matches...)
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
	ignored := 0
	for _, b := range buckets {
		if !b.IsDir() {
			ignored++
			continue
		}
		files, err := os.ReadDir(filepath.Join(s.dir, b.Name()))
		if err != nil {
			ignored++
			continue
		}
		for _, f := range files {
			// An object is a metadata file and a content file beside it.
			// The content file is reached from the metadata, so it is not
			// an orphan on its own account -- only a .json with nothing
			// behind it, or a file that is neither, is unaccounted for.
			if !strings.HasSuffix(f.Name(), ".json") {
				if _, err := os.Stat(filepath.Join(s.dir, b.Name(), f.Name()+".json")); err != nil {
					ignored++
				}
				continue
			}
			metaPath := filepath.Join(s.dir, b.Name(), f.Name())
			raw, err := os.ReadFile(metaPath)
			if err != nil {
				ignored++
				continue
			}
			var meta map[string]any
			if json.Unmarshal(raw, &meta) != nil {
				ignored++
				continue
			}
			data, err := os.ReadFile(strings.TrimSuffix(metaPath, ".json"))
			if err != nil {
				ignored++
				continue
			}
			// The bucket comes from the METADATA, not from the directory
			// name: the directory is a sanitised version and cannot be
			// turned back into the real name.
			bucket, name := str(meta["bucket"]), str(meta["name"])
			if bucket == "" || name == "" {
				ignored++
				continue
			}
			if s.data[bucket] == nil {
				s.data[bucket] = map[string][]storedObject{}
			}
			s.data[bucket][name] = append(s.data[bucket][name],
				storedObject{meta: meta, data: data})
		}
	}
	// Oldest generation first, which is the order every reader assumes --
	// recovered from the generation NUMBER rather than from the filename or
	// a timestamp, because the number is the thing a client orders by and
	// the directory hands files back in whatever order it likes.
	for _, names := range s.data {
		for _, gens := range names {
			if h := highestGeneration(gens); h > s.lastGen {
				s.lastGen = h
			}
			sort.SliceStable(gens, func(i, j int) bool {
				a, _ := strconv.ParseInt(str(gens[i].meta["generation"]), 10, 64)
				b, _ := strconv.ParseInt(str(gens[j].meta["generation"]), 10, 64)
				return a < b
			})
		}
	}
	datadir.ReportIgnored("cloud storage", s.dir, ignored)
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
