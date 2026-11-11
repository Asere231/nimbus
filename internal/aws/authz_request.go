package aws

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Asere231/nimbus/internal/authz"
)

// The enforcement point.
//
// One place, in front of every service, rather than a call at the top of
// each handler -- because a handler that forgets to ask is a silent GRANT,
// and a gap that fails open is exactly what this milestone exists to
// remove. The cost is that `route` has to know what action and resource a
// request is about before dispatching it, which is the per-service
// `authzTarget` below.
//
// **An action nobody has mapped is REFUSED**, naming itself. That is the
// uncomfortable choice and it is the right one: the alternative turns
// every gap in the table into a permission nobody granted. It also means a
// gap fails an apply loudly, where it can be seen and fixed, rather than
// quietly passing.
//
// One structural wrinkle: `readParams` (query.go) and `readJSON`
// (restjson.go) CONSUME the request body, so a check that runs before
// dispatch cannot read the body and still leave it for the handler. The
// rule that falls out is clean -- buffer the body for every service except
// S3. Query, DynamoDB and Logs bodies are small and already bounded, while
// S3's can be half a gigabyte and never carries the action anyway: S3 says
// what it wants with the method, the path and the query string.

// authzRequestLimit bounds what is buffered to find a request's target.
// Query bodies are bounded at 8 MB by readParams and JSON ones at 16 MB by
// readJSON, so this is the larger of the two.
const authzRequestLimit = 16 << 20

// target is what a request is asking to do.
type target struct {
	// Action is the IAM action name, e.g. "s3:GetObject".
	Action string
	// Resource is the ARN acted on, or "*" when the request names
	// nothing in particular (listing an account's buckets, say).
	Resource string
}

// authorize decides whether a request may proceed. It returns nil when it
// may, and the service's own refusal when it may not.
//
// The second return value is the note for the activity feed: a denial that
// nobody can see is worse than no enforcement at all, because then an
// apply simply fails.
func (s *Server) authorize(r *http.Request, service string) (*apiError, string) {
	if !s.cfg.EnforceIAM {
		return nil, ""
	}

	c := s.resolveCaller(r)

	// An unrestricted principal is allowed before the request is even
	// classified. That is not a shortcut: whether an action is mapped is
	// a question about POLICY, and root has none -- so refusing root for
	// an unmapped action would be refusing it for a reason that cannot
	// apply to it. Found by a test that created an IAM role as root and
	// was refused, which would have made the flag unusable.
	if c.Principal.Unrestricted {
		return nil, ""
	}

	tgt, ok := s.authzTarget(r, service)
	if !ok {
		// Refuse rather than guess. The message names the service and
		// the method so the gap can be closed, the same way an
		// unimplemented action names itself.
		return accessDenied(service, "unknown", c.Principal,
				fmt.Sprintf("Nimbus cannot yet work out which permission %s %s on the %s service needs, "+
					"so with --enforce-iam it is refused rather than allowed", r.Method, r.URL.Path, service)),
			"denied: unmapped " + service + " request " + r.Method + " " + r.URL.Path
	}

	if tgt.Action == alwaysAllowed {
		return nil, ""
	}

	req := authz.Request{
		Principal: c.Principal,
		Action:    tgt.Action,
		Resource:  tgt.Resource,
		Context: map[string]string{
			"aws:PrincipalArn":     c.Principal.ID,
			"aws:PrincipalAccount": s.cfg.AccountID,
			"aws:RequestedRegion":  regionOf(r, s.cfg.Region),
		},
	}
	// aws:username exists only for a user. Absent is not empty: a
	// StringEquals on it must not match a role, which is what makes the
	// key usable at all.
	if c.Principal.Kind == "user" {
		req.Context["aws:username"] = c.Principal.Name
	}

	d := authz.Evaluate(req, s.policies.rules(c.docs))
	if d.Allowed {
		return nil, ""
	}
	return accessDenied(service, tgt.Action, c.Principal, d.Reason),
		"denied: " + tgt.Action + " for " + describePrincipal(c.Principal) + " -- " + d.Reason
}

func describePrincipal(p authz.Principal) string {
	if p.ID == "" {
		return "an unidentified caller"
	}
	return p.Kind + " " + p.ID
}

// accessDenied is the refusal, in the code and the envelope the service's
// own clients recognise.
//
// Getting this wrong is expensive in a way that is easy to miss: a client
// that cannot recognise a denial does not report it, it RETRIES it -- five
// times with backoff for a 5xx. So a refusal is 403, never 500, and it
// carries each service's own code, because the provider matches on the
// code to tell "you may not" from "something is broken".
func accessDenied(service, action string, p authz.Principal, reason string) *apiError {
	message := fmt.Sprintf("%s is not authorized to perform %s: %s",
		describePrincipal(p), action, reason)

	switch service {
	case "ec2":
		// EC2 has its own code, and the aws provider matches on it.
		return &apiError{Status: 403, Code: "UnauthorizedOperation", Message: message}
	case "dynamodb", "lambda", "logs", "eks":
		// The JSON services. DynamoDB qualifies its own type name; see
		// writeDynamoError, which does that on the way out.
		return &apiError{Status: 403, Code: "AccessDeniedException", Message: message}
	default:
		// S3, IAM and STS all say AccessDenied.
		return &apiError{Status: 403, Code: "AccessDenied", Message: message}
	}
}

// writeServiceError sends an error in whichever envelope the service's
// clients can read. The three XML shapes go through writeError; the JSON
// services each have their own writer, and DynamoDB's qualifies the
// exception name, which botocore depends on.
func (s *Server) writeServiceError(w http.ResponseWriter, service string, e *apiError) {
	switch service {
	case "dynamodb":
		writeDynamoError(w, e)
	case "logs":
		writeLogsError(w, e)
	case "lambda", "eks":
		writeJSONError(w, e)
	default:
		writeError(w, service, e)
	}
}

// authzTarget works out the action and resource for a request, dispatching
// to whichever service's classifier knows.
func (s *Server) authzTarget(r *http.Request, service string) (target, bool) {
	switch service {
	case "s3":
		return s.s3Target(r)
	case "sts":
		return s.stsTarget(r)
	case "ec2":
		return s.ec2Target(r)
	case "iam":
		return s.iamTarget(r)
	case "dynamodb":
		return s.dynamoTarget(r)
	case "logs":
		return s.logsTarget(r)
	case "lambda":
		return s.lambdaTarget(r)
	case "eks":
		return s.eksTarget(r)
	}
	return target{}, false
}

// stsTarget covers the two STS actions Nimbus serves.
//
// `sts:GetCallerIdentity` requires NO permissions in real AWS -- it is
// granted to every principal implicitly and cannot be denied by an
// identity policy -- so it is answered without evaluating anything. That
// matters practically: it is how someone checks which identity a tool is
// using, and a setup where you cannot ask "who am I" until you already
// have permissions is one nobody can debug.
func (s *Server) stsTarget(r *http.Request) (target, bool) {
	p, err := readParams(r)
	if err != nil {
		return target{}, false
	}
	switch p.str("Action") {
	case "GetCallerIdentity":
		return target{Action: alwaysAllowed, Resource: "*"}, true
	case "AssumeRole":
		// A request with no RoleArn is malformed, and the handler says so
		// precisely ("Value null at 'roleArn'"). Authorization must not
		// pre-empt that with a denial naming an empty resource: a
		// confusing refusal in place of a clear validation error is a
		// worse answer, so an absent one widens to "*" and lets the
		// handler do its job.
		return target{Action: "sts:AssumeRole", Resource: p.strOr("RoleArn", "*")}, true
	}
	return target{}, false
}

// alwaysAllowed marks an action that needs no permission at all.
const alwaysAllowed = "(no permission required)"

// deniedReasonHeader carries a refusal's explanation where a body cannot.
// Nimbus's own, not an AWS header: no client reads it, and it exists
// because an HTTP HEAD response has no body to put the reason in. `curl
// -I` shows it, which is the difference between a debuggable refusal and
// "Forbidden: Forbidden".
const deniedReasonHeader = "X-Nimbus-Denied"

// bufferBody reads the request body into memory and returns it, so that
// the authorization check and the handler can both read it.
//
// The caller must put it back with restoreBody AFTER the check, not only
// before: the check's own classifier calls readParams, which reads to EOF,
// so a single reset leaves the handler with an empty body. That cost a
// test -- CreateRole answered "InvalidAction" for a request that plainly
// carried one -- and it is the whole reason this is two functions.
//
// Not done for S3: its bodies are the big ones and it needs no body to
// know what is being asked.
func bufferBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, authzRequestLimit))
	if err != nil {
		return nil, err
	}
	restoreBody(r, body)
	return body, nil
}

func restoreBody(r *http.Request, body []byte) {
	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
}

// --- explaining ------------------------------------------------------------

// explainActions are the actions worth asking about per resource type.
//
// A TABLE, where Azure and GCP derive theirs -- for the same reason the
// request classifier needed one: AWS's action names do not follow from the
// type. `AWS::S3::Bucket` is acted on by `s3:ListBucket` but its CONTENTS
// by `s3:GetObject` on a different ARN, which no derivation would produce.
//
// A type that is not here gets an empty report rather than invented
// actions, and the inspector says so.
var explainActions = map[string][]string{
	typeBucket:         {"s3:ListBucket", "s3:GetObject", "s3:PutObject", "s3:DeleteBucket"},
	typeDynamoTable:    {"dynamodb:DescribeTable", "dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:DeleteTable"},
	typeVPC:            {"ec2:DescribeVpcs", "ec2:CreateSubnet", "ec2:DeleteVpc"},
	typeLambdaFunction: {"lambda:GetFunction", "lambda:InvokeFunction", "lambda:UpdateFunctionCode", "lambda:DeleteFunction"},
	typeEKSCluster:     {"eks:DescribeCluster", "eks:CreateNodegroup", "eks:DeleteCluster"},
	typeRole:           {"iam:GetRole", "iam:PassRole", "iam:DeleteRole"},
	typeUser:           {"iam:GetUser", "iam:DeleteUser"},
	typePolicy:         {"iam:GetPolicy", "iam:DeletePolicy"},
	typeInstance:       {"ec2:DescribeInstances", "ec2:StopInstances", "ec2:TerminateInstances"},
	typeLogGroup:       {"logs:DescribeLogStreams", "logs:PutLogEvents", "logs:DeleteLogGroup"},
}

// resourceARNFor is the ARN those actions are asked about. For S3 the
// object actions are about the bucket's CONTENTS, which is a different ARN
// from the bucket -- the distinction `s3:ListBucket` on the bucket and
// `s3:GetObject` on `bucket/*` exists to draw.
func resourceARNFor(resourceType, action, id string) string {
	if resourceType == typeBucket && strings.Contains(action, "Object") {
		return id + "/*"
	}
	return id
}

// ExplainAccess answers what a principal may do to one resource.
func (s *Server) ExplainAccess(principal, resourceID, resourceType string) authz.Report {
	c := s.callerFor(principal)
	report := authz.Report{
		Principal: c.Principal.ID, Kind: c.Principal.Kind,
		Unrestricted: c.Principal.Unrestricted,
		Enforcing:    s.cfg.EnforceIAM, Resource: resourceID,
	}
	actions := explainActions[resourceType]
	if len(actions) == 0 {
		return report
	}
	rules := s.policies.rules(c.docs)
	for _, action := range actions {
		arn := resourceARNFor(resourceType, action, resourceID)
		report.Checks = append(report.Checks, authz.Explain(c.Principal, arn, []string{action}, rules)...)
	}
	return report
}

// ExplainOne answers a single question, which is what `nimbus can` asks.
func (s *Server) ExplainOne(principal, action, resource string) authz.Check {
	c := s.callerFor(principal)
	return authz.Explain(c.Principal, resource, []string{action}, s.policies.rules(c.docs))[0]
}

// callerFor resolves a named principal rather than one from a request, so
// the inspector and the CLI can ask about somebody else.
func (s *Server) callerFor(principal string) caller {
	if principal == "" {
		principal = s.cfg.Principal
	}
	if principal == "" {
		return s.rootCaller()
	}
	if c, ok := s.callerForKey(principal); ok {
		return c
	}
	// A name that maps to nothing is reported as itself holding no
	// policies, rather than silently as root: "this principal does not
	// exist" and "this principal may do anything" must not look alike.
	return caller{Principal: authz.Principal{ID: principal, Name: principal, Kind: "unknown"}}
}
