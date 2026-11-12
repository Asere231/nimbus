package aws

import (
	"encoding/xml"
	"net/http"
	"strings"
	"time"
)

// STS: who am I, and let me pretend to be somebody else.
//
// GetCallerIdentity is the first call almost anything makes -- the aws CLI
// uses it as a connectivity check, and the Terraform provider calls it
// unless skip_credentials_validation is set -- so it is also the smallest
// end-to-end proof that routing, the Query protocol and the XML namespaces
// all work.

const stsVersion = "2011-06-15"

func (s *Server) serveSTS(w http.ResponseWriter, r *http.Request) string {
	p, err := readParams(r)
	if err != nil {
		writeError(w, "sts", errorf(400, "MalformedInput", "reading the request: %v", err))
		return "bad sts request"
	}
	action := p.str("Action")
	switch action {
	case "GetCallerIdentity":
		s.getCallerIdentity(w, r)
	case "AssumeRole":
		s.assumeRole(w, p)
	default:
		writeError(w, "sts", invalidAction("sts", action))
		return "unimplemented: sts " + action
	}
	return ""
}

type getCallerIdentityResponse struct {
	XMLName xml.Name `xml:"GetCallerIdentityResponse"`
	Xmlns   string   `xml:"xmlns,attr"`
	Result  struct {
		Arn     string `xml:"Arn"`
		UserID  string `xml:"UserId"`
		Account string `xml:"Account"`
	} `xml:"GetCallerIdentityResult"`
	Metadata responseMetadata `xml:"ResponseMetadata"`
}

// getCallerIdentity answers with the principal the request RESOLVES to,
// not a fixed name. It is the one call whose whole job is to say who you
// are, so once a request can be attributed to a role it has to say so --
// otherwise there is no way to check which identity a tool is using before
// finding out the hard way, through a denial.
func (s *Server) getCallerIdentity(w http.ResponseWriter, r *http.Request) {
	c := s.resolveCaller(r, "sts")

	doc := getCallerIdentityResponse{Xmlns: namespaceFor("sts")}
	doc.Result.Account = s.cfg.AccountID
	doc.Result.Arn = c.Principal.ID
	if doc.Result.Arn == "" {
		doc.Result.Arn = s.accountARN()
	}
	// An opaque, stable id: AIDA for a user, AROA for a role. Derived from
	// the ARN rather than random so that two calls agree, which is the
	// rule every other id in Nimbus follows.
	prefix := "AIDA"
	if c.Principal.Kind == "role" {
		prefix = "AROA"
	}
	doc.Result.UserID = prefix + strings.ToUpper(stableID(doc.Result.Arn, 17))
	doc.Metadata.RequestID = requestID()
	writeXML(w, 200, doc)
}

type assumeRoleResponse struct {
	XMLName xml.Name `xml:"AssumeRoleResponse"`
	Xmlns   string   `xml:"xmlns,attr"`
	Result  struct {
		Credentials struct {
			AccessKeyID     string `xml:"AccessKeyId"`
			SecretAccessKey string `xml:"SecretAccessKey"`
			SessionToken    string `xml:"SessionToken"`
			Expiration      string `xml:"Expiration"`
		} `xml:"Credentials"`
		AssumedRoleUser struct {
			Arn           string `xml:"Arn"`
			AssumedRoleID string `xml:"AssumedRoleId"`
		} `xml:"AssumedRoleUser"`
		PackedPolicySize int `xml:"PackedPolicySize"`
	} `xml:"AssumeRoleResult"`
	Metadata responseMetadata `xml:"ResponseMetadata"`
}

// assumeRole hands back credentials that are never checked, because Nimbus
// never checks a signature. They are well-formed so that a client which
// parses or stores them is happy, and derived from the role so that two
// calls for the same role agree.
func (s *Server) assumeRole(w http.ResponseWriter, p params) {
	roleARN := p.str("RoleArn")
	if roleARN == "" {
		writeError(w, "sts", errorf(400, "ValidationError",
			"1 validation error detected: Value null at 'roleArn' failed to satisfy constraint: Member must not be null"))
		return
	}
	sessionName := p.str("RoleSessionName")
	if sessionName == "" {
		sessionName = "nimbus"
	}
	name := roleARN
	if i := strings.LastIndex(roleARN, "/"); i >= 0 {
		name = roleARN[i+1:]
	}
	duration := time.Duration(p.intOr("DurationSeconds", 3600)) * time.Second

	doc := assumeRoleResponse{Xmlns: namespaceFor("sts")}
	c := &doc.Result.Credentials
	// Derived from the role, which is what lets a later request carrying
	// this key be mapped BACK to the role -- see principal.go. The one
	// definition lives there, because two copies would drift and the
	// symptom would be a principal silently becoming root.
	c.AccessKeyID = sessionKeyFor(roleARN)
	c.SecretAccessKey = stableID(roleARN+"/secret", 40)
	c.SessionToken = "nimbus/" + stableID(roleARN+"/"+sessionName, 32)
	c.Expiration = time.Now().UTC().Add(duration).Format(time.RFC3339)
	doc.Result.AssumedRoleUser.Arn = "arn:aws:sts::" + s.cfg.AccountID +
		":assumed-role/" + name + "/" + sessionName
	doc.Result.AssumedRoleUser.AssumedRoleID = "AROA" + strings.ToUpper(stableID(roleARN, 17)) + ":" + sessionName
	doc.Metadata.RequestID = requestID()
	writeXML(w, 200, doc)
}
