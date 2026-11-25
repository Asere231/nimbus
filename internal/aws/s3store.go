package aws

import (
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Asere231/nimbus/internal/fsname"
)

// metaSuffix names an object's metadata file.
//
// It ends in "%" because fsname.Encode can never produce a name that does:
// every "%" it emits is followed by two hex digits. So no key, whatever it is
// called, can collide with a metadata file -- and fsname.Decode refuses the
// name, which is a second reason load skips it.
//
// The old suffix was ".json", and that was a silent data-loss bug with
// nothing to do with any platform: load skipped every path ending in ".json",
// which is the object's OWN file whenever a key ends in ".json". Measured, on
// a bucket holding one object called config.json -- after a restart the
// bucket was empty and the key 404'd.
const metaSuffix = ".meta%"

// versionPrefix names a NONCURRENT version's file: "{key}.v{n}%", where n
// is its position in the key's history. It ends in "%" for the same reason
// metaSuffix does, which is what makes it unmistakably ours: an encoded key
// never ends in "%", so any name that does was appended by this file.
//
// The CURRENT version keeps the plain "{key}" path it has always had. That
// is not tidiness -- it means a data directory written before versioning
// existed loads unchanged, and the overwhelmingly common case (one version)
// is byte-for-byte what it was.
const versionPrefix = ".v"

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

// objectInfo is everything about an object version except its bytes.
type objectInfo struct {
	Key          string            `json:"key"`
	Size         int64             `json:"size"`
	ETag         string            `json:"etag"`
	ContentType  string            `json:"contentType"`
	LastModified time.Time         `json:"lastModified"`
	Metadata     map[string]string `json:"metadata,omitempty"`

	// VersionID is the id a client deletes by. It is "null" for an object
	// in a bucket that has never had versioning enabled -- real S3's own
	// id for one written before versioning, and a real id rather than an
	// absent field, because emptying a bucket is a list followed by a
	// delete per version.
	VersionID string `json:"versionId,omitempty"`

	// DeleteMarker is a version with no bytes, laid OVER the history by a
	// delete against a versioned bucket. The object then reads 404 while
	// every earlier version is still there, and removing the marker
	// undeletes it. This is the piece that makes versioning a history
	// rather than a label.
	DeleteMarker bool `json:"deleteMarker,omitempty"`
}

// objectStore holds every bucket's objects, each as a HISTORY.
//
// The value is a slice of versions, OLDEST FIRST, so the last element is
// the current one. Order lives in the slice rather than being recovered by
// sorting on a timestamp, because azurerm and the aws provider both write
// concurrently and two versions of one key can share a millisecond -- the
// same reason the activity feed sorts with nanosecond precision and the
// visualizer trusts the array.
type objectStore struct {
	mu  sync.RWMutex
	dir string // empty = memory only
	mem map[string]map[string][]storedObject

	// seq makes a version id unique within a process. The id is otherwise
	// derived, which keeps it stable once written -- what matters is that
	// two writes of one key in the same nanosecond cannot collide.
	seq int64
}

type storedObject struct {
	info objectInfo
	data []byte
}

func newObjectStore(dataDir string) *objectStore {
	s := &objectStore{mem: map[string]map[string][]storedObject{}}
	if dataDir != "" {
		s.dir = filepath.Join(dataDir, "s3")
		_ = os.MkdirAll(s.dir, 0o755)
	}
	return s
}

// nullVersionID is the version id S3 gives an object in a bucket where
// versioning has never been enabled, and the id a SUSPENDED bucket's
// writes carry. The literal string "null", not an absent field.
//
// There is at most one "null" version of a key at a time: a write while
// versioning is off or suspended REPLACES it, which is exactly how real S3
// behaves and why suspending versioning stops a bucket growing without
// discarding the history it already has.
const nullVersionID = "null"

// --- reading --------------------------------------------------------------

// get is the current version, which is what a request with no versionId
// asks for. A delete marker comes back like any other version: the caller
// decides what a 404 looks like, because GET and DELETE answer differently.
func (s *objectStore) get(bucket, key string) ([]byte, objectInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	vs := s.mem[bucket][key]
	if len(vs) == 0 {
		return nil, objectInfo{}, false
	}
	o := vs[len(vs)-1]
	return o.data, o.info, true
}

// getVersion is one named version, which may be a noncurrent one or a
// delete marker.
func (s *objectStore) getVersion(bucket, key, versionID string) ([]byte, objectInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, o := range s.mem[bucket][key] {
		if o.info.VersionID == versionID {
			return o.data, o.info, true
		}
	}
	return nil, objectInfo{}, false
}

// list is the current version of every object whose key starts with
// prefix, sorted by key, which is the order S3 guarantees. A key whose
// current version is a DELETE MARKER is absent, because that is what a
// delete against a versioned bucket means to anything that is not asking
// about versions.
func (s *objectStore) list(bucket, prefix string) []objectInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []objectInfo{}
	for key, vs := range s.mem[bucket] {
		if !strings.HasPrefix(key, prefix) || len(vs) == 0 {
			continue
		}
		if info := vs[len(vs)-1].info; !info.DeleteMarker {
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// versions is every version of every matching key, NEWEST FIRST within a
// key and keys in order -- which is the order ListObjectVersions reports,
// and it is what lets a caller mark the first of each key as IsLatest
// without asking the store again.
func (s *objectStore) versions(bucket, prefix string) []objectInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.mem[bucket]))
	for key := range s.mem[bucket] {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	out := []objectInfo{}
	for _, key := range keys {
		vs := s.mem[bucket][key]
		for i := len(vs) - 1; i >= 0; i-- {
			out = append(out, vs[i].info)
		}
	}
	return out
}

// count is every version in the bucket, delete markers included.
//
// Markers count because S3 refuses to delete a bucket that still holds any
// version at all: a bucket emptied by deleting its objects without their
// version ids is NOT empty, and the provider's force_destroy depends on
// being told so -- otherwise it would stop after one pass and ask for the
// bucket, which is the shape of the bug that made a versioned bucket
// impossible to destroy in the first place.
func (s *objectStore) count(bucket string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, vs := range s.mem[bucket] {
		n += len(vs)
	}
	return n
}

// --- writing --------------------------------------------------------------

// put stores a new version of an object and returns its info.
//
// The ETag is the MD5 of the content, which is what S3 returns for a
// single-part upload and what clients compare against to decide whether an
// upload needs repeating. Unlike Azure's Content-MD5 -- a content setting
// that must NOT be invented -- this one is always present and always the
// digest, so computing it is correct rather than a guess.
func (s *objectStore) put(bucket, key string, data []byte, info objectInfo, versioned bool) objectInfo {
	sum := md5.Sum(data)
	info.Key = key
	info.Size = int64(len(data))
	info.ETag = `"` + hex.EncodeToString(sum[:]) + `"`
	info.DeleteMarker = false
	return s.addVersion(bucket, key, append([]byte(nil), data...), info, versioned)
}

// putWithETag stores an object whose ETag is already decided, which is the
// multipart case: that ETag is the digest of the PART digests, not of the
// bytes, so computing it here from the content would be wrong.
func (s *objectStore) putWithETag(bucket, key string, data []byte, info objectInfo, versioned bool) objectInfo {
	etag := info.ETag
	stored := s.put(bucket, key, data, info, versioned)
	if etag == "" {
		return stored
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	vs := s.mem[bucket][key]
	if len(vs) == 0 {
		return stored
	}
	vs[len(vs)-1].info.ETag = etag
	s.writeKey(bucket, key, vs)
	return vs[len(vs)-1].info
}

// putMarker lays a delete marker over a key's history. Only ever called
// for a bucket with versioning configured: without it there is no history
// to lay anything over and a delete simply removes the object.
func (s *objectStore) putMarker(bucket, key string, versioned bool) objectInfo {
	return s.addVersion(bucket, key, nil, objectInfo{
		Key: key, DeleteMarker: true, ContentType: "",
	}, versioned)
}

// addVersion appends a version, replacing the "null" one if this write is
// not versioned. One function for both, because the replace rule is the
// only difference between an enabled bucket and a suspended one.
func (s *objectStore) addVersion(bucket, key string, data []byte, info objectInfo, versioned bool) objectInfo {
	info.LastModified = time.Now().UTC().Truncate(time.Second)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mem[bucket] == nil {
		s.mem[bucket] = map[string][]storedObject{}
	}
	vs := s.mem[bucket][key]
	if versioned {
		s.seq++
		info.VersionID = stableID(bucket+"/"+key+"/"+
			strconv.FormatInt(time.Now().UnixNano(), 10)+"/"+
			strconv.FormatInt(s.seq, 10), 32)
	} else {
		info.VersionID = nullVersionID
		// At most one "null" version at a time.
		kept := vs[:0:0]
		for _, o := range vs {
			if o.info.VersionID != nullVersionID {
				kept = append(kept, o)
			}
		}
		vs = kept
	}
	vs = append(vs, storedObject{info: info, data: data})
	s.mem[bucket][key] = vs
	s.writeKey(bucket, key, vs)
	return info
}

// deleteVersion removes one named version permanently. Removing a delete
// marker is how an object comes back.
func (s *objectStore) deleteVersion(bucket, key, versionID string) (objectInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vs := s.mem[bucket][key]
	for i, o := range vs {
		if o.info.VersionID != versionID {
			continue
		}
		vs = append(vs[:i:i], vs[i+1:]...)
		if len(vs) == 0 {
			delete(s.mem[bucket], key)
		} else {
			s.mem[bucket][key] = vs
		}
		s.writeKey(bucket, key, vs)
		return o.info, true
	}
	return objectInfo{}, false
}

// deleteKey removes a key and every version of it. This is an UNVERSIONED
// bucket's delete, where there is only ever one version anyway, and the
// path a bucket delete takes.
func (s *objectStore) deleteKey(bucket, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.mem[bucket][key]) == 0 {
		return false
	}
	delete(s.mem[bucket], key)
	s.writeKey(bucket, key, nil)
	return true
}

// deleteBucket removes every object in a bucket.
func (s *objectStore) deleteBucket(bucket string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.mem, bucket)
	if s.dir != "" {
		_ = os.RemoveAll(filepath.Join(s.dir, fsname.Encode(bucket)))
	}
}
