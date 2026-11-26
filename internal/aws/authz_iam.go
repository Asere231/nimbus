package aws

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Asere231/nimbus/internal/authz"
)

// Compiling IAM policy documents into decisions.
//
// This is the AWS half of the authorization work: `internal/authz` holds
// the shape of a decision and the order rules settle in, and knows nothing
// about IAM. Everything IAM-shaped is here -- `Sid`, `NotAction`,
// `Condition`, the string-or-list sloppiness of the JSON, and the fact that
// action matching ignores case while resource matching does not.
//
// Three things about real documents that a first attempt gets wrong:
//
//   - **Statement, Action and Resource are each a string OR a list.**
//     `aws_iam_policy_document` emits lists; a hand-written example usually
//     does not. Both have to decode or half the policies in the world are
//     silently empty.
//   - **Action matching is case-insensitive, resource matching is not.**
//     `s3:getobject` grants `s3:GetObject`; `arn:aws:s3:::Demo` is not
//     `arn:aws:s3:::demo`.
//   - **An unsupported Condition must refuse, not pass** -- and must refuse
//     only the requests its own statement is about. A statement with an
//     unevaluable condition on `ec2:RunInstances` has no business refusing
//     an S3 read, which is why `Applies` settles the action and the
//     resource before it ever looks at a condition.

// policyKind says whether a document hangs off the CALLER or off the
// RESOURCE. Two rules differ, and both matter:
//
//   - A resource policy MUST name a Principal and an identity policy must
//     NOT. The tempting reading of a missing Principal -- "it applies to
//     everybody" -- is the most dangerous one available, so it refuses.
//   - A resource policy's Resource is optional, defaulting to the thing
//     the policy is attached to. An identity policy's is required.
//
// What does NOT differ is how the two combine, which is the surprise of
// this milestone: `authz.Evaluate` already does "an explicit deny beats
// everything, an allow from anywhere allows" over a flat rule set, and
// that IS AWS's same-account rule for identity plus resource policies. So
// the two sets are simply concatenated and the core needed no change.
// (Cross-account requires BOTH to allow; Nimbus has one account, so that
// path cannot arise -- docs/ROADMAP.md says so.)
type policyKind int

const (
	identityPolicy policyKind = iota
	resourcePolicy
)

// policyDocument is one document and where it came from, the `Source`
// being what a decision quotes back so somebody can find the statement in
// their own configuration.
type policyDocument struct {
	Source   string
	Document string
	// Kind and Attached describe a resource policy: which thing it hangs
	// off, so a statement that names no Resource can default to it.
	Kind     policyKind
	Attached string
	// Err records a document that could not even be fetched -- an AWS
	// managed policy Nimbus does not bundle, above all. It is carried
	// here rather than dropped so that it becomes a rule that refuses
	// and explains itself: a policy nobody can read is not a policy that
	// grants nothing.
	Err error
}

// --- the JSON, as it really arrives ---------------------------------------

// stringOrList decodes a field that may be either.
type stringOrList []string

func (s *stringOrList) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*s = stringOrList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("want a string or a list of strings, got %s", truncate(string(data), 40))
	}
	*s = many
	return nil
}

type rawStatement struct {
	Sid         string       `json:"Sid"`
	Effect      string       `json:"Effect"`
	Action      stringOrList `json:"Action"`
	NotAction   stringOrList `json:"NotAction"`
	Resource    stringOrList `json:"Resource"`
	NotResource stringOrList `json:"NotResource"`
	// Principal and NotPrincipal appear only in a RESOURCE policy, and
	// are required there.
	Principal    rawPrincipal `json:"Principal"`
	NotPrincipal rawPrincipal `json:"NotPrincipal"`
	// Condition is {operator: {key: value-or-list}}.
	Condition map[string]map[string]stringOrList `json:"Condition"`
}

// rawPrincipal decodes the Principal element, which has more shapes than
// anything else in a policy document:
//
//	"Principal": "*"
//	"Principal": {"AWS": "arn:aws:iam::000000000000:role/reader"}
//	"Principal": {"AWS": ["arn:...", "arn:..."]}
//	"Principal": {"Service": "lambda.amazonaws.com"}
//
// A `Service`, `Federated` or `CanonicalUser` principal is a definite NO
// against an IAM caller rather than something Nimbus cannot evaluate --
// `{"Service": "cloudfront.amazonaws.com"}` is ordinary in a bucket
// policy and must not refuse the whole request.
//
// An ACCOUNT-level principal (a bare account id, or `...:root`) is
// different: it delegates to the account and then requires the caller's
// own identity policy to allow as well, which is a combination rule
// Nimbus does not implement. That one is carried as unsupported, so the
// statement refuses and names itself rather than being silently read as
// a plain allow.
type rawPrincipal struct {
	present bool
	any     bool     // "*" or {"AWS": "*"}
	ids     []string // exact principal ARNs
	account []string // account ids and root ARNs, which Nimbus cannot evaluate
	other   bool     // Service/CanonicalUser: not this caller
	// federated are OIDC provider ARNs. Kept apart from `other` because
	// a web identity really can match one: a role's trust policy names
	// its provider here, and AssumeRoleWithWebIdentity asks whether the
	// token's issuer is that provider. Against an ordinary IAM caller it
	// is still a definite NO -- a federated principal is not a role or a
	// user -- which is why it is a third field rather than folded into
	// `ids`.
	federated []string
}

func (p *rawPrincipal) UnmarshalJSON(data []byte) error {
	p.present = true

	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		if one == "*" {
			p.any = true
			return nil
		}
		p.classify(one)
		return nil
	}

	var byType map[string]stringOrList
	if err := json.Unmarshal(data, &byType); err != nil {
		return fmt.Errorf("want \"*\" or a map of principal types, got %s", truncate(string(data), 40))
	}
	for kind, values := range byType {
		if strings.EqualFold(kind, "Federated") {
			p.federated = append(p.federated, values...)
			continue
		}
		if !strings.EqualFold(kind, "AWS") {
			p.other = true
			continue
		}
		for _, v := range values {
			if v == "*" {
				p.any = true
				continue
			}
			p.classify(v)
		}
	}
	return nil
}

// classify sorts one AWS principal value into an exact ARN or an
// account-level delegation.
func (p *rawPrincipal) classify(v string) {
	if strings.HasSuffix(v, ":root") || !strings.HasPrefix(v, "arn:") {
		p.account = append(p.account, v)
		return
	}
	p.ids = append(p.ids, v)
}

// matches reports whether this element speaks to a caller. ARNs are
// compared exactly: IAM does no wildcard matching inside a Principal, and
// `*` is the whole element rather than a pattern.
//
// A FEDERATED principal is deliberately not consulted here. This answers
// "is the caller in front of me this principal", and an IAM role or user
// is never a federated identity -- only AssumeRoleWithWebIdentity asks
// that question, through matchesFederated.
func (p *rawPrincipal) matches(id string) bool {
	if p.any {
		return true
	}
	for _, want := range p.ids {
		if want == id {
			return true
		}
	}
	return false
}

// matchesFederated reports whether this element names an OIDC provider.
// Exact, like every other principal comparison: IAM does no wildcard
// matching inside a Principal.
func (p *rawPrincipal) matchesFederated(providerARN string) bool {
	for _, want := range p.federated {
		if want == providerARN {
			return true
		}
	}
	return false
}

type rawDocument struct {
	Version   string `json:"Version"`
	Statement []rawStatement
}

// UnmarshalJSON exists because Statement may be a single object. Decoding
// it as a list then fails with "cannot unmarshal object into array", which
// is an error -- but a document that fails to compile refuses every
// request, so being sloppy here is expensive.
func (d *rawDocument) UnmarshalJSON(data []byte) error {
	var asList struct {
		Version   string         `json:"Version"`
		Statement []rawStatement `json:"Statement"`
	}
	if err := json.Unmarshal(data, &asList); err == nil {
		d.Version, d.Statement = asList.Version, asList.Statement
		return nil
	}
	var asOne struct {
		Version   string       `json:"Version"`
		Statement rawStatement `json:"Statement"`
	}
	if err := json.Unmarshal(data, &asOne); err != nil {
		return err
	}
	d.Version, d.Statement = asOne.Version, []rawStatement{asOne.Statement}
	return nil
}

// --- the compiled form ----------------------------------------------------

// iamStatement is one statement, ready to evaluate. It is used through a
// POINTER so that two compilations of the same documents can be told apart
// by identity -- which is how the cache's test proves it is caching.
type iamStatement struct {
	source string
	effect authz.Effect

	actions, notActions     []string
	resources, notResources []string

	// principals is set only for a resource policy. A nil one means this
	// is an identity policy, where the question does not arise.
	principals, notPrincipals *rawPrincipal

	conditions []iamCondition
	// unsupported is set when the statement carries something this
	// emulator cannot evaluate. It is NOT a compile error: the statement
	// still has to be matched on action and resource first, so that it
	// refuses only the requests it is actually about.
	unsupported error
}

func (st *iamStatement) Effect() authz.Effect { return st.effect }
func (st *iamStatement) Source() string       { return st.source }

func (st *iamStatement) Applies(req authz.Request) (bool, error) {
	if !st.matchesAction(req.Action) || !st.matchesResource(req.Resource) {
		return false, nil
	}
	// The principal is part of "does this statement speak to this
	// request", so it settles alongside the action and the resource --
	// before anything that can fail.
	if !st.matchesPrincipal(req.Principal.ID) {
		return false, nil
	}
	// Only now, with the statement known to be about this request, may
	// anything fail.
	if st.unsupported != nil {
		return false, st.unsupported
	}
	for _, c := range st.conditions {
		ok, err := c.satisfied(req.Context)
		if err != nil {
			return false, err
		}
		if !ok {
			// Conditions are ANDed, and an unsatisfied one simply means
			// the statement does not apply -- not that the request is
			// refused.
			return false, nil
		}
	}
	return true, nil
}

// matchesAction is case-insensitive, as IAM's is. The patterns were
// lower-cased at compile time so only the request's action is folded here.
func (st *iamStatement) matchesAction(action string) bool {
	action = strings.ToLower(action)
	if len(st.notActions) > 0 {
		for _, p := range st.notActions {
			if authz.Glob(p, action) {
				return false
			}
		}
		return true
	}
	for _, p := range st.actions {
		if authz.Glob(p, action) {
			return true
		}
	}
	return false
}

// matchesPrincipal is the resource-policy question: does this statement
// speak to this caller. An identity policy carries no Principal, and
// there the answer is always yes -- the document is already attached to
// the principal it governs.
func (st *iamStatement) matchesPrincipal(id string) bool {
	switch {
	case st.notPrincipals != nil:
		return !st.notPrincipals.matches(id)
	case st.principals != nil:
		return st.principals.matches(id)
	}
	return true
}

// matchesResource is case-SENSITIVE, because ARNs are. An S3 key differing
// only in case is a different object.
func (st *iamStatement) matchesResource(resource string) bool {
	if len(st.notResources) > 0 {
		for _, p := range st.notResources {
			if authz.Glob(p, resource) {
				return false
			}
		}
		return true
	}
	for _, p := range st.resources {
		if authz.Glob(p, resource) {
			return true
		}
	}
	return false
}

// --- conditions -----------------------------------------------------------

// iamCondition is one operator/key/values triple. Several keys under one
// operator, and several operators, are all ANDed; several values for one
// key are ORed. That is AWS's rule and it is easy to get backwards.
type iamCondition struct {
	op       string
	key      string
	values   []string
	ifExists bool // the operator carried the IfExists suffix
}

// supportedConditionKeys are the keys Nimbus can genuinely answer, because
// it knows the principal, the request's region and nothing else. Anything
// outside this list refuses rather than being ignored: a condition that
// silently passes is how a policy appears to protect something it does
// not.
// Every key here is one the SERVER can answer from the request in front of
// it -- who the caller is, where it came from, whether the connection is
// encrypted, and the clock. Nothing is invented: there are no resource
// tags, no organisation and no MFA in Nimbus's authorization path, so
// `aws:ResourceTag/...`, `aws:PrincipalOrgID` and `aws:MultiFactorAuthAge`
// still refuse by name.
//
// aws:SecureTransport deserves its own note. Nimbus serves AWS over plain
// HTTP by design -- path-style S3 needs no certificate and no DNS -- so the
// honest answer is "false", and the standard hardening policy (a Deny on
// `aws:SecureTransport: false`) really does deny everything here. That is
// what the policy says, and watching it happen teaches more than a refusal
// claiming the key is unsupported. It is pinned by a test so nobody
// "fixes" it to true.
var supportedConditionKeys = map[string]bool{
	"aws:principalarn":     true,
	"aws:username":         true,
	"aws:userid":           true,
	"aws:principaltype":    true,
	"aws:requestedregion":  true,
	"aws:principalaccount": true,
	"aws:sourceip":         true,
	"aws:securetransport":  true,
	"aws:currenttime":      true,
	"aws:epochtime":        true,
}

// supportedConditionOps are the operators implemented below.
//
// The `IfExists` SUFFIX is supported on all of them except Null, which is
// AWS's own rule and the reason it is handled at compile time rather than
// listed here. Its meaning is not a guess: an absent key satisfies the
// condition, full stop.
//
// The `ForAllValues:`/`ForAnyValue:` PREFIXES stay unsupported, and that is
// a decision rather than a gap. They quantify over a MULTIVALUED context
// key, and every key Nimbus can answer is a single string -- so treating
// them as aliases of the base operator would be right today and silently
// wrong the day a multivalued key is added. The policies that use them are
// about `aws:TagKeys`, which is not a supported key either, so nothing that
// worked is lost.
var supportedConditionOps = map[string]bool{
	"stringequals":              true,
	"stringnotequals":           true,
	"stringequalsignorecase":    true,
	"stringnotequalsignorecase": true,
	"stringlike":                true,
	"stringnotlike":             true,
	"arnequals":                 true,
	"arnlike":                   true,
	"arnnotequals":              true,
	"arnnotlike":                true,
	"numericequals":             true,
	"numericnotequals":          true,
	"numericlessthan":           true,
	"numericlessthanequals":     true,
	"numericgreaterthan":        true,
	"numericgreaterthanequals":  true,
	"dateequals":                true,
	"datenotequals":             true,
	"datelessthan":              true,
	"datelessthanequals":        true,
	"dategreaterthan":           true,
	"dategreaterthanequals":     true,
	"ipaddress":                 true,
	"notipaddress":              true,
	"bool":                      true,
	"null":                      true,
}

// negativeOps are the operators a MISSING key satisfies.
//
// AWS's rule: an absent key does not match a positive operator (so the
// statement does not apply) and does match a negative one, which is the
// only reading that makes `StringNotEquals` usable at all. This is an
// explicit set rather than a prefix test, because a prefix test gets
// `NotIpAddress` and `NumericNotEquals` wrong in opposite directions --
// one starts with "not" and the other merely contains it.
var negativeOps = map[string]bool{
	"stringnotequals":           true,
	"stringnotequalsignorecase": true,
	"stringnotlike":             true,
	"arnnotequals":              true,
	"arnnotlike":                true,
	"numericnotequals":          true,
	"datenotequals":             true,
	"notipaddress":              true,
}

// satisfied evaluates one condition against the request's context.
//
// A key the request does not carry is not the empty string. For the
// positive operators an absent key does not match (so the statement does
// not apply); for the negative ones it does, which is what real IAM does
// and the only reading that makes `StringNotEquals` usable. `aws:username`
// is genuinely absent for a role, so this is an ordinary case rather than
// an edge one.
func (c iamCondition) satisfied(ctx map[string]string) (bool, error) {
	have, present := lookupKey(ctx, c.key)

	// Null comes first, because it is the one operator whose SUBJECT is
	// whether the key exists -- so the "an absent key does not match" rule
	// below would answer the wrong question for it.
	if c.op == "null" {
		for _, v := range c.values {
			switch strings.ToLower(v) {
			case "true":
				if !present {
					return true, nil
				}
			case "false":
				if present {
					return true, nil
				}
			default:
				return false, fmt.Errorf("the Null condition on %s takes true or false, not %q",
					c.key, v)
			}
		}
		return false, nil
	}

	if !present {
		// IfExists has exactly one rule and it is not a guess: an absent
		// key satisfies the condition, whatever the operator.
		if c.ifExists {
			return true, nil
		}
		return negativeOps[c.op], nil
	}

	switch c.op {
	case "stringequals", "arnequals":
		return anyEqual(c.values, have), nil
	case "stringnotequals", "arnnotequals":
		return !anyEqual(c.values, have), nil
	case "stringequalsignorecase":
		return anyEqualFold(c.values, have), nil
	case "stringnotequalsignorecase":
		return !anyEqualFold(c.values, have), nil
	case "stringlike", "arnlike":
		return anyGlob(c.values, have), nil
	case "stringnotlike", "arnnotlike":
		return !anyGlob(c.values, have), nil
	case "bool":
		return anyEqual(c.values, strings.ToLower(have)), nil
	case "numericequals", "numericnotequals", "numericlessthan",
		"numericlessthanequals", "numericgreaterthan", "numericgreaterthanequals":
		return c.numericSatisfied(have)
	case "dateequals", "datenotequals", "datelessthan",
		"datelessthanequals", "dategreaterthan", "dategreaterthanequals":
		return c.dateSatisfied(have)
	case "ipaddress", "notipaddress":
		return c.addressSatisfied(have)
	}
	// Unreachable: an unsupported operator is caught at compile time and
	// recorded on the statement. Refusing rather than returning true is
	// the right failure anyway.
	return false, fmt.Errorf("the condition operator %q is not supported by Nimbus", c.op)
}

// numericSatisfied compares the context value against each policy value.
//
// A value neither side can read as a number REFUSES rather than failing to
// match. A non-match reads as "the statement does not apply", which is
// indistinguishable from a policy that was never meant to match -- the same
// trap an unparsed DynamoDB FilterExpression sets, one subsystem over.
//
// float64 is enough: these keys are ages, ports and epoch seconds. It is
// not DynamoDB, where a number is a decimal string precisely because it
// does not fit one.
func (c iamCondition) numericSatisfied(have string) (bool, error) {
	actual, err := strconv.ParseFloat(strings.TrimSpace(have), 64)
	if err != nil {
		return false, fmt.Errorf("%s is %q, which %s cannot read as a number",
			c.key, have, c.op)
	}
	for _, v := range c.values {
		want, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return false, fmt.Errorf("the %s condition on %s compares against %q, "+
				"which is not a number", c.op, c.key, v)
		}
		if numericHolds(c.op, actual, want) {
			return true, nil
		}
	}
	return false, nil
}

func numericHolds(op string, actual, want float64) bool {
	switch op {
	case "numericequals":
		return actual == want
	case "numericnotequals":
		return actual != want
	case "numericlessthan":
		return actual < want
	case "numericlessthanequals":
		return actual <= want
	case "numericgreaterthan":
		return actual > want
	case "numericgreaterthanequals":
		return actual >= want
	}
	return false
}

// dateSatisfied compares times rather than strings, so the two spellings
// AWS accepts for the same instant compare equal.
func (c iamCondition) dateSatisfied(have string) (bool, error) {
	actual, err := parseConditionTime(have)
	if err != nil {
		return false, fmt.Errorf("%s is %q, which %s cannot read as a time",
			c.key, have, c.op)
	}
	for _, v := range c.values {
		want, err := parseConditionTime(v)
		if err != nil {
			return false, fmt.Errorf("the %s condition on %s compares against %q, "+
				"which is not a time", c.op, c.key, v)
		}
		if dateHolds(c.op, actual, want) {
			return true, nil
		}
	}
	return false, nil
}

func dateHolds(op string, actual, want time.Time) bool {
	switch op {
	case "dateequals":
		return actual.Equal(want)
	case "datenotequals":
		return !actual.Equal(want)
	case "datelessthan":
		return actual.Before(want)
	case "datelessthanequals":
		return !actual.After(want)
	case "dategreaterthan":
		return actual.After(want)
	case "dategreaterthanequals":
		return !actual.Before(want)
	}
	return false
}

// parseConditionTime reads the spellings AWS accepts in a Date condition:
// full RFC 3339, a bare date, and epoch SECONDS -- which is why a policy
// may legitimately compare `aws:CurrentTime` against a number.
func parseConditionTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(secs, 0).UTC(), nil
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02T15:04Z07:00",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot read %q as a time", v)
}

// addressSatisfied matches the caller's address against each range.
//
// A bare address with no mask is a single host, which is how most policies
// are written, and a range nothing can parse REFUSES rather than failing to
// match -- a policy restricting access to a network nobody can read is not
// a policy that should quietly let everything through.
func (c iamCondition) addressSatisfied(have string) (bool, error) {
	// The context carries a bare address; a host:port would mean the
	// server put the wrong thing there, so no port is stripped here.
	actual := net.ParseIP(strings.TrimSpace(have))
	if actual == nil {
		return false, fmt.Errorf("%s is %q, which is not an address", c.key, have)
	}
	negative := c.op == "notipaddress"
	for _, v := range c.values {
		v = strings.TrimSpace(v)
		var contains bool
		if strings.Contains(v, "/") {
			_, network, err := net.ParseCIDR(v)
			if err != nil {
				return false, fmt.Errorf("the %s condition on %s names %q, "+
					"which is not a CIDR range", c.op, c.key, v)
			}
			contains = network.Contains(actual)
		} else {
			want := net.ParseIP(v)
			if want == nil {
				return false, fmt.Errorf("the %s condition on %s names %q, "+
					"which is not an address", c.op, c.key, v)
			}
			contains = want.Equal(actual)
		}
		// Values are ORed for a positive operator; for the negative one
		// the whole set has to miss, so a single hit settles it.
		if contains {
			return !negative, nil
		}
	}
	return negative, nil
}

func anyEqualFold(values []string, have string) bool {
	for _, v := range values {
		if strings.EqualFold(v, have) {
			return true
		}
	}
	return false
}

// lookupKey finds a context key case-insensitively.
//
// IAM condition KEYS are case-insensitive (`aws:principalarn` and
// `aws:PrincipalArn` are the same key), while their VALUES are not. The
// compiled condition holds the key folded; a context is built with the
// canonical spelling, because that is what anyone reading the activity
// feed or a test expects to see. A context has three or four entries, so
// scanning it costs nothing.
func lookupKey(ctx map[string]string, key string) (string, bool) {
	if v, ok := ctx[key]; ok {
		return v, true
	}
	for k, v := range ctx {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

func anyEqual(values []string, have string) bool {
	for _, v := range values {
		if v == have {
			return true
		}
	}
	return false
}

func anyGlob(patterns []string, have string) bool {
	for _, p := range patterns {
		if authz.Glob(p, have) {
			return true
		}
	}
	return false
}

// --- compiling ------------------------------------------------------------

// compileDocument turns one IAM document into rules.
//
// An error here means the document is unusable, and the caller must turn it
// into a rule that REFUSES rather than dropping it: a policy nobody can
// read is not a policy that grants nothing, and the difference is the whole
// point of this milestone.
func compileDocument(source, document string, kind policyKind, attached string) ([]authz.Rule, error) {
	text := decodePolicyDocument(document)
	var doc rawDocument
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("%s is not a readable policy document: %w", source, err)
	}
	if len(doc.Statement) == 0 {
		return nil, fmt.Errorf("%s has no statements", source)
	}

	var rules []authz.Rule
	for i, raw := range doc.Statement {
		name := raw.Sid
		if name == "" {
			name = fmt.Sprintf("#%d", i+1)
		}
		st, err := compileStatement(source+" statement "+name, raw, kind, attached)
		if err != nil {
			return nil, err
		}
		rules = append(rules, st)
	}
	return rules, nil
}

func compileStatement(source string, raw rawStatement, kind policyKind, attached string) (*iamStatement, error) {
	st := &iamStatement{source: source}

	switch strings.ToLower(raw.Effect) {
	case "allow":
		st.effect = authz.Allow
	case "deny":
		st.effect = authz.Deny
	default:
		return nil, fmt.Errorf("%s has Effect %q, which must be Allow or Deny", source, raw.Effect)
	}

	// Exactly one of Action/NotAction, and exactly one of
	// Resource/NotResource. AWS rejects anything else, and guessing what a
	// document meant is how a policy ends up granting more than it says.
	switch {
	case len(raw.Action) > 0 && len(raw.NotAction) > 0:
		return nil, fmt.Errorf("%s has both Action and NotAction", source)
	case len(raw.Action) > 0:
		st.actions = lowerAll(raw.Action)
	case len(raw.NotAction) > 0:
		st.notActions = lowerAll(raw.NotAction)
	default:
		return nil, fmt.Errorf("%s has neither Action nor NotAction", source)
	}

	switch {
	case len(raw.Resource) > 0 && len(raw.NotResource) > 0:
		return nil, fmt.Errorf("%s has both Resource and NotResource", source)
	case len(raw.Resource) > 0:
		st.resources = raw.Resource
	case len(raw.NotResource) > 0:
		st.notResources = raw.NotResource
	case kind == resourcePolicy && attached != "":
		// A resource policy's Resource is optional: left out, it means
		// the thing the policy hangs off. S3 requires it in practice,
		// but defaulting beats refusing a document AWS would accept.
		st.resources = []string{attached, attached + "/*"}
	default:
		return nil, fmt.Errorf("%s has no Resource; identity-based policies require one", source)
	}

	if err := compilePrincipal(st, source, raw, kind); err != nil {
		return nil, err
	}

	for op, keys := range raw.Condition {
		lowerOp := strings.ToLower(op)
		// The IfExists suffix is stripped here so every operator below
		// has one implementation. AWS allows it on any operator EXCEPT
		// Null -- which makes sense, since Null is already the question
		// "does this key exist" and NullIfExists would be asking it
		// twice. Spelling it is a mistake worth naming rather than
		// quietly treating as Null.
		ifExists := strings.HasSuffix(lowerOp, "ifexists")
		if ifExists {
			lowerOp = strings.TrimSuffix(lowerOp, "ifexists")
			if lowerOp == "null" {
				st.unsupported = fmt.Errorf(
					"the condition operator %q does not exist: IfExists cannot be added to Null, "+
						"which already tests whether the key is present", op)
				continue
			}
		}
		for key, values := range keys {
			lowerKey := strings.ToLower(key)
			switch {
			case !supportedConditionOps[lowerOp]:
				st.unsupported = fmt.Errorf(
					"the condition operator %q is not supported by Nimbus (supported: %s)",
					op, strings.Join(sortedKeys(supportedConditionOps), ", "))
			case isWebIdentityConditionKey(lowerKey):
				st.conditions = append(st.conditions, iamCondition{
					op: lowerOp, key: lowerKey, values: lowerIfBool(lowerOp, values),
					ifExists: ifExists,
				})
			case !supportedConditionKeys[lowerKey]:
				st.unsupported = fmt.Errorf(
					"the condition key %q is not supported by Nimbus (supported: %s)",
					key, strings.Join(sortedKeys(supportedConditionKeys), ", "))
			default:
				st.conditions = append(st.conditions, iamCondition{
					op: lowerOp, key: lowerKey, values: lowerIfBool(lowerOp, values),
					ifExists: ifExists,
				})
			}
		}
	}
	return st, nil
}

// compilePrincipal settles the Principal element, which is the one part
// of a statement whose rules depend on what the document is attached to.
//
// Both directions are errors rather than something to ignore. An identity
// policy carrying a Principal is rejected by AWS itself, and ignoring the
// element would read as "this applies to everyone" -- which is not what
// the author meant and is wrong in the generous direction. A resource
// policy with no Principal is equally invalid, and the tempting default
// is the dangerous one.
func compilePrincipal(st *iamStatement, source string, raw rawStatement, kind policyKind) error {
	hasPrincipal := raw.Principal.present || raw.NotPrincipal.present

	if kind == identityPolicy {
		if hasPrincipal {
			return fmt.Errorf("%s has a Principal, which only a resource policy may carry; "+
				"an identity policy already applies to the principal it is attached to", source)
		}
		return nil
	}

	switch {
	case raw.Principal.present && raw.NotPrincipal.present:
		return fmt.Errorf("%s has both Principal and NotPrincipal", source)
	case !hasPrincipal:
		return fmt.Errorf("%s has no Principal; a resource policy must say who it is about, "+
			"and Nimbus will not read a missing one as everybody", source)
	case raw.Principal.present:
		st.principals = &raw.Principal
	default:
		st.notPrincipals = &raw.NotPrincipal
	}

	// Account-level delegation really means "this account may, IF the
	// caller's own identity policy also allows" -- a combination rule
	// Nimbus does not implement. Unsupported rather than approximated,
	// so the statement refuses the requests it is about and names
	// itself, instead of granting on half a rule.
	if accounts := accountPrincipals(raw); len(accounts) > 0 {
		st.unsupported = fmt.Errorf(
			"the principal %s delegates to a whole account, which also requires the caller's own "+
				"identity policy to allow the action -- a rule Nimbus does not implement",
			strings.Join(accounts, ", "))
	}
	return nil
}

func accountPrincipals(raw rawStatement) []string {
	if raw.Principal.present {
		return raw.Principal.account
	}
	return raw.NotPrincipal.account
}

func lowerIfBool(op string, values []string) []string {
	if op != "bool" {
		return values
	}
	return lowerAll(values)
}

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// A stable order so an error message does not change between runs.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// decodePolicyDocument unescapes a document that arrived URL-encoded.
//
// IAM RETURNS documents URL-encoded (see encodeDocument), so a client that
// reads one and hands it back sends `%7B%22Version%22...`. Unescaping
// unconditionally would corrupt a raw document containing a legitimate `%`,
// so this only acts when the text does not already look like JSON.
func decodePolicyDocument(document string) string {
	trimmed := strings.TrimSpace(document)
	if strings.HasPrefix(trimmed, "{") {
		return trimmed
	}
	if decoded, err := url.QueryUnescape(trimmed); err == nil {
		return strings.TrimSpace(decoded)
	}
	return trimmed
}

// brokenRule stands in for a document that could not be compiled. It
// applies to everything and always refuses, carrying the reason -- so a
// policy Nimbus cannot read refuses loudly instead of quietly granting
// nothing, which looks identical to a policy that simply does not cover
// the action.
type brokenRule struct {
	source string
	err    error
}

func (b *brokenRule) Applies(authz.Request) (bool, error) { return false, b.err }
func (b *brokenRule) Effect() authz.Effect                { return authz.Deny }
func (b *brokenRule) Source() string                      { return b.source }

// --- the cache ------------------------------------------------------------

// policyCache compiles a document set once.
//
// Keyed by a hash of the documents themselves rather than by the
// principal, which means there is no invalidation to get wrong: editing a
// policy produces different text, which is a different key. Reading the
// documents out of the in-memory store on each request is cheap; parsing
// JSON and compiling globs is what this avoids.
type policyCache struct {
	mu       sync.Mutex
	compiled map[string][]authz.Rule
}

func newPolicyCache() *policyCache {
	return &policyCache{compiled: map[string][]authz.Rule{}}
}

func (c *policyCache) rules(docs []policyDocument) []authz.Rule {
	key := documentSetKey(docs)

	c.mu.Lock()
	if rules, ok := c.compiled[key]; ok {
		c.mu.Unlock()
		return rules
	}
	c.mu.Unlock()

	var rules []authz.Rule
	for _, d := range docs {
		if d.Err != nil {
			rules = append(rules, &brokenRule{source: d.Source, err: d.Err})
			continue
		}
		compiled, err := compileDocument(d.Source, d.Document, d.Kind, d.Attached)
		if err != nil {
			rules = append(rules, &brokenRule{source: d.Source, err: err})
			continue
		}
		rules = append(rules, compiled...)
	}

	c.mu.Lock()
	// Another goroutine may have won the race; either result is correct,
	// and keeping the one already published means two callers holding
	// rules from the same key always hold the same ones.
	if existing, ok := c.compiled[key]; ok {
		rules = existing
	} else {
		c.compiled[key] = rules
	}
	c.mu.Unlock()
	return rules
}

func documentSetKey(docs []policyDocument) string {
	h := sha256.New()
	for _, d := range docs {
		// Lengths as well as content, so that two different splits of the
		// same bytes cannot hash alike.
		// Kind and Attached are part of the key because they change what
		// the same text COMPILES TO: a document with no Principal is a
		// usable identity policy and a refusing resource policy.
		fmt.Fprintf(h, "%d:%s|%d:%s|%v|%d|%d:%s|",
			len(d.Source), d.Source, len(d.Document), d.Document, d.Err, d.Kind, len(d.Attached), d.Attached)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// isWebIdentityConditionKey recognises the keys a web-identity trust
// policy is written with: `<issuer>:sub` and `<issuer>:aud`.
//
// They cannot be in supportedConditionKeys because the key NAME contains
// the issuer, which differs per cluster -- `oidc.eks.us-east-1.amazonaws
// .com/id/ABC:sub`. The project's rule still holds: a key is supported
// when the server can answer it from the request in front of it, and
// during AssumeRoleWithWebIdentity Nimbus has the verified token, so it
// can answer both exactly.
//
// Narrow on purpose. Only `:sub` and `:aud`, and only when something
// issuer-shaped precedes them, so an ordinary mistyped key is still
// refused by name rather than silently accepted and never matched.
func isWebIdentityConditionKey(lowerKey string) bool {
	issuer, claim, found := strings.Cut(lowerKey, ":")
	for found && strings.Contains(claim, ":") {
		// An issuer contains no colon of its own once the scheme is
		// gone, but splitting from the left is still the wrong end: take
		// the LAST colon so a key keeps its whole issuer.
		next, rest, ok := strings.Cut(claim, ":")
		issuer, claim, found = issuer+":"+next, rest, ok
	}
	if !found || (claim != "sub" && claim != "aud") {
		return false
	}
	// Issuer-shaped: a host, so at least one dot or a path.
	return strings.Contains(issuer, ".") || strings.Contains(issuer, "/")
}
