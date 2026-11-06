package aws

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/core"
)

// S3, path-style.
//
// Path-style (`/bucket/key`) rather than virtual-hosted
// (`bucket.s3.amazonaws.com/key`) is why AWS needs no TLS here and no DNS
// work: there is no per-bucket hostname to put on a certificate. The
// provider is told with `s3_use_path_style = true` and the CLI with
// `addressing_style = path`.
//
// Unlike EC2, this is REST: the method and the path say what to do, and
// the sub-resource is a query parameter with no value -- `?versioning`,
// `?tagging`, `?policy`. A bucket's configuration is therefore not one
// document but a dozen, each with its own GET, PUT and DELETE.
//
// The lesson that shapes all of it, learned on Azure's Key Vault and
// repeated here: AFTER CREATING A BUCKET THE PROVIDER READS EVERY ONE OF
// THOSE SUB-RESOURCES. Each must answer the way AWS answers when nothing
// has been set -- which for most of them is a specific 404 code that the
// provider is written to tolerate. A 501, or the wrong code, fails the
// apply on a bucket that was created a moment ago.

const typeBucket = "AWS::S3::Bucket"

// bucket is the stored shape. The sub-configurations live on it because
// they belong to the bucket, even though each is read separately.
type bucket struct {
	Name         string             `json:"name"`
	ARN          string             `json:"arn"`
	Region       string             `json:"region"`
	CreationDate time.Time          `json:"creationDate"`
	Versioning   string             `json:"versioning,omitempty"` // "", Enabled, Suspended
	Tags         []tag              `json:"tags,omitempty"`
	Policy       string             `json:"policy,omitempty"`
	Encryption   *s3Encryption      `json:"encryption,omitempty"`
	PublicAccess *publicAccessBlock `json:"publicAccessBlock,omitempty"`
	Ownership    string             `json:"ownership,omitempty"`
	ACL          string             `json:"acl,omitempty"`
}

type s3Encryption struct {
	Algorithm string `json:"algorithm"`
	KMSKeyID  string `json:"kmsKeyId,omitempty"`
}

type publicAccessBlock struct {
	BlockPublicAcls       bool `json:"blockPublicAcls"`
	IgnorePublicAcls      bool `json:"ignorePublicAcls"`
	BlockPublicPolicy     bool `json:"blockPublicPolicy"`
	RestrictPublicBuckets bool `json:"restrictPublicBuckets"`
}

func bucketARN(name string) string { return "arn:aws:s3:::" + name }

func (s *Server) loadBucket(name string) (*bucket, *apiError) {
	res, err := s.store.Get(bucketARN(name))
	if err != nil || res.Type != typeBucket {
		return nil, &apiError{Status: 404, Code: "NoSuchBucket",
			Message: "The specified bucket does not exist", Resource: "/" + name}
	}
	var b bucket
	if err := json.Unmarshal(res.Body, &b); err != nil {
		return nil, errorf(500, "InternalError", "reading the bucket: %v", err)
	}
	return &b, nil
}

func (s *Server) saveBucket(b *bucket) *apiError {
	body, err := json.Marshal(b)
	if err != nil {
		return errorf(500, "InternalError", "storing the bucket: %v", err)
	}
	s.store.Put(&core.Resource{
		Cloud: "aws", ID: b.ARN, Type: typeBucket, Name: b.Name,
		ParentID: s.accountARN(), Location: b.Region, Backing: "real", Body: body,
	})
	return nil
}

// serveS3 routes on the method, the path and which sub-resource query
// parameter is present.
func (s *Server) serveS3(w http.ResponseWriter, r *http.Request) string {
	path := strings.TrimPrefix(r.URL.Path, "/")
	name, key, _ := strings.Cut(path, "/")

	var aerr *apiError
	switch {
	case name == "":
		if r.Method != http.MethodGet {
			aerr = errorf(405, "MethodNotAllowed", "%s on the service root", r.Method)
			break
		}
		aerr = s.listBuckets(w)
	case key == "":
		aerr = s.serveBucket(w, r, name)
	default:
		aerr = s.serveObject(w, r, name, key)
	}

	if aerr != nil {
		writeError(w, "s3", aerr)
		return "s3 " + r.Method + " " + r.URL.Path + ": " + aerr.Code
	}
	return ""
}

// subResource is the query parameter that names which part of a bucket's
// configuration a request is about: ?versioning, ?tagging and so on. They
// carry no value, so Query().Get returns "" for all of them and the raw
// query has to be inspected.
func subResource(r *http.Request) string {
	for key := range r.URL.Query() {
		switch key {
		case "versioning", "tagging", "policy", "acl", "cors", "website", "lifecycle",
			"encryption", "publicAccessBlock", "ownershipControls", "replication",
			"accelerate", "requestPayment", "logging", "notification", "object-lock",
			"location", "uploads", "delete", "policyStatus", "intelligent-tiering",
			"analytics", "inventory", "metrics", "versions":
			return key
		}
	}
	return ""
}

func (s *Server) serveBucket(w http.ResponseWriter, r *http.Request, name string) *apiError {
	sub := subResource(r)

	// Create and head come first: they are the only ones that may run
	// before the bucket exists.
	if sub == "" {
		switch r.Method {
		case http.MethodPut:
			return s.createBucket(w, r, name)
		case http.MethodHead:
			return s.headBucket(w, name)
		case http.MethodDelete:
			return s.deleteBucket(w, name)
		case http.MethodGet:
			return s.listObjects(w, r, name)
		}
		return errorf(405, "MethodNotAllowed", "%s on a bucket", r.Method)
	}

	b, aerr := s.loadBucket(name)
	if aerr != nil {
		return aerr
	}
	switch r.Method {
	case http.MethodGet:
		return s.getBucketConfig(w, r, b, sub)
	case http.MethodPut:
		return s.putBucketConfig(w, r, b, sub)
	case http.MethodDelete:
		return s.deleteBucketConfig(w, b, sub)
	case http.MethodPost:
		if sub == "delete" {
			return s.deleteObjects(w, r, b)
		}
	}
	return errorf(405, "MethodNotAllowed", "%s on ?%s", r.Method, sub)
}

// --- the bucket itself ----------------------------------------------------

func (s *Server) createBucket(w http.ResponseWriter, r *http.Request, name string) *apiError {
	if existing, _ := s.store.Get(bucketARN(name)); existing != nil {
		// A bucket you already own is not an error for the owner in most
		// regions, and the provider relies on that when it retries.
		w.Header().Set("Location", "/"+name)
		w.WriteHeader(200)
		return nil
	}
	b := &bucket{
		Name:         name,
		ARN:          bucketARN(name),
		Region:       regionOf(r, s.cfg.Region),
		CreationDate: time.Now().UTC(),
	}
	if acl := r.Header.Get("x-amz-acl"); acl != "" {
		b.ACL = acl
	}
	if aerr := s.saveBucket(b); aerr != nil {
		return aerr
	}
	w.Header().Set("Location", "/"+name)
	w.WriteHeader(200)
	return nil
}

func (s *Server) headBucket(w http.ResponseWriter, name string) *apiError {
	b, aerr := s.loadBucket(name)
	if aerr != nil {
		// HEAD has no body, so the code goes in a header -- otherwise the
		// client sees a bare 404 and cannot tell why.
		return aerr
	}
	w.Header().Set("x-amz-bucket-region", b.Region)
	w.WriteHeader(200)
	return nil
}

func (s *Server) deleteBucket(w http.ResponseWriter, name string) *apiError {
	b, aerr := s.loadBucket(name)
	if aerr != nil {
		return aerr
	}
	// S3 refuses to delete a bucket with anything in it, and the provider
	// depends on that: force_destroy works by emptying it first, and a
	// silent cascade would hide a bucket it meant to keep.
	if n := s.objects.count(name); n > 0 {
		return &apiError{Status: 409, Code: "BucketNotEmpty",
			Message: "The bucket you tried to delete is not empty", Resource: "/" + name}
	}
	if _, err := s.store.Delete(b.ARN); err != nil {
		return &apiError{Status: 404, Code: "NoSuchBucket", Message: "The specified bucket does not exist"}
	}
	s.objects.deleteBucket(name)
	w.WriteHeader(204)
	return nil
}

type listAllBucketsResult struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	Xmlns   string   `xml:"xmlns,attr"`
	Owner   struct {
		ID          string `xml:"ID"`
		DisplayName string `xml:"DisplayName"`
	} `xml:"Owner"`
	Buckets []listedBucket `xml:"Buckets>Bucket"`
}

type listedBucket struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

func (s *Server) listBuckets(w http.ResponseWriter) *apiError {
	doc := listAllBucketsResult{Xmlns: s3Namespace}
	doc.Owner.ID = stableID(s.cfg.AccountID, 64)
	doc.Owner.DisplayName = "nimbus"
	for _, res := range s.store.List(func(r *core.Resource) bool { return r.Type == typeBucket }) {
		var b bucket
		if json.Unmarshal(res.Body, &b) != nil {
			continue
		}
		doc.Buckets = append(doc.Buckets, listedBucket{
			Name:         b.Name,
			CreationDate: b.CreationDate.Format(time.RFC3339),
		})
	}
	writeXML(w, 200, doc)
	return nil
}

const s3Namespace = "http://s3.amazonaws.com/doc/2006-03-01/"

// --- the sub-configuration matrix ----------------------------------------

// notConfigured is the error S3 gives for a sub-resource nobody has set.
//
// These codes are not interchangeable and they are not optional. The
// provider reads every one of them after creating a bucket and is written
// to tolerate exactly this answer; anything else -- a 501, a 200 with an
// empty document, the wrong code -- fails the apply. This is the Key Vault
// certificate-contacts lesson from Milestone 4, one service later.
var notConfigured = map[string]string{
	"tagging":           "NoSuchTagSet",
	"policy":            "NoSuchBucketPolicy",
	"cors":              "NoSuchCORSConfiguration",
	"website":           "NoSuchWebsiteConfiguration",
	"lifecycle":         "NoSuchLifecycleConfiguration",
	"replication":       "ReplicationConfigurationNotFoundError",
	"publicAccessBlock": "NoSuchPublicAccessBlockConfiguration",
	"encryption":        "ServerSideEncryptionConfigurationNotFoundError",
	"ownershipControls": "OwnershipControlsNotFoundError",
	"object-lock":       "ObjectLockConfigurationNotFoundError",
	"policyStatus":      "NoSuchPublicAccessBlockConfiguration",
}

func (s *Server) getBucketConfig(w http.ResponseWriter, r *http.Request, b *bucket, sub string) *apiError {
	switch sub {
	case "location":
		// An empty LocationConstraint means us-east-1, which is S3's
		// oldest wart and still how every client reads it.
		constraint := b.Region
		if constraint == "us-east-1" {
			constraint = ""
		}
		writeXML(w, 200, struct {
			XMLName xml.Name `xml:"LocationConstraint"`
			Xmlns   string   `xml:"xmlns,attr"`
			Value   string   `xml:",chardata"`
		}{Xmlns: s3Namespace, Value: constraint})
		return nil

	case "versioning":
		// Never an error: an unversioned bucket answers with an empty
		// document rather than a 404.
		doc := struct {
			XMLName xml.Name `xml:"VersioningConfiguration"`
			Xmlns   string   `xml:"xmlns,attr"`
			Status  string   `xml:"Status,omitempty"`
		}{Xmlns: s3Namespace, Status: b.Versioning}
		writeXML(w, 200, doc)
		return nil

	case "tagging":
		if len(b.Tags) == 0 {
			return notSet(sub, b.Name)
		}
		type xmlTag struct {
			Key   string `xml:"Key"`
			Value string `xml:"Value"`
		}
		doc := struct {
			XMLName xml.Name `xml:"Tagging"`
			Xmlns   string   `xml:"xmlns,attr"`
			TagSet  []xmlTag `xml:"TagSet>Tag"`
		}{Xmlns: s3Namespace}
		for _, t := range b.Tags {
			doc.TagSet = append(doc.TagSet, xmlTag{Key: t.Key, Value: t.Value})
		}
		writeXML(w, 200, doc)
		return nil

	case "policy":
		if b.Policy == "" {
			return notSet(sub, b.Name)
		}
		// A bucket policy is JSON, not XML -- the one S3 call that is.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, b.Policy)
		return nil

	case "encryption":
		if b.Encryption == nil {
			return notSet(sub, b.Name)
		}
		type rule struct {
			Apply struct {
				Algorithm string `xml:"SSEAlgorithm"`
				KMSKeyID  string `xml:"KMSMasterKeyID,omitempty"`
			} `xml:"ApplyServerSideEncryptionByDefault"`
		}
		doc := struct {
			XMLName xml.Name `xml:"ServerSideEncryptionConfiguration"`
			Xmlns   string   `xml:"xmlns,attr"`
			Rules   []rule   `xml:"Rule"`
		}{Xmlns: s3Namespace}
		var ru rule
		ru.Apply.Algorithm = b.Encryption.Algorithm
		ru.Apply.KMSKeyID = b.Encryption.KMSKeyID
		doc.Rules = append(doc.Rules, ru)
		writeXML(w, 200, doc)
		return nil

	case "publicAccessBlock":
		if b.PublicAccess == nil {
			return notSet(sub, b.Name)
		}
		writeXML(w, 200, struct {
			XMLName               xml.Name `xml:"PublicAccessBlockConfiguration"`
			Xmlns                 string   `xml:"xmlns,attr"`
			BlockPublicAcls       bool     `xml:"BlockPublicAcls"`
			IgnorePublicAcls      bool     `xml:"IgnorePublicAcls"`
			BlockPublicPolicy     bool     `xml:"BlockPublicPolicy"`
			RestrictPublicBuckets bool     `xml:"RestrictPublicBuckets"`
		}{
			Xmlns:                 s3Namespace,
			BlockPublicAcls:       b.PublicAccess.BlockPublicAcls,
			IgnorePublicAcls:      b.PublicAccess.IgnorePublicAcls,
			BlockPublicPolicy:     b.PublicAccess.BlockPublicPolicy,
			RestrictPublicBuckets: b.PublicAccess.RestrictPublicBuckets,
		})
		return nil

	case "ownershipControls":
		if b.Ownership == "" {
			return notSet(sub, b.Name)
		}
		type rule struct {
			ObjectOwnership string `xml:"ObjectOwnership"`
		}
		writeXML(w, 200, struct {
			XMLName xml.Name `xml:"OwnershipControls"`
			Xmlns   string   `xml:"xmlns,attr"`
			Rules   []rule   `xml:"Rule"`
		}{Xmlns: s3Namespace, Rules: []rule{{ObjectOwnership: b.Ownership}}})
		return nil

	case "acl":
		// Always answers: a bucket always has an owner and a grant.
		type grantee struct {
			XMLNS       string `xml:"xmlns:xsi,attr"`
			Type        string `xml:"xsi:type,attr"`
			ID          string `xml:"ID"`
			DisplayName string `xml:"DisplayName"`
		}
		type grant struct {
			Grantee    grantee `xml:"Grantee"`
			Permission string  `xml:"Permission"`
		}
		owner := stableID(s.cfg.AccountID, 64)
		writeXML(w, 200, struct {
			XMLName xml.Name `xml:"AccessControlPolicy"`
			Xmlns   string   `xml:"xmlns,attr"`
			Owner   struct {
				ID          string `xml:"ID"`
				DisplayName string `xml:"DisplayName"`
			} `xml:"Owner"`
			Grants []grant `xml:"AccessControlList>Grant"`
		}{
			Xmlns: s3Namespace,
			Owner: struct {
				ID          string `xml:"ID"`
				DisplayName string `xml:"DisplayName"`
			}{ID: owner, DisplayName: "nimbus"},
			Grants: []grant{{
				Grantee: grantee{
					XMLNS: "http://www.w3.org/2001/XMLSchema-instance", Type: "CanonicalUser",
					ID: owner, DisplayName: "nimbus",
				},
				Permission: "FULL_CONTROL",
			}},
		})
		return nil

	case "requestPayment":
		writeXML(w, 200, struct {
			XMLName xml.Name `xml:"RequestPaymentConfiguration"`
			Xmlns   string   `xml:"xmlns,attr"`
			Payer   string   `xml:"Payer"`
		}{Xmlns: s3Namespace, Payer: "BucketOwner"})
		return nil

	case "accelerate":
		writeXML(w, 200, struct {
			XMLName xml.Name `xml:"AccelerateConfiguration"`
			Xmlns   string   `xml:"xmlns,attr"`
		}{Xmlns: s3Namespace})
		return nil

	case "logging":
		writeXML(w, 200, struct {
			XMLName xml.Name `xml:"BucketLoggingStatus"`
			Xmlns   string   `xml:"xmlns,attr"`
		}{Xmlns: s3Namespace})
		return nil

	case "notification":
		writeXML(w, 200, struct {
			XMLName xml.Name `xml:"NotificationConfiguration"`
			Xmlns   string   `xml:"xmlns,attr"`
		}{Xmlns: s3Namespace})
		return nil
	}

	// Anything else that has a "nothing is set" code uses it; the rest
	// get an empty document, which is the safer of the two wrong answers.
	if code, ok := notConfigured[sub]; ok {
		return &apiError{Status: 404, Code: code,
			Message: "The " + sub + " configuration was not found", Resource: "/" + b.Name}
	}
	writeXML(w, 200, struct {
		XMLName xml.Name `xml:"Configuration"`
	}{})
	return nil
}

// notSet is the 404 S3 gives for a sub-resource nobody has configured.
func notSet(sub, bucket string) *apiError {
	code := notConfigured[sub]
	if code == "" {
		code = "NoSuchConfiguration"
	}
	return &apiError{Status: 404, Code: code,
		Message: "The " + sub + " configuration does not exist", Resource: "/" + bucket}
}

func (s *Server) putBucketConfig(w http.ResponseWriter, r *http.Request, b *bucket, sub string) *apiError {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return errorf(400, "MalformedXML", "reading the request: %v", err)
	}
	switch sub {
	case "versioning":
		var doc struct {
			Status string `xml:"Status"`
		}
		_ = xml.Unmarshal(body, &doc)
		b.Versioning = doc.Status
	case "tagging":
		var doc struct {
			Tags []struct {
				Key   string `xml:"Key"`
				Value string `xml:"Value"`
			} `xml:"TagSet>Tag"`
		}
		_ = xml.Unmarshal(body, &doc)
		b.Tags = nil
		for _, t := range doc.Tags {
			b.Tags = append(b.Tags, tag{Key: t.Key, Value: t.Value})
		}
	case "policy":
		b.Policy = string(body)
	case "encryption":
		var doc struct {
			Rules []struct {
				Apply struct {
					Algorithm string `xml:"SSEAlgorithm"`
					KMSKeyID  string `xml:"KMSMasterKeyID"`
				} `xml:"ApplyServerSideEncryptionByDefault"`
			} `xml:"Rule"`
		}
		_ = xml.Unmarshal(body, &doc)
		if len(doc.Rules) > 0 {
			b.Encryption = &s3Encryption{
				Algorithm: doc.Rules[0].Apply.Algorithm,
				KMSKeyID:  doc.Rules[0].Apply.KMSKeyID,
			}
		}
	case "publicAccessBlock":
		var doc publicAccessBlockXML
		_ = xml.Unmarshal(body, &doc)
		b.PublicAccess = &publicAccessBlock{
			BlockPublicAcls:       doc.BlockPublicAcls,
			IgnorePublicAcls:      doc.IgnorePublicAcls,
			BlockPublicPolicy:     doc.BlockPublicPolicy,
			RestrictPublicBuckets: doc.RestrictPublicBuckets,
		}
	case "ownershipControls":
		var doc struct {
			Rules []struct {
				ObjectOwnership string `xml:"ObjectOwnership"`
			} `xml:"Rule"`
		}
		_ = xml.Unmarshal(body, &doc)
		if len(doc.Rules) > 0 {
			b.Ownership = doc.Rules[0].ObjectOwnership
		}
	case "acl":
		if acl := r.Header.Get("x-amz-acl"); acl != "" {
			b.ACL = acl
		}
	}
	if aerr := s.saveBucket(b); aerr != nil {
		return aerr
	}
	w.WriteHeader(200)
	return nil
}

type publicAccessBlockXML struct {
	BlockPublicAcls       bool `xml:"BlockPublicAcls"`
	IgnorePublicAcls      bool `xml:"IgnorePublicAcls"`
	BlockPublicPolicy     bool `xml:"BlockPublicPolicy"`
	RestrictPublicBuckets bool `xml:"RestrictPublicBuckets"`
}

func (s *Server) deleteBucketConfig(w http.ResponseWriter, b *bucket, sub string) *apiError {
	switch sub {
	case "tagging":
		b.Tags = nil
	case "policy":
		b.Policy = ""
	case "encryption":
		b.Encryption = nil
	case "publicAccessBlock":
		b.PublicAccess = nil
	case "ownershipControls":
		b.Ownership = ""
	}
	if aerr := s.saveBucket(b); aerr != nil {
		return aerr
	}
	w.WriteHeader(204)
	return nil
}
