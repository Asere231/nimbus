package azure

// Tolerating the body the az CLI sends for a deployment.
//
// `az deployment group create` does not send JSON. It serialises everything
// except the template, chops the closing braces off, and splices the template
// back in with an UNQUOTED key, from azure-cli's own resource/custom.py:
//
//	json_data = partial_request[:-2] + ", template:" + template + r"}}"
//
// with the comment "This is not an ordinary JSON format, but it is a JSONC
// format that service can deserialize". It does this to preserve the
// template's whitespace exactly. Go's encoding/json refuses it with
//
//	invalid character 't' looking for beginning of object key string
//
// and, because az logs the body BEFORE rewriting it, --debug shows valid JSON
// and tells you nothing. azure-cli's own source says this "caused a very
// difficult-to-debug issue".
//
// So: when a body fails to parse as JSON, quote any bare object keys and try
// once more. This runs only after a strict parse has already failed, so a
// valid request is never touched.

// quoteBareKeys returns src with unquoted object keys quoted. It is a
// character scan rather than a parser: everything inside a string literal is
// left alone, and a bare word is only quoted where a key belongs -- directly
// after '{' or after a ',' inside an object, and followed by ':'.
func quoteBareKeys(src []byte) []byte {
	out := make([]byte, 0, len(src)+16)

	// Context stack: true for an object, false for an array. A ',' only
	// introduces a key inside an object.
	var stack []bool
	inObject := func() bool { return len(stack) > 0 && stack[len(stack)-1] }

	// wantKey is true where the next non-space token should be a key.
	wantKey := false

	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '"':
			// Copy the whole string literal verbatim, escapes included.
			out = append(out, c)
			for i++; i < len(src); i++ {
				out = append(out, src[i])
				if src[i] == '\\' && i+1 < len(src) {
					i++
					out = append(out, src[i])
					continue
				}
				if src[i] == '"' {
					break
				}
			}
			wantKey = false
			continue

		case c == '{':
			stack = append(stack, true)
			wantKey = true
			out = append(out, c)
			continue

		case c == '[':
			stack = append(stack, false)
			wantKey = false
			out = append(out, c)
			continue

		case c == '}' || c == ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			wantKey = false
			out = append(out, c)
			continue

		case c == ',':
			wantKey = inObject()
			out = append(out, c)
			continue

		case c == ':':
			wantKey = false
			out = append(out, c)
			continue

		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			out = append(out, c)
			continue
		}

		if wantKey && isKeyStart(c) {
			j := i
			for j < len(src) && isKeyPart(src[j]) {
				j++
			}
			// Only a word followed by ':' is a key; anything else (true,
			// false, null as a value) is left exactly as it is.
			k := j
			for k < len(src) && (src[k] == ' ' || src[k] == '\t' || src[k] == '\n' || src[k] == '\r') {
				k++
			}
			if k < len(src) && src[k] == ':' {
				out = append(out, '"')
				out = append(out, src[i:j]...)
				out = append(out, '"')
				i = j - 1
				wantKey = false
				continue
			}
		}
		wantKey = false
		out = append(out, c)
	}
	return out
}

func isKeyStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isKeyPart(c byte) bool {
	return isKeyStart(c) || (c >= '0' && c <= '9') || c == '-' || c == '.'
}
