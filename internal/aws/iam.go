package aws

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/core"
)

// IAM: roles, users, policies and instance profiles.
//
// Recorded, never enforced -- exactly as Azure's RBAC is. Every role and
// policy round-trips so Terraform can manage them and the graph can draw
// them, but no request is ever refused for lacking a permission. A local
// apply therefore cannot prove a deployment's permissions are right, and
// CLAUDE.md says so for Azure already; it is just as true here.
//
// Two things make IAM's wire format differ from EC2's even though both are
// Query/XML:
//
//   - lists are wrapped in <member>, not <item>;
//   - results are nested one level deeper, inside <XxxResult>.
//
// And one thing catches everybody: IAM returns policy documents
// URL-ENCODED, and the SDKs unescape them on the way in. Returning raw
// JSON mostly works until a document contains a + or a %, at which point
// the client silently sees different text than was stored.

const iamVersion = "2010-05-08"

const (
	typeRole            = "AWS::IAM::Role"
	typeUser            = "AWS::IAM::User"
	typePolicy          = "AWS::IAM::Policy"
	typeInstanceProfile = "AWS::IAM::InstanceProfile"
)

func (s *Server) serveIAM(w http.ResponseWriter, r *http.Request) string {
	p, err := readParams(r)
	if err != nil {
		writeError(w, "iam", errorf(400, "MalformedInput", "reading the request: %v", err))
		return "bad iam request"
	}
	action := p.str("Action")

	var aerr *apiError
	switch action {
	// --- roles
	case "CreateRole":
		aerr = s.createRole(w, p)
	case "GetRole":
		aerr = s.getRole(w, p)
	case "ListRoles":
		aerr = s.listRoles(w, p)
	case "DeleteRole":
		aerr = s.deleteIAM(w, p, "RoleName", typeRole, "role", "DeleteRoleResponse")
	case "UpdateRole", "UpdateRoleDescription":
		aerr = s.updateRole(w, p)
	case "UpdateAssumeRolePolicy":
		aerr = s.updateAssumeRolePolicy(w, p)
	case "TagRole", "UntagRole":
		aerr = s.tagIAM(w, p, "RoleName", typeRole, "role", action == "TagRole")
	case "ListRoleTags":
		aerr = s.listTags(w, p, "RoleName", "role")

	// --- users
	case "CreateUser":
		aerr = s.createUser(w, p)
	case "GetUser":
		aerr = s.getUser(w, p)
	case "ListUsers":
		aerr = s.listUsers(w, p)
	case "DeleteUser":
		aerr = s.deleteIAM(w, p, "UserName", typeUser, "user", "DeleteUserResponse")
	case "TagUser", "UntagUser":
		aerr = s.tagIAM(w, p, "UserName", typeUser, "user", action == "TagUser")
	case "ListUserTags":
		aerr = s.listTags(w, p, "UserName", "user")

	// --- managed policies
	case "CreatePolicy":
		aerr = s.createPolicy(w, p)
	case "GetPolicy":
		aerr = s.getPolicy(w, p)
	case "GetPolicyVersion":
		aerr = s.getPolicyVersion(w, p)
	case "ListPolicies":
		aerr = s.listPolicies(w, p)
	case "ListPolicyVersions":
		aerr = s.listPolicyVersions(w, p)
	case "DeletePolicy":
		aerr = s.deletePolicy(w, p)
	case "AttachRolePolicy":
		aerr = s.attachPolicy(w, p, "RoleName", typeRole, "role", true, "AttachRolePolicyResponse")
	case "DetachRolePolicy":
		aerr = s.attachPolicy(w, p, "RoleName", typeRole, "role", false, "DetachRolePolicyResponse")
	case "ListAttachedRolePolicies":
		aerr = s.listAttached(w, p, "RoleName", typeRole, "role", "ListAttachedRolePoliciesResponse")
	case "AttachUserPolicy":
		aerr = s.attachPolicy(w, p, "UserName", typeUser, "user", true, "AttachUserPolicyResponse")
	case "DetachUserPolicy":
		aerr = s.attachPolicy(w, p, "UserName", typeUser, "user", false, "DetachUserPolicyResponse")
	case "ListAttachedUserPolicies":
		aerr = s.listAttached(w, p, "UserName", typeUser, "user", "ListAttachedUserPoliciesResponse")

	// --- inline policies
	case "PutRolePolicy":
		aerr = s.putInlinePolicy(w, p, "RoleName", typeRole, "role", "PutRolePolicyResponse")
	case "GetRolePolicy":
		aerr = s.getInlinePolicy(w, p, "RoleName", typeRole, "role", "GetRolePolicy")
	case "DeleteRolePolicy":
		aerr = s.deleteInlinePolicy(w, p, "RoleName", typeRole, "role", "DeleteRolePolicyResponse")
	case "ListRolePolicies":
		aerr = s.listInlinePolicies(w, p, "RoleName", typeRole, "role", "ListRolePolicies")

	// --- instance profiles
	case "CreateInstanceProfile":
		aerr = s.createInstanceProfile(w, p)
	case "GetInstanceProfile":
		aerr = s.getInstanceProfile(w, p)
	case "ListInstanceProfiles":
		aerr = s.listInstanceProfiles(w, p)
	case "DeleteInstanceProfile":
		aerr = s.deleteIAM(w, p, "InstanceProfileName", typeInstanceProfile, "instance-profile", "DeleteInstanceProfileResponse")
	case "AddRoleToInstanceProfile":
		aerr = s.instanceProfileRole(w, p, true)
	case "RemoveRoleFromInstanceProfile":
		aerr = s.instanceProfileRole(w, p, false)
	case "ListInstanceProfilesForRole":
		aerr = s.listInstanceProfilesForRole(w, p)

	default:
		writeError(w, "iam", invalidAction("iam", action))
		return "unimplemented: iam " + action
	}

	if aerr != nil {
		writeError(w, "iam", aerr)
		return "iam " + action + ": " + aerr.Code
	}
	return ""
}

// --- the stored shapes ----------------------------------------------------

type iamTag struct {
	Key   string `xml:"Key" json:"key"`
	Value string `xml:"Value" json:"value"`
}

type attachedPolicy struct {
	PolicyName string `xml:"PolicyName" json:"policyName"`
	PolicyArn  string `xml:"PolicyArn" json:"policyArn"`
}

type iamRole struct {
	Path                     string   `xml:"Path" json:"path"`
	RoleName                 string   `xml:"RoleName" json:"roleName"`
	RoleID                   string   `xml:"RoleId" json:"roleId"`
	Arn                      string   `xml:"Arn" json:"arn"`
	CreateDate               string   `xml:"CreateDate" json:"createDate"`
	AssumeRolePolicyDocument string   `xml:"AssumeRolePolicyDocument" json:"assumeRolePolicyDocument"`
	Description              string   `xml:"Description,omitempty" json:"description,omitempty"`
	MaxSessionDuration       int      `xml:"MaxSessionDuration" json:"maxSessionDuration"`
	Tags                     []iamTag `xml:"Tags>member,omitempty" json:"tags,omitempty"`

	// Not part of the Role document: attachments and inline policies are
	// read with their own calls.
	Attached []attachedPolicy  `xml:"-" json:"attached,omitempty"`
	Inline   map[string]string `xml:"-" json:"inline,omitempty"`
}

type iamUser struct {
	Path       string   `xml:"Path" json:"path"`
	UserName   string   `xml:"UserName" json:"userName"`
	UserID     string   `xml:"UserId" json:"userId"`
	Arn        string   `xml:"Arn" json:"arn"`
	CreateDate string   `xml:"CreateDate" json:"createDate"`
	Tags       []iamTag `xml:"Tags>member,omitempty" json:"tags,omitempty"`

	Attached []attachedPolicy  `xml:"-" json:"attached,omitempty"`
	Inline   map[string]string `xml:"-" json:"inline,omitempty"`
}

type iamPolicy struct {
	PolicyName       string   `xml:"PolicyName" json:"policyName"`
	PolicyID         string   `xml:"PolicyId" json:"policyId"`
	Arn              string   `xml:"Arn" json:"arn"`
	Path             string   `xml:"Path" json:"path"`
	DefaultVersionID string   `xml:"DefaultVersionId" json:"defaultVersionId"`
	AttachmentCount  int      `xml:"AttachmentCount" json:"attachmentCount"`
	IsAttachable     bool     `xml:"IsAttachable" json:"isAttachable"`
	Description      string   `xml:"Description,omitempty" json:"description,omitempty"`
	CreateDate       string   `xml:"CreateDate" json:"createDate"`
	UpdateDate       string   `xml:"UpdateDate" json:"updateDate"`
	Tags             []iamTag `xml:"Tags>member,omitempty" json:"tags,omitempty"`

	Document string `xml:"-" json:"document"`
}

type iamInstanceProfile struct {
	Path                string    `xml:"Path" json:"path"`
	InstanceProfileName string    `xml:"InstanceProfileName" json:"instanceProfileName"`
	InstanceProfileID   string    `xml:"InstanceProfileId" json:"instanceProfileId"`
	Arn                 string    `xml:"Arn" json:"arn"`
	CreateDate          string    `xml:"CreateDate" json:"createDate"`
	Roles               []iamRole `xml:"Roles>member" json:"roles"`
	Tags                []iamTag  `xml:"Tags>member,omitempty" json:"tags,omitempty"`
}

// iamARN builds the ARN that is also the store's id for a thing.
func (s *Server) iamARN(kind, path, name string) string {
	if path == "" {
		path = "/"
	}
	return "arn:aws:iam::" + s.cfg.AccountID + ":" + kind + path + name
}

// uniqueID is IAM's AROA/AIDA/ANPA-prefixed opaque id. Derived from the ARN
// so that two reads of one role agree, which is the rule every id in
// Nimbus follows.
func uniqueID(prefix, arn string) string {
	return prefix + strings.ToUpper(stableID(arn, 17))
}

func iamNow() string { return time.Now().UTC().Format(time.RFC3339) }

// encodeDocument is how IAM returns a policy document: URL-encoded.
//
// The SDKs unescape whatever comes back. Returning raw JSON looks fine in
// a browser and works right up until a document contains a + or a %, at
// which point the client quietly reads different text than was stored --
// a diff that never settles, with nothing to point at.
func encodeDocument(doc string) string {
	if doc == "" {
		return ""
	}
	return url.QueryEscape(doc)
}

func (s *Server) loadIAM(kind, name string, into any) *apiError {
	arn := s.iamARN(kind, "/", name)
	res, err := s.store.Get(arn)
	if err != nil {
		return noSuchEntity(kind, name)
	}
	if err := json.Unmarshal(res.Body, into); err != nil {
		return errorf(500, "InternalError", "reading the %s: %v", kind, err)
	}
	return nil
}

func (s *Server) saveIAM(typ, kind, name, arn string, v any) *apiError {
	body, err := json.Marshal(v)
	if err != nil {
		return errorf(500, "InternalError", "storing the %s: %v", kind, err)
	}
	s.store.Put(&core.Resource{
		Cloud: "aws", ID: arn, Type: typ, Name: name,
		ParentID: s.accountARN(), Backing: "modelled", Body: body,
	})
	return nil
}

// noSuchEntity is IAM's one not-found code, whatever the kind. Unlike
// EC2, which has a different code per kind, IAM uses this for everything
// and the provider matches on it to tell "already gone" from "broken".
func noSuchEntity(kind, name string) *apiError {
	return errorf(404, "NoSuchEntity", "The %s with name %s cannot be found.", kind, name)
}

// --- roles ----------------------------------------------------------------

type roleResponse struct {
	XMLName xml.Name
	Xmlns   string `xml:"xmlns,attr"`
	Result  struct {
		Role iamRole `xml:"Role"`
	} `xml:"CreateRoleResult"`
	Metadata responseMetadata `xml:"ResponseMetadata"`
}

func (s *Server) createRole(w http.ResponseWriter, p params) *apiError {
	name := p.str("RoleName")
	if name == "" {
		return errorf(400, "ValidationError", "RoleName is required")
	}
	path := p.strOr("Path", "/")
	arn := s.iamARN("role", path, name)
	if s.store.Exists(arn) {
		return errorf(409, "EntityAlreadyExists", "Role with name %s already exists.", name)
	}
	role := iamRole{
		Path:                     path,
		RoleName:                 name,
		Arn:                      arn,
		RoleID:                   uniqueID("AROA", arn),
		CreateDate:               iamNow(),
		AssumeRolePolicyDocument: p.str("AssumeRolePolicyDocument"),
		Description:              p.str("Description"),
		MaxSessionDuration:       p.intOr("MaxSessionDuration", 3600),
		Tags:                     readIAMTags(p),
		Inline:                   map[string]string{},
	}
	if aerr := s.saveIAM(typeRole, "role", name, arn, role); aerr != nil {
		return aerr
	}
	writeRole(w, "CreateRoleResponse", "CreateRoleResult", role)
	return nil
}

// writeRole sends a Role inside whichever result element the action uses.
// The element names differ per action while the body is identical, which
// is why this builds the document rather than declaring twenty structs.
func writeRole(w http.ResponseWriter, response, result string, role iamRole) {
	role.AssumeRolePolicyDocument = encodeDocument(role.AssumeRolePolicyDocument)
	var buf strings.Builder
	inner, _ := xml.MarshalIndent(role, "    ", "  ")
	buf.WriteString(fmt.Sprintf("<%s xmlns=%q>\n  <%s>\n", response, namespaceFor("iam"), result))
	buf.WriteString("    " + rename(string(inner), "Role"))
	buf.WriteString(fmt.Sprintf("\n  </%s>\n  <ResponseMetadata><RequestId>%s</RequestId></ResponseMetadata>\n</%s>",
		result, requestID(), response))
	writeRawXML(w, 200, buf.String())
}

func (s *Server) getRole(w http.ResponseWriter, p params) *apiError {
	var role iamRole
	if aerr := s.loadIAM("role", p.str("RoleName"), &role); aerr != nil {
		return aerr
	}
	writeRole(w, "GetRoleResponse", "GetRoleResult", role)
	return nil
}

func (s *Server) updateRole(w http.ResponseWriter, p params) *apiError {
	name := p.str("RoleName")
	var role iamRole
	if aerr := s.loadIAM("role", name, &role); aerr != nil {
		return aerr
	}
	if v := p.str("Description"); v != "" {
		role.Description = v
	}
	if v := p.intOr("MaxSessionDuration", 0); v != 0 {
		role.MaxSessionDuration = v
	}
	if aerr := s.saveIAM(typeRole, "role", name, role.Arn, role); aerr != nil {
		return aerr
	}
	writeIAMEmpty(w, "UpdateRoleResponse")
	return nil
}

func (s *Server) updateAssumeRolePolicy(w http.ResponseWriter, p params) *apiError {
	name := p.str("RoleName")
	var role iamRole
	if aerr := s.loadIAM("role", name, &role); aerr != nil {
		return aerr
	}
	role.AssumeRolePolicyDocument = p.str("PolicyDocument")
	if aerr := s.saveIAM(typeRole, "role", name, role.Arn, role); aerr != nil {
		return aerr
	}
	writeIAMEmpty(w, "UpdateAssumeRolePolicyResponse")
	return nil
}

func (s *Server) listRoles(w http.ResponseWriter, p params) *apiError {
	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("<ListRolesResponse xmlns=%q>\n  <ListRolesResult>\n    <IsTruncated>false</IsTruncated>\n    <Roles>\n",
		namespaceFor("iam")))
	for _, role := range list[iamRole](s, typeRole) {
		role.AssumeRolePolicyDocument = encodeDocument(role.AssumeRolePolicyDocument)
		inner, _ := xml.MarshalIndent(role, "      ", "  ")
		buf.WriteString("      " + rename(string(inner), "member") + "\n")
	}
	buf.WriteString(fmt.Sprintf("    </Roles>\n  </ListRolesResult>\n  <ResponseMetadata><RequestId>%s</RequestId></ResponseMetadata>\n</ListRolesResponse>", requestID()))
	writeRawXML(w, 200, buf.String())
	return nil
}

// --- users ----------------------------------------------------------------

func (s *Server) createUser(w http.ResponseWriter, p params) *apiError {
	name := p.str("UserName")
	if name == "" {
		return errorf(400, "ValidationError", "UserName is required")
	}
	path := p.strOr("Path", "/")
	arn := s.iamARN("user", path, name)
	if s.store.Exists(arn) {
		return errorf(409, "EntityAlreadyExists", "User with name %s already exists.", name)
	}
	user := iamUser{
		Path: path, UserName: name, Arn: arn,
		UserID: uniqueID("AIDA", arn), CreateDate: iamNow(),
		Tags: readIAMTags(p), Inline: map[string]string{},
	}
	if aerr := s.saveIAM(typeUser, "user", name, arn, user); aerr != nil {
		return aerr
	}
	writeMember(w, "CreateUserResponse", "CreateUserResult", "User", user)
	return nil
}

func (s *Server) getUser(w http.ResponseWriter, p params) *apiError {
	name := p.str("UserName")
	if name == "" {
		// GetUser with no name means "who am I", which is a caller we do
		// not have -- answer with a synthetic one rather than an error,
		// because clients use it as a connectivity check.
		arn := s.iamARN("user", "/", "nimbus")
		writeMember(w, "GetUserResponse", "GetUserResult", "User", iamUser{
			Path: "/", UserName: "nimbus", Arn: arn,
			UserID: uniqueID("AIDA", arn), CreateDate: iamNow(),
		})
		return nil
	}
	var user iamUser
	if aerr := s.loadIAM("user", name, &user); aerr != nil {
		return aerr
	}
	writeMember(w, "GetUserResponse", "GetUserResult", "User", user)
	return nil
}

func (s *Server) listUsers(w http.ResponseWriter, p params) *apiError {
	writeMembers(w, "ListUsersResponse", "ListUsersResult", "Users", list[iamUser](s, typeUser))
	return nil
}

// --- managed policies -----------------------------------------------------

func (s *Server) createPolicy(w http.ResponseWriter, p params) *apiError {
	name := p.str("PolicyName")
	if name == "" {
		return errorf(400, "ValidationError", "PolicyName is required")
	}
	path := p.strOr("Path", "/")
	arn := s.iamARN("policy", path, name)
	if s.store.Exists(arn) {
		return errorf(409, "EntityAlreadyExists", "A policy called %s already exists.", name)
	}
	policy := iamPolicy{
		PolicyName: name, Arn: arn, Path: path,
		PolicyID:         uniqueID("ANPA", arn),
		DefaultVersionID: "v1",
		IsAttachable:     true,
		Description:      p.str("Description"),
		CreateDate:       iamNow(),
		UpdateDate:       iamNow(),
		Document:         p.str("PolicyDocument"),
		Tags:             readIAMTags(p),
	}
	if aerr := s.saveIAM(typePolicy, "policy", name, arn, policy); aerr != nil {
		return aerr
	}
	writeMember(w, "CreatePolicyResponse", "CreatePolicyResult", "Policy", policy)
	return nil
}

// policyByARN finds a managed policy by its ARN, which is how everything
// except CreatePolicy refers to one.
func (s *Server) policyByARN(arn string) (*iamPolicy, *apiError) {
	res, err := s.store.Get(arn)
	if err != nil || res.Type != typePolicy {
		return nil, errorf(404, "NoSuchEntity", "Policy %s does not exist.", arn)
	}
	var policy iamPolicy
	if err := json.Unmarshal(res.Body, &policy); err != nil {
		return nil, errorf(500, "InternalError", "reading the policy: %v", err)
	}
	return &policy, nil
}

func (s *Server) getPolicy(w http.ResponseWriter, p params) *apiError {
	policy, aerr := s.policyByARN(p.str("PolicyArn"))
	if aerr != nil {
		return aerr
	}
	writeMember(w, "GetPolicyResponse", "GetPolicyResult", "Policy", *policy)
	return nil
}

func (s *Server) getPolicyVersion(w http.ResponseWriter, p params) *apiError {
	policy, aerr := s.policyByARN(p.str("PolicyArn"))
	if aerr != nil {
		return aerr
	}
	type version struct {
		Document         string `xml:"Document"`
		VersionID        string `xml:"VersionId"`
		IsDefaultVersion bool   `xml:"IsDefaultVersion"`
		CreateDate       string `xml:"CreateDate"`
	}
	writeMember(w, "GetPolicyVersionResponse", "GetPolicyVersionResult", "PolicyVersion", version{
		Document:         encodeDocument(policy.Document),
		VersionID:        policy.DefaultVersionID,
		IsDefaultVersion: true,
		CreateDate:       policy.CreateDate,
	})
	return nil
}

func (s *Server) listPolicyVersions(w http.ResponseWriter, p params) *apiError {
	policy, aerr := s.policyByARN(p.str("PolicyArn"))
	if aerr != nil {
		return aerr
	}
	type version struct {
		VersionID        string `xml:"VersionId"`
		IsDefaultVersion bool   `xml:"IsDefaultVersion"`
		CreateDate       string `xml:"CreateDate"`
	}
	writeMembers(w, "ListPolicyVersionsResponse", "ListPolicyVersionsResult", "Versions",
		[]version{{VersionID: policy.DefaultVersionID, IsDefaultVersion: true, CreateDate: policy.CreateDate}})
	return nil
}

func (s *Server) listPolicies(w http.ResponseWriter, p params) *apiError {
	writeMembers(w, "ListPoliciesResponse", "ListPoliciesResult", "Policies", list[iamPolicy](s, typePolicy))
	return nil
}

func (s *Server) deletePolicy(w http.ResponseWriter, p params) *apiError {
	arn := p.str("PolicyArn")
	if _, aerr := s.policyByARN(arn); aerr != nil {
		return aerr
	}
	if _, err := s.store.Delete(arn); err != nil {
		return errorf(404, "NoSuchEntity", "Policy %s does not exist.", arn)
	}
	writeIAMEmpty(w, "DeletePolicyResponse")
	return nil
}

// --- attaching ------------------------------------------------------------

// attachPolicy adds or removes a managed policy on a role or a user. The
// attachment lives on the principal rather than on the policy, which is
// how IAM reports it: ListAttachedRolePolicies asks about the role.
func (s *Server) attachPolicy(w http.ResponseWriter, p params, nameParam, typ, kind string, add bool, element string) *apiError {
	name := p.str(nameParam)
	arn := s.iamARN(kind, "/", name)
	policyArn := p.str("PolicyArn")
	policy, aerr := s.policyByARN(policyArn)
	if aerr != nil {
		return aerr
	}
	err := s.store.Update(arn, func(body map[string]any) error {
		attached := decodeAttached(body["attached"])
		found := -1
		for i, a := range attached {
			if a.PolicyArn == policyArn {
				found = i
			}
		}
		switch {
		case add && found < 0:
			attached = append(attached, attachedPolicy{PolicyName: policy.PolicyName, PolicyArn: policyArn})
		case !add && found >= 0:
			attached = append(attached[:found], attached[found+1:]...)
		}
		body["attached"] = encodeAttached(attached)
		return nil
	})
	if err != nil {
		return noSuchEntity(kind, name)
	}
	writeIAMEmpty(w, element)
	return nil
}

func (s *Server) listAttached(w http.ResponseWriter, p params, nameParam, typ, kind, element string) *apiError {
	name := p.str(nameParam)
	res, err := s.store.Get(s.iamARN(kind, "/", name))
	if err != nil {
		return noSuchEntity(kind, name)
	}
	attached := decodeAttached(bodyField(res.Body, "attached"))
	result := strings.TrimSuffix(element, "Response") + "Result"
	writeMembers(w, element, result, "AttachedPolicies", attached)
	return nil
}

func decodeAttached(v any) []attachedPolicy {
	raw, _ := v.([]any)
	out := make([]attachedPolicy, 0, len(raw))
	for _, item := range raw {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		name, _ := m["policyName"].(string)
		arn, _ := m["policyArn"].(string)
		out = append(out, attachedPolicy{PolicyName: name, PolicyArn: arn})
	}
	return out
}

func encodeAttached(list []attachedPolicy) []any {
	out := make([]any, 0, len(list))
	for _, a := range list {
		out = append(out, map[string]any{"policyName": a.PolicyName, "policyArn": a.PolicyArn})
	}
	return out
}

// --- inline policies ------------------------------------------------------

func (s *Server) putInlinePolicy(w http.ResponseWriter, p params, nameParam, typ, kind, element string) *apiError {
	name := p.str(nameParam)
	err := s.store.Update(s.iamARN(kind, "/", name), func(body map[string]any) error {
		inline, _ := body["inline"].(map[string]any)
		if inline == nil {
			inline = map[string]any{}
		}
		inline[p.str("PolicyName")] = p.str("PolicyDocument")
		body["inline"] = inline
		return nil
	})
	if err != nil {
		return noSuchEntity(kind, name)
	}
	writeIAMEmpty(w, element)
	return nil
}

func (s *Server) getInlinePolicy(w http.ResponseWriter, p params, nameParam, typ, kind, action string) *apiError {
	name := p.str(nameParam)
	res, err := s.store.Get(s.iamARN(kind, "/", name))
	if err != nil {
		return noSuchEntity(kind, name)
	}
	inline, _ := bodyField(res.Body, "inline").(map[string]any)
	doc, ok := inline[p.str("PolicyName")].(string)
	if !ok {
		return noSuchEntity("policy", p.str("PolicyName"))
	}
	type result struct {
		RoleName       string `xml:"RoleName,omitempty"`
		UserName       string `xml:"UserName,omitempty"`
		PolicyName     string `xml:"PolicyName"`
		PolicyDocument string `xml:"PolicyDocument"`
	}
	r := result{PolicyName: p.str("PolicyName"), PolicyDocument: encodeDocument(doc)}
	if kind == "role" {
		r.RoleName = name
	} else {
		r.UserName = name
	}
	writeFlat(w, action+"Response", action+"Result", r)
	return nil
}

func (s *Server) deleteInlinePolicy(w http.ResponseWriter, p params, nameParam, typ, kind, element string) *apiError {
	name := p.str(nameParam)
	err := s.store.Update(s.iamARN(kind, "/", name), func(body map[string]any) error {
		inline, _ := body["inline"].(map[string]any)
		delete(inline, p.str("PolicyName"))
		body["inline"] = inline
		return nil
	})
	if err != nil {
		return noSuchEntity(kind, name)
	}
	writeIAMEmpty(w, element)
	return nil
}

func (s *Server) listInlinePolicies(w http.ResponseWriter, p params, nameParam, typ, kind, action string) *apiError {
	name := p.str(nameParam)
	res, err := s.store.Get(s.iamARN(kind, "/", name))
	if err != nil {
		return noSuchEntity(kind, name)
	}
	inline, _ := bodyField(res.Body, "inline").(map[string]any)
	names := make([]string, 0, len(inline))
	for k := range inline {
		names = append(names, k)
	}
	sort.Strings(names)
	writeStringMembers(w, action+"Response", action+"Result", "PolicyNames", names)
	return nil
}

// --- instance profiles ----------------------------------------------------

func (s *Server) createInstanceProfile(w http.ResponseWriter, p params) *apiError {
	name := p.str("InstanceProfileName")
	path := p.strOr("Path", "/")
	arn := s.iamARN("instance-profile", path, name)
	if s.store.Exists(arn) {
		return errorf(409, "EntityAlreadyExists", "Instance Profile %s already exists.", name)
	}
	profile := iamInstanceProfile{
		Path: path, InstanceProfileName: name, Arn: arn,
		InstanceProfileID: uniqueID("AIPA", arn), CreateDate: iamNow(),
		Roles: []iamRole{}, Tags: readIAMTags(p),
	}
	if aerr := s.saveIAM(typeInstanceProfile, "instance-profile", name, arn, profile); aerr != nil {
		return aerr
	}
	writeMember(w, "CreateInstanceProfileResponse", "CreateInstanceProfileResult", "InstanceProfile", profile)
	return nil
}

func (s *Server) getInstanceProfile(w http.ResponseWriter, p params) *apiError {
	var profile iamInstanceProfile
	if aerr := s.loadIAM("instance-profile", p.str("InstanceProfileName"), &profile); aerr != nil {
		return aerr
	}
	writeMember(w, "GetInstanceProfileResponse", "GetInstanceProfileResult", "InstanceProfile", profile)
	return nil
}

func (s *Server) listInstanceProfiles(w http.ResponseWriter, p params) *apiError {
	writeMembers(w, "ListInstanceProfilesResponse", "ListInstanceProfilesResult", "InstanceProfiles",
		list[iamInstanceProfile](s, typeInstanceProfile))
	return nil
}

func (s *Server) instanceProfileRole(w http.ResponseWriter, p params, add bool) *apiError {
	name := p.str("InstanceProfileName")
	var profile iamInstanceProfile
	if aerr := s.loadIAM("instance-profile", name, &profile); aerr != nil {
		return aerr
	}
	roleName := p.str("RoleName")
	var role iamRole
	if aerr := s.loadIAM("role", roleName, &role); aerr != nil {
		return aerr
	}
	if add {
		profile.Roles = []iamRole{role}
	} else {
		profile.Roles = []iamRole{}
	}
	if aerr := s.saveIAM(typeInstanceProfile, "instance-profile", name, profile.Arn, profile); aerr != nil {
		return aerr
	}
	if add {
		writeIAMEmpty(w, "AddRoleToInstanceProfileResponse")
	} else {
		writeIAMEmpty(w, "RemoveRoleFromInstanceProfileResponse")
	}
	return nil
}

func (s *Server) listInstanceProfilesForRole(w http.ResponseWriter, p params) *apiError {
	roleName := p.str("RoleName")
	out := []iamInstanceProfile{}
	for _, profile := range list[iamInstanceProfile](s, typeInstanceProfile) {
		for _, r := range profile.Roles {
			if r.RoleName == roleName {
				out = append(out, profile)
			}
		}
	}
	writeMembers(w, "ListInstanceProfilesForRoleResponse", "ListInstanceProfilesForRoleResult", "InstanceProfiles", out)
	return nil
}

// --- tags and deletes -----------------------------------------------------

// readIAMTags reads the Tags.member.N.Key form IAM uses, which differs
// from EC2's Tag.N.Key for no reason anyone has ever explained.
func readIAMTags(p params) []iamTag {
	var out []iamTag
	for i := 1; ; i++ {
		key := p.str(fmt.Sprintf("Tags.member.%d.Key", i))
		if key == "" {
			return out
		}
		out = append(out, iamTag{Key: key, Value: p.str(fmt.Sprintf("Tags.member.%d.Value", i))})
	}
}

func (s *Server) tagIAM(w http.ResponseWriter, p params, nameParam, typ, kind string, add bool) *apiError {
	name := p.str(nameParam)
	incoming := readIAMTags(p)
	if !add {
		for i := 1; ; i++ {
			key := p.str(fmt.Sprintf("TagKeys.member.%d", i))
			if key == "" {
				break
			}
			incoming = append(incoming, iamTag{Key: key})
		}
	}
	err := s.store.Update(s.iamARN(kind, "/", name), func(body map[string]any) error {
		current := decodeIAMTags(body["tags"])
		body["tags"] = encodeIAMTags(mergeIAMTags(current, incoming, add))
		return nil
	})
	if err != nil {
		return noSuchEntity(kind, name)
	}
	writeIAMEmpty(w, map[bool]string{true: "TagRoleResponse", false: "UntagRoleResponse"}[add])
	return nil
}

func (s *Server) listTags(w http.ResponseWriter, p params, nameParam, kind string) *apiError {
	name := p.str(nameParam)
	res, err := s.store.Get(s.iamARN(kind, "/", name))
	if err != nil {
		return noSuchEntity(kind, name)
	}
	action := "ListRoleTags"
	if kind == "user" {
		action = "ListUserTags"
	}
	writeMembers(w, action+"Response", action+"Result", "Tags", decodeIAMTags(bodyField(res.Body, "tags")))
	return nil
}

func decodeIAMTags(v any) []iamTag {
	raw, _ := v.([]any)
	out := make([]iamTag, 0, len(raw))
	for _, item := range raw {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		key, _ := m["key"].(string)
		value, _ := m["value"].(string)
		out = append(out, iamTag{Key: key, Value: value})
	}
	return out
}

func encodeIAMTags(tags []iamTag) []any {
	out := make([]any, 0, len(tags))
	for _, t := range tags {
		out = append(out, map[string]any{"key": t.Key, "value": t.Value})
	}
	return out
}

func mergeIAMTags(existing, incoming []iamTag, add bool) []iamTag {
	out := append([]iamTag(nil), existing...)
	for _, t := range incoming {
		found := -1
		for i, e := range out {
			if e.Key == t.Key {
				found = i
			}
		}
		switch {
		case !add && found >= 0:
			out = append(out[:found], out[found+1:]...)
		case add && found >= 0:
			out[found] = t
		case add:
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (s *Server) deleteIAM(w http.ResponseWriter, p params, nameParam, typ, kind, element string) *apiError {
	name := p.str(nameParam)
	arn := s.iamARN(kind, "/", name)
	if _, err := s.store.Get(arn); err != nil {
		return noSuchEntity(kind, name)
	}
	if _, err := s.store.Delete(arn); err != nil {
		return noSuchEntity(kind, name)
	}
	writeIAMEmpty(w, element)
	return nil
}
