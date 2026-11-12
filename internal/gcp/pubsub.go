package gcp

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/core"
)

// Pub/Sub: topics, subscriptions, and messages that really move.
//
// Measured against the google provider 6.50.0 and gcloud 592.0.0:
//
//   - A topic and a subscription are created with **PUT on their own
//     URL**, not POST to a collection -- the name is in the path and not
//     in the body. That is this API's convention and not GCP's in
//     general: Compute posts to a collection, Cloud Run posts with a
//     `?serviceId=`.
//   - The verb is part of the path, after a colon:
//     `topics/{t}:publish`, `subscriptions/{s}:pull`.
//   - The provider sends explicit JSON **nulls** for the policies a config
//     left out (`"retryPolicy": null`), so anything reading a field has to
//     survive it being present and null, which is not the same as absent.
//
// A resource NAME here is `projects/{p}/topics/{t}` -- a path, not a self
// link URL. Pub/Sub is one of the newer APIs, like IAM, and does not use
// self links at all. That name is what the provider stores as the
// resource's id, so it is what goes in the store.

const (
	TypeTopic        = "pubsub.googleapis.com/Topic"
	TypeSubscription = "pubsub.googleapis.com/Subscription"
)

func (s *Server) topicName(name string) string {
	return "projects/" + s.cfg.ProjectID + "/topics/" + name
}

func (s *Server) subscriptionName(name string) string {
	return "projects/" + s.cfg.ProjectID + "/subscriptions/" + name
}

// servePubSub routes /v1/projects/{p}/{topics|subscriptions}[/{name}[:verb]].
func (s *Server) servePubSub(w http.ResponseWriter, r *http.Request, parts []string) string {
	// A path that arrived without its version prefix is one segment
	// shorter; see router.go on why both are accepted.
	if parts[0] == "projects" {
		parts = append([]string{"v1"}, parts...)
	}
	collection := parts[3]
	var name, verb string
	if len(parts) > 4 {
		name, verb = trimVerb(strings.Join(parts[4:], "/"))
	}

	var e *apiError
	switch {
	case name == "" && r.Method == http.MethodGet:
		e = s.listPubSub(w, collection)

	case verb != "":
		e = s.pubsubVerb(w, r, collection, name, verb)

	case r.Method == http.MethodPut:
		e = s.putPubSub(w, r, collection, name)
	case r.Method == http.MethodGet:
		e = s.getPubSub(w, collection, name)
	case r.Method == http.MethodPatch:
		e = s.patchPubSub(w, r, collection, name)
	case r.Method == http.MethodDelete:
		e = s.deletePubSub(w, collection, name)

	default:
		e = notFound("Nimbus does not emulate %s %s yet", r.Method, r.URL.Path)
	}
	if e != nil {
		writeError(w, e)
		return "pubsub " + r.Method + " " + collection + ": " + e.Code
	}
	return ""
}

// pubsubKind maps a collection to what Nimbus calls it and how it nests.
func (s *Server) pubsubKind(collection string) (resourceType string, nameFor func(string) string, ok bool) {
	switch collection {
	case "topics":
		return TypeTopic, s.topicName, true
	case "subscriptions":
		return TypeSubscription, s.subscriptionName, true
	}
	return "", nil, false
}

// pubsubID is the store id for a topic or a subscription, which is also
// its full resource name.
func (s *Server) pubsubID(collection, name string) string {
	if _, nameFor, ok := s.pubsubKind(collection); ok {
		return nameFor(name)
	}
	return name
}

func (s *Server) loadPubSub(collection, name string) (map[string]any, *apiError) {
	resourceType, nameFor, ok := s.pubsubKind(collection)
	if !ok {
		return nil, notFound("Nimbus does not emulate Pub/Sub %s yet", collection)
	}
	res, err := s.store.Get(nameFor(name))
	if err != nil || res.Type != resourceType {
		return nil, notFound("Resource not found (resource=%s).", name)
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body, &body); err != nil {
		return nil, internalError(err)
	}
	return body, nil
}

// putPubSub creates or replaces a topic or a subscription.
func (s *Server) putPubSub(w http.ResponseWriter, r *http.Request, collection, name string) *apiError {
	resourceType, nameFor, ok := s.pubsubKind(collection)
	if !ok {
		return notFound("Nimbus does not emulate Pub/Sub %s yet", collection)
	}
	var body map[string]any
	if err := readJSON(r, &body); err != nil {
		return badRequest("%v", err)
	}
	if body == nil {
		body = map[string]any{}
	}
	resourceName := nameFor(name)
	if s.store.Exists(resourceName) {
		return errorf(http.StatusConflict, "alreadyExists", "ALREADY_EXISTS",
			"Resource already exists in the project (resource=%s).", name)
	}
	// The name comes from the PATH. A client that also put one in the body
	// agrees with it; one that did not still gets the right answer.
	body["name"] = resourceName

	parent := s.projectSelfLink()
	if resourceType == TypeSubscription {
		if aerr := s.fillSubscription(body, name); aerr != nil {
			return aerr
		}
		// A subscription nests under its topic, so the graph shows what
		// feeds what.
		if topic := str(body["topic"]); topic != "" && s.store.Exists(topic) {
			parent = topic
		}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return internalError(err)
	}
	s.store.Put(&core.Resource{
		Cloud: "gcp", ID: resourceName, Type: resourceType, Name: name,
		ParentID: parent, Backing: "real", Body: raw,
	})
	writeJSON(w, http.StatusOK, body)
	return nil
}

// fillSubscription applies the defaults a client reads back, and refuses a
// subscription to a topic that does not exist -- as Pub/Sub does, and
// because a subscription on a mistyped topic is silence that looks like a
// broken publisher.
func (s *Server) fillSubscription(body map[string]any, name string) *apiError {
	topic := str(body["topic"])
	if topic == "" {
		return badRequest("subscription.topic is required")
	}
	if !s.store.Exists(topic) {
		return notFound("Resource not found (resource=%s).", lastSegment(topic))
	}
	if _, ok := body["ackDeadlineSeconds"].(float64); !ok {
		body["ackDeadlineSeconds"] = defaultAckDeadline
	}
	if str(body["messageRetentionDuration"]) == "" {
		body["messageRetentionDuration"] = "604800s"
	}
	if _, ok := body["expirationPolicy"]; !ok {
		body["expirationPolicy"] = map[string]any{"ttl": "2678400s"}
	}
	return nil
}

func (s *Server) getPubSub(w http.ResponseWriter, collection, name string) *apiError {
	body, aerr := s.loadPubSub(collection, name)
	if aerr != nil {
		return aerr
	}
	writeJSON(w, http.StatusOK, body)
	return nil
}

func (s *Server) listPubSub(w http.ResponseWriter, collection string) *apiError {
	resourceType, _, ok := s.pubsubKind(collection)
	if !ok {
		return notFound("Nimbus does not emulate Pub/Sub %s yet", collection)
	}
	out := []any{}
	for _, res := range s.store.List(func(r *core.Resource) bool { return r.Type == resourceType }) {
		var body map[string]any
		if err := json.Unmarshal(res.Body, &body); err == nil {
			out = append(out, body)
		}
	}
	// The field is named after the collection: `topics`, `subscriptions`.
	writeJSON(w, http.StatusOK, map[string]any{collection: out})
	return nil
}

// patchPubSub applies an update. The body is `{"topic": {...},
// "updateMask": "labels,..."}` -- the resource wrapped under its own
// singular name, with the mask beside it, the way IAM does it and Compute
// does not.
func (s *Server) patchPubSub(w http.ResponseWriter, r *http.Request, collection, name string) *apiError {
	resourceType, nameFor, ok := s.pubsubKind(collection)
	if !ok {
		return notFound("Nimbus does not emulate Pub/Sub %s yet", collection)
	}
	var req map[string]any
	if err := readJSON(r, &req); err != nil {
		return badRequest("%v", err)
	}
	singular := strings.TrimSuffix(collection, "s")
	patch, _ := req[singular].(map[string]any)
	if patch == nil {
		patch = req
	}
	mask := maskFields(str(req["updateMask"]))

	var updated map[string]any
	err := s.store.Update(nameFor(name), func(body map[string]any) error {
		for k, v := range patch {
			if k == "name" || !mask.allows(k) {
				continue
			}
			body[k] = v
		}
		updated = cloneMap(body)
		return nil
	})
	if err != nil {
		return notFound("Resource not found (resource=%s).", name)
	}
	_ = resourceType
	writeJSON(w, http.StatusOK, updated)
	return nil
}

func (s *Server) deletePubSub(w http.ResponseWriter, collection, name string) *apiError {
	_, nameFor, ok := s.pubsubKind(collection)
	if !ok {
		return notFound("Nimbus does not emulate Pub/Sub %s yet", collection)
	}
	if _, aerr := s.loadPubSub(collection, name); aerr != nil {
		return aerr
	}
	resourceName := nameFor(name)
	if _, err := s.store.Delete(resourceName); err != nil {
		return internalError(err)
	}
	if collection == "subscriptions" {
		s.messages.drop(resourceName)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
	return nil
}

// --- the verbs ------------------------------------------------------------

func (s *Server) pubsubVerb(w http.ResponseWriter, r *http.Request, collection, name, verb string) *apiError {
	if _, aerr := s.loadPubSub(collection, name); aerr != nil {
		return aerr
	}
	var req map[string]any
	if err := readJSON(r, &req); err != nil {
		return badRequest("%v", err)
	}

	switch verb {
	case "publish":
		return s.publish(w, name, req)
	case "pull":
		return s.pull(w, name, req)
	case "acknowledge":
		s.messages.ack(s.subscriptionName(name), stringList(req["ackIds"]))
		writeJSON(w, http.StatusOK, map[string]any{})
		return nil
	case "modifyAckDeadline":
		seconds := 0
		if n, ok := req["ackDeadlineSeconds"].(float64); ok {
			seconds = int(n)
		}
		s.messages.modifyAckDeadline(s.subscriptionName(name), stringList(req["ackIds"]), seconds)
		writeJSON(w, http.StatusOK, map[string]any{})
		return nil
	case "modifyPushConfig":
		return s.modifyPushConfig(w, name, req)
	case "getIamPolicy":
		writeJSON(w, http.StatusOK, s.getResourcePolicy(s.pubsubID(collection, name)))
		return nil
	case "setIamPolicy":
		// This used to echo the policy back and store NOTHING, under a
		// comment claiming it was recorded. A binding a client sets
		// successfully and then reads back as absent is worse than no
		// support at all, because it looks like it worked.
		id := s.pubsubID(collection, name)
		policy, _ := req["policy"].(map[string]any)
		if err := s.setResourcePolicy(id, policy); err != nil {
			return internalError(err)
		}
		writeJSON(w, http.StatusOK, s.getResourcePolicy(id))
		return nil
	case "testIamPermissions":
		// The truth, now that there is a truth to tell -- the same fix
		// the project policy's got in Milestone 14, for the same
		// reason: this is the one call whose job is to stop a client
		// being surprised by a 403.
		writeJSON(w, http.StatusOK, map[string]any{
			"permissions": s.heldPermissions(r, name, stringList(req["permissions"])),
		})
		return nil
	default:
		// Refused with the construct named, rather than silently doing
		// nothing: a `seek` that appeared to work and rewound nothing is
		// worse than one that says it is not built.
		return notFound("Nimbus does not emulate the Pub/Sub %s verb yet", verb)
	}
}

// publish accepts messages and fans them out to every subscription of the
// topic. A topic with no subscriptions accepts and discards, exactly as
// Pub/Sub does -- there is nowhere for the message to go.
func (s *Server) publish(w http.ResponseWriter, topic string, req map[string]any) *apiError {
	raw, _ := req["messages"].([]any)
	if len(raw) == 0 {
		return badRequest("at least one message is required")
	}
	topicName := s.topicName(topic)
	subs := s.subscriptionsOf(topicName)

	ids := make([]string, 0, len(raw))
	for _, item := range raw {
		sent, _ := item.(map[string]any)
		if sent == nil {
			return badRequest("a message must be an object")
		}
		msg := message{
			ID: s.messages.nextID("msg"),
			// The payload is base64 on the wire and is stored exactly as
			// it arrived. Nothing decodes and re-encodes it, for the
			// reason DynamoDB's numbers taught: a round trip through a
			// different representation is where values change silently.
			Data:        str(sent["data"]),
			Attributes:  stringMap(sent["attributes"]),
			PublishTime: timestamp(),
			OrderingKey: str(sent["orderingKey"]),
		}
		ids = append(ids, msg.ID)
		for _, sub := range subs {
			if endpoint := pushEndpoint(sub.body); endpoint != "" {
				go s.deliverPush(sub.name, endpoint, msg)
				continue
			}
			s.messages.publish(sub.name, msg)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"messageIds": ids})
	return nil
}

type subRef struct {
	name string
	body map[string]any
}

func (s *Server) subscriptionsOf(topicName string) []subRef {
	var out []subRef
	for _, res := range s.store.List(func(r *core.Resource) bool { return r.Type == TypeSubscription }) {
		var body map[string]any
		if err := json.Unmarshal(res.Body, &body); err != nil {
			continue
		}
		if str(body["topic"]) == topicName {
			out = append(out, subRef{name: res.ID, body: body})
		}
	}
	return out
}

// pushEndpoint reads a subscription's push URL, tolerating the explicit
// null the provider sends for a pull subscription.
func pushEndpoint(body map[string]any) string {
	cfg, _ := body["pushConfig"].(map[string]any)
	if cfg == nil {
		return ""
	}
	return str(cfg["pushEndpoint"])
}

func (s *Server) pull(w http.ResponseWriter, sub string, req map[string]any) *apiError {
	max := 1
	if n, ok := req["maxMessages"].(float64); ok {
		max = int(n)
	}
	body, aerr := s.loadPubSub("subscriptions", sub)
	if aerr != nil {
		return aerr
	}
	deadline := defaultAckDeadline
	if n, ok := body["ackDeadlineSeconds"].(float64); ok && n > 0 {
		deadline = int(n)
	}
	leased := s.messages.pull(s.subscriptionName(sub), max, time.Duration(deadline)*time.Second)

	received := make([]any, 0, len(leased))
	for _, q := range leased {
		received = append(received, map[string]any{
			"ackId":           q.ackID,
			"message":         q.msg,
			"deliveryAttempt": q.deliveries,
		})
	}
	// Always present, even when empty: a client reads the field before
	// checking it, which is the lesson DynamoDB's UnprocessedItems taught.
	writeJSON(w, http.StatusOK, map[string]any{"receivedMessages": received})
	return nil
}

func (s *Server) modifyPushConfig(w http.ResponseWriter, sub string, req map[string]any) *apiError {
	cfg, _ := req["pushConfig"].(map[string]any)
	err := s.store.Update(s.subscriptionName(sub), func(body map[string]any) error {
		body["pushConfig"] = cfg
		return nil
	})
	if err != nil {
		return notFound("Resource not found (resource=%s).", sub)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
	return nil
}

func stringList(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func stringMap(v any) map[string]string {
	in, _ := v.(map[string]any)
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, val := range in {
		out[k] = str(val)
	}
	return out
}

// atoiOr parses a number a client sent as a string, which Pub/Sub's
// durations are ("604800s").
func atoiOr(s string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSuffix(s, "s"))
	if err != nil {
		return fallback
	}
	return n
}
