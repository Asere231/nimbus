package gcp

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Asere231/nimbus/internal/core"
)

// IAM: service accounts and their keys.
//
// A newer API than Compute, and it shows: no self links, no operations, and
// a "resource name" (`projects/{p}/serviceAccounts/{email}`) instead of a
// URL. A create answers with the account itself, not with an operation.
//
// Measured: after creating a service account the provider waits about ten
// seconds before it will use it -- a client-side sleep for IAM propagation
// that nothing Nimbus returns can shorten, exactly like azurerm's twenty
// silent seconds on a resource group. Don't chase it.
//
// Recorded, never enforced. No request is refused for lacking a role, so a
// local apply cannot prove a deployment's permissions are right -- the same
// honest limitation Azure RBAC and AWS IAM have here.

type serviceAccount struct {
	Name           string `json:"name"`
	ProjectID      string `json:"projectId"`
	UniqueID       string `json:"uniqueId"`
	Email          string `json:"email"`
	DisplayName    string `json:"displayName,omitempty"`
	Description    string `json:"description,omitempty"`
	Etag           string `json:"etag"`
	OAuth2ClientID string `json:"oauth2ClientId"`
	Disabled       bool   `json:"disabled,omitempty"`
}

// serveIAM routes /v1/projects/{p}/serviceAccounts[/{email}][/keys...].
func (s *Server) serveIAM(w http.ResponseWriter, r *http.Request, parts []string) string {
	i := indexOf(parts, "serviceAccounts")
	if i < 0 {
		writeError(w, notFound("Nimbus does not implement %s %s for IAM yet", r.Method, r.URL.Path))
		return "iam " + r.URL.Path
	}
	rest := parts[i+1:]

	// The colon-verb form again: .../serviceAccounts/{email}:signJwt.
	if len(rest) == 1 && strings.Contains(rest[0], ":") {
		email, verb, _ := strings.Cut(rest[0], ":")
		return s.serviceAccountVerb(w, r, email, verb)
	}

	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		return s.listServiceAccounts(w)
	case len(rest) == 0 && r.Method == http.MethodPost:
		return s.createServiceAccount(w, r)
	case len(rest) == 1 && r.Method == http.MethodGet:
		return s.getServiceAccount(w, rest[0])
	case len(rest) == 1 && (r.Method == http.MethodPatch || r.Method == http.MethodPut):
		return s.patchServiceAccount(w, r, rest[0])
	case len(rest) == 1 && r.Method == http.MethodDelete:
		return s.deleteServiceAccount(w, rest[0])
	case len(rest) == 2 && rest[1] == "keys" && r.Method == http.MethodGet:
		// Keys are not modelled: a service account here has none, and an
		// empty list is the truthful answer. Creating one would mean
		// minting a private key nobody can use.
		writeJSON(w, http.StatusOK, map[string]any{"keys": []any{}})
		return ""
	}
	writeError(w, notFound("Nimbus does not implement %s %s for IAM yet", r.Method, r.URL.Path))
	return "iam " + r.Method + " " + r.URL.Path
}

func (s *Server) saResourceID(email string) string {
	return s.base() + "/v1/" + s.serviceAccountName(email)
}

func (s *Server) createServiceAccount(w http.ResponseWriter, r *http.Request) string {
	var req struct {
		AccountID      string         `json:"accountId"`
		ServiceAccount serviceAccount `json:"serviceAccount"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, badRequest("%v", err))
		return "iam create: bad body"
	}
	if req.AccountID == "" {
		writeError(w, badRequest("Required field 'accountId' not specified"))
		return "iam create: no accountId"
	}
	email := s.serviceAccountEmail(req.AccountID)
	if s.store.Exists(s.saResourceID(email)) {
		writeError(w, errorf(http.StatusConflict, "alreadyExists", "ALREADY_EXISTS",
			"Service account %s already exists within project projects/%s.",
			email, s.cfg.ProjectID))
		return "iam create: exists"
	}

	sa := &serviceAccount{
		Name:      s.serviceAccountName(email),
		ProjectID: s.cfg.ProjectID,
		// Derived from the email, so two reads agree -- and so a deleted
		// account recreated with the same id is recognisably the same one,
		// which is what a client comparing uniqueId expects.
		UniqueID:       numericID(email),
		Email:          email,
		DisplayName:    req.ServiceAccount.DisplayName,
		Description:    req.ServiceAccount.Description,
		Etag:           etagOf(email),
		OAuth2ClientID: numericID("oauth/" + email),
	}
	if aerr := s.saveServiceAccount(sa); aerr != nil {
		writeError(w, aerr)
		return "iam create: " + aerr.Message
	}
	writeJSON(w, http.StatusOK, sa)
	return ""
}

func (s *Server) saveServiceAccount(sa *serviceAccount) *apiError {
	body, err := json.Marshal(sa)
	if err != nil {
		return internalError(err)
	}
	s.store.Put(&core.Resource{
		Cloud: "gcp", ID: s.saResourceID(sa.Email), Type: TypeServiceAccount,
		Name: sa.Email, ParentID: s.projectSelfLink(), Backing: "state", Body: body,
	})
	return nil
}

func (s *Server) loadServiceAccount(ref string) (*serviceAccount, *apiError) {
	// A client addresses an account by email, by unique id, or by the full
	// resource name. Only the email is a stable key here, so the others are
	// resolved by looking.
	email := lastSegment(ref)
	res, err := s.store.Get(s.saResourceID(email))
	if err != nil {
		for _, candidate := range s.store.List(func(r *core.Resource) bool {
			return r.Type == TypeServiceAccount
		}) {
			var sa serviceAccount
			if json.Unmarshal(candidate.Body, &sa) == nil && sa.UniqueID == email {
				return &sa, nil
			}
		}
		return nil, notFound("Unknown service account %s", ref)
	}
	var sa serviceAccount
	if err := json.Unmarshal(res.Body, &sa); err != nil {
		return nil, internalError(err)
	}
	return &sa, nil
}

func (s *Server) getServiceAccount(w http.ResponseWriter, ref string) string {
	sa, aerr := s.loadServiceAccount(ref)
	if aerr != nil {
		writeError(w, aerr)
		return "iam get: not found"
	}
	writeJSON(w, http.StatusOK, sa)
	return ""
}

func (s *Server) listServiceAccounts(w http.ResponseWriter) string {
	accounts := []json.RawMessage{}
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return r.Type == TypeServiceAccount
	}) {
		accounts = append(accounts, json.RawMessage(res.Body))
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
	return ""
}

// patchServiceAccount applies an update mask, like Compute's PATCH -- but
// IAM puts the mask IN THE BODY rather than in the query string, under
// `updateMask`, beside the account itself.
func (s *Server) patchServiceAccount(w http.ResponseWriter, r *http.Request, ref string) string {
	sa, aerr := s.loadServiceAccount(ref)
	if aerr != nil {
		writeError(w, aerr)
		return "iam patch: not found"
	}
	var req struct {
		ServiceAccount serviceAccount `json:"serviceAccount"`
		UpdateMask     string         `json:"updateMask"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, badRequest("%v", err))
		return "iam patch: bad body"
	}
	mask := maskFields(req.UpdateMask)
	if mask.allows("displayName") {
		sa.DisplayName = req.ServiceAccount.DisplayName
	}
	if mask.allows("description") {
		sa.Description = req.ServiceAccount.Description
	}
	if aerr := s.saveServiceAccount(sa); aerr != nil {
		writeError(w, aerr)
		return "iam patch: " + aerr.Message
	}
	writeJSON(w, http.StatusOK, sa)
	return ""
}

func (s *Server) deleteServiceAccount(w http.ResponseWriter, ref string) string {
	sa, aerr := s.loadServiceAccount(ref)
	if aerr != nil {
		writeError(w, aerr)
		return "iam delete: not found"
	}
	_, _ = s.store.Delete(s.saResourceID(sa.Email))
	// An empty object, not an operation: IAM has none.
	writeJSON(w, http.StatusOK, map[string]any{})
	return ""
}

func (s *Server) serviceAccountVerb(w http.ResponseWriter, r *http.Request, email, verb string) string {
	switch verb {
	case "getIamPolicy", "setIamPolicy":
		return s.projectIAM(w, r, email, verb)
	case "enable", "disable":
		sa, aerr := s.loadServiceAccount(email)
		if aerr != nil {
			writeError(w, aerr)
			return "iam " + verb + ": not found"
		}
		sa.Disabled = verb == "disable"
		if aerr := s.saveServiceAccount(sa); aerr != nil {
			writeError(w, aerr)
			return "iam " + verb
		}
		writeJSON(w, http.StatusOK, map[string]any{})
		return ""
	}
	writeError(w, notFound("Nimbus does not implement the service-account verb %q yet", verb))
	return "iam verb " + verb
}

func indexOf(parts []string, want string) int {
	for i, p := range parts {
		if p == want {
			return i
		}
	}
	return -1
}
