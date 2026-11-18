package aws

import (
	"context"
	"testing"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
)

// A log group survives a restart.
//
// It did not. A log group's EXISTENCE lived in two places -- the resource in
// core.Store, which persists, and logStore's own map of groups to streams,
// which is memory and is what DescribeLogGroups actually reads. So after a
// restart the store still held the group and nothing could see it:
//
//	# aws_cloudwatch_log_group.fn has been deleted
//	# aws_cloudwatch_log_group.fn will be created
//
// Terraform recreated a resource that was right there. Measured against aws
// provider 5.100.0 by restarting Nimbus under the url-shortener example.
//
// This is the "every engine must be reattached on startup" lesson about
// something that is not an engine: an in-memory INDEX over persisted state
// needs rebuilding exactly as an engine's registry does, and for the same
// reason -- the store survives and the index does not. Milestone 19's restart
// jobs missed it because neither example they restart declares a log group,
// and a Lambda's implicit group is recreated by the next invoke.
//
// Log EVENTS are deliberately not restored: nothing persists them, real
// CloudWatch would keep them, and the divergence is harmless in the
// direction that matters -- an empty group reads as "nothing has run yet",
// which after a restart is true of the process doing the reading.
func TestALogGroupSurvivesARestart(t *testing.T) {
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}

	// A log group a previous Nimbus created, still in the store, with the
	// retention and tags it was created with.
	const name = "/aws/lambda/shortener"
	first := New(Config{PublicURL: "http://localhost:4702", DataDir: t.TempDir()}, store, bus)
	arn := first.logGroupARN(name)
	store.Put(&core.Resource{
		Cloud: "aws", ID: arn, Type: typeLogGroup, Name: name,
		Body: jsonBytes(map[string]any{
			"logGroupName":    name,
			"arn":             arn,
			"retentionInDays": 7,
			"tags":            map[string]any{"Project": "nimbus"},
		}),
	})

	// The restart: a fresh Server over the same store, which is what a new
	// process has.
	s := New(Config{PublicURL: "http://localhost:4702", DataDir: t.TempDir()}, store, bus)
	f := docker.NewFake()
	f.Unavailable = true
	s.UseDocker(f)
	s.reattach(context.Background())

	doc := mustLogs(t, s, "DescribeLogGroups", "{}")
	groups, _ := doc["logGroups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("the log group did not survive the restart: %v", doc)
	}
	got, _ := groups[0].(map[string]any)
	if got["logGroupName"] != name {
		t.Errorf("wrong group came back: %v", got)
	}
	// Retention and tags are in the body, so they come back with it -- which
	// is what makes the plan clean rather than merely non-empty.
	if got["retentionInDays"] != float64(7) {
		t.Errorf("retention did not survive: %v", got)
	}
	tags := mustLogs(t, s, "ListTagsLogGroup", `{"logGroupName": "`+name+`"}`)
	if m, _ := tags["tags"].(map[string]any); m["Project"] != "nimbus" {
		t.Errorf("tags did not survive the restart: %v", tags)
	}

	// And it can be written to without being created again, which is the
	// other half: a handler logging after a restart must not have to
	// discover that its own group is missing.
	mustLogs(t, s, "CreateLogStream",
		`{"logGroupName": "`+name+`", "logStreamName": "s1"}`)
	mustLogs(t, s, "PutLogEvents", `{
		"logGroupName": "`+name+`", "logStreamName": "s1",
		"logEvents": [{"timestamp": 1, "message": "after the restart"}]
	}`)
}

// Nothing else in the store is mistaken for a log group.
//
// reattach walks every resource, so the filter has to be the type and not
// something looser like "has an arn with logs in it" -- a Lambda's ARN and
// its log group's differ by one segment.
func TestARestartRebuildsOnlyLogGroups(t *testing.T) {
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	store.Put(&core.Resource{
		Cloud: "aws", ID: "arn:aws:lambda:us-east-1:000000000000:function:writer",
		Type: "AWS::Lambda::Function", Name: "writer",
	})
	store.Put(&core.Resource{
		Cloud: "aws", ID: "arn:aws:dynamodb:us-east-1:000000000000:table/items",
		Type: "AWS::DynamoDB::Table", Name: "items",
	})

	s := New(Config{PublicURL: "http://localhost:4702", DataDir: t.TempDir()}, store, bus)
	f := docker.NewFake()
	f.Unavailable = true
	s.UseDocker(f)
	s.reattach(context.Background())

	doc := mustLogs(t, s, "DescribeLogGroups", "{}")
	if groups, _ := doc["logGroups"].([]any); len(groups) != 0 {
		t.Errorf("a restart invented log groups from other resources: %v", doc)
	}
}
