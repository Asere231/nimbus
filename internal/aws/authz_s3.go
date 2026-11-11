package aws

import (
	"net/http"
	"strings"
)

// Which permission an S3 request needs.
//
// S3 is the one service where the path and the query string say everything,
// which is why it is also the one service whose body is never buffered.
// The mapping is mechanical but not derivable: AWS's action names are
// inconsistent in a way no rule captures. Most sub-resources become
// `s3:GetBucketX`, but `?lifecycle` is `s3:GetLifecycleConfiguration` with
// no "Bucket" in it, `?replication`, `?encryption` and `?accelerate` are
// the same, and `?cors` capitalises as `CORS`. So they are tabulated, and
// a sub-resource missing from the table refuses rather than being guessed
// at -- the same choice as an unmapped action anywhere else.

// s3BucketSubResourceActions maps a bucket sub-resource to the part of the
// action name that follows Get/Put/Delete.
var s3BucketSubResourceActions = map[string]string{
	"versioning":          "BucketVersioning",
	"tagging":             "BucketTagging",
	"policy":              "BucketPolicy",
	"policyStatus":        "BucketPolicyStatus",
	"acl":                 "BucketAcl",
	"cors":                "BucketCORS",
	"website":             "BucketWebsite",
	"logging":             "BucketLogging",
	"notification":        "BucketNotification",
	"requestPayment":      "BucketRequestPayment",
	"publicAccessBlock":   "BucketPublicAccessBlock",
	"ownershipControls":   "BucketOwnershipControls",
	"location":            "BucketLocation",
	"object-lock":         "BucketObjectLockConfiguration",
	"lifecycle":           "LifecycleConfiguration",
	"encryption":          "EncryptionConfiguration",
	"replication":         "ReplicationConfiguration",
	"accelerate":          "AccelerateConfiguration",
	"intelligent-tiering": "IntelligentTieringConfiguration",
	"analytics":           "AnalyticsConfiguration",
	"inventory":           "InventoryConfiguration",
	"metrics":             "MetricsConfiguration",
}

// s3Target names the action and resource for an S3 request. The routing
// here deliberately mirrors serveS3/serveBucket/serveObject: if the two
// ever disagree, a request is authorized as one thing and served as
// another.
func (s *Server) s3Target(r *http.Request) (target, bool) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	name, key, _ := strings.Cut(path, "/")

	switch {
	case name == "":
		// The service root: listing the account's buckets. Its resource
		// is every bucket, which AWS writes as plain "*".
		if r.Method == http.MethodGet {
			return target{Action: "s3:ListAllMyBuckets", Resource: "*"}, true
		}
		return target{}, false
	case key == "":
		return s3BucketTarget(r, name)
	default:
		return s3ObjectTarget(r, name, key)
	}
}

func s3BucketTarget(r *http.Request, name string) (target, bool) {
	arn := bucketARN(name)
	sub := subResource(r)

	if sub == "" {
		switch r.Method {
		case http.MethodPut:
			return target{Action: "s3:CreateBucket", Resource: arn}, true
		case http.MethodDelete:
			return target{Action: "s3:DeleteBucket", Resource: arn}, true
		case http.MethodGet, http.MethodHead:
			// Listing a bucket's objects, and HEAD to find out whether it
			// exists at all. Both are s3:ListBucket in IAM, whose resource
			// is the BUCKET rather than its objects -- the distinction
			// that makes "list the bucket but read nothing in it"
			// expressible.
			return target{Action: "s3:ListBucket", Resource: arn}, true
		}
		return target{}, false
	}

	// Three sub-resources are not configuration at all.
	switch sub {
	case "versions":
		return target{Action: "s3:ListBucketVersions", Resource: arn}, true
	case "uploads":
		return target{Action: "s3:ListBucketMultipartUploads", Resource: arn}, true
	case "delete":
		// POST ?delete is a bulk object delete, so its resource is the
		// objects rather than the bucket.
		return target{Action: "s3:DeleteObject", Resource: arn + "/*"}, true
	}

	suffix, known := s3BucketSubResourceActions[sub]
	if !known {
		return target{}, false
	}
	verb, ok := s3Verb(r.Method)
	if !ok {
		return target{}, false
	}
	return target{Action: "s3:" + verb + suffix, Resource: arn}, true
}

func s3ObjectTarget(r *http.Request, name, key string) (target, bool) {
	arn := bucketARN(name) + "/" + key
	q := r.URL.Query()
	uploadID := q.Get("uploadId")

	// Multipart upload, told apart by query parameters rather than by
	// verb. Uploading and completing are both s3:PutObject in IAM --
	// there is no s3:UploadPart -- while aborting and listing parts have
	// actions of their own.
	switch {
	case uploadID != "" && r.Method == http.MethodDelete:
		return target{Action: "s3:AbortMultipartUpload", Resource: arn}, true
	case uploadID != "" && r.Method == http.MethodGet:
		return target{Action: "s3:ListMultipartUploadParts", Resource: arn}, true
	case uploadID != "" || hasFlag(r, "uploads"):
		return target{Action: "s3:PutObject", Resource: arn}, true
	}

	switch r.Method {
	case http.MethodPut, http.MethodPost:
		return target{Action: "s3:PutObject", Resource: arn}, true
	case http.MethodGet, http.MethodHead:
		return target{Action: "s3:GetObject", Resource: arn}, true
	case http.MethodDelete:
		return target{Action: "s3:DeleteObject", Resource: arn}, true
	}
	return target{}, false
}

func s3Verb(method string) (string, bool) {
	switch method {
	case http.MethodGet, http.MethodHead:
		return "Get", true
	case http.MethodPut, http.MethodPost:
		return "Put", true
	case http.MethodDelete:
		return "Delete", true
	}
	return "", false
}
