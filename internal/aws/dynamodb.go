package aws

import (
	"fmt"
	"net/http"
	"strings"
)

// DynamoDB: JSON 1.0, and a FOURTH dispatch rule.
//
// Every other service here is routed by the SigV4 credential scope, which
// is all the URL-free Query protocol leaves to go on. DynamoDB puts the
// scope to the same use -- it says "dynamodb" -- but the ACTION is in a
// header instead: `X-Amz-Target: DynamoDB_20120810.PutItem`. The body is
// JSON, the path is "/", and the Content-Type announces the dialect
// (application/x-amz-json-1.0). Measured against aws-cli 1.40.33.
//
// The error envelope is a third one again. Not XML, and not REST-JSON's
// bare code either: the `__type` carries a FULLY QUALIFIED name,
// `com.amazonaws.dynamodb.v20120810#ResourceNotFoundException`, and
// botocore keeps only what follows the '#'. Sending the bare code works
// with botocore and is wrong, so Nimbus sends the qualified one.
//
// Nothing about an item is decoded and re-encoded. An attribute arrives as
// {"S": "hi"} or {"N": "5"} and is stored and returned in exactly that
// shape, because that is the only way a value survives a round trip
// unchanged -- the same rule that keeps Terraform plans clean elsewhere.

const (
	typeDynamoTable = "AWS::DynamoDB::Table"
	dynamoTargetPfx = "DynamoDB_20120810."
	// The namespace every DynamoDB exception name is qualified with.
	dynamoErrNS = "com.amazonaws.dynamodb.v20120810#"
)

// dynamoError is this service's envelope. The header carries the qualified
// name too, for the SDKs that read it instead of the body.
func writeDynamoError(w http.ResponseWriter, e *apiError) {
	status := e.Status
	if status == 0 {
		status = 400
	}
	w.Header().Set("x-amzn-ErrorType", dynamoErrNS+e.Code)
	writeJSON(w, status, map[string]string{
		"__type":  dynamoErrNS + e.Code,
		"message": e.Message,
	})
}

// dynamoNotFound is the one a client is written to expect for a table that
// is not there. 400, not 404: DynamoDB answers every error with 400.
func dynamoNotFound(table string) *apiError {
	return &apiError{Status: 400, Code: "ResourceNotFoundException",
		Message: "Requested resource not found: Table: " + table + " not found"}
}

func dynamoBadRequest(format string, args ...any) *apiError {
	return &apiError{Status: 400, Code: "ValidationException", Message: sprintf(format, args...)}
}

func (s *Server) dynamoTableARN(name string) string {
	return arn("dynamodb", s.cfg.Region, s.cfg.AccountID, "table/"+name)
}

// serveDynamoDB dispatches on the target header.
func (s *Server) serveDynamoDB(w http.ResponseWriter, r *http.Request) string {
	target := r.Header.Get("X-Amz-Target")
	action := target
	if i := strings.LastIndex(target, "."); i >= 0 {
		action = target[i+1:]
	}

	var req map[string]any
	if err := readJSON(r, &req); err != nil {
		writeDynamoError(w, &apiError{Status: 400, Code: "SerializationException", Message: err.Error()})
		return "dynamodb: unreadable body"
	}
	if req == nil {
		req = map[string]any{}
	}

	var aerr *apiError
	switch action {
	case "CreateTable":
		aerr = s.createDynamoTable(w, req)
	case "DescribeTable":
		aerr = s.describeDynamoTable(w, req)
	case "ListTables":
		aerr = s.listDynamoTables(w)
	case "DeleteTable":
		aerr = s.deleteDynamoTable(w, req)
	case "UpdateTable":
		aerr = s.updateDynamoTable(w, req)
	case "TagResource", "UntagResource":
		aerr = s.tagDynamoResource(w, req, action == "TagResource")
	case "ListTagsOfResource":
		aerr = s.listDynamoTags(w, req)

	// The provider reads these three straight after creating a table, the
	// way it reads a dozen sub-resources off a new S3 bucket. Each has its
	// own "nothing is configured" answer, and a refusal fails the apply.
	case "DescribeContinuousBackups":
		aerr = s.describeContinuousBackups(w, req)
	case "DescribeTimeToLive":
		aerr = s.describeTimeToLive(w, req)
	case "UpdateTimeToLive":
		aerr = s.updateTimeToLive(w, req)
	case "UpdateContinuousBackups":
		aerr = s.updateContinuousBackups(w, req)

	case "PutItem":
		aerr = s.putItem(w, req)
	case "GetItem":
		aerr = s.getItem(w, req)
	case "UpdateItem":
		aerr = s.updateItem(w, req)
	case "DeleteItem":
		aerr = s.deleteItem(w, req)
	case "Query":
		aerr = s.queryItems(w, req)
	case "Scan":
		aerr = s.scanItems(w, req)
	case "BatchWriteItem":
		aerr = s.batchWriteItem(w, req)
	case "BatchGetItem":
		aerr = s.batchGetItem(w, req)

	default:
		// 400 with the action named, so the activity feed reads as the
		// to-do list rather than as five retries of a 500.
		aerr = invalidAction("DynamoDB", action)
	}

	if aerr != nil {
		writeDynamoError(w, aerr)
		return "dynamodb " + action + ": " + aerr.Code
	}
	return ""
}

// --- small helpers --------------------------------------------------------

func str(v any) string {
	s, _ := v.(string)
	return s
}

func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

func objList(v any) []map[string]any {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func strList(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func throughputOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	read, write := 0.0, 0.0
	if m != nil {
		read, _ = m["ReadCapacityUnits"].(float64)
		write, _ = m["WriteCapacityUnits"].(float64)
	}
	return map[string]any{
		"ReadCapacityUnits": read, "WriteCapacityUnits": write,
		"NumberOfDecreasesToday": 0,
	}
}

// indexDescriptions turns the index SPECIFICATIONS a create sends into the
// index DESCRIPTIONS a describe returns: the same fields plus a status, a
// count and an ARN.
func indexDescriptions(specs []map[string]any, kind string) []map[string]any {
	if len(specs) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(specs))
	for _, spec := range specs {
		desc := map[string]any{}
		for k, v := range spec {
			desc[k] = v
		}
		if kind == "GSI" {
			desc["IndexStatus"] = "ACTIVE"
			desc["ProvisionedThroughput"] = throughputOf(spec["ProvisionedThroughput"])
		}
		desc["ItemCount"] = 0
		desc["IndexSizeBytes"] = 0
		out = append(out, desc)
	}
	return out
}

// tableFromARN reads the table name out of an ARN, which is how the tag
// calls name their table: arn:aws:dynamodb:region:account:table/items.
func tableFromARN(s string) string {
	if i := strings.LastIndex(s, "table/"); i >= 0 {
		name := s[i+len("table/"):]
		// An index's ARN carries the table's name followed by /index/...
		if j := strings.Index(name, "/"); j >= 0 {
			name = name[:j]
		}
		return name
	}
	return s
}

// uuidLike is a fresh identifier in the shape DynamoDB's TableId has. Not a
// real UUID -- nothing parses it -- but the same length and layout, so a
// client that validates the format is satisfied.
func uuidLike() string {
	h := hexID(32)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}
