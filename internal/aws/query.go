package aws

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The Query protocol: a form-encoded request in, XML out.
//
// EC2, IAM and STS all speak it. The request is `Action=X&Version=Y` plus
// the operation's parameters, POSTed to `/` as
// application/x-www-form-urlencoded (measured against aws-cli 1.40.33).
// The response is XML whose root element is the action's name with
// "Response" on the end, in a namespace that is per service and per
// version.

// params is one request's parameters, however they arrived.
type params struct {
	url.Values
}

// readParams reads the form body of a POST, or the query string of a GET.
func readParams(r *http.Request) (params, error) {
	if r.Method == http.MethodGet {
		return params{r.URL.Query()}, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return params{}, err
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return params{}, err
	}
	// A client may also put parameters in the query string of a POST.
	for k, vs := range r.URL.Query() {
		for _, v := range vs {
			values.Add(k, v)
		}
	}
	return params{values}, nil
}

func (p params) str(key string) string { return p.Get(key) }

func (p params) intOr(key string, fallback int) int {
	if v := p.Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func (p params) boolOr(key string, fallback bool) bool {
	switch strings.ToLower(p.Get(key)) {
	case "true", "1":
		return true
	case "false", "0":
		return false
	}
	return fallback
}

// list returns the values of an indexed parameter, which is how the Query
// protocol carries a list: Tag.1.Key, Tag.2.Key and so on, one-based and
// contiguous. The prefix is everything before the number.
//
// EC2 and IAM differ here in a way that catches people out: EC2 writes
// "Filter.1.Name" while IAM writes "Tags.member.1.Key". Both are handled by
// passing the whole prefix in.
func (p params) list(prefix, suffix string) []string {
	var out []string
	for i := 1; ; i++ {
		key := fmt.Sprintf("%s.%d", prefix, i)
		if suffix != "" {
			key += "." + suffix
		}
		v := p.Get(key)
		if v == "" {
			return out
		}
		out = append(out, v)
	}
}

// requestID is the id every AWS response carries. Nothing reads it back, so
// unlike an ARM resource id it may be random -- see the "derive, do not
// generate" rule in CLAUDE.md, which is about values a client COMPARES
// between reads.
func requestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "00000000-0000-0000-0000-000000000000"
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// hexID returns n hex characters, for the ids AWS hands out.
func hexID(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n)
	}
	return hex.EncodeToString(b)[:n]
}

// writeXML sends a response document. The caller supplies a struct whose
// XMLName carries the element name and the namespace, because both are part
// of the contract: a client parsing with the wrong namespace sees an empty
// document rather than an error.
func writeXML(w http.ResponseWriter, status int, doc any) {
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		http.Error(w, "marshalling the response: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}

// responseMetadata is the tail every Query response carries.
type responseMetadata struct {
	RequestID string `xml:"RequestId"`
}
