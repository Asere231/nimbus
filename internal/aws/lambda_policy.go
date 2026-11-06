package aws

import (
	"encoding/json"
	"net/http"
)

// A function's resource policy and its tags.
//
// Both are read straight after a create, and the policy's "there is none"
// answer has to be ResourceNotFoundException: that is the one the provider
// is written to tolerate, exactly as Key Vault's certificate contacts had
// to be 404 ContactsNotFound rather than a refusal.
//
// The tags path is the odd one: /2017-03-31/tags/{arn}, with the ARN
// URL-encoded into a single path segment, colons and all.

// --- permissions and tags -------------------------------------------------

func (s *Server) getFunctionPolicy(w http.ResponseWriter, name string) *apiError {
	fn, aerr := s.loadFunction(name)
	if aerr != nil {
		return aerr
	}
	if fn.NimbusPolicy == nil {
		// No policy is ResourceNotFoundException, which the provider is
		// written to tolerate. Anything else fails the refresh.
		return &apiError{Status: 404, Code: "ResourceNotFoundException",
			Message: "The resource you requested does not exist."}
	}
	// The policy comes back as a JSON STRING inside the response, not as an
	// object -- the same shape IAM uses, one encoding layer deep.
	body, err := json.Marshal(fn.NimbusPolicy)
	if err != nil {
		return &apiError{Status: 500, Code: "ServiceException", Message: err.Error()}
	}
	writeJSON(w, 200, map[string]any{"Policy": string(body), "RevisionId": fn.RevisionId})
	return nil
}

func (s *Server) addFunctionPermission(w http.ResponseWriter, r *http.Request, name string) *apiError {
	fn, aerr := s.loadFunction(name)
	if aerr != nil {
		return aerr
	}
	var req struct {
		StatementId string `json:"StatementId"`
		Action      string `json:"Action"`
		Principal   string `json:"Principal"`
		SourceArn   string `json:"SourceArn"`
	}
	if err := readJSON(r, &req); err != nil {
		return &apiError{Status: 400, Code: "InvalidParameterValueException", Message: err.Error()}
	}
	statement := map[string]any{
		"Sid": req.StatementId, "Effect": "Allow", "Action": req.Action,
		"Principal": map[string]any{"Service": req.Principal},
		"Resource":  fn.FunctionArn,
	}
	if req.SourceArn != "" {
		statement["Condition"] = map[string]any{
			"ArnLike": map[string]any{"AWS:SourceArn": req.SourceArn},
		}
	}
	if fn.NimbusPolicy == nil {
		fn.NimbusPolicy = map[string]any{"Version": "2012-10-17", "Id": "default", "Statement": []any{}}
	}
	existing, _ := fn.NimbusPolicy["Statement"].([]any)
	fn.NimbusPolicy["Statement"] = append(existing, statement)
	fn.RevisionId = uuidLike()
	if aerr := s.saveFunction(fn); aerr != nil {
		return aerr
	}
	body, _ := json.Marshal(statement)
	writeJSON(w, 201, map[string]any{"Statement": string(body)})
	return nil
}

func (s *Server) removeFunctionPermission(w http.ResponseWriter, name, sid string) *apiError {
	fn, aerr := s.loadFunction(name)
	if aerr != nil {
		return aerr
	}
	if fn.NimbusPolicy != nil {
		existing, _ := fn.NimbusPolicy["Statement"].([]any)
		kept := []any{}
		for _, raw := range existing {
			if st, ok := raw.(map[string]any); ok && str(st["Sid"]) == sid {
				continue
			}
			kept = append(kept, raw)
		}
		if len(kept) == 0 {
			fn.NimbusPolicy = nil
		} else {
			fn.NimbusPolicy["Statement"] = kept
		}
	}
	fn.RevisionId = uuidLike()
	if aerr := s.saveFunction(fn); aerr != nil {
		return aerr
	}
	w.WriteHeader(204)
	return nil
}

// lambdaTags is GET/POST/DELETE on /2017-03-31/tags/{arn}, where the ARN is
// URL-ENCODED into one path segment -- colons and all.
func (s *Server) lambdaTags(w http.ResponseWriter, r *http.Request, encodedARN string) *apiError {
	fn, aerr := s.loadFunction(encodedARN)
	if aerr != nil {
		return aerr
	}
	switch r.Method {
	case http.MethodGet:
		tags := fn.NimbusTags
		if tags == nil {
			tags = map[string]string{}
		}
		writeJSON(w, 200, map[string]any{"Tags": tags})
		return nil
	case http.MethodPost:
		var req struct {
			Tags map[string]string `json:"Tags"`
		}
		if err := readJSON(r, &req); err != nil {
			return &apiError{Status: 400, Code: "InvalidParameterValueException", Message: err.Error()}
		}
		if fn.NimbusTags == nil {
			fn.NimbusTags = map[string]string{}
		}
		for k, v := range req.Tags {
			fn.NimbusTags[k] = v
		}
	case http.MethodDelete:
		for _, key := range r.URL.Query()["tagKeys"] {
			delete(fn.NimbusTags, key)
		}
	default:
		return &apiError{Status: 404, Code: "ResourceNotFoundException",
			Message: "unsupported method on tags: " + r.Method}
	}
	if aerr := s.saveFunction(fn); aerr != nil {
		return aerr
	}
	w.WriteHeader(204)
	return nil
}
