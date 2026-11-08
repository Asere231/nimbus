package gcp

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The paths and bodies here were captured from the google provider 6.50.0
// and from `gcloud pubsub topics publish` / `subscriptions pull` against a
// logging server.

const topicPath = "/v1/projects/nimbus-local/topics/nimbus-events"
const subPath = "/v1/projects/nimbus-local/subscriptions/nimbus-sub"

// A topic is created with PUT on its own URL, not POST to a collection,
// and the body carries no name at all -- the name is in the path. That is
// this API's convention and not GCP's in general.
func TestTopicIsCreatedByPuttingToItsOwnURL(t *testing.T) {
	s := newTestServer(t)
	doc := must(t, s, http.MethodPut, topicPath+"?alt=json",
		`{"labels": {"goog-terraform-provisioned": "true"}}`)

	if doc["name"] != "projects/nimbus-local/topics/nimbus-events" {
		t.Fatalf("name = %v, want the resource name built from the path", doc["name"])
	}

	read := must(t, s, http.MethodGet, topicPath, "")
	if read["name"] != doc["name"] {
		t.Errorf("the read disagrees with the create: %v vs %v", read["name"], doc["name"])
	}
	labels, _ := read["labels"].(map[string]any)
	if labels["goog-terraform-provisioned"] != "true" {
		t.Errorf("labels did not round-trip: %v", read["labels"])
	}
}

// The provider sends explicit JSON nulls for the policies a config left
// out. Present-and-null is not the same as absent, and anything reading a
// field has to survive it.
func TestSubscriptionToleratesTheProvidersExplicitNulls(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPut, topicPath, `{}`)
	doc := must(t, s, http.MethodPut, subPath+"?alt=json", `{
		"deadLetterPolicy": null,
		"expirationPolicy": null,
		"retryPolicy": null,
		"labels": {"goog-terraform-provisioned": "true"},
		"messageRetentionDuration": "604800s",
		"pushConfig": {"pushEndpoint": "http://127.0.0.1:9/"},
		"topic": "projects/nimbus-local/topics/nimbus-events"
	}`)

	if doc["name"] != "projects/nimbus-local/subscriptions/nimbus-sub" {
		t.Fatalf("name = %v", doc["name"])
	}
	if doc["ackDeadlineSeconds"] != float64(defaultAckDeadline) {
		t.Errorf("ackDeadlineSeconds = %v, want the default %d", doc["ackDeadlineSeconds"], defaultAckDeadline)
	}
}

// A subscription nests under its topic, so the graph shows what feeds what.
func TestSubscriptionHangsOffItsTopicInTheGraph(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPut, topicPath, `{}`)
	must(t, s, http.MethodPut, subPath, `{"topic": "projects/nimbus-local/topics/nimbus-events"}`)

	res, err := s.store.Get("projects/nimbus-local/subscriptions/nimbus-sub")
	if err != nil {
		t.Fatal(err)
	}
	if res.ParentID != "projects/nimbus-local/topics/nimbus-events" {
		t.Errorf("parent = %q, want the topic", res.ParentID)
	}
}

// A subscription on a topic that does not exist is silence that looks like
// a broken publisher, so it is refused -- as Pub/Sub refuses it.
func TestSubscriptionToAMissingTopicIsRefused(t *testing.T) {
	s := newTestServer(t)
	rec, doc := call(t, s, http.MethodPut, subPath,
		`{"topic": "projects/nimbus-local/topics/nope"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body)
	}
	e, _ := doc["error"].(map[string]any)
	if e == nil || e["status"] != "NOT_FOUND" {
		t.Errorf("error = %v", doc["error"])
	}
}

// --- publish and pull -----------------------------------------------------

func TestPublishThenPullRoundTripsThePayloadByteForByte(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPut, topicPath, `{}`)
	must(t, s, http.MethodPut, subPath, `{"topic": "projects/nimbus-local/topics/nimbus-events"}`)

	// What `gcloud pubsub topics publish --message hi` sends.
	payload := base64.StdEncoding.EncodeToString([]byte(`{"hello":"world"}`))
	pub := must(t, s, http.MethodPost, topicPath+":publish?alt=json",
		`{"messages": [{"attributes": {"source": "test"}, "data": "`+payload+`"}]}`)
	ids, _ := pub["messageIds"].([]any)
	if len(ids) != 1 {
		t.Fatalf("messageIds = %v, want one", pub["messageIds"])
	}

	pulled := must(t, s, http.MethodPost, subPath+":pull?alt=json",
		`{"maxMessages": 10, "returnImmediately": false}`)
	received, _ := pulled["receivedMessages"].([]any)
	if len(received) != 1 {
		t.Fatalf("receivedMessages = %v, want one", pulled["receivedMessages"])
	}
	entry := received[0].(map[string]any)
	msg := entry["message"].(map[string]any)
	// Stored exactly as it arrived: nothing decodes and re-encodes it, for
	// the reason DynamoDB's numbers taught.
	if msg["data"] != payload {
		t.Errorf("data = %v, want the base64 exactly as sent", msg["data"])
	}
	attrs := msg["attributes"].(map[string]any)
	if attrs["source"] != "test" {
		t.Errorf("attributes = %v", msg["attributes"])
	}
	if entry["ackId"] == "" || entry["ackId"] == nil {
		t.Error("a pull without an ackId cannot be acknowledged")
	}
}

// A client reads the field before it checks it, which is the lesson
// DynamoDB's UnprocessedItems taught: it must always be present.
func TestPullOnAnEmptySubscriptionStillReturnsTheField(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPut, topicPath, `{}`)
	must(t, s, http.MethodPut, subPath, `{"topic": "projects/nimbus-local/topics/nimbus-events"}`)

	doc := must(t, s, http.MethodPost, subPath+":pull", `{"maxMessages": 1}`)
	if _, ok := doc["receivedMessages"]; !ok {
		t.Fatal("receivedMessages is missing entirely, not merely empty")
	}
}

func TestAcknowledgeRemovesTheMessage(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPut, topicPath, `{}`)
	must(t, s, http.MethodPut, subPath, `{"topic": "projects/nimbus-local/topics/nimbus-events"}`)
	must(t, s, http.MethodPost, topicPath+":publish", `{"messages": [{"data": "aGk="}]}`)

	pulled := must(t, s, http.MethodPost, subPath+":pull", `{"maxMessages": 1}`)
	ackID := pulled["receivedMessages"].([]any)[0].(map[string]any)["ackId"].(string)
	must(t, s, http.MethodPost, subPath+":acknowledge", `{"ackIds": ["`+ackID+`"]}`)

	if depth := s.messages.depth("projects/nimbus-local/subscriptions/nimbus-sub"); depth != 0 {
		t.Errorf("the subscription still holds %d messages after an ack", depth)
	}
}

// A leased message is not handed out twice; one whose lease was released
// is. That is how Pub/Sub redelivers something nobody acknowledged.
func TestALeasedMessageIsNotRedeliveredUntilItsDeadlinePasses(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPut, topicPath, `{}`)
	must(t, s, http.MethodPut, subPath, `{"topic": "projects/nimbus-local/topics/nimbus-events"}`)
	must(t, s, http.MethodPost, topicPath+":publish", `{"messages": [{"data": "aGk="}]}`)

	first := must(t, s, http.MethodPost, subPath+":pull", `{"maxMessages": 10}`)
	if got := len(first["receivedMessages"].([]any)); got != 1 {
		t.Fatalf("first pull got %d, want 1", got)
	}
	second := must(t, s, http.MethodPost, subPath+":pull", `{"maxMessages": 10}`)
	if got := len(second["receivedMessages"].([]any)); got != 0 {
		t.Errorf("second pull got %d while the lease was held, want 0", got)
	}

	// Releasing the lease with a zero deadline is how a client says "I
	// cannot handle this".
	ackID := first["receivedMessages"].([]any)[0].(map[string]any)["ackId"].(string)
	must(t, s, http.MethodPost, subPath+":modifyAckDeadline",
		`{"ackIds": ["`+ackID+`"], "ackDeadlineSeconds": 0}`)

	third := must(t, s, http.MethodPost, subPath+":pull", `{"maxMessages": 10}`)
	if got := len(third["receivedMessages"].([]any)); got != 1 {
		t.Errorf("after the lease was released the pull got %d, want 1", got)
	}
}

// An unimplemented verb is refused with its name, rather than answering
// something that looks like success. A `seek` that rewound nothing is
// worse than one that says it is not built.
func TestAnUnimplementedVerbIsRefusedByName(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPut, topicPath, `{}`)
	rec, doc := call(t, s, http.MethodPost, topicPath+":detach", `{}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	e, _ := doc["error"].(map[string]any)
	msg, _ := e["message"].(string)
	if !strings.Contains(msg, "detach") {
		t.Errorf("the refusal does not name the verb: %q", msg)
	}
}

// --- push delivery --------------------------------------------------------

// The envelope is Pub/Sub's own, which is what any handler written for
// real Pub/Sub expects.
func TestPublishDeliversThePubSubEnvelopeToAPushEndpoint(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	done := make(chan struct{})

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if got == nil {
			_ = readJSON(r, &got)
			close(done)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	s := newTestServer(t)
	must(t, s, http.MethodPut, topicPath, `{}`)
	must(t, s, http.MethodPut, subPath, `{
		"topic": "projects/nimbus-local/topics/nimbus-events",
		"pushConfig": {"pushEndpoint": "`+target.URL+`/"}
	}`)
	must(t, s, http.MethodPost, topicPath+":publish", `{"messages": [{"data": "aGk="}]}`)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("nothing was pushed to the endpoint")
	}

	mu.Lock()
	defer mu.Unlock()
	if got["subscription"] != "projects/nimbus-local/subscriptions/nimbus-sub" {
		t.Errorf("subscription = %v", got["subscription"])
	}
	msg, _ := got["message"].(map[string]any)
	if msg == nil || msg["data"] != "aGk=" {
		t.Fatalf("message = %v, want the base64 payload under `message`", got["message"])
	}
	if msg["messageId"] == "" || msg["messageId"] == nil {
		t.Error("a handler may read messageId; it must be there")
	}
	if msg["publishTime"] == "" || msg["publishTime"] == nil {
		t.Error("publishTime is part of the envelope")
	}
}

// A push subscription queues nothing: delivery IS the acknowledgement.
func TestAPushSubscriptionDoesNotAlsoQueue(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	s := newTestServer(t)
	must(t, s, http.MethodPut, topicPath, `{}`)
	must(t, s, http.MethodPut, subPath, `{
		"topic": "projects/nimbus-local/topics/nimbus-events",
		"pushConfig": {"pushEndpoint": "`+target.URL+`/"}
	}`)
	must(t, s, http.MethodPost, topicPath+":publish", `{"messages": [{"data": "aGk="}]}`)

	if depth := s.messages.depth("projects/nimbus-local/subscriptions/nimbus-sub"); depth != 0 {
		t.Errorf("a push subscription queued %d messages as well as pushing them", depth)
	}
}

// Nimbus is an HTTP client here, against a URL that came out of a config.
// Anything not on this machine is recorded and not called -- the same line
// "never call real cloud endpoints" draws everywhere else.
func TestPushRefusesAnEndpointThatIsNotLocal(t *testing.T) {
	for _, endpoint := range []string{
		"https://example.com/hook",
		"http://8.8.8.8/hook",
	} {
		if err := localEndpoint(endpoint); err == nil {
			t.Errorf("localEndpoint(%q) allowed a remote endpoint", endpoint)
		}
	}
	for _, endpoint := range []string{
		"http://127.0.0.1:8080/",
		"http://localhost:8080/x",
		// A handler in a container pushing back to the host is exactly
		// the case this has to allow.
		"http://host.docker.internal:4703/_nimbus/run/hello",
	} {
		if err := localEndpoint(endpoint); err != nil {
			t.Errorf("localEndpoint(%q) refused a local endpoint: %v", endpoint, err)
		}
	}
}
