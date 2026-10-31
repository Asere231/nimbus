package deployments

import (
	"fmt"
	"sort"
	"strings"
)

// Plan is a template turned into the list of resources to create, in the
// order they have to be created.
type Plan struct {
	Resources []Resource
	// Outputs is the template's outputs section, still unevaluated: outputs
	// usually call reference(), so they can only be resolved once everything
	// has actually been deployed.
	Outputs map[string]any
	// Parameters and Variables as resolved, for the deployment record ARM
	// returns and for evaluating outputs later.
	Parameters map[string]any
	Variables  map[string]any
}

// Resource is one resource a deployment will create.
type Resource struct {
	ID         string
	Type       string
	Name       string
	APIVersion string
	Body       map[string]any
	DependsOn  []string
}

// Expand resolves a template into a Plan: parameters merged with their
// defaults, variables evaluated, copy loops unrolled, conditions applied, and
// everything sorted so a resource is created after what it depends on.
//
// Expressions that call reference() are NOT resolved here -- the resource does
// not exist yet. They are left for the deployer, which re-evaluates each body
// immediately before the PUT.
func Expand(template map[string]any, params map[string]any, scope *Scope) (*Plan, error) {
	s := *scope // a copy: expansion fills in parameters and variables
	resolved, err := resolveParameters(template, params, &s)
	if err != nil {
		return nil, err
	}
	s.Parameters = resolved

	vars, err := resolveVariables(template, &s)
	if err != nil {
		return nil, err
	}
	s.Variables = vars

	raw, _ := template["resources"].([]any)
	var out []Resource
	for _, item := range raw {
		res, ok := item.(map[string]any)
		if !ok {
			continue
		}
		expanded, err := expandResource(res, &s)
		if err != nil {
			return nil, err
		}
		out = append(out, expanded...)
	}

	ordered, err := order(out)
	if err != nil {
		return nil, err
	}
	outputs, _ := template["outputs"].(map[string]any)
	return &Plan{Resources: ordered, Outputs: outputs, Parameters: resolved, Variables: vars}, nil
}

// resolveParameters merges supplied values with the template's defaults. The
// az CLI and ARM both send values wrapped as {"name": {"value": x}}, while a
// caller inside Nimbus may pass them bare; both are accepted.
func resolveParameters(template map[string]any, supplied map[string]any, scope *Scope) (map[string]any, error) {
	declared, _ := template["parameters"].(map[string]any)
	out := map[string]any{}

	// A DEFAULT can be an expression -- "[resourceGroup().location]" is the
	// commonest line in any Bicep file -- and may refer to another parameter,
	// so defaults are evaluated, and repeated until they settle. Supplied
	// values are literal JSON and are taken as they are.
	pending := map[string]any{}
	for name, raw := range declared {
		spec, _ := raw.(map[string]any)
		if v, ok := lookup(supplied, name); ok {
			out[name] = unwrapValue(v)
			continue
		}
		if spec != nil {
			if def, ok := spec["defaultValue"]; ok {
				pending[name] = def
				continue
			}
		}
		return nil, fmt.Errorf("no value was supplied for parameter %q and it has no default", name)
	}

	s := *scope
	s.Parameters = out
	for len(pending) > 0 {
		progress := false
		var lastErr error
		for name, raw := range pending {
			v, err := EvaluateDeep(raw, &s)
			if err != nil {
				lastErr = fmt.Errorf("default for parameter %q: %w", name, err)
				continue
			}
			out[name] = v
			delete(pending, name)
			progress = true
		}
		if !progress {
			return nil, lastErr
		}
	}
	// A value for something the template does not declare is ignored rather
	// than rejected, which is what ARM does.
	return out, nil
}

func unwrapValue(v any) any {
	if m, ok := v.(map[string]any); ok {
		if inner, ok := m["value"]; ok && len(m) == 1 {
			return inner
		}
	}
	return v
}

// resolveVariables evaluates variables in dependency order. Rather than build
// another graph, it repeats until nothing more resolves, which handles
// variables referring to each other and reports the ones that never do.
func resolveVariables(template map[string]any, s *Scope) (map[string]any, error) {
	declared, _ := template["variables"].(map[string]any)
	out := map[string]any{}
	s.Variables = out

	pending := map[string]any{}
	for k, v := range declared {
		if strings.EqualFold(k, "copy") {
			continue // variable copy loops are not implemented
		}
		pending[k] = v
	}
	for len(pending) > 0 {
		progress := false
		var lastErr error
		for name, raw := range pending {
			v, err := EvaluateDeep(raw, s)
			if err != nil {
				lastErr = fmt.Errorf("variable %q: %w", name, err)
				continue
			}
			out[name] = v
			delete(pending, name)
			progress = true
		}
		if !progress {
			return nil, lastErr
		}
	}
	return out, nil
}

// expandResource turns one template resource into one or more real ones,
// unrolling a copy loop and honouring a condition.
func expandResource(res map[string]any, s *Scope) ([]Resource, error) {
	copySpec, _ := res["copy"].(map[string]any)
	if copySpec == nil {
		one, skip, err := materialise(res, s, -1)
		if err != nil || skip {
			return nil, err
		}
		return []Resource{one}, nil
	}

	rawCount, err := EvaluateDeep(copySpec["count"], s)
	if err != nil {
		return nil, fmt.Errorf("copy count: %w", err)
	}
	count, ok := rawCount.(float64)
	if !ok {
		return nil, fmt.Errorf("copy count must be a number, got %T", rawCount)
	}
	var out []Resource
	for i := 0; i < int(count); i++ {
		one, skip, err := materialise(res, s, i)
		if err != nil {
			return nil, err
		}
		if !skip {
			out = append(out, one)
		}
	}
	return out, nil
}

// materialise evaluates one resource's template keys. index is the copy-loop
// iteration, or -1 outside a loop.
func materialise(res map[string]any, s *Scope, index int) (Resource, bool, error) {
	scope := *s
	scope.copyIndexes = map[string]int{}
	for k, v := range s.copyIndexes {
		scope.copyIndexes[k] = v
	}
	if index >= 0 {
		// The resource-level loop is the one copyIndex() means when it is
		// given no name.
		scope.copyIndexes[""] = index
		scope.currentCopy = ""
		scope.inCopy = true
	}

	typ := strings.TrimSpace(fmt.Sprint(res["type"]))
	rawName, err := EvaluateDeep(res["name"], &scope)
	if err != nil {
		return Resource{}, false, fmt.Errorf("%s: name: %w", typ, err)
	}
	name := toString(rawName)

	if cond, present := res["condition"]; present {
		v, err := EvaluateDeep(cond, &scope)
		if err != nil {
			return Resource{}, false, fmt.Errorf("%s/%s: condition: %w", typ, name, err)
		}
		if !truthy(v) {
			return Resource{}, true, nil
		}
	}

	// The body is everything except the template-only keys, with its
	// expressions resolved. Anything needing reference() is left as-is for
	// the deployer, which evaluates it once the resource it names exists.
	raw := map[string]any{}
	for k, v := range res {
		switch strings.ToLower(k) {
		case "type", "apiversion", "dependson", "condition", "copy", "scope", "comments":
			continue
		}
		raw[k] = v
	}
	evaluated, err := expandCopies(raw, &scope)
	if err != nil {
		return Resource{}, false, fmt.Errorf("%s/%s: %w", typ, name, err)
	}
	body, _ := evaluated.(map[string]any)
	if body == nil {
		body = map[string]any{}
	}
	body["name"] = name

	id, err := scope.scopedResourceID(scope.resourceGroupID(), []any{typ, name})
	if err != nil {
		// A child type written as "parent/child" with a name "p/c" is the
		// other spelling ARM allows.
		id2, err2 := childID(&scope, typ, name)
		if err2 != nil {
			return Resource{}, false, fmt.Errorf("%s/%s: %w", typ, name, err)
		}
		id = id2
	}

	deps, err := dependencies(res, &scope)
	if err != nil {
		return Resource{}, false, fmt.Errorf("%s/%s: dependsOn: %w", typ, name, err)
	}

	return Resource{
		ID:         toString(id),
		Type:       typ,
		Name:       name,
		APIVersion: toString(res["apiVersion"]),
		Body:       body,
		DependsOn:  deps,
	}, false, nil
}

// childID handles the "Microsoft.X/parents/children" + "parent/child" form.
func childID(s *Scope, typ, name string) (string, error) {
	typeSegs := strings.Split(typ, "/")
	nameSegs := strings.Split(name, "/")
	if len(typeSegs)-1 != len(nameSegs) {
		return "", fmt.Errorf("type %s does not match the name %q", typ, name)
	}
	args := []any{typ}
	for _, n := range nameSegs {
		args = append(args, n)
	}
	id, err := s.scopedResourceID(s.resourceGroupID(), args)
	return toString(id), err
}

// dependencies collects what a resource must be created after: its explicit
// dependsOn, plus every resource ID its body mentions. Bicep output often
// relies on the second kind alone.
func dependencies(res map[string]any, s *Scope) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if id != "" && !seen[strings.ToLower(id)] {
			seen[strings.ToLower(id)] = true
			out = append(out, id)
		}
	}
	for _, raw := range asList(res["dependsOn"]) {
		v, err := EvaluateDeep(raw, s)
		if err != nil {
			return nil, err
		}
		add(toString(v))
	}
	// Implicit: any resourceId(...) inside the body.
	collectResourceIDs(res, s, add)
	return out, nil
}

// collectResourceIDs evaluates every resourceId() expression it can find in a
// value. Expressions it cannot evaluate yet (reference(), mostly) are skipped:
// a missing implicit edge only costs ordering, which dependsOn usually
// provides anyway.
func collectResourceIDs(v any, s *Scope, add func(string)) {
	switch x := v.(type) {
	case string:
		if !strings.Contains(strings.ToLower(x), "resourceid(") {
			return
		}
		if out, err := Evaluate(x, s); err == nil {
			if id, ok := out.(string); ok && strings.HasPrefix(id, "/subscriptions/") {
				add(id)
			}
		}
	case map[string]any:
		for k, val := range x {
			if strings.EqualFold(k, "dependsOn") {
				continue
			}
			collectResourceIDs(val, s, add)
		}
	case []any:
		for _, val := range x {
			collectResourceIDs(val, s, add)
		}
	}
}

// order sorts resources so each comes after everything it depends on. Ties
// keep template order, so a template with no dependencies deploys top to
// bottom, as ARM does.
func order(resources []Resource) ([]Resource, error) {
	index := map[string]int{}
	for i, r := range resources {
		index[strings.ToLower(r.ID)] = i
	}
	const (
		unvisited = 0
		active    = 1
		done      = 2
	)
	state := make([]int, len(resources))
	var out []Resource
	var visit func(i int, trail []string) error
	visit = func(i int, trail []string) error {
		switch state[i] {
		case done:
			return nil
		case active:
			return fmt.Errorf("circular dependency: %s", strings.Join(append(trail, resources[i].Name), " -> "))
		}
		state[i] = active
		// Deterministic order among dependencies, so a plan is reproducible.
		deps := append([]string{}, resources[i].DependsOn...)
		sort.Strings(deps)
		for _, dep := range deps {
			j, ok := index[strings.ToLower(dep)]
			if !ok || j == i {
				continue // a dependency on something outside the template
			}
			if err := visit(j, append(trail, resources[i].Name)); err != nil {
				return err
			}
		}
		state[i] = done
		out = append(out, resources[i])
		return nil
	}
	for i := range resources {
		if err := visit(i, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// EvaluateOutputs resolves the outputs section once everything is deployed,
// which is when reference() can finally answer.
func (p *Plan) EvaluateOutputs(scope *Scope) (map[string]any, error) {
	if len(p.Outputs) == 0 {
		return map[string]any{}, nil
	}
	s := *scope
	s.Parameters = p.Parameters
	s.Variables = p.Variables
	out := map[string]any{}
	for name, raw := range p.Outputs {
		v, err := EvaluateDeep(raw, &s)
		if err != nil {
			return nil, fmt.Errorf("output %q: %w", name, err)
		}
		out[name] = v
	}
	return out, nil
}

func asList(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

// expandCopies evaluates a resource body, expanding property-level copy loops
// as it goes.
//
// This is what a Bicep `for` inside a property compiles to: instead of
// repeating the resource, the object carries
//
//	"copy": [{"name": "subnets", "count": 2, "input": {...}}]
//
// and the result is a sibling array named "subnets". It is easy to miss,
// because the resource-level copy -- which repeats the whole resource -- looks
// nothing like it; the azure-bicep example in examples/bicep uses both.
//
// Strings are evaluated with reference() deferred, exactly as elsewhere.
func expandCopies(v any, s *Scope) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if strings.EqualFold(k, "copy") {
				if specs, ok := val.([]any); ok {
					if err := expandPropertyCopies(specs, out, s); err != nil {
						return nil, err
					}
					continue
				}
			}
			ev, err := expandCopies(val, s)
			if err != nil {
				return nil, err
			}
			out[k] = ev
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(x))
		for _, val := range x {
			ev, err := expandCopies(val, s)
			if err != nil {
				return nil, err
			}
			out = append(out, ev)
		}
		return out, nil
	default:
		return EvaluateDeferring(v, s)
	}
}

// expandPropertyCopies turns each {name, count, input} into a sibling array.
func expandPropertyCopies(specs []any, into map[string]any, s *Scope) error {
	for _, raw := range specs {
		spec, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("a property copy must be an object")
		}
		name := toString(spec["name"])
		if name == "" {
			return fmt.Errorf("a property copy needs a name")
		}
		rawCount, err := EvaluateDeep(spec["count"], s)
		if err != nil {
			return fmt.Errorf("copy %q: count: %w", name, err)
		}
		count, ok := rawCount.(float64)
		if !ok {
			return fmt.Errorf("copy %q: count must be a number", name)
		}
		items := make([]any, 0, int(count))
		for i := 0; i < int(count); i++ {
			loop := *s
			loop.copyIndexes = map[string]int{}
			for k, v := range s.copyIndexes {
				loop.copyIndexes[k] = v
			}
			loop.copyIndexes[name] = i
			loop.currentCopy = name
			loop.inCopy = true
			item, err := expandCopies(spec["input"], &loop)
			if err != nil {
				return fmt.Errorf("copy %q item %d: %w", name, i, err)
			}
			items = append(items, item)
		}
		into[name] = items
	}
	return nil
}
