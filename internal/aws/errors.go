package aws

import (
	"encoding/xml"
	"net/http"
)

// AWS has three error envelopes, and which one a client can read depends on
// the service it thinks it is talking to.
//
// They are genuinely different documents, not stylistic variants, and a
// client handed the wrong one reports the unhelpful
//
//	An error occurred (Unknown) when calling the GetCallerIdentity operation
//
// which is what aws-cli printed while this was being built.
//
// The status code matters as much as the body. Measured against aws-cli
// 1.40.33: a 500 is retried five times with backoff, so an unimplemented
// action answered with a 5xx turns one mistake into five seconds of
// hammering and five identical lines in the activity feed. Client errors
// are 400, which is answered once.

// apiError is an error on its way out, in whichever envelope the service
// uses.
type apiError struct {
	Status  int
	Code    string
	Message string
	// Resource is the S3 envelope's extra field: the bucket or key.
	Resource string
}

func errorf(status int, code, format string, args ...any) *apiError {
	return &apiError{Status: status, Code: code, Message: sprintf(format, args...)}
}

// invalidAction is what AWS answers for an action it does not have, and
// what Nimbus answers for one it has not built yet. A 400 rather than a
// 501 so clients do not retry it, with the action named so the activity
// feed reads as a to-do list.
func invalidAction(service, action string) *apiError {
	return errorf(400, "InvalidAction",
		"Nimbus does not implement the %s action %q yet", service, action)
}

// --- the envelopes --------------------------------------------------------

// queryError is IAM's and STS's shape.
type queryError struct {
	XMLName xml.Name `xml:"ErrorResponse"`
	Xmlns   string   `xml:"xmlns,attr"`
	Error   struct {
		Type    string `xml:"Type"`
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	} `xml:"Error"`
	RequestID string `xml:"RequestId"`
}

// ec2Error is EC2's, which wraps a LIST of errors and spells the request id
// differently -- RequestID, not RequestId.
type ec2Error struct {
	XMLName xml.Name `xml:"Response"`
	Errors  struct {
		Error struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		} `xml:"Error"`
	} `xml:"Errors"`
	RequestID string `xml:"RequestID"`
}

// s3Error is S3's: no wrapper at all, and it names the resource.
type s3Error struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId"`
}

// writeError sends an error in the envelope the service expects.
func writeError(w http.ResponseWriter, service string, e *apiError) {
	id := requestID()
	status := e.Status
	if status == 0 {
		status = 400
	}
	switch service {
	case "ec2":
		doc := ec2Error{RequestID: id}
		doc.Errors.Error.Code = e.Code
		doc.Errors.Error.Message = e.Message
		writeXML(w, status, doc)
	case "s3":
		writeXML(w, status, s3Error{Code: e.Code, Message: e.Message, Resource: e.Resource, RequestID: id})
	default:
		doc := queryError{Xmlns: namespaceFor(service), RequestID: id}
		// Sender means "you got this wrong", Receiver means "we did".
		doc.Error.Type = "Sender"
		if status >= 500 {
			doc.Error.Type = "Receiver"
		}
		doc.Error.Code = e.Code
		doc.Error.Message = e.Message
		writeXML(w, status, doc)
	}
}

// namespaceFor is the XML namespace a service's documents live in. A client
// parsing with the wrong one sees an empty document, so these are part of
// the wire contract rather than decoration.
func namespaceFor(service string) string {
	switch service {
	case "sts":
		return "https://sts.amazonaws.com/doc/2011-06-15/"
	case "iam":
		return "https://iam.amazonaws.com/doc/2010-05-08/"
	case "ec2":
		return "http://ec2.amazonaws.com/doc/2016-11-15/"
	}
	return ""
}
