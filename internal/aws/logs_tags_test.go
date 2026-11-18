package aws

import (
	"strings"
	"testing"
)

// A log group's tags come back, because the aws provider READS them.
//
// They used to be accepted and discarded, under a comment saying they were
// "never read back by anything that matters". They are: `aws_cloudwatch_log_group`
// with a `tags` block applied cleanly and then wanted the same change on
// every subsequent plan, for ever --
//
//	~ resource "aws_cloudwatch_log_group" "fn" {
//	  ~ tags = { + "App" = "shortener" + "Project" = "nimbus" }
//
// -- with nothing in the diff saying where it came from. Found by building a
// real application on Nimbus rather than by a test of one service, because
// no example here had put tags on a log group.
//
// This is the "derive, don't generate" family one service further on: a value
// a client re-reads has to be the value it wrote. And the comment is the
// lesson, not the code: a note claiming something is not read back is not
// evidence that nothing reads it. Pub/Sub's setIamPolicy said "recorded,
// never enforced" while throwing the policy away.
func TestALogGroupKeepsTheTagsItWasCreatedWith(t *testing.T) {
	s := newLogsTestServer(t)
	mustLogs(t, s, "CreateLogGroup", `{
		"logGroupName": "/aws/lambda/shortener",
		"tags": {"Project": "nimbus", "App": "shortener"}
	}`)

	doc := mustLogs(t, s, "ListTagsForResource",
		`{"resourceArn": "`+s.logGroupARN("/aws/lambda/shortener")+`"}`)
	tags, _ := doc["tags"].(map[string]any)
	if len(tags) != 2 {
		t.Fatalf("expected the two tags the create carried, got %v", doc)
	}
	if tags["Project"] != "nimbus" || tags["App"] != "shortener" {
		t.Errorf("tags came back changed: %v", tags)
	}
}

// Tagging an existing group adds to it, and the OLD spelling works too.
//
// The provider calls TagResource with an ARN; `aws logs tag-log-group` calls
// TagLogGroup with a name. Both are live API, so both have to resolve to the
// same group -- the ARN carrying CloudWatch's trailing ":*" or not, since a
// client composes it either way.
func TestALogGroupCanBeTaggedByNameAndByArn(t *testing.T) {
	s := newLogsTestServer(t)
	mustLogs(t, s, "CreateLogGroup", `{"logGroupName": "/aws/lambda/w"}`)

	mustLogs(t, s, "TagLogGroup",
		`{"logGroupName": "/aws/lambda/w", "tags": {"Owner": "bryan"}}`)
	mustLogs(t, s, "TagResource",
		`{"resourceArn": "`+s.logGroupARN("/aws/lambda/w")+`", "tags": {"Env": "dev"}}`)

	// The ARN without the trailing ":*" must find the same group.
	bare := "arn:aws:logs:us-east-1:000000000000:log-group:/aws/lambda/w"
	doc := mustLogs(t, s, "ListTagsForResource", `{"resourceArn": "`+bare+`"}`)
	tags, _ := doc["tags"].(map[string]any)
	if tags["Owner"] != "bryan" || tags["Env"] != "dev" {
		t.Errorf("expected both tags from both spellings, got %v", doc)
	}

	doc = mustLogs(t, s, "ListTagsLogGroup", `{"logGroupName": "/aws/lambda/w"}`)
	if tags, _ := doc["tags"].(map[string]any); tags["Owner"] != "bryan" {
		t.Errorf("the old spelling does not read the same tags: %v", doc)
	}
}

// Untagging removes only what it names. Its "tags" field is a LIST of keys,
// not a map -- the one place in this API where that field is not an object.
func TestUntaggingALogGroupRemovesOnlyTheKeysItNames(t *testing.T) {
	s := newLogsTestServer(t)
	mustLogs(t, s, "CreateLogGroup", `{
		"logGroupName": "/aws/lambda/w",
		"tags": {"Keep": "yes", "Drop": "no", "AlsoDrop": "no"}
	}`)

	mustLogs(t, s, "UntagResource", `{
		"resourceArn": "`+s.logGroupARN("/aws/lambda/w")+`",
		"tagKeys": ["Drop"]
	}`)
	mustLogs(t, s, "UntagLogGroup", `{
		"logGroupName": "/aws/lambda/w",
		"tags": ["AlsoDrop"]
	}`)

	doc := mustLogs(t, s, "ListTagsLogGroup", `{"logGroupName": "/aws/lambda/w"}`)
	tags, _ := doc["tags"].(map[string]any)
	if len(tags) != 1 || tags["Keep"] != "yes" {
		t.Errorf("expected only Keep to survive, got %v", doc)
	}
}

// A group with no tags answers an empty map, not an error.
//
// Every Lambda creates its own log group untagged, so this is the common
// case, and the provider tolerates an empty map and nothing else.
func TestAnUntaggedLogGroupAnswersAnEmptyMap(t *testing.T) {
	s := newLogsTestServer(t)
	mustLogs(t, s, "CreateLogGroup", `{"logGroupName": "/aws/lambda/w"}`)

	doc := mustLogs(t, s, "ListTagsLogGroup", `{"logGroupName": "/aws/lambda/w"}`)
	tags, ok := doc["tags"].(map[string]any)
	if !ok {
		t.Fatalf("tags should be present and an object, got %v", doc)
	}
	if len(tags) != 0 {
		t.Errorf("expected no tags, got %v", tags)
	}
}

// Tags on a group nobody created are a not-found, with this service's code.
func TestTaggingAMissingLogGroupIsResourceNotFound(t *testing.T) {
	s := newLogsTestServer(t)
	rec, doc := logs(t, s, "TagLogGroup",
		`{"logGroupName": "/aws/lambda/absent", "tags": {"a": "b"}}`)
	if rec.Code == 200 {
		t.Fatalf("tagging a group that does not exist should not succeed: %v", doc)
	}
	if got, _ := doc["__type"].(string); !strings.Contains(got, "ResourceNotFoundException") {
		t.Errorf("wrong error type for a missing group: %v", doc)
	}
}
