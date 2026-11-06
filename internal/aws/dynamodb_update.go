package aws

import (
	"math/big"
	"strings"
)

// UpdateExpression, and the projection that goes with a read.
//
// An update is a list of clauses -- SET, REMOVE, ADD, DELETE -- in any
// order, each with comma-separated parts. The parsing is small but has two
// traps worth naming: a verb has to be matched on a WORD boundary, or the
// "ADD" inside an attribute called ADDRESS is read as a clause; and the
// parts cannot be split on every comma, because if_not_exists(a, :v)
// contains one.

// --- update expressions ---------------------------------------------------

// applyUpdate changes an item in place, and returns what it did for the
// benefit of the error messages.
func applyUpdate(item map[string]any, expr string, names map[string]string, values map[string]any) *apiError {
	clauses, aerr := splitUpdateClauses(expr)
	if aerr != nil {
		return aerr
	}
	for verb, body := range clauses {
		for _, part := range splitTopLevel(body, ',') {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if aerr := applyUpdatePart(item, verb, part, names, values); aerr != nil {
				return aerr
			}
		}
	}
	return nil
}

// splitUpdateClauses breaks "SET a = :v REMOVE b" into its verbs. The verbs
// may appear in any order and at most once each.
func splitUpdateClauses(expr string) (map[string]string, *apiError) {
	verbs := []string{"SET", "REMOVE", "ADD", "DELETE"}
	type at struct {
		verb  string
		start int
		end   int
	}
	var found []at
	upper := strings.ToUpper(expr)
	for _, v := range verbs {
		idx := indexWord(upper, v)
		if idx >= 0 {
			found = append(found, at{verb: v, start: idx + len(v)})
		}
	}
	if len(found) == 0 {
		return nil, dynamoBadRequest(
			"UpdateExpression %q names none of SET, REMOVE, ADD or DELETE", expr)
	}
	// Each clause runs until the next verb starts.
	for i := range found {
		found[i].end = len(expr)
		for j := range found {
			s := found[j].start - len(found[j].verb)
			if s > found[i].start && s < found[i].end {
				found[i].end = s
			}
		}
	}
	out := map[string]string{}
	for _, f := range found {
		out[f.verb] = expr[f.start:f.end]
	}
	return out, nil
}

// indexWord finds a word on a word boundary, so the "ADD" inside an
// attribute named "ADDRESS" is not mistaken for the verb.
func indexWord(s, word string) int {
	from := 0
	for {
		i := strings.Index(s[from:], word)
		if i < 0 {
			return -1
		}
		i += from
		beforeOK := i == 0 || !isWordByte(s[i-1])
		after := i + len(word)
		afterOK := after >= len(s) || !isWordByte(s[after])
		if beforeOK && afterOK {
			return i
		}
		from = i + 1
	}
}

func isWordByte(b byte) bool {
	return b == '_' || b == '#' || b == ':' ||
		(b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// splitTopLevel splits on a separator outside any parentheses, so the comma
// inside if_not_exists(a, :v) does not end the assignment.
func splitTopLevel(s string, sep rune) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case sep:
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + len(string(sep))
			}
		}
	}
	return append(out, s[start:])
}

func applyUpdatePart(item map[string]any, verb, part string, names map[string]string, values map[string]any) *apiError {
	toks := tokenise(part)
	if len(toks) == 0 {
		return nil
	}
	attr, aerr := resolveName(toks[0], names)
	if aerr != nil {
		return aerr
	}

	switch verb {
	case "REMOVE":
		delete(item, attr)
		return nil

	case "DELETE":
		// Removes members from a set. With no value it is not legal, and
		// DynamoDB says so rather than removing the attribute.
		if len(toks) < 2 {
			return dynamoBadRequest("DELETE on %q needs a value", attr)
		}
		val, aerr := resolveValue(toks[1], values)
		if aerr != nil {
			return aerr
		}
		return deleteFromSet(item, attr, val)

	case "ADD":
		if len(toks) < 2 {
			return dynamoBadRequest("ADD on %q needs a value", attr)
		}
		val, aerr := resolveValue(toks[1], values)
		if aerr != nil {
			return aerr
		}
		return addToAttribute(item, attr, val)

	case "SET":
		if len(toks) < 3 || toks[1] != "=" {
			return dynamoBadRequest("SET on %q needs `= value`", attr)
		}
		val, aerr := evalSetValue(item, toks[2:], names, values)
		if aerr != nil {
			return aerr
		}
		item[attr] = val
		return nil
	}
	return dynamoBadRequest("unsupported update verb %q", verb)
}

// evalSetValue handles the right-hand sides the SDKs generate: a value, an
// attribute, arithmetic on either, if_not_exists and list_append.
func evalSetValue(item map[string]any, toks []string, names map[string]string, values map[string]any) (any, *apiError) {
	operand := func(tok string) (any, *apiError) {
		if strings.HasPrefix(tok, ":") {
			return resolveValue(tok, values)
		}
		attr, aerr := resolveName(tok, names)
		if aerr != nil {
			return nil, aerr
		}
		return item[attr], nil
	}

	// if_not_exists(attr, :v) / list_append(a, b)
	if len(toks) >= 2 && toks[1] == "(" {
		fn := strings.ToLower(toks[0])
		args, _, aerr := parseCallArgs(toks, 1)
		if aerr != nil {
			return nil, aerr
		}
		if len(args) != 2 {
			return nil, dynamoBadRequest("%s takes two arguments", fn)
		}
		switch fn {
		case "if_not_exists":
			attr, aerr := resolveName(args[0], names)
			if aerr != nil {
				return nil, aerr
			}
			if have, ok := item[attr]; ok {
				return have, nil
			}
			return resolveValue(args[1], values)
		case "list_append":
			a, aerr := operand(args[0])
			if aerr != nil {
				return nil, aerr
			}
			b, aerr := operand(args[1])
			if aerr != nil {
				return nil, aerr
			}
			return appendLists(a, b)
		}
		return nil, dynamoBadRequest("Nimbus does not support %q in an update expression yet", toks[0])
	}

	first, aerr := operand(toks[0])
	if aerr != nil {
		return nil, aerr
	}
	if len(toks) == 1 {
		return first, nil
	}
	if len(toks) != 3 || (toks[1] != "+" && toks[1] != "-") {
		return nil, dynamoBadRequest("unsupported expression %q on the right of a SET", strings.Join(toks, " "))
	}
	second, aerr := operand(toks[2])
	if aerr != nil {
		return nil, aerr
	}
	return arithmetic(first, second, toks[1])
}

func arithmetic(a, b any, op string) (any, *apiError) {
	na, oka := avNumber(a)
	nb, okb := avNumber(b)
	if !oka || !okb {
		return nil, dynamoBadRequest("arithmetic in an update expression needs two numbers")
	}
	sum := new(big.Float).SetPrec(200)
	if op == "+" {
		sum.Add(na, nb)
	} else {
		sum.Sub(na, nb)
	}
	return map[string]any{"N": formatNumber(sum)}, nil
}

// formatNumber writes a number the way DynamoDB does: plain decimal digits,
// no exponent, no trailing ".0" on a whole number. A client stores "1" and
// reads "1", not "1e+00".
func formatNumber(f *big.Float) string {
	if f.IsInt() {
		i, _ := f.Int(nil)
		return i.String()
	}
	return strings.TrimRight(strings.TrimRight(f.Text('f', 20), "0"), ".")
}

// addToAttribute is ADD: arithmetic on a number, union on a set. The
// attribute not existing is not an error -- ADD creates it, which is what
// makes it the usual way to keep a counter.
func addToAttribute(item map[string]any, attr string, val any) *apiError {
	have, ok := item[attr]
	if !ok {
		item[attr] = val
		return nil
	}
	if avType(val) == "N" {
		sum, aerr := arithmetic(have, val, "+")
		if aerr != nil {
			return aerr
		}
		item[attr] = sum
		return nil
	}
	t := avType(val)
	if t != "SS" && t != "NS" && t != "BS" {
		return dynamoBadRequest("ADD on %q needs a number or a set, not %s", attr, t)
	}
	if avType(have) != t {
		return dynamoBadRequest("ADD on %q: cannot add a %s to a %s", attr, t, avType(have))
	}
	members := strList(have.(map[string]any)[t])
	for _, m := range strList(val.(map[string]any)[t]) {
		found := false
		for _, existing := range members {
			if existing == m {
				found = true
				break
			}
		}
		if !found {
			members = append(members, m)
		}
	}
	item[attr] = map[string]any{t: toAnyList(members)}
	return nil
}

func deleteFromSet(item map[string]any, attr string, val any) *apiError {
	have, ok := item[attr]
	if !ok {
		return nil
	}
	t := avType(val)
	if avType(have) != t {
		return dynamoBadRequest("DELETE on %q: %s is not a %s", attr, avType(have), t)
	}
	drop := map[string]bool{}
	for _, m := range strList(val.(map[string]any)[t]) {
		drop[m] = true
	}
	var kept []string
	for _, m := range strList(have.(map[string]any)[t]) {
		if !drop[m] {
			kept = append(kept, m)
		}
	}
	// An empty set is not a legal value: the attribute goes instead.
	if len(kept) == 0 {
		delete(item, attr)
		return nil
	}
	item[attr] = map[string]any{t: toAnyList(kept)}
	return nil
}

func appendLists(a, b any) (any, *apiError) {
	la, oka := a.(map[string]any)
	lb, okb := b.(map[string]any)
	if !oka || !okb {
		return nil, dynamoBadRequest("list_append needs two lists")
	}
	first, oka := la["L"].([]any)
	second, okb := lb["L"].([]any)
	if !oka || !okb {
		return nil, dynamoBadRequest("list_append needs two lists")
	}
	return map[string]any{"L": append(append([]any{}, first...), second...)}, nil
}

func toAnyList(list []string) []any {
	out := make([]any, 0, len(list))
	for _, s := range list {
		out = append(out, s)
	}
	return out
}

// projection narrows an item to the attributes a ProjectionExpression asks
// for. An empty expression returns the whole item.
func projection(item map[string]any, expr string, names map[string]string) (map[string]any, *apiError) {
	if strings.TrimSpace(expr) == "" {
		return item, nil
	}
	out := map[string]any{}
	for _, raw := range splitTopLevel(expr, ',') {
		attr, aerr := resolveName(strings.TrimSpace(raw), names)
		if aerr != nil {
			return nil, aerr
		}
		if v, ok := item[attr]; ok {
			out[attr] = v
		}
	}
	return out, nil
}
