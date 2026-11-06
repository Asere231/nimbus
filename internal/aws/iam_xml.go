package aws

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
)

// Writing IAM's XML.
//
// IAM nests a result one level deeper than EC2 (<XxxResult> inside
// <XxxResponse>) and wraps list entries in <member> rather than <item>.
// The element names change per action while the bodies do not, so these
// helpers take the names and marshal whatever struct they are given,
// rather than declaring a pair of structs for each of forty actions.
//
// encoding/xml names an element after its Go type unless the struct has an
// XMLName field, which is why each helper renames the outermost element
// after marshalling: the alternative is an XMLName on every type, set to
// a different value depending on which action is answering.

func writeRawXML(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write([]byte(body))
}

// rename rewrites the outermost element of a marshalled fragment.
func rename(fragment, to string) string {
	fragment = strings.TrimSpace(fragment)
	if !strings.HasPrefix(fragment, "<") {
		return fragment
	}
	end := strings.Index(fragment, ">")
	if end < 0 {
		return fragment
	}
	from := strings.TrimSuffix(strings.TrimPrefix(fragment[:end+1], "<"), ">")
	from = strings.Fields(from)[0]
	fragment = "<" + to + fragment[end:]
	return strings.TrimSuffix(fragment, "</"+from+">") + "</" + to + ">"
}

// writeMember sends one object inside a named element inside a result.
func writeMember(w http.ResponseWriter, response, result, element string, v any) {
	inner, err := xml.MarshalIndent(v, "    ", "  ")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeRawXML(w, 200, fmt.Sprintf(
		"<%s xmlns=%q>\n  <%s>\n%s\n  </%s>\n  <ResponseMetadata><RequestId>%s</RequestId></ResponseMetadata>\n</%s>",
		response, namespaceFor("iam"), result, "    "+rename(string(inner), element), result, requestID(), response))
}

// writeMembers sends a list, each entry wrapped in <member>, inside a
// named collection element.
func writeMembers[T any](w http.ResponseWriter, response, result, collection string, items []T) {
	var buf strings.Builder
	fmt.Fprintf(&buf, "<%s xmlns=%q>\n  <%s>\n    <IsTruncated>false</IsTruncated>\n    <%s>\n",
		response, namespaceFor("iam"), result, collection)
	for _, item := range items {
		inner, err := xml.MarshalIndent(item, "      ", "  ")
		if err != nil {
			continue
		}
		buf.WriteString("      " + rename(string(inner), "member") + "\n")
	}
	fmt.Fprintf(&buf, "    </%s>\n  </%s>\n  <ResponseMetadata><RequestId>%s</RequestId></ResponseMetadata>\n</%s>",
		collection, result, requestID(), response)
	writeRawXML(w, 200, buf.String())
}

// writeStringMembers is the same for a list of plain strings, which is how
// ListRolePolicies answers.
func writeStringMembers(w http.ResponseWriter, response, result, collection string, items []string) {
	var buf strings.Builder
	fmt.Fprintf(&buf, "<%s xmlns=%q>\n  <%s>\n    <IsTruncated>false</IsTruncated>\n    <%s>\n",
		response, namespaceFor("iam"), result, collection)
	for _, item := range items {
		buf.WriteString("      <member>" + escapeXML(item) + "</member>\n")
	}
	fmt.Fprintf(&buf, "    </%s>\n  </%s>\n  <ResponseMetadata><RequestId>%s</RequestId></ResponseMetadata>\n</%s>",
		collection, result, requestID(), response)
	writeRawXML(w, 200, buf.String())
}

// writeFlat sends a struct's fields directly inside the result, with no
// wrapper element -- GetRolePolicy answers that way.
func writeFlat(w http.ResponseWriter, response, result string, v any) {
	inner, err := xml.MarshalIndent(v, "    ", "  ")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// Strip the struct's own outer element, keeping its children.
	body := strings.TrimSpace(string(inner))
	if i := strings.Index(body, ">"); i >= 0 {
		if j := strings.LastIndex(body, "</"); j > i {
			body = body[i+1 : j]
		}
	}
	writeRawXML(w, 200, fmt.Sprintf(
		"<%s xmlns=%q>\n  <%s>%s</%s>\n  <ResponseMetadata><RequestId>%s</RequestId></ResponseMetadata>\n</%s>",
		response, namespaceFor("iam"), result, body, result, requestID(), response))
}

// writeIAMEmpty is the acknowledgement every IAM mutation that returns
// nothing answers with.
func writeIAMEmpty(w http.ResponseWriter, response string) {
	writeRawXML(w, 200, fmt.Sprintf(
		"<%s xmlns=%q>\n  <ResponseMetadata><RequestId>%s</RequestId></ResponseMetadata>\n</%s>",
		response, namespaceFor("iam"), requestID(), response))
}

func escapeXML(s string) string {
	var buf strings.Builder
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}
