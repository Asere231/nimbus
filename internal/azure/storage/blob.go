package storage

import (
	"encoding/xml"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The blob service. Paths are "/", "/{container}" or "/{container}/{blob}",
// and the operation is decided by the method plus the restype and comp query
// parameters -- there are no verbs in the path.
//
//	GET  /?comp=properties&restype=service   service settings (azurerm polls this)
//	GET  /?comp=list                         list containers
//	PUT  /{c}?restype=container              create container
//	GET  /{c}?restype=container&comp=list    list blobs
//	PUT  /{c}/{b}                            put blob
//	GET  /{c}/{b}                            get blob
func (h *Handler) serveBlob(w http.ResponseWriter, r *http.Request, a *Account) string {
	q := r.URL.Query()
	restype, comp := q.Get("restype"), q.Get("comp")
	container, blob := splitBlobPath(r.URL.Path)

	switch {
	case container == "" && restype == "service":
		return h.serviceProperties(w, r, a, ServiceBlob, comp)
	case container == "" && comp == "list":
		return h.listContainers(w, a, q)
	case container == "":
		WriteError(w, 400, "InvalidUri", "The request URI is not valid.")
		return "blob: unrecognised account-level request"

	case blob == "" && restype == "container":
		return h.containerOp(w, r, a, container, comp)
	case blob == "":
		WriteError(w, 400, "InvalidUri", "Missing restype=container.")
		return "blob: container request without restype"

	default:
		return h.blobOp(w, r, a, container, blob, comp)
	}
}

// splitBlobPath turns "/container/some/blob.txt" into ("container",
// "some/blob.txt"). Blob names routinely contain slashes -- that is how the
// portal fakes directories -- so only the first segment is the container.
func splitBlobPath(p string) (container, blob string) {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", ""
	}
	if i := strings.Index(p, "/"); i >= 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}

// --- service level ---------------------------------------------------------

// serviceProperties stores and returns the service settings document verbatim.
//
// azurerm polls GET here right after creating an account to decide the data
// plane is available, and fails the apply if it does not answer. The document
// also carries the static-website settings, which azurerm reads back, so
// echoing exactly what was written keeps the plan clean.
func (h *Handler) serviceProperties(w http.ResponseWriter, r *http.Request, a *Account, svc Service, comp string) string {
	if comp == "stats" {
		writeXML(w, 200, struct {
			XMLName xml.Name `xml:"StorageServiceStats"`
			Geo     struct {
				Status       string `xml:"Status"`
				LastSyncTime string `xml:"LastSyncTime"`
			} `xml:"GeoReplication"`
		}{})
		return ""
	}
	switch r.Method {
	case http.MethodGet:
		if raw, ok := a.ServiceProps[svc]; ok && len(raw) > 0 {
			w.Header().Set("Content-Type", "application/xml")
			setCommonHeaders(w)
			w.WriteHeader(200)
			_, _ = w.Write(raw)
			return ""
		}
		writeXML(w, 200, defaultServiceProperties(svc))
		return ""
	case http.MethodPut:
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			WriteError(w, 400, "InvalidXmlDocument", "The XML document could not be read.")
			return ""
		}
		if a.ServiceProps == nil {
			a.ServiceProps = map[Service][]byte{}
		}
		a.ServiceProps[svc] = raw
		setCommonHeaders(w)
		w.WriteHeader(202)
		return ""
	}
	WriteError(w, 405, "UnsupportedHttpVerb", "The resource does not support "+r.Method+".")
	return ""
}

// StorageServiceProperties is the settings document. Only the fields clients
// actually read are modelled; the rest round-trip as raw XML because PUT
// stores the body verbatim.
type StorageServiceProperties struct {
	XMLName       xml.Name `xml:"StorageServiceProperties"`
	DefaultVer    string   `xml:"DefaultServiceVersion,omitempty"`
	Logging       *logging `xml:"Logging,omitempty"`
	HourMetrics   *metrics `xml:"HourMetrics,omitempty"`
	MinuteMetrics *metrics `xml:"MinuteMetrics,omitempty"`
	Cors          struct{} `xml:"Cors"`
	StaticWebsite *struct {
		Enabled bool `xml:"Enabled"`
	} `xml:"StaticWebsite,omitempty"`
}

type logging struct {
	Version         string `xml:"Version"`
	Delete          bool   `xml:"Delete"`
	Read            bool   `xml:"Read"`
	Write           bool   `xml:"Write"`
	RetentionPolicy struct {
		Enabled bool `xml:"Enabled"`
	} `xml:"RetentionPolicy"`
}

type metrics struct {
	Version         string `xml:"Version"`
	Enabled         bool   `xml:"Enabled"`
	IncludeAPIs     *bool  `xml:"IncludeAPIs,omitempty"`
	RetentionPolicy struct {
		Enabled bool `xml:"Enabled"`
	} `xml:"RetentionPolicy"`
}

func defaultServiceProperties(svc Service) StorageServiceProperties {
	p := StorageServiceProperties{
		Logging:       &logging{Version: "1.0"},
		HourMetrics:   &metrics{Version: "1.0", Enabled: false},
		MinuteMetrics: &metrics{Version: "1.0", Enabled: false},
	}
	if svc == ServiceBlob {
		p.StaticWebsite = &struct {
			Enabled bool `xml:"Enabled"`
		}{Enabled: false}
	}
	return p
}

// EnumerationResults is the container listing document.
type containerList struct {
	XMLName    xml.Name        `xml:"EnumerationResults"`
	Endpoint   string          `xml:"ServiceEndpoint,attr"`
	Prefix     string          `xml:"Prefix,omitempty"`
	Containers []containerItem `xml:"Containers>Container"`
	NextMarker string          `xml:"NextMarker"`
}

type containerItem struct {
	Name       string            `xml:"Name"`
	Properties containerProps    `xml:"Properties"`
	Metadata   map[string]string `xml:"-"`
}

type containerProps struct {
	LastModified string `xml:"Last-Modified"`
	ETag         string `xml:"Etag"`
	LeaseStatus  string `xml:"LeaseStatus"`
	LeaseState   string `xml:"LeaseState"`
	PublicAccess string `xml:"PublicAccess,omitempty"`
	HasImmutabil bool   `xml:"HasImmutabilityPolicy"`
	HasLegalHold bool   `xml:"HasLegalHold"`
}

func (h *Handler) listContainers(w http.ResponseWriter, a *Account, q map[string][]string) string {
	prefix := first(q, "prefix")
	out := containerList{Endpoint: "https://" + a.Name + ".blob/"}
	out.Prefix = prefix
	for _, c := range a.Containers() {
		if !strings.HasPrefix(c.Name, prefix) {
			continue
		}
		out.Containers = append(out.Containers, containerItem{
			Name: c.Name,
			Properties: containerProps{
				LastModified: httpTime(c.Modified), ETag: c.ETag,
				LeaseStatus: "unlocked", LeaseState: "available",
				PublicAccess: c.PublicAccess,
			},
		})
	}
	writeXML(w, 200, out)
	return ""
}

// --- container level -------------------------------------------------------

func (h *Handler) containerOp(w http.ResponseWriter, r *http.Request, a *Account, name, comp string) string {
	switch {
	case r.Method == http.MethodPut && comp == "":
		meta := metadataFrom(r.Header)
		access := r.Header.Get("x-ms-blob-public-access")
		c, created := a.CreateContainer(name, meta, access)
		if !created {
			WriteError(w, 409, "ContainerAlreadyExists", "The specified container already exists.")
			return ""
		}
		h.changed(a.Name, name, "create")
		h.persist(a)
		w.Header().Set("ETag", c.ETag)
		w.Header().Set("Last-Modified", httpTime(c.Modified))
		setCommonHeaders(w)
		w.WriteHeader(201)
		return ""

	case r.Method == http.MethodDelete:
		if !a.DeleteContainer(name) {
			WriteError(w, 404, "ContainerNotFound", "The specified container does not exist.")
			return ""
		}
		h.changed(a.Name, name, "delete")
		h.persist(a)
		setCommonHeaders(w)
		w.WriteHeader(202)
		return ""

	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		c := a.Container(name)
		if c == nil {
			WriteError(w, 404, "ContainerNotFound", "The specified container does not exist.")
			return ""
		}
		switch comp {
		case "list":
			return h.listBlobs(w, r, a, c)
		case "acl":
			writeXML(w, 200, struct {
				XMLName xml.Name `xml:"SignedIdentifiers"`
			}{})
			return ""
		case "metadata", "":
			setMetadata(w, c.Metadata)
			if c.PublicAccess != "" {
				w.Header().Set("x-ms-blob-public-access", c.PublicAccess)
			}
			w.Header().Set("ETag", c.ETag)
			w.Header().Set("Last-Modified", httpTime(c.Modified))
			w.Header().Set("x-ms-lease-status", "unlocked")
			w.Header().Set("x-ms-lease-state", "available")
			w.Header().Set("x-ms-has-immutability-policy", "false")
			w.Header().Set("x-ms-has-legal-hold", "false")
			setCommonHeaders(w)
			w.WriteHeader(200)
			return ""
		}

	case r.Method == http.MethodPut && (comp == "metadata" || comp == "acl"):
		c := a.Container(name)
		if c == nil {
			WriteError(w, 404, "ContainerNotFound", "The specified container does not exist.")
			return ""
		}
		if comp == "metadata" {
			c.Metadata = metadataFrom(r.Header)
		}
		c.ETag, c.Modified = newETag(), time.Now().UTC()
		w.Header().Set("ETag", c.ETag)
		w.Header().Set("Last-Modified", httpTime(c.Modified))
		setCommonHeaders(w)
		w.WriteHeader(200)
		return ""
	}
	WriteError(w, 400, "InvalidUri", "Unsupported container operation.")
	return "blob: unsupported container op " + r.Method + " comp=" + comp
}

// --- blob listing ----------------------------------------------------------

type blobList struct {
	XMLName    xml.Name  `xml:"EnumerationResults"`
	Endpoint   string    `xml:"ServiceEndpoint,attr"`
	Container  string    `xml:"ContainerName,attr"`
	Prefix     string    `xml:"Prefix,omitempty"`
	Delimiter  string    `xml:"Delimiter,omitempty"`
	MaxResults int       `xml:"MaxResults,omitempty"`
	Blobs      blobsNode `xml:"Blobs"`
	NextMarker string    `xml:"NextMarker"`
}

type blobsNode struct {
	Blob       []blobItem   `xml:"Blob"`
	BlobPrefix []prefixItem `xml:"BlobPrefix"`
}

type prefixItem struct {
	Name string `xml:"Name"`
}

type blobItem struct {
	Name       string    `xml:"Name"`
	Properties blobProps `xml:"Properties"`
}

type blobProps struct {
	LastModified string `xml:"Last-Modified"`
	ETag         string `xml:"Etag"`
	Length       int    `xml:"Content-Length"`
	ContentType  string `xml:"Content-Type"`
	ContentMD5   string `xml:"Content-MD5,omitempty"`
	BlobType     string `xml:"BlobType"`
	LeaseStatus  string `xml:"LeaseStatus"`
	LeaseState   string `xml:"LeaseState"`
	AccessTier   string `xml:"AccessTier,omitempty"`
	ServerEncr   bool   `xml:"ServerEncrypted"`
}

// listBlobs implements the prefix/delimiter walk. With a delimiter, names that
// share a prefix up to the next delimiter collapse into a single BlobPrefix --
// that is what makes `az storage blob list` look like it has directories.
func (h *Handler) listBlobs(w http.ResponseWriter, r *http.Request, a *Account, c *Container) string {
	q := r.URL.Query()
	prefix, delim := q.Get("prefix"), q.Get("delimiter")
	max, _ := strconv.Atoi(q.Get("maxresults"))

	out := blobList{
		Endpoint:  "https://" + a.Name + ".blob/",
		Container: c.Name, Prefix: prefix, Delimiter: delim,
	}
	seen := map[string]bool{}
	for _, b := range c.Blobs(prefix) {
		if delim != "" {
			rest := strings.TrimPrefix(b.Name, prefix)
			if i := strings.Index(rest, delim); i >= 0 {
				p := prefix + rest[:i+len(delim)]
				if !seen[p] {
					seen[p] = true
					out.Blobs.BlobPrefix = append(out.Blobs.BlobPrefix, prefixItem{Name: p})
				}
				continue
			}
		}
		out.Blobs.Blob = append(out.Blobs.Blob, blobItem{
			Name: b.Name,
			Properties: blobProps{
				LastModified: httpTime(b.Modified), ETag: b.ETag,
				Length: len(b.Content), ContentType: contentTypeOf(b),
				ContentMD5: b.MD5(), BlobType: b.BlobType,
				LeaseStatus: "unlocked", LeaseState: "available",
				AccessTier: "Hot", ServerEncr: true,
			},
		})
		if max > 0 && len(out.Blobs.Blob) >= max {
			break
		}
	}
	writeXML(w, 200, out)
	return ""
}

func contentTypeOf(b *Blob) string {
	if b.ContentType != "" {
		return b.ContentType
	}
	return "application/octet-stream"
}

func first(q map[string][]string, k string) string {
	if v := q[k]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// metadataFrom collects x-ms-meta-* headers, lower-casing the name after the
// prefix.
//
// Clients compare metadata names literally -- the Python SDK strips
// "x-ms-meta-" off whatever it receives and uses the rest as a dictionary key,
// so "Owner" and "owner" are different entries to it. We cannot echo the case
// the client sent, because Go's server canonicalises inbound header names
// while parsing the request ("x-ms-meta-owner" is already "X-Ms-Meta-Owner" by
// the time a handler sees it) and the original is unrecoverable. Lower-case is
// what clients send and what they expect back, so that is what is stored.
func metadataFrom(h http.Header) map[string]string {
	out := map[string]string{}
	for name, vals := range h {
		if len(vals) == 0 {
			continue
		}
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-ms-meta-") && len(lower) > len("x-ms-meta-") {
			out[lower[len("x-ms-meta-"):]] = vals[0]
		}
	}
	return out
}

// setMetadata writes x-ms-meta-* headers with their names exactly as stored.
// Assigning into the map directly skips the canonicalisation Header.Set
// applies, which would turn "x-ms-meta-owner" into "X-Ms-Meta-Owner" and make
// the client read the name back as "Owner".
func setMetadata(w http.ResponseWriter, meta map[string]string) {
	for k, v := range meta {
		w.Header()["x-ms-meta-"+k] = []string{v}
	}
}

func httpTime(t time.Time) string { return t.UTC().Format(http.TimeFormat) }

func writeXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/xml")
	setCommonHeaders(w)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(v)
}
