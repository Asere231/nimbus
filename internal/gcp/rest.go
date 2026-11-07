package gcp

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// The wire format, and the one error envelope.
//
// Google has a single error shape across its JSON APIs, and clients read
// more of it than they look like they do: the `status` string and the
// `errors[].reason` are both matched on, and a 404 whose reason is not
// "notFound" is not recognised as "it is not there". This is the same
// lesson as AWS's three envelopes and Azure's `code`, one cloud later.
//
// Every response is JSON. Query parameters `alt=json` and
// `prettyPrint=false` ride along on nearly every request and change
// nothing -- JSON is the only representation Nimbus serves.

// apiError is an error on its way out.
type apiError struct {
	Status  int    // HTTP status
	Reason  string // errors[].reason, which clients match on
	Code    string // the `status` enum: NOT_FOUND, ALREADY_EXISTS, ...
	Message string
}

func errorf(status int, reason, code, format string, args ...any) *apiError {
	return &apiError{Status: status, Reason: reason, Code: code, Message: sprintf(format, args...)}
}

// notFound is the shape a client must see to conclude "it is not there"
// rather than "something went wrong". The provider treats this as a
// resource that needs creating; anything else fails the apply.
func notFound(format string, args ...any) *apiError {
	return errorf(http.StatusNotFound, "notFound", "NOT_FOUND", format, args...)
}

func alreadyExists(what, name string) *apiError {
	return errorf(http.StatusConflict, "alreadyExists", "ALREADY_EXISTS",
		"The resource 'projects/%s' already exists: %s", name, what)
}

func badRequest(format string, args ...any) *apiError {
	return errorf(http.StatusBadRequest, "invalid", "INVALID_ARGUMENT", format, args...)
}

func internalError(err error) *apiError {
	return errorf(http.StatusInternalServerError, "internalError", "INTERNAL", "%v", err)
}

// writeError sends Google's envelope. Both spellings of the code are
// filled: `error.code` is the HTTP status as a number, `error.status` is
// the enum, and `errors[].reason` is the string clients switch on.
func writeError(w http.ResponseWriter, e *apiError) {
	status := e.Status
	if status == 0 {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    status,
			"message": e.Message,
			"status":  e.Code,
			"errors": []map[string]any{{
				"domain":  "global",
				"reason":  e.Reason,
				"message": e.Message,
			}},
		},
	})
}

// readJSON decodes a request body, tolerating an empty one.
func readJSON(r *http.Request, into any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
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

// numberString is how GCP writes a number that will not fit a client's
// integer: as a decimal string. An `id` comes back as "1234", not 1234, and
// a client that is handed the number instead fails to unmarshal it --
// silently, into a zero. Nothing here ever converts one.
type numberString = string
