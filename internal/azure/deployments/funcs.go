package deployments

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// call dispatches one template function.
//
// The set here is what Bicep's compiled output actually emits, which is a good
// deal smaller than the documented language. Anything missing fails by name
// rather than returning something plausible: a template that silently deploys
// the wrong thing is worse than one that refuses.
func (p *parser) call(name string, args []any) (any, error) {
	s := p.scope
	switch strings.ToLower(name) {

	// --- template inputs ---
	case "parameters":
		key, err := str1(name, args)
		if err != nil {
			return nil, err
		}
		v, ok := lookup(s.Parameters, key)
		if !ok {
			return nil, fmt.Errorf("parameter %q is not defined", key)
		}
		return v, nil
	case "variables":
		key, err := str1(name, args)
		if err != nil {
			return nil, err
		}
		v, ok := lookup(s.Variables, key)
		if !ok {
			return nil, fmt.Errorf("variable %q is not defined", key)
		}
		return v, nil

	// --- scope ---
	case "resourcegroup":
		return map[string]any{
			"id":       s.resourceGroupID(),
			"name":     s.ResourceGroup,
			"location": s.Location,
			"tags":     map[string]any{},
		}, nil
	case "subscription":
		return map[string]any{
			"id":             "/subscriptions/" + s.SubscriptionID,
			"subscriptionId": s.SubscriptionID,
			"tenantId":       s.TenantID,
			"displayName":    "Nimbus Local",
		}, nil
	case "tenant":
		return map[string]any{"tenantId": s.TenantID}, nil
	case "copyindex":
		if !s.inCopy {
			return nil, fmt.Errorf("copyIndex() is only valid inside a copy loop")
		}
		// copyIndex(), copyIndex(offset), copyIndex('loop') and
		// copyIndex('loop', offset) are all spellings ARM accepts.
		name := s.currentCopy
		offset := 0.0
		for i, a := range args {
			switch v := a.(type) {
			case string:
				if i == 0 {
					name = v
				}
			case float64:
				offset = v
			}
		}
		idx, ok := s.copyIndexes[name]
		if !ok {
			return nil, fmt.Errorf("copyIndex(%q): no such copy loop is in scope", name)
		}
		return float64(idx) + offset, nil
	case "deployment":
		return map[string]any{"name": s.DeploymentName}, nil
	case "resourceid":
		return s.resourceID(args)
	case "subscriptionresourceid":
		return s.scopedResourceID("/subscriptions/"+s.SubscriptionID, args)
	case "tenantresourceid":
		return s.scopedResourceID("", args)
	case "extensionresourceid":
		if len(args) < 2 {
			return nil, fmt.Errorf("extensionResourceId needs a scope and a type")
		}
		base, _ := args[0].(string)
		return s.scopedResourceID(base, args[1:])
	case "reference":
		return s.reference(args)

	// --- strings ---
	case "concat":
		return concat(args)
	case "format":
		return format(args)
	case "tolower":
		v, err := str1(name, args)
		return strings.ToLower(v), err
	case "toupper":
		v, err := str1(name, args)
		return strings.ToUpper(v), err
	case "trim":
		v, err := str1(name, args)
		return strings.TrimSpace(v), err
	case "string":
		if len(args) != 1 {
			return nil, fmt.Errorf("string() takes one argument")
		}
		return toString(args[0]), nil
	case "int":
		if len(args) != 1 {
			return nil, fmt.Errorf("int() takes one argument")
		}
		switch x := args[0].(type) {
		case float64:
			return float64(int64(x)), nil
		case string:
			n, err := strconv.ParseFloat(x, 64)
			if err != nil {
				return nil, fmt.Errorf("int(%q): not a number", x)
			}
			return float64(int64(n)), nil
		}
		return nil, fmt.Errorf("int() cannot convert %T", args[0])
	case "bool":
		if len(args) != 1 {
			return nil, fmt.Errorf("bool() takes one argument")
		}
		return truthy(args[0]), nil
	case "substring":
		return substring(args)
	case "replace":
		if len(args) != 3 {
			return nil, fmt.Errorf("replace() takes three arguments")
		}
		return strings.ReplaceAll(toString(args[0]), toString(args[1]), toString(args[2])), nil
	case "split":
		if len(args) != 2 {
			return nil, fmt.Errorf("split() takes two arguments")
		}
		parts := strings.Split(toString(args[0]), toString(args[1]))
		out := make([]any, len(parts))
		for i, part := range parts {
			out[i] = part
		}
		return out, nil
	case "join":
		if len(args) != 2 {
			return nil, fmt.Errorf("join() takes an array and a separator")
		}
		items, ok := args[0].([]any)
		if !ok {
			return nil, fmt.Errorf("join() needs an array")
		}
		parts := make([]string, len(items))
		for i, it := range items {
			parts[i] = toString(it)
		}
		return strings.Join(parts, toString(args[1])), nil
	case "startswith":
		if len(args) != 2 {
			return nil, fmt.Errorf("startsWith() takes two arguments")
		}
		return strings.HasPrefix(toString(args[0]), toString(args[1])), nil
	case "endswith":
		if len(args) != 2 {
			return nil, fmt.Errorf("endsWith() takes two arguments")
		}
		return strings.HasSuffix(toString(args[0]), toString(args[1])), nil
	case "guid":
		return deterministicGUID(joinArgs(args)), nil
	case "uniquestring":
		return uniqueString(joinArgs(args)), nil
	case "newguid", "utcnow":
		// Both are non-deterministic in Azure and are only allowed in
		// parameter defaults. A value that changes between reads is a
		// permanent diff here, so they are derived from the deployment.
		if strings.EqualFold(name, "utcNow") {
			return "2024-01-01T00:00:00Z", nil
		}
		return deterministicGUID(s.DeploymentName + "/newGuid"), nil

	// --- logic ---
	case "if":
		if len(args) != 3 {
			return nil, fmt.Errorf("if() takes three arguments")
		}
		if truthy(args[0]) {
			return args[1], nil
		}
		return args[2], nil
	case "equals":
		if len(args) != 2 {
			return nil, fmt.Errorf("equals() takes two arguments")
		}
		return deepEqual(args[0], args[1]), nil
	case "not":
		if len(args) != 1 {
			return nil, fmt.Errorf("not() takes one argument")
		}
		return !truthy(args[0]), nil
	case "and":
		for _, a := range args {
			if !truthy(a) {
				return false, nil
			}
		}
		return true, nil
	case "or":
		for _, a := range args {
			if truthy(a) {
				return true, nil
			}
		}
		return false, nil
	case "coalesce":
		for _, a := range args {
			if a != nil {
				return a, nil
			}
		}
		return nil, nil
	case "greater", "less", "greaterorequals", "lessorequals":
		return compare(strings.ToLower(name), args)

	// --- numbers ---
	case "add", "sub", "mul", "div", "mod":
		return arithmetic(strings.ToLower(name), args)
	case "min", "max":
		return minMax(strings.ToLower(name), args)

	// --- collections ---
	case "length":
		if len(args) != 1 {
			return nil, fmt.Errorf("length() takes one argument")
		}
		switch x := args[0].(type) {
		case string:
			return float64(len(x)), nil
		case []any:
			return float64(len(x)), nil
		case map[string]any:
			return float64(len(x)), nil
		}
		return nil, fmt.Errorf("length() cannot measure a %T", args[0])
	case "empty":
		if len(args) != 1 {
			return nil, fmt.Errorf("empty() takes one argument")
		}
		switch x := args[0].(type) {
		case nil:
			return true, nil
		case string:
			return x == "", nil
		case []any:
			return len(x) == 0, nil
		case map[string]any:
			return len(x) == 0, nil
		}
		return false, nil
	case "first", "last":
		return firstLast(strings.ToLower(name), args)
	case "contains":
		return contains(args)
	case "union":
		return union(args)
	case "createarray":
		return append([]any{}, args...), nil
	case "createobject":
		if len(args)%2 != 0 {
			return nil, fmt.Errorf("createObject() needs key/value pairs")
		}
		out := map[string]any{}
		for i := 0; i < len(args); i += 2 {
			out[toString(args[i])] = args[i+1]
		}
		return out, nil
	case "range":
		if len(args) != 2 {
			return nil, fmt.Errorf("range() takes a start and a count")
		}
		start, ok1 := args[0].(float64)
		count, ok2 := args[1].(float64)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("range() takes two numbers")
		}
		out := make([]any, 0, int(count))
		for i := 0; i < int(count); i++ {
			out = append(out, start+float64(i))
		}
		return out, nil
	case "json":
		raw, err := str1(name, args)
		if err != nil {
			return nil, err
		}
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, fmt.Errorf("json(): %w", err)
		}
		return v, nil
	case "base64":
		v, err := str1(name, args)
		return base64Encode(v), err
	}

	return nil, fmt.Errorf("%s() is not implemented by Nimbus", name)
}

func (s *Scope) resourceGroupID() string {
	if s.ResourceGroupID != "" {
		return s.ResourceGroupID
	}
	return "/subscriptions/" + s.SubscriptionID + "/resourceGroups/" + s.ResourceGroup
}

// resourceID builds an ID at resource-group scope. An optional first argument
// names a different resource group, as resourceId() allows.
func (s *Scope) resourceID(args []any) (any, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("resourceId() needs a type and a name")
	}
	base := s.resourceGroupID()
	// resourceId([subscriptionId], [resourceGroupName], type, name...): the
	// type is the first argument containing a slash.
	first := 0
	for i, a := range args {
		if str, ok := a.(string); ok && strings.Contains(str, "/") {
			first = i
			break
		}
	}
	switch first {
	case 1:
		base = "/subscriptions/" + s.SubscriptionID + "/resourceGroups/" + toString(args[0])
	case 2:
		base = "/subscriptions/" + toString(args[0]) + "/resourceGroups/" + toString(args[1])
	}
	return s.scopedResourceID(base, args[first:])
}

// scopedResourceID joins a type and its names under a scope. A type with N
// slash-separated segments after the namespace takes N names.
func (s *Scope) scopedResourceID(base string, args []any) (any, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("a resource ID needs a type and at least one name")
	}
	full := toString(args[0])
	names := args[1:]
	segs := strings.Split(full, "/")
	if len(segs) < 2 {
		return nil, fmt.Errorf("%q is not a resource type", full)
	}
	namespace, types := segs[0], segs[1:]
	if len(types) != len(names) {
		return nil, fmt.Errorf("type %s takes %d name(s), got %d", full, len(types), len(names))
	}
	id := base + "/providers/" + namespace
	for i, typ := range types {
		id += "/" + typ + "/" + toString(names[i])
	}
	return id, nil
}

// reference returns what a deployed resource looks like. Azure returns the
// resource's properties by default, or the whole resource with 'Full'.
func (s *Scope) reference(args []any) (any, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("reference() needs a resource ID")
	}
	id := toString(args[0])
	full := false
	for _, a := range args[1:] {
		if strings.EqualFold(toString(a), "full") {
			full = true
		}
	}
	if s.Reference == nil {
		return nil, fmt.Errorf("reference(%s) cannot be resolved here", id)
	}
	res, ok := s.Reference(id)
	if !ok {
		return nil, fmt.Errorf("reference(%s): the resource has not been deployed", id)
	}
	if full {
		return jsonClone(res), nil
	}
	props, _ := res["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	return jsonClone(props), nil
}

// uniqueString is deterministic, 13 lower-case base-32 characters, and is NOT
// Azure's algorithm -- Azure's is an unpublished 64-bit hash. A template that
// only needs a unique, repeatable name behaves identically; one that compares
// against a value computed in real Azure will differ. Recorded in CLAUDE.md.
func uniqueString(input string) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	sum := sha256.Sum256([]byte(input))
	out := make([]byte, 13)
	for i := range out {
		out[i] = alphabet[sum[i]%byte(len(alphabet))]
	}
	return string(out)
}

// deterministicGUID formats a hash as a GUID, so guid() is stable across runs.
func deterministicGUID(input string) string {
	sum := sha256.Sum256([]byte(input))
	hex := fmt.Sprintf("%x", sum)
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex[0:8], hex[8:12], hex[12:16], hex[16:20], hex[20:32])
}

func base64Encode(s string) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out strings.Builder
	data := []byte(s)
	for i := 0; i < len(data); i += 3 {
		var b [3]byte
		n := copy(b[:], data[i:])
		out.WriteByte(chars[b[0]>>2])
		out.WriteByte(chars[(b[0]&0x03)<<4|b[1]>>4])
		if n > 1 {
			out.WriteByte(chars[(b[1]&0x0f)<<2|b[2]>>6])
		} else {
			out.WriteByte('=')
		}
		if n > 2 {
			out.WriteByte(chars[b[2]&0x3f])
		} else {
			out.WriteByte('=')
		}
	}
	return out.String()
}

// --- small helpers ---------------------------------------------------------

func str1(name string, args []any) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("%s() takes one argument", name)
	}
	return toString(args[0]), nil
}

// lookup is case-insensitive, as ARM's parameter and variable names are.
func lookup(m map[string]any, key string) (any, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return nil, false
}

func toString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(x, "true")
	case float64:
		return x != 0
	case nil:
		return false
	}
	return true
}

func deepEqual(a, b any) bool {
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	return string(ra) == string(rb)
}

func concat(args []any) (any, error) {
	// concat works on arrays too, and the two forms never mix.
	if len(args) > 0 {
		if _, isArray := args[0].([]any); isArray {
			out := []any{}
			for _, a := range args {
				items, ok := a.([]any)
				if !ok {
					return nil, fmt.Errorf("concat() cannot mix arrays and other values")
				}
				out = append(out, items...)
			}
			return out, nil
		}
	}
	var b strings.Builder
	for _, a := range args {
		b.WriteString(toString(a))
	}
	return b.String(), nil
}

func format(args []any) (any, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("format() needs a format string")
	}
	out := toString(args[0])
	for i, a := range args[1:] {
		out = strings.ReplaceAll(out, "{"+strconv.Itoa(i)+"}", toString(a))
	}
	return out, nil
}

func substring(args []any) (any, error) {
	if len(args) < 2 || len(args) > 3 {
		return nil, fmt.Errorf("substring() takes a string, a start and an optional length")
	}
	s := toString(args[0])
	start, ok := args[1].(float64)
	if !ok {
		return nil, fmt.Errorf("substring(): start must be a number")
	}
	from := int(start)
	if from < 0 || from > len(s) {
		return nil, fmt.Errorf("substring(): start %d is out of range for a string of %d", from, len(s))
	}
	if len(args) == 2 {
		return s[from:], nil
	}
	length, ok := args[2].(float64)
	if !ok {
		return nil, fmt.Errorf("substring(): length must be a number")
	}
	to := from + int(length)
	if to > len(s) {
		return nil, fmt.Errorf("substring(): %d characters from %d exceeds the string length %d", int(length), from, len(s))
	}
	return s[from:to], nil
}

func arithmetic(op string, args []any) (any, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("%s() takes two numbers", op)
	}
	a, ok1 := args[0].(float64)
	b, ok2 := args[1].(float64)
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("%s() takes two numbers", op)
	}
	switch op {
	case "add":
		return a + b, nil
	case "sub":
		return a - b, nil
	case "mul":
		return a * b, nil
	case "div":
		if b == 0 {
			return nil, fmt.Errorf("div() by zero")
		}
		return float64(int64(a) / int64(b)), nil
	case "mod":
		if b == 0 {
			return nil, fmt.Errorf("mod() by zero")
		}
		return float64(int64(a) % int64(b)), nil
	}
	return nil, fmt.Errorf("%s() is not arithmetic", op)
}

func compare(op string, args []any) (any, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("%s() takes two arguments", op)
	}
	a, ok1 := args[0].(float64)
	b, ok2 := args[1].(float64)
	if !ok1 || !ok2 {
		sa, sb := toString(args[0]), toString(args[1])
		switch op {
		case "greater":
			return sa > sb, nil
		case "less":
			return sa < sb, nil
		case "greaterorequals":
			return sa >= sb, nil
		case "lessorequals":
			return sa <= sb, nil
		}
	}
	switch op {
	case "greater":
		return a > b, nil
	case "less":
		return a < b, nil
	case "greaterorequals":
		return a >= b, nil
	case "lessorequals":
		return a <= b, nil
	}
	return nil, fmt.Errorf("%s() is not a comparison", op)
}

func minMax(op string, args []any) (any, error) {
	items := args
	if len(args) == 1 {
		arr, ok := args[0].([]any)
		if !ok {
			return nil, fmt.Errorf("%s() takes numbers or an array", op)
		}
		items = arr
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%s() needs at least one number", op)
	}
	best, ok := items[0].(float64)
	if !ok {
		return nil, fmt.Errorf("%s() takes numbers", op)
	}
	for _, it := range items[1:] {
		n, ok := it.(float64)
		if !ok {
			return nil, fmt.Errorf("%s() takes numbers", op)
		}
		if (op == "min" && n < best) || (op == "max" && n > best) {
			best = n
		}
	}
	return best, nil
}

func firstLast(op string, args []any) (any, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("%s() takes one argument", op)
	}
	switch x := args[0].(type) {
	case []any:
		if len(x) == 0 {
			return nil, nil
		}
		if op == "first" {
			return x[0], nil
		}
		return x[len(x)-1], nil
	case string:
		if x == "" {
			return "", nil
		}
		if op == "first" {
			return string(x[0]), nil
		}
		return string(x[len(x)-1]), nil
	}
	return nil, fmt.Errorf("%s() needs an array or a string", op)
}

func contains(args []any) (any, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("contains() takes two arguments")
	}
	switch x := args[0].(type) {
	case string:
		return strings.Contains(x, toString(args[1])), nil
	case []any:
		for _, it := range x {
			if deepEqual(it, args[1]) {
				return true, nil
			}
		}
		return false, nil
	case map[string]any:
		_, ok := lookup(x, toString(args[1]))
		return ok, nil
	}
	return nil, fmt.Errorf("contains() cannot search a %T", args[0])
}

func union(args []any) (any, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("union() needs at least one argument")
	}
	if _, isArray := args[0].([]any); isArray {
		out := []any{}
		for _, a := range args {
			items, ok := a.([]any)
			if !ok {
				return nil, fmt.Errorf("union() cannot mix arrays and objects")
			}
			for _, it := range items {
				dup := false
				for _, have := range out {
					if deepEqual(have, it) {
						dup = true
						break
					}
				}
				if !dup {
					out = append(out, it)
				}
			}
		}
		return out, nil
	}
	out := map[string]any{}
	for _, a := range args {
		m, ok := a.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("union() cannot mix objects and other values")
		}
		for k, v := range m {
			out[k] = v
		}
	}
	return out, nil
}

func joinArgs(args []any) string {
	var b strings.Builder
	for _, a := range args {
		b.WriteString(toString(a))
	}
	return b.String()
}
