// Package deployments implements ARM template deployments: the expression
// language a template is written in, and the expansion of a template into the
// list of resources to create.
//
// Bicep never reaches here. `az deployment group create --template-file x.bicep`
// runs the Bicep compiler locally and sends ARM JSON, so this package only
// ever sees JSON -- measured in az 2.90.0, which shells out to
// `bicep build --stdout` before it makes a single HTTP call.
package deployments

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// Scope is everything an expression can ask about where it is being deployed.
type Scope struct {
	SubscriptionID  string
	ResourceGroup   string
	ResourceGroupID string
	Location        string
	TenantID        string
	DeploymentName  string

	Parameters map[string]any
	Variables  map[string]any

	// Reference resolves reference(...) against what has actually been
	// deployed. nil means no resource is available yet, which is what
	// validation and what-if pass.
	Reference func(resourceID string) (map[string]any, bool)

	// copyIndexes holds the current iteration of every copy loop in scope,
	// keyed by loop name; the resource-level loop is the empty key.
	// currentCopy names the innermost one, which is what a bare copyIndex()
	// means. A copyIndex() with no loop at all is an error rather than a
	// silent zero.
	copyIndexes map[string]int
	currentCopy string
	inCopy      bool
	// deferReferences keeps expressions that need reference() unevaluated,
	// because the resource they name has not been created yet.
	deferReferences bool
}

// Evaluate resolves one template value. A string that is not bracketed is
// returned unchanged; "[[" at the start is the documented escape for a literal
// bracket, so "[[x]" means the string "[x]".
func Evaluate(src string, s *Scope) (any, error) {
	if !strings.HasPrefix(src, "[") || !strings.HasSuffix(src, "]") || len(src) < 2 {
		return src, nil
	}
	if strings.HasPrefix(src, "[[") {
		return src[1:], nil
	}
	p := &parser{input: src[1 : len(src)-1], scope: s}
	p.skipSpace()
	v, err := p.expression()
	if err != nil {
		return nil, fmt.Errorf("in %s: %w", src, err)
	}
	p.skipSpace()
	if p.pos < len(p.input) {
		return nil, fmt.Errorf("in %s: unexpected %q", src, p.input[p.pos:])
	}
	return v, nil
}

// EvaluateDeferring is EvaluateDeep, except that an expression which can only
// fail because the resource it references has not been created yet is left
// alone, to be evaluated again by the deployer when it has. Everything else
// still fails immediately: a typo should not survive until deploy time.
func EvaluateDeferring(v any, s *Scope) (any, error) {
	d := *s
	d.deferReferences = true
	return EvaluateDeep(v, &d)
}

// EvaluateDeep walks a whole JSON value, resolving every string in it. This is
// what turns a template's resource into the body to PUT.
func EvaluateDeep(v any, s *Scope) (any, error) {
	switch x := v.(type) {
	case string:
		out, err := Evaluate(x, s)
		if err != nil && s.deferReferences && strings.Contains(strings.ToLower(x), "reference(") {
			return x, nil // the deployer will try again once it exists
		}
		return out, err
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			// Keys can be expressions too, in a copy loop's output.
			key, err := Evaluate(k, s)
			if err != nil {
				return nil, err
			}
			ks, ok := key.(string)
			if !ok {
				ks = k
			}
			ev, err := EvaluateDeep(val, s)
			if err != nil {
				return nil, err
			}
			out[ks] = ev
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(x))
		for _, val := range x {
			ev, err := EvaluateDeep(val, s)
			if err != nil {
				return nil, err
			}
			out = append(out, ev)
		}
		return out, nil
	default:
		return v, nil
	}
}

// --- parsing ---------------------------------------------------------------
//
// The language is small enough to evaluate while parsing: a value is a
// literal or a function call, each optionally followed by accessors
// (.property or [index]). There is no operator precedence to worry about,
// because ARM templates have no infix operators -- add(1, 2), not 1 + 2.

type parser struct {
	input string
	pos   int
	scope *Scope
}

func (p *parser) skipSpace() {
	for p.pos < len(p.input) && unicode.IsSpace(rune(p.input[p.pos])) {
		p.pos++
	}
}

func (p *parser) expression() (any, error) {
	v, err := p.operand()
	if err != nil {
		return nil, err
	}
	return p.accessors(v)
}

func (p *parser) operand() (any, error) {
	p.skipSpace()
	if p.pos >= len(p.input) {
		return nil, fmt.Errorf("expected a value")
	}
	c := p.input[p.pos]
	switch {
	case c == '\'':
		return p.stringLiteral()
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	case isIdentStart(c):
		return p.identifier()
	}
	return nil, fmt.Errorf("unexpected %q", string(c))
}

func (p *parser) stringLiteral() (any, error) {
	p.pos++ // opening quote
	var b strings.Builder
	for p.pos < len(p.input) {
		c := p.input[p.pos]
		if c == '\'' {
			// '' inside a literal is one quote, as in ARM.
			if p.pos+1 < len(p.input) && p.input[p.pos+1] == '\'' {
				b.WriteByte('\'')
				p.pos += 2
				continue
			}
			p.pos++
			return b.String(), nil
		}
		b.WriteByte(c)
		p.pos++
	}
	return nil, fmt.Errorf("unterminated string")
}

func (p *parser) number() (any, error) {
	start := p.pos
	if p.input[p.pos] == '-' {
		p.pos++
	}
	for p.pos < len(p.input) && (p.input[p.pos] == '.' || (p.input[p.pos] >= '0' && p.input[p.pos] <= '9')) {
		p.pos++
	}
	f, err := strconv.ParseFloat(p.input[start:p.pos], 64)
	if err != nil {
		return nil, fmt.Errorf("bad number %q", p.input[start:p.pos])
	}
	return f, nil
}

// identifier is a bare word: true, false, null, or a function name followed
// by its arguments.
func (p *parser) identifier() (any, error) {
	start := p.pos
	for p.pos < len(p.input) && isIdentPart(p.input[p.pos]) {
		p.pos++
	}
	name := p.input[start:p.pos]
	p.skipSpace()
	if p.pos >= len(p.input) || p.input[p.pos] != '(' {
		switch strings.ToLower(name) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		}
		return nil, fmt.Errorf("%q is not a value; a function call needs parentheses", name)
	}
	p.pos++ // (
	var args []any
	p.skipSpace()
	if p.pos < len(p.input) && p.input[p.pos] == ')' {
		p.pos++
		return p.call(name, args)
	}
	for {
		arg, err := p.expression()
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
		p.skipSpace()
		if p.pos >= len(p.input) {
			return nil, fmt.Errorf("unterminated call to %s", name)
		}
		switch p.input[p.pos] {
		case ',':
			p.pos++
		case ')':
			p.pos++
			return p.call(name, args)
		default:
			return nil, fmt.Errorf("unexpected %q in call to %s", string(p.input[p.pos]), name)
		}
	}
}

// accessors applies any .property or [index] that follows a value.
func (p *parser) accessors(v any) (any, error) {
	for {
		p.skipSpace()
		if p.pos >= len(p.input) {
			return v, nil
		}
		switch p.input[p.pos] {
		case '.':
			p.pos++
			start := p.pos
			for p.pos < len(p.input) && isIdentPart(p.input[p.pos]) {
				p.pos++
			}
			name := p.input[start:p.pos]
			if name == "" {
				return nil, fmt.Errorf("expected a property name after '.'")
			}
			m, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("cannot read property %q of a non-object", name)
			}
			next, ok := m[name]
			if !ok {
				// Case-insensitive, as ARM is.
				for k, val := range m {
					if strings.EqualFold(k, name) {
						next, ok = val, true
						break
					}
				}
			}
			if !ok {
				return nil, fmt.Errorf("property %q not found", name)
			}
			v = next
		case '[':
			p.pos++
			idx, err := p.expression()
			if err != nil {
				return nil, err
			}
			p.skipSpace()
			if p.pos >= len(p.input) || p.input[p.pos] != ']' {
				return nil, fmt.Errorf("expected ']'")
			}
			p.pos++
			next, err := index(v, idx)
			if err != nil {
				return nil, err
			}
			v = next
		default:
			return v, nil
		}
	}
}

func index(v, idx any) (any, error) {
	switch container := v.(type) {
	case []any:
		n, ok := idx.(float64)
		if !ok {
			return nil, fmt.Errorf("an array index must be a number, got %T", idx)
		}
		i := int(n)
		if i < 0 || i >= len(container) {
			return nil, fmt.Errorf("index %d is out of range (length %d)", i, len(container))
		}
		return container[i], nil
	case map[string]any:
		k, ok := idx.(string)
		if !ok {
			return nil, fmt.Errorf("an object index must be a string, got %T", idx)
		}
		val, ok := container[k]
		if !ok {
			return nil, fmt.Errorf("key %q not found", k)
		}
		return val, nil
	case string:
		n, ok := idx.(float64)
		if !ok {
			return nil, fmt.Errorf("a string index must be a number")
		}
		i := int(n)
		if i < 0 || i >= len(container) {
			return nil, fmt.Errorf("index %d is out of range", i)
		}
		return string(container[i]), nil
	}
	return nil, fmt.Errorf("cannot index a %T", v)
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

// jsonClone round-trips a value so callers cannot mutate what they are given.
func jsonClone(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if json.Unmarshal(raw, &out) != nil {
		return v
	}
	return out
}
