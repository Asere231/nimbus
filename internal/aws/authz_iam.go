package aws

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"

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

// policyDocument is one document and where it came from, the `Source`
// being what a decision quotes back so somebody can find the statement in
// their own configuration.
type policyDocument struct {
	Source   string
	Document string
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
	// Condition is {operator: {key: value-or-list}}.
	Condition map[string]map[string]stringOrList `json:"Condition"`
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
	op     string
	key    string
	values []string
}

// supportedConditionKeys are the keys Nimbus can genuinely answer, because
// it knows the principal, the request's region and nothing else. Anything
// outside this list refuses rather than being ignored: a condition that
// silently passes is how a policy appears to protect something it does
// not.
var supportedConditionKeys = map[string]bool{
	"aws:principalarn":     true,
	"aws:username":         true,
	"aws:requestedregion":  true,
	"aws:principalaccount": true,
}

// supportedConditionOps are the operators implemented below. The IfExists
// and ForAllValues/ForAnyValue variants are deliberately absent -- each has
// its own rule about a missing key, and guessing one wrong is a silent
// change of meaning.
var supportedConditionOps = map[string]bool{
	"stringequals":    true,
	"stringnotequals": true,
	"stringlike":      true,
	"stringnotlike":   true,
	"arnequals":       true,
	"arnlike":         true,
	"arnnotequals":    true,
	"bool":            true,
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
	negative := strings.HasPrefix(c.op, "stringnot") || strings.HasPrefix(c.op, "arnnot")

	if !present {
		return negative, nil
	}

	switch c.op {
	case "stringequals", "arnequals":
		return anyEqual(c.values, have), nil
	case "stringnotequals", "arnnotequals":
		return !anyEqual(c.values, have), nil
	case "stringlike", "arnlike":
		return anyGlob(c.values, have), nil
	case "stringnotlike":
		return !anyGlob(c.values, have), nil
	case "bool":
		return anyEqual(c.values, strings.ToLower(have)), nil
	}
	// Unreachable: an unsupported operator is caught at compile time and
	// recorded on the statement. Refusing rather than returning true is
	// the right failure anyway.
	return false, fmt.Errorf("the condition operator %q is not supported by Nimbus", c.op)
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
func compileDocument(source, document string) ([]authz.Rule, error) {
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
		st, err := compileStatement(source+" statement "+name, raw)
		if err != nil {
			return nil, err
		}
		rules = append(rules, st)
	}
	return rules, nil
}

func compileStatement(source string, raw rawStatement) (*iamStatement, error) {
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
	default:
		return nil, fmt.Errorf("%s has no Resource; identity-based policies require one", source)
	}

	for op, keys := range raw.Condition {
		lowerOp := strings.ToLower(op)
		for key, values := range keys {
			lowerKey := strings.ToLower(key)
			switch {
			case !supportedConditionOps[lowerOp]:
				st.unsupported = fmt.Errorf(
					"the condition operator %q is not supported by Nimbus (supported: %s)",
					op, strings.Join(sortedKeys(supportedConditionOps), ", "))
			case !supportedConditionKeys[lowerKey]:
				st.unsupported = fmt.Errorf(
					"the condition key %q is not supported by Nimbus (supported: %s)",
					key, strings.Join(sortedKeys(supportedConditionKeys), ", "))
			default:
				st.conditions = append(st.conditions, iamCondition{
					op: lowerOp, key: lowerKey, values: lowerIfBool(lowerOp, values),
				})
			}
		}
	}
	return st, nil
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
		compiled, err := compileDocument(d.Source, d.Document)
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
		fmt.Fprintf(h, "%d:%s|%d:%s|%v|", len(d.Source), d.Source, len(d.Document), d.Document, d.Err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
