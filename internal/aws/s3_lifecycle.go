package aws

import (
	"encoding/json"
	"encoding/xml"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/core"
)

// Bucket lifecycle configuration: rules that delete things on a schedule.
//
// `?lifecycle` fell through applyBucketConfig with no case at all, so a
// PUT was accepted and discarded and the GET went on answering
// NoSuchLifecycleConfiguration. That is worse than a permanent diff:
// `aws_s3_bucket_lifecycle_configuration` PUTs the rules and then POLLS
// for them, so the apply FAILED after about two minutes with
//
//	While waiting: couldn't find resource (21 retries)
//
// -- measured against aws provider 5.100.0. The same shape as the
// `?versions` listing falling through to an empty document: an
// unimplemented sub-resource failing politely is not safe when a client
// is reading the failure as an answer.
//
// # Nothing will expire in a session you are watching, and that is honest
//
// S3's own minimum is ONE DAY -- `Days` and `NoncurrentDays` are integers
// of days and the API refuses 0 -- so no rule a client can write will
// fire while somebody is looking at it. The sweep below is implemented
// faithfully and takes `now` as an argument, so a test can ask what
// happens ten days out and the behaviour is pinned rather than
// hypothetical. What it does NOT do is pretend: docs/ROADMAP.md says
// plainly that nothing expires in a short session.

// lifecycleRule is one rule, in the shape the wire uses.
type lifecycleRule struct {
	ID     string `xml:"ID,omitempty" json:"id,omitempty"`
	Status string `xml:"Status" json:"status"`

	// Filter is how a rule is narrowed. Only a prefix is honoured; a tag
	// filter or an object-size filter is REFUSED rather than ignored,
	// because a rule that silently matches more than it was told to
	// deletes data -- the one mistake here that cannot be undone.
	Prefix string `xml:"-" json:"prefix,omitempty"`

	ExpirationDays           int `xml:"-" json:"expirationDays,omitempty"`
	NoncurrentExpirationDays int `xml:"-" json:"noncurrentExpirationDays,omitempty"`
}

// lifecycleXML is the document on the wire. Separate from the stored
// shape because the XML nests each setting in its own element, and
// flattening at the boundary keeps the sweep readable.
type lifecycleXML struct {
	XMLName xml.Name `xml:"LifecycleConfiguration"`
	Xmlns   string   `xml:"xmlns,attr,omitempty"`
	Rules   []struct {
		ID     string `xml:"ID"`
		Status string `xml:"Status"`
		Prefix string `xml:"Prefix"`
		Filter *struct {
			Prefix                string `xml:"Prefix"`
			ObjectSizeGreaterThan *int64 `xml:"ObjectSizeGreaterThan"`
			ObjectSizeLessThan    *int64 `xml:"ObjectSizeLessThan"`
			Tag                   *struct {
				Key string `xml:"Key"`
			} `xml:"Tag"`
			And *struct {
				Prefix string `xml:"Prefix"`
			} `xml:"And"`
		} `xml:"Filter"`
		Expiration *struct {
			Days                      int    `xml:"Days"`
			Date                      string `xml:"Date"`
			ExpiredObjectDeleteMarker *bool  `xml:"ExpiredObjectDeleteMarker"`
		} `xml:"Expiration"`
		NoncurrentVersionExpiration *struct {
			NoncurrentDays int `xml:"NoncurrentDays"`
		} `xml:"NoncurrentVersionExpiration"`
		Transition *struct {
			Days int `xml:"Days"`
		} `xml:"Transition"`
		AbortIncompleteMultipartUpload *struct {
			DaysAfterInitiation int `xml:"DaysAfterInitiation"`
		} `xml:"AbortIncompleteMultipartUpload"`
	} `xml:"Rule"`
}

// parseLifecycle reads the document a client sent.
//
// Anything Nimbus does not apply is REFUSED by name rather than stored
// and ignored. A lifecycle rule deletes data: a rule accepted but not
// applied is a backup that silently is not running, and a rule applied
// more broadly than written deletes what was meant to be kept. Neither
// belongs in an emulator somebody is learning from.
func parseLifecycle(body []byte) ([]lifecycleRule, string, *apiError) {
	var inner struct {
		Raw string `xml:",innerxml"`
	}
	// The raw inner XML is kept so a GET can echo it exactly; the parse
	// below is for validation and for the sweep.
	_ = xml.Unmarshal(body, &inner)

	var doc lifecycleXML
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, "", errorf(400, "MalformedXML", "parsing the lifecycle configuration: %v", err)
	}
	if len(doc.Rules) == 0 {
		return nil, "", errorf(400, "MalformedXML", "a lifecycle configuration needs at least one rule")
	}
	var out []lifecycleRule
	for _, r := range doc.Rules {
		rule := lifecycleRule{ID: r.ID, Status: r.Status, Prefix: r.Prefix}
		if rule.Status != "Enabled" && rule.Status != "Disabled" {
			return nil, "", errorf(400, "MalformedXML",
				"rule %q has status %q; it must be Enabled or Disabled", r.ID, r.Status)
		}
		if f := r.Filter; f != nil {
			if f.Tag != nil || f.ObjectSizeGreaterThan != nil || f.ObjectSizeLessThan != nil {
				return nil, "", errorf(400, "InvalidArgument",
					"rule %q filters on tags or object size, which Nimbus does not apply; "+
						"only a prefix filter is honoured", r.ID)
			}
			rule.Prefix = f.Prefix
			if f.And != nil && f.And.Prefix != "" {
				rule.Prefix = f.And.Prefix
			}
		}
		if e := r.Expiration; e != nil {
			if e.Date != "" {
				return nil, "", errorf(400, "InvalidArgument",
					"rule %q expires on a Date, which Nimbus does not apply; use Days", r.ID)
			}
			if e.ExpiredObjectDeleteMarker != nil {
				return nil, "", errorf(400, "InvalidArgument",
					"rule %q uses ExpiredObjectDeleteMarker, which Nimbus does not apply", r.ID)
			}
			rule.ExpirationDays = e.Days
		}
		if n := r.NoncurrentVersionExpiration; n != nil {
			rule.NoncurrentExpirationDays = n.NoncurrentDays
		}
		if r.Transition != nil {
			return nil, "", errorf(400, "InvalidArgument",
				"rule %q has a storage-class Transition; Nimbus stores one class and "+
					"applying it would mean pretending otherwise", r.ID)
		}
		if r.AbortIncompleteMultipartUpload != nil {
			return nil, "", errorf(400, "InvalidArgument",
				"rule %q aborts incomplete multipart uploads, which Nimbus holds in "+
					"memory and never reaps", r.ID)
		}
		out = append(out, rule)
	}
	return out, inner.Raw, nil
}

// transitionDefaultMinimumObjectSize is S3's own default for the field of
// the same name, which `GetBucketLifecycleConfiguration` returns in a
// HEADER rather than in the body.
//
// It has to be answered because the provider compares it FIRST: its wait
// is `lifecycleConfigEqual`, which returns false unless the transition
// minimum matches AND every rule it sent is reflect.DeepEqual to one that
// came back. Returning nothing left the SDK with the zero value against
// the provider's "all_storage_classes_128K", so the wait could never
// succeed and the apply timed out after three minutes -- with an error
// about "state" that names neither the field nor lifecycle rules.
const transitionDefaultMinimumObjectSize = "all_storage_classes_128K"

// lifecycleBody is what a GET returns: the rules EXACTLY as the client
// sent them.
//
// Echoed byte-for-byte rather than rebuilt from the parsed rules, because
// the provider's check is reflect.DeepEqual against the structs it sent
// -- so a nil pointer where it sent an empty string, or an element in a
// different order, never matches however correct the document is. The
// same rule a Key Vault certificate policy needed, and for the same
// reason: a client that READS BACK what it wrote is comparing, not
// parsing.
func lifecycleBody(raw string) string {
	return xml.Header + `<LifecycleConfiguration xmlns="` + s3Namespace + `">` +
		raw + `</LifecycleConfiguration>`
}

// lifecycleDocument is the configuration on its way back out, which the
// provider READS to decide whether its apply worked.
func lifecycleDocument(rules []lifecycleRule) any {
	type expiration struct {
		Days int `xml:"Days,omitempty"`
	}
	type noncurrent struct {
		NoncurrentDays int `xml:"NoncurrentDays,omitempty"`
	}
	type filter struct {
		Prefix string `xml:"Prefix,omitempty"`
	}
	type rule struct {
		ID         string      `xml:"ID,omitempty"`
		Status     string      `xml:"Status"`
		Filter     filter      `xml:"Filter"`
		Expiration *expiration `xml:"Expiration,omitempty"`
		Noncurrent *noncurrent `xml:"NoncurrentVersionExpiration,omitempty"`
	}
	doc := struct {
		XMLName xml.Name `xml:"LifecycleConfiguration"`
		Xmlns   string   `xml:"xmlns,attr"`
		Rules   []rule   `xml:"Rule"`
	}{Xmlns: s3Namespace}
	for _, r := range rules {
		out := rule{ID: r.ID, Status: r.Status, Filter: filter{Prefix: r.Prefix}}
		if r.ExpirationDays > 0 {
			out.Expiration = &expiration{Days: r.ExpirationDays}
		}
		if r.NoncurrentExpirationDays > 0 {
			out.Noncurrent = &noncurrent{NoncurrentDays: r.NoncurrentExpirationDays}
		}
		doc.Rules = append(doc.Rules, out)
	}
	return doc
}

// --- applying them --------------------------------------------------------

// sweepLifecycle applies every bucket's rules as at `now`, and returns how
// many versions it removed.
//
// `now` is a parameter rather than time.Now() because a day is the
// smallest unit S3 allows: no rule a client can write will fire while
// somebody is watching, so a test that could not ask "what about ten days
// from now" would be asserting nothing at all.
func (s *Server) sweepLifecycle(now time.Time) int {
	removed := 0
	for _, res := range s.store.List(func(r *core.Resource) bool { return r.Type == typeBucket }) {
		var b bucket
		if json.Unmarshal(res.Body, &b) != nil || len(b.Lifecycle) == 0 {
			continue
		}
		// versions() hands them back newest-first within a key, so the
		// first one seen for a key is the CURRENT version -- the same
		// way listObjectVersions decides IsLatest, rather than a second
		// field on objectInfo that would have to be kept in step.
		seen := map[string]bool{}
		for _, info := range s.objects.versions(b.Name, "") {
			current := !seen[info.Key]
			seen[info.Key] = true

			rule, applies := matchLifecycle(b.Lifecycle, info)
			if !applies {
				continue
			}
			if !lifecycleHasExpired(rule, info, current, now) {
				continue
			}
			if _, ok := s.objects.deleteVersion(b.Name, info.Key, info.VersionID); ok {
				removed++
			}
		}
	}
	return removed
}

// matchLifecycle finds the first ENABLED rule whose prefix the key starts
// with. S3 applies every matching rule; taking the first is a divergence
// only when two rules overlap, and the alternative -- applying whichever
// expires soonest -- is the same answer for every rule a prefix can
// express.
func matchLifecycle(rules []lifecycleRule, info objectInfo) (lifecycleRule, bool) {
	for _, r := range rules {
		if r.Status != "Enabled" {
			continue
		}
		if r.Prefix != "" && !strings.HasPrefix(info.Key, r.Prefix) {
			continue
		}
		return r, true
	}
	return lifecycleRule{}, false
}

// lifecycleHasExpired decides whether one version is past its rule.
//
// A NONCURRENT version is judged by NoncurrentDays and a current one by
// Days, and the two are not interchangeable: an Expiration rule on a
// versioned bucket removes the CURRENT version by laying a delete marker,
// which is a different thing from reaping the history behind it. Nimbus
// applies Days to a current version only when the bucket is unversioned,
// where the two coincide -- the versioned case needs a delete marker and
// is refused rather than approximated.
func lifecycleHasExpired(rule lifecycleRule, info objectInfo, current bool, now time.Time) bool {
	age := now.Sub(info.LastModified)
	if !current {
		return rule.NoncurrentExpirationDays > 0 &&
			age >= time.Duration(rule.NoncurrentExpirationDays)*24*time.Hour
	}
	// A current version in an UNVERSIONED bucket is the only one Days is
	// applied to. On a versioned bucket, expiring a current version means
	// laying a delete MARKER rather than removing it -- a different
	// operation, and approximating it would delete history the rule said
	// to keep.
	if info.VersionID == nullVersionID {
		return rule.ExpirationDays > 0 &&
			age >= time.Duration(rule.ExpirationDays)*24*time.Hour
	}
	return false
}
