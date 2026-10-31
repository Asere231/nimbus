package storage

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Shared Key is Azure storage's HMAC scheme. The client builds a string from
// a fixed list of headers plus every x-ms-* header and the request's resource,
// signs it with the account key, and sends
//
//	Authorization: SharedKey {account}:{base64 signature}
//
// Nimbus verifies it for real rather than waving it through, so a wrong key
// fails here exactly as it would against Azure. That matters: a connection
// string with the wrong key is a common mistake, and an emulator that accepts
// it teaches you the wrong thing.
//
// The canonicalisation is unforgiving and every detail below is load-bearing;
// see TestStringToSign for the cases that bite.

// AuthResult says how a request was authenticated.
type AuthResult int

const (
	AuthFailed AuthResult = iota
	AuthSharedKey
	AuthSAS
	AuthBearer
	AuthAnonymous
)

// Authenticate checks a data-plane request's credentials against the account.
//
// Bearer tokens and SAS are accepted on shape alone: Nimbus issues unsigned
// tokens (see identity.go) and there is nothing to verify them against. Shared
// Key is the one scheme where a real signature exists, so it is really checked.
func Authenticate(r *http.Request, a *Account, svc Service) (AuthResult, string) {
	auth := r.Header.Get("Authorization")

	switch {
	case strings.HasPrefix(auth, "SharedKey "), strings.HasPrefix(auth, "SharedKeyLite "):
		lite := strings.HasPrefix(auth, "SharedKeyLite ")
		rest := auth[strings.Index(auth, " ")+1:]
		colon := strings.Index(rest, ":")
		if colon < 0 {
			return AuthFailed, "Authorization header is malformed: expected '{account}:{signature}'."
		}
		account, sig := rest[:colon], rest[colon+1:]
		if !strings.EqualFold(account, a.Name) {
			return AuthFailed, "The account named in the Authorization header does not match the request host."
		}
		sts := StringToSign(r, a.Name, svc, lite)
		for _, k := range a.Keys {
			if want, err := Sign(k, sts); err == nil && hmac.Equal([]byte(want), []byte(sig)) {
				return AuthSharedKey, ""
			}
		}
		return AuthFailed, "Signature did not match. String-to-sign used was: " + strconv.Quote(sts)

	case strings.HasPrefix(auth, "Bearer "):
		return AuthBearer, ""

	case auth == "":
		// A SAS carries its credentials in the query string instead.
		if q := r.URL.Query(); q.Get("sig") != "" {
			if q.Get("se") == "" || q.Get("sp") == "" {
				return AuthFailed, "SAS token is missing required parameters."
			}
			return AuthSAS, ""
		}
		// Anonymous. Whether that is allowed depends on the container's public
		// access level, which the caller decides.
		return AuthAnonymous, ""
	}
	return AuthFailed, "Unsupported Authorization scheme."
}

// Sign computes the base64 HMAC-SHA256 of stringToSign with a base64 account key.
func Sign(accountKey, stringToSign string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(accountKey)
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, raw)
	m.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(m.Sum(nil)), nil
}

// StringToSign builds the exact string the client signed.
//
// There are FOUR layouts, not two, because the Table service signs differently
// from blob and queue -- a difference that is easy to miss and shows up only
// as a 403:
//
//	blob/queue/file, SharedKey      12 standard headers, x-ms-* headers, full resource
//	blob/queue/file, SharedKeyLite  Content-MD5, Content-Type, Date, x-ms-*, comp only
//	table, SharedKey                Content-MD5, Content-Type, Date, resource with comp only
//	table, SharedKeyLite            Date, resource with comp only
//
// The table forms sign NO x-ms-* headers at all, and their canonicalised
// resource carries only the comp parameter however many others are present.
func StringToSign(r *http.Request, account string, svc Service, lite bool) string {
	h := r.Header
	if svc == ServiceTable {
		// The table scheme signs x-ms-date, NOT the Date header, and clients
		// put the timestamp only in x-ms-date -- so reading Date here yields
		// an empty field and a 403 that says nothing about why.
		date := h.Get("x-ms-date")
		if date == "" {
			date = h.Get("Date")
		}
		if lite {
			return date + "\n" + canonicalResource(r, account, true)
		}
		return strings.Join([]string{
			r.Method,
			h.Get("Content-MD5"),
			h.Get("Content-Type"),
			date,
		}, "\n") + "\n" + canonicalResource(r, account, true)
	}
	if lite {
		return strings.Join([]string{
			r.Method,
			h.Get("Content-MD5"),
			h.Get("Content-Type"),
			h.Get("Date"),
		}, "\n") + "\n" + canonicalHeaders(h) + canonicalResource(r, account, true)
	}
	return strings.Join([]string{
		r.Method,
		h.Get("Content-Encoding"),
		h.Get("Content-Language"),
		contentLength(r),
		h.Get("Content-MD5"),
		h.Get("Content-Type"),
		h.Get("Date"),
		h.Get("If-Modified-Since"),
		h.Get("If-Match"),
		h.Get("If-None-Match"),
		h.Get("If-Unmodified-Since"),
		h.Get("Range"),
	}, "\n") + "\n" + canonicalHeaders(h) + canonicalResource(r, account, false)
}

// contentLength is the empty string for a zero-length body, NOT "0".
//
// This is the single most common Shared Key bug. Azure changed the rule in
// version 2015-02-21 and every client follows the new one: a request with no
// body signs an empty field here.
func contentLength(r *http.Request) string {
	if v := r.Header.Get("Content-Length"); v != "" && v != "0" {
		return v
	}
	if r.ContentLength > 0 {
		return strconv.FormatInt(r.ContentLength, 10)
	}
	return ""
}

// canonicalHeaders is every x-ms-* header, lower-cased, sorted by name, as
// "name:value\n" lines. Values are trimmed, and any internal line breaks are
// folded to a single space so a multi-line value cannot forge extra lines.
func canonicalHeaders(h http.Header) string {
	var names []string
	for name := range h {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-ms-") {
			names = append(names, lower)
		}
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		// http.Header canonicalises names, so read through the same mapping.
		values := h.Values(http.CanonicalHeaderKey(name))
		joined := strings.Join(values, ",")
		joined = strings.ReplaceAll(joined, "\r\n", " ")
		joined = strings.ReplaceAll(joined, "\n", " ")
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(strings.TrimSpace(joined))
		b.WriteByte('\n')
	}
	return b.String()
}

// canonicalResource is "/{account}{path}" followed by the query parameters.
//
// The full scheme appends every parameter, lower-cased and sorted, one per
// line as "\nname:value" with repeated values comma-joined and sorted. Lite
// appends only "?comp=..." when present -- the Table service relies on that.
func canonicalResource(r *http.Request, account string, lite bool) string {
	path := r.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	base := "/" + strings.ToLower(account) + path

	q := r.URL.Query()
	if lite {
		if comp := q.Get("comp"); comp != "" {
			return base + "?comp=" + comp
		}
		return base
	}

	if len(q) == 0 {
		return base
	}
	keys := make([]string, 0, len(q))
	lowered := map[string][]string{}
	for k, v := range q {
		lk := strings.ToLower(k)
		if _, seen := lowered[lk]; !seen {
			keys = append(keys, lk)
		}
		lowered[lk] = append(lowered[lk], v...)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(base)
	for _, k := range keys {
		vals := lowered[k]
		sort.Strings(vals)
		b.WriteByte('\n')
		b.WriteString(k)
		b.WriteByte(':')
		b.WriteString(strings.Join(vals, ","))
	}
	return b.String()
}

// ConnectionString is what `az storage` and the SDKs take instead of separate
// endpoint and key settings. Explicit per-service endpoints are used rather
// than EndpointSuffix, so clients never have to guess our port.
func ConnectionString(account, key, suffix string) string {
	ep := func(svc string) string {
		return svc + "Endpoint=https://" + account + "." + svc + "." + suffix + "/"
	}
	return strings.Join([]string{
		"DefaultEndpointsProtocol=https",
		"AccountName=" + account,
		"AccountKey=" + key,
		ep("Blob"), ep("Queue"), ep("Table"), ep("File"),
	}, ";")
}
