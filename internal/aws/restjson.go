package aws

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// REST-JSON, the third protocol behind this port.
//
// EKS and Lambda speak it: the method and the path say what to do, the
// body is JSON, and an error is a JSON object plus a header naming its
// type. That header matters more than it looks — the SDKs read
// `x-amzn-ErrorType` to decide WHICH exception to raise, and without it a
// 404 arrives as a generic failure rather than as the
// ResourceNotFoundException the provider is written to catch.

// readJSON decodes a request body, tolerating an empty one.
func readJSON(r *http.Request, into any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	return json.Unmarshal(body, into)
}

// writeJSON sends a JSON response.
func writeJSON(w http.ResponseWriter, status int, doc any) {
	body, err := json.Marshal(doc)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeJSONError sends the shape a REST-JSON client expects.
func writeJSONError(w http.ResponseWriter, e *apiError) {
	status := e.Status
	if status == 0 {
		status = 400
	}
	// Both spellings: some SDK versions read the header, others the
	// __type field in the body, and sending one without the other leaves
	// whichever client reads the missing one with no exception type.
	w.Header().Set("x-amzn-ErrorType", e.Code)
	writeJSON(w, status, map[string]string{
		"__type":  e.Code,
		"message": e.Message,
	})
}

// pathParts splits a request path into its non-empty segments.
func pathParts(p string) []string {
	var out []string
	for _, part := range strings.Split(p, "/") {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// notFoundJSON is REST-JSON's "it is not there", which the aws provider
// matches on to tell a deleted resource from a broken request.
func notFoundJSON(what, name string) *apiError {
	return &apiError{
		Status:  404,
		Code:    "ResourceNotFoundException",
		Message: "No " + what + " found for name: " + name + ".",
	}
}
