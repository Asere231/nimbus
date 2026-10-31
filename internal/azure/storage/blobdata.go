package storage

import (
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Blob is one stored blob: its bytes plus the properties clients can set.
type Blob struct {
	Name         string
	Content      []byte
	ContentType  string
	CacheControl string
	Encoding     string
	Disposition  string
	Language     string
	// ContentMD5 is the content-setting the client supplied, stored verbatim.
	// Azure echoes back exactly what was sent rather than recomputing, and
	// azurerm compares the returned string against what it put in state -- it
	// sends hex, not the base64 of the Content-MD5 response header, so
	// recomputing makes content_md5 look changed and forces a replacement on
	// every plan.
	ContentMD5 string
	Metadata   map[string]string
	BlobType   string // BlockBlob; Nimbus does not emulate page or append blobs
	ETag       string
	Modified   time.Time

	// Uncommitted blocks from Put Block, keyed by block ID. A large upload
	// arrives as many of these and is assembled by Put Block List.
	blocks map[string][]byte
}

// MD5 is the Content-MD5 content setting, which is EMPTY unless the client
// set one with x-ms-blob-content-md5.
//
// Do not be tempted to compute it from the bytes. Content-MD5 is a content
// setting on Azure, not an automatic property, and a blob uploaded without one
// has none. Returning a computed value makes azurerm store it in state on the
// first read, after which the config (which says nothing about content_md5)
// disagrees with state and every plan wants to replace the blob --
// content_md5 is ForceNew.
func (b *Blob) MD5() string { return b.ContentMD5 }

// Digest is the actual MD5 of the stored bytes, for callers that want to
// verify content rather than report a property.
func (b *Blob) Digest() string {
	sum := md5.Sum(b.Content)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Container holds blobs and its own metadata.
type Container struct {
	Name         string
	Metadata     map[string]string
	PublicAccess string // "", "blob" or "container"
	ETag         string
	Modified     time.Time

	mu    sync.RWMutex
	blobs map[string]*Blob
}

// ServiceProperties is the blob service's settings document. azurerm reads
// this immediately after creating an account to decide the data plane is up,
// and writes it back for logging, CORS and static-website settings.
type ServiceProperties struct {
	Raw []byte // the exact XML last written, echoed back verbatim
}

func newETag() string {
	return fmt.Sprintf("\"0x%X\"", time.Now().UTC().UnixNano())
}

// --- container operations on an account ------------------------------------

// CreateContainer adds a container. ok is false if it already exists, which
// Azure reports as ContainerAlreadyExists rather than silently succeeding.
func (a *Account) CreateContainer(name string, meta map[string]string, access string) (*Container, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.containers == nil {
		a.containers = map[string]*Container{}
	}
	if _, exists := a.containers[strings.ToLower(name)]; exists {
		return nil, false
	}
	c := &Container{
		Name: name, Metadata: meta, PublicAccess: access,
		ETag: newETag(), Modified: time.Now().UTC(),
		blobs: map[string]*Blob{},
	}
	a.containers[strings.ToLower(name)] = c
	return c, true
}

// Container returns a container, or nil.
func (a *Account) Container(name string) *Container {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.containers[strings.ToLower(name)]
}

// DeleteContainer removes a container and its blobs.
func (a *Account) DeleteContainer(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := strings.ToLower(name)
	if _, ok := a.containers[k]; !ok {
		return false
	}
	delete(a.containers, k)
	return true
}

// Containers lists containers by name.
func (a *Account) Containers() []*Container {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]*Container, 0, len(a.containers))
	for _, c := range a.containers {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// --- blob operations on a container ----------------------------------------

// PutBlob stores a blob, replacing any existing one of the same name.
func (c *Container) PutBlob(b *Blob) *Blob {
	c.mu.Lock()
	defer c.mu.Unlock()
	b.ETag = newETag()
	b.Modified = time.Now().UTC()
	if b.Metadata == nil {
		b.Metadata = map[string]string{}
	}
	if b.BlobType == "" {
		b.BlobType = "BlockBlob"
	}
	c.blobs[b.Name] = b
	return b
}

// Blob returns a blob, or nil. Blob names ARE case-sensitive, unlike account
// and container names.
func (c *Container) Blob(name string) *Blob {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.blobs[name]
}

// DeleteBlob removes a blob.
func (c *Container) DeleteBlob(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.blobs[name]; !ok {
		return false
	}
	delete(c.blobs, name)
	return true
}

// Blobs lists blobs whose names start with prefix, sorted. Listing is always
// in name order, which is what the delimiter-based directory walk relies on.
func (c *Container) Blobs(prefix string) []*Blob {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := []*Blob{}
	for _, b := range c.blobs {
		if strings.HasPrefix(b.Name, prefix) {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Count returns how many blobs the container holds, and their total size.
func (c *Container) Count() (n int, bytes int64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, b := range c.blobs {
		n++
		bytes += int64(len(b.Content))
	}
	return n, bytes
}

// StageBlock holds one uncommitted block for a later Put Block List.
func (c *Container) StageBlock(blobName, blockID string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.blobs[blobName]
	if b == nil {
		b = &Blob{Name: blobName, BlobType: "BlockBlob", Metadata: map[string]string{}}
		c.blobs[blobName] = b
	}
	if b.blocks == nil {
		b.blocks = map[string][]byte{}
	}
	b.blocks[blockID] = data
}

// CommitBlocks assembles a blob from previously staged blocks, in the order
// the client listed them. Unlisted blocks are discarded, as Azure does.
func (c *Container) CommitBlocks(blobName string, ids []string) (*Blob, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.blobs[blobName]
	if b == nil || b.blocks == nil {
		return nil, false
	}
	var content []byte
	for _, id := range ids {
		data, ok := b.blocks[id]
		if !ok {
			return nil, false
		}
		content = append(content, data...)
	}
	b.Content = content
	b.blocks = nil
	b.ETag = newETag()
	b.Modified = time.Now().UTC()
	if b.Metadata == nil {
		b.Metadata = map[string]string{}
	}
	return b, true
}
