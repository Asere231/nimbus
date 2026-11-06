package aws

import (
	"encoding/json"
	"math/big"
	"strings"
)

// DynamoDB's expression language, as much of it as the SDKs actually send.
//
// Expressions are the whole interface to the data: a Query says what it
// wants with a KeyConditionExpression, a Scan narrows with a
// FilterExpression, and an UpdateItem changes an item with an
// UpdateExpression. There is no way to serve a client without reading them.
//
// What is implemented is the grammar without parentheses: comparisons,
// BETWEEN, IN, the attribute_* and begins_with/contains functions, joined
// by AND and OR, and SET/REMOVE/ADD/DELETE for updates. Anything outside
// that -- nested paths, parentheses, size() -- is REFUSED with a
// ValidationException that names the construct.
//
// Refusing is the important part. The tempting failure mode for an
// expression evaluator is to treat what it cannot parse as "matches
// everything", which turns a filter nobody implemented into a silent wrong
// answer: a Scan that returns rows it should have excluded, discovered much
// later and nowhere near here. A loud error is a worse demo and a better
// tool.

// --- attribute values -----------------------------------------------------

// avType is the single key an attribute value has: S, N, B, BOOL, NULL, M,
// L, SS, NS, BS.
func avType(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	for k := range m {
		return k
	}
	return ""
}

// avEqual compares two attribute values as documents, which is what
// DynamoDB's equality means for every type including maps and lists.
func avEqual(a, b any) bool {
	ja, erra := json.Marshal(a)
	jb, errb := json.Marshal(b)
	if erra != nil || errb != nil {
		return false
	}
	if string(ja) == string(jb) {
		return true
	}
	// Numbers are decimal STRINGS on the wire, so "5" and "5.0" are the
	// same number written two ways and must compare equal.
	if avType(a) == "N" && avType(b) == "N" {
		na, oka := avNumber(a)
		nb, okb := avNumber(b)
		return oka && okb && na.Cmp(nb) == 0
	}
	return false
}

// avNumber reads a number without going through a float. DynamoDB numbers
// carry up to 38 significant digits, which a float64 cannot hold, so
// parsing one into a float and printing it back is how "1e+06" appears
// where "1000000" was stored.
func avNumber(v any) (*big.Float, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	s, ok := m["N"].(string)
	if !ok {
		return nil, false
	}
	f, _, err := big.ParseFloat(s, 10, 200, big.ToNearestEven)
	if err != nil {
		return nil, false
	}
	return f, true
}

// avCompare orders two values of the same type: numbers numerically,
// strings and binary bytewise. Returns ok=false when they cannot be
// ordered, which DynamoDB treats as "the condition does not hold" rather
// than as an error.
func avCompare(a, b any) (int, bool) {
	ta, tb := avType(a), avType(b)
	if ta != tb {
		return 0, false
	}
	switch ta {
	case "N":
		na, oka := avNumber(a)
		nb, okb := avNumber(b)
		if !oka || !okb {
			return 0, false
		}
		return na.Cmp(nb), true
	case "S", "B":
		sa := str(a.(map[string]any)[ta])
		sb := str(b.(map[string]any)[ta])
		return strings.Compare(sa, sb), true
	}
	return 0, false
}

// --- tokenising -----------------------------------------------------------

// tokenise splits an expression into words, with parentheses and commas as
// tokens of their own so a function call reads as a sequence.
func tokenise(expr string) []string {
	var out []string
	cur := strings.Builder{}
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range expr {
		switch r {
		case ' ', '\t', '\n':
			flush()
		case '(', ')', ',':
			flush()
			out = append(out, string(r))
		case '=', '<', '>':
			// <= and >= and <> are single operators; = is its own.
			if cur.Len() > 0 && isOpChar(cur.String()) {
				cur.WriteRune(r)
				flush()
				continue
			}
			flush()
			cur.WriteRune(r)
			if r == '=' {
				flush()
			}
		default:
			if cur.Len() > 0 && isOpChar(cur.String()) {
				flush()
			}
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

func isOpChar(s string) bool {
	return s == "<" || s == ">" || s == "="
}

// --- conditions -----------------------------------------------------------

// condExpr is a parsed condition: a flat list of terms joined by AND or OR,
// evaluated left to right. DynamoDB allows parentheses; nothing the SDKs
// generate for a Terraform apply uses them, and pretending to support them
// would be worse than saying no.
type condExpr struct {
	terms []condTerm
	joins []string // len(terms)-1 of "AND"/"OR"
}

type condTerm struct {
	fn    string // "", "begins_with", "contains", "attribute_exists", ...
	attr  string
	op    string // "=", "<>", "<", "<=", ">", ">=", "BETWEEN", "IN"
	args  []any  // resolved attribute values
	names map[string]string
}

// parseCondition reads an expression, resolving #names and :values as it
// goes so evaluation has nothing left to look up.
func parseCondition(expr string, names map[string]string, values map[string]any) (*condExpr, *apiError) {
	if strings.TrimSpace(expr) == "" {
		return nil, nil
	}
	toks := tokenise(expr)
	out := &condExpr{}
	i := 0
	for i < len(toks) {
		term, next, aerr := parseTerm(toks, i, names, values)
		if aerr != nil {
			return nil, aerr
		}
		out.terms = append(out.terms, *term)
		i = next
		if i >= len(toks) {
			break
		}
		join := strings.ToUpper(toks[i])
		if join != "AND" && join != "OR" {
			return nil, dynamoBadRequest("expression %q: expected AND or OR near %q", expr, toks[i])
		}
		out.joins = append(out.joins, join)
		i++
	}
	return out, nil
}

func parseTerm(toks []string, i int, names map[string]string, values map[string]any) (*condTerm, int, *apiError) {
	if i >= len(toks) {
		return nil, i, dynamoBadRequest("expression ends where a condition was expected")
	}
	if toks[i] == "(" {
		return nil, i, dynamoBadRequest(
			"Nimbus does not support parentheses in a condition expression yet; " +
				"split the condition or combine the results client-side")
	}
	head := toks[i]

	// A function call: name ( arg [, arg] ).
	if i+1 < len(toks) && toks[i+1] == "(" {
		fn := strings.ToLower(head)
		args, next, aerr := parseCallArgs(toks, i+1)
		if aerr != nil {
			return nil, i, aerr
		}
		switch fn {
		case "attribute_exists", "attribute_not_exists":
			if len(args) != 1 {
				return nil, i, dynamoBadRequest("%s takes one argument", fn)
			}
			attr, aerr := resolveName(args[0], names)
			if aerr != nil {
				return nil, i, aerr
			}
			return &condTerm{fn: fn, attr: attr}, next, nil
		case "begins_with", "contains", "attribute_type":
			if len(args) != 2 {
				return nil, i, dynamoBadRequest("%s takes two arguments", fn)
			}
			attr, aerr := resolveName(args[0], names)
			if aerr != nil {
				return nil, i, aerr
			}
			val, aerr := resolveValue(args[1], values)
			if aerr != nil {
				return nil, i, aerr
			}
			return &condTerm{fn: fn, attr: attr, args: []any{val}}, next, nil
		default:
			return nil, i, dynamoBadRequest(
				"Nimbus does not support the function %q in an expression yet", head)
		}
	}

	attr, aerr := resolveName(head, names)
	if aerr != nil {
		return nil, i, aerr
	}
	i++
	if i >= len(toks) {
		return nil, i, dynamoBadRequest("condition on %q has no operator", attr)
	}
	op := strings.ToUpper(toks[i])
	i++

	switch op {
	case "=", "<>", "<", "<=", ">", ">=":
		if i >= len(toks) {
			return nil, i, dynamoBadRequest("condition on %q has no operand", attr)
		}
		val, aerr := resolveValue(toks[i], values)
		if aerr != nil {
			return nil, i, aerr
		}
		return &condTerm{attr: attr, op: op, args: []any{val}}, i + 1, nil

	case "BETWEEN":
		// lo AND hi -- which is why conditions cannot be split on " AND ".
		if i+2 >= len(toks) || strings.ToUpper(toks[i+1]) != "AND" {
			return nil, i, dynamoBadRequest("BETWEEN on %q needs `:lo AND :hi`", attr)
		}
		lo, aerr := resolveValue(toks[i], values)
		if aerr != nil {
			return nil, i, aerr
		}
		hi, aerr := resolveValue(toks[i+2], values)
		if aerr != nil {
			return nil, i, aerr
		}
		return &condTerm{attr: attr, op: "BETWEEN", args: []any{lo, hi}}, i + 3, nil

	case "IN":
		args, next, aerr := parseCallArgs(toks, i)
		if aerr != nil {
			return nil, i, aerr
		}
		var vals []any
		for _, a := range args {
			v, aerr := resolveValue(a, values)
			if aerr != nil {
				return nil, i, aerr
			}
			vals = append(vals, v)
		}
		return &condTerm{attr: attr, op: "IN", args: vals}, next, nil
	}
	return nil, i, dynamoBadRequest("unsupported operator %q in a condition on %q", op, attr)
}

// parseCallArgs reads "( a , b )" starting at the open parenthesis and
// returns the raw argument tokens.
func parseCallArgs(toks []string, open int) ([]string, int, *apiError) {
	if open >= len(toks) || toks[open] != "(" {
		return nil, open, dynamoBadRequest("expected ( in an expression")
	}
	var args []string
	for i := open + 1; i < len(toks); i++ {
		switch toks[i] {
		case ")":
			return args, i + 1, nil
		case ",":
		default:
			args = append(args, toks[i])
		}
	}
	return nil, open, dynamoBadRequest("unclosed ( in an expression")
}

// resolveName turns a token into an attribute name, substituting #aliases.
// A nested path is refused rather than silently read as a flat name.
func resolveName(tok string, names map[string]string) (string, *apiError) {
	name := tok
	if strings.HasPrefix(tok, "#") {
		got, ok := names[tok]
		if !ok {
			return "", dynamoBadRequest(
				"ExpressionAttributeNames has no entry for %q", tok)
		}
		name = got
	}
	if strings.ContainsAny(name, ".[]") {
		return "", dynamoBadRequest(
			"Nimbus does not support the nested attribute path %q yet", name)
	}
	return name, nil
}

func resolveValue(tok string, values map[string]any) (any, *apiError) {
	if !strings.HasPrefix(tok, ":") {
		return nil, dynamoBadRequest(
			"%q is not a value placeholder; conditions compare an attribute with a :value", tok)
	}
	v, ok := values[tok]
	if !ok {
		return nil, dynamoBadRequest("ExpressionAttributeValues has no entry for %q", tok)
	}
	return v, nil
}

// --- evaluating -----------------------------------------------------------

func (e *condExpr) matches(item map[string]any) bool {
	if e == nil || len(e.terms) == 0 {
		return true
	}
	result := e.terms[0].matches(item)
	for i, join := range e.joins {
		next := e.terms[i+1].matches(item)
		if join == "AND" {
			result = result && next
		} else {
			result = result || next
		}
	}
	return result
}

func (t condTerm) matches(item map[string]any) bool {
	have, present := item[t.attr]

	switch t.fn {
	case "attribute_exists":
		return present
	case "attribute_not_exists":
		return !present
	case "attribute_type":
		return present && avType(have) == str(t.args[0].(map[string]any)["S"])
	case "begins_with":
		if !present {
			return false
		}
		return strings.HasPrefix(stringOf(have), stringOf(t.args[0]))
	case "contains":
		if !present {
			return false
		}
		// A string contains a substring; a set or list contains a member.
		switch avType(have) {
		case "S":
			return strings.Contains(stringOf(have), stringOf(t.args[0]))
		case "SS", "NS", "BS":
			for _, member := range strList(have.(map[string]any)[avType(have)]) {
				if member == stringOf(t.args[0]) {
					return true
				}
			}
			return false
		case "L":
			for _, member := range have.(map[string]any)["L"].([]any) {
				if avEqual(member, t.args[0]) {
					return true
				}
			}
			return false
		}
		return false
	}

	if !present {
		return false
	}
	switch t.op {
	case "=":
		return avEqual(have, t.args[0])
	case "<>":
		return !avEqual(have, t.args[0])
	case "IN":
		for _, want := range t.args {
			if avEqual(have, want) {
				return true
			}
		}
		return false
	case "BETWEEN":
		lo, ok1 := avCompare(have, t.args[0])
		hi, ok2 := avCompare(have, t.args[1])
		return ok1 && ok2 && lo >= 0 && hi <= 0
	}
	cmp, ok := avCompare(have, t.args[0])
	if !ok {
		return false
	}
	switch t.op {
	case "<":
		return cmp < 0
	case "<=":
		return cmp <= 0
	case ">":
		return cmp > 0
	case ">=":
		return cmp >= 0
	}
	return false
}

// stringOf is the scalar inside an attribute value, for the functions that
// work on text.
func stringOf(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"S", "N", "B"} {
		if s, ok := m[key].(string); ok {
			return s
		}
	}
	return ""
}
