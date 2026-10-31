package storage

import (
	"encoding/xml"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The queue service. Unlike blob, messages are not addressed by name: a client
// GETs them off the queue, which makes them invisible for a while, and must
// DELETE each one with the pop receipt it was handed. If it does not, the
// message reappears when the visibility timeout lapses. That redelivery is the
// whole point of a queue, so it is emulated properly rather than stubbed.

// QueueMessage is one message and its visibility state.
type QueueMessage struct {
	ID           string
	Text         string // stored exactly as sent, usually base64
	Inserted     time.Time
	Expires      time.Time
	Visible      time.Time // not returned to a GET before this
	PopReceipt   string
	DequeueCount int
}

// Queue is a named message queue.
type Queue struct {
	Name     string
	Metadata map[string]string

	mu       sync.Mutex
	messages []*QueueMessage
}

// --- queue operations on an account ----------------------------------------

func (a *Account) queueMap() map[string]*Queue {
	if a.queues == nil {
		a.queues = map[string]*Queue{}
	}
	return a.queues
}

// CreateQueue adds a queue. ok is false if one of that name already exists,
// which Azure answers 204 rather than treating as an error.
func (a *Account) CreateQueue(name string, meta map[string]string) (*Queue, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if q, exists := a.queueMap()[strings.ToLower(name)]; exists {
		return q, false
	}
	q := &Queue{Name: name, Metadata: meta}
	a.queueMap()[strings.ToLower(name)] = q
	return q, true
}

// Queue returns a queue, or nil.
func (a *Account) Queue(name string) *Queue {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.queues[strings.ToLower(name)]
}

// DeleteQueue removes a queue and its messages.
func (a *Account) DeleteQueue(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := strings.ToLower(name)
	if _, ok := a.queueMap()[k]; !ok {
		return false
	}
	delete(a.queues, k)
	return true
}

// Queues lists queues by name.
func (a *Account) Queues() []*Queue {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]*Queue, 0, len(a.queues))
	for _, q := range a.queues {
		out = append(out, q)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// --- message operations ----------------------------------------------------

// Put appends a message.
func (q *Queue) Put(text string, ttl, delay time.Duration) *QueueMessage {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now().UTC()
	m := &QueueMessage{
		ID: newRequestID(), Text: text, Inserted: now,
		Expires: now.Add(ttl), Visible: now.Add(delay),
		PopReceipt: newRequestID(),
	}
	q.messages = append(q.messages, m)
	return m
}

// Get takes up to n visible messages off the queue and hides them for the
// visibility timeout, handing back a fresh pop receipt for each.
func (q *Queue) Get(n int, visibility time.Duration, peek bool) []*QueueMessage {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.expireLocked()

	now := time.Now().UTC()
	out := []*QueueMessage{}
	for _, m := range q.messages {
		if len(out) >= n {
			break
		}
		if m.Visible.After(now) {
			continue
		}
		if !peek {
			// A peek does not count as a dequeue and does not hide anything.
			m.DequeueCount++
			m.Visible = now.Add(visibility)
			m.PopReceipt = newRequestID()
		}
		out = append(out, m)
	}
	return out
}

// Delete removes a message, but only if the pop receipt matches the one handed
// out by the last Get -- otherwise a slow consumer could delete a message that
// has already been redelivered to someone else.
func (q *Queue) Delete(id, popReceipt string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, m := range q.messages {
		if m.ID == id {
			if m.PopReceipt != popReceipt {
				return false
			}
			q.messages = append(q.messages[:i], q.messages[i+1:]...)
			return true
		}
	}
	return false
}

// Clear removes every message.
func (q *Queue) Clear() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.messages = nil
}

// Len reports how many messages are present, expired ones excluded.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.expireLocked()
	return len(q.messages)
}

func (q *Queue) expireLocked() {
	now := time.Now().UTC()
	kept := q.messages[:0]
	for _, m := range q.messages {
		if m.Expires.After(now) {
			kept = append(kept, m)
		}
	}
	q.messages = kept
}

// --- HTTP ------------------------------------------------------------------

func (h *Handler) serveQueue(w http.ResponseWriter, r *http.Request, a *Account) string {
	q := r.URL.Query()
	comp := q.Get("comp")
	name, rest := splitBlobPath(r.URL.Path) // same shape: /{queue}/messages/{id}

	switch {
	case name == "" && comp == "list":
		return h.listQueues(w, a, q.Get("prefix"))
	case name == "":
		WriteError(w, 400, "InvalidUri", "The request URI is not valid.")
		return "queue: unrecognised account-level request"
	case rest == "":
		return h.queueOp(w, r, a, name, comp)
	default:
		return h.messageOp(w, r, a, name, rest)
	}
}

type queueList struct {
	XMLName  xml.Name    `xml:"EnumerationResults"`
	Endpoint string      `xml:"ServiceEndpoint,attr"`
	Prefix   string      `xml:"Prefix,omitempty"`
	Queues   []queueItem `xml:"Queues>Queue"`
	Next     string      `xml:"NextMarker"`
}

type queueItem struct {
	Name string `xml:"Name"`
}

func (h *Handler) listQueues(w http.ResponseWriter, a *Account, prefix string) string {
	out := queueList{Endpoint: "https://" + a.Name + ".queue/", Prefix: prefix}
	for _, q := range a.Queues() {
		if strings.HasPrefix(q.Name, prefix) {
			out.Queues = append(out.Queues, queueItem{Name: q.Name})
		}
	}
	writeXML(w, 200, out)
	return ""
}

func (h *Handler) queueOp(w http.ResponseWriter, r *http.Request, a *Account, name, comp string) string {
	switch r.Method {
	case http.MethodPut:
		_, created := a.CreateQueue(name, metadataFrom(r.Header))
		setCommonHeaders(w)
		if created {
			w.WriteHeader(201)
		} else {
			// Creating a queue that already exists is a no-op, not an error.
			w.WriteHeader(204)
		}
		return ""

	case http.MethodDelete:
		if !a.DeleteQueue(name) {
			WriteError(w, 404, "QueueNotFound", "The specified queue does not exist.")
			return ""
		}
		setCommonHeaders(w)
		w.WriteHeader(204)
		return ""

	case http.MethodGet, http.MethodHead:
		q := a.Queue(name)
		if q == nil {
			WriteError(w, 404, "QueueNotFound", "The specified queue does not exist.")
			return ""
		}
		for k, v := range q.Metadata {
			w.Header().Set("x-ms-meta-"+k, v)
		}
		w.Header().Set("x-ms-approximate-messages-count", strconv.Itoa(q.Len()))
		setCommonHeaders(w)
		w.WriteHeader(200)
		return ""
	}
	WriteError(w, 400, "InvalidUri", "Unsupported queue operation.")
	return "queue: unsupported op " + r.Method + " comp=" + comp
}

// QueueMessagesList is the wire shape for both put and get responses.
type queueMessagesList struct {
	XMLName  xml.Name           `xml:"QueueMessagesList"`
	Messages []queueMessageItem `xml:"QueueMessage"`
}

type queueMessageItem struct {
	MessageID       string `xml:"MessageId"`
	InsertionTime   string `xml:"InsertionTime"`
	ExpirationTime  string `xml:"ExpirationTime"`
	PopReceipt      string `xml:"PopReceipt,omitempty"`
	TimeNextVisible string `xml:"TimeNextVisible,omitempty"`
	DequeueCount    int    `xml:"DequeueCount,omitempty"`
	MessageText     string `xml:"MessageText,omitempty"`
}

type putMessageRequest struct {
	XMLName     xml.Name `xml:"QueueMessage"`
	MessageText string   `xml:"MessageText"`
}

func (h *Handler) messageOp(w http.ResponseWriter, r *http.Request, a *Account, queueName, rest string) string {
	q := a.Queue(queueName)
	if q == nil {
		WriteError(w, 404, "QueueNotFound", "The specified queue does not exist.")
		return ""
	}
	// rest is "messages" or "messages/{messageid}".
	head, messageID := splitBlobPath(rest)
	if !strings.EqualFold(head, "messages") {
		WriteError(w, 400, "InvalidUri", "The request URI is not valid.")
		return "queue: unexpected path segment " + head
	}
	query := r.URL.Query()

	switch r.Method {
	case http.MethodPost:
		var req putMessageRequest
		if err := xml.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			WriteError(w, 400, "InvalidXmlDocument", "The message could not be parsed.")
			return ""
		}
		ttl := durationParam(query, "messagettl", 7*24*time.Hour)
		delay := durationParam(query, "visibilitytimeout", 0)
		m := q.Put(req.MessageText, ttl, delay)
		writeXML(w, 201, queueMessagesList{Messages: []queueMessageItem{{
			MessageID:       m.ID,
			InsertionTime:   httpTime(m.Inserted),
			ExpirationTime:  httpTime(m.Expires),
			PopReceipt:      m.PopReceipt,
			TimeNextVisible: httpTime(m.Visible),
		}}})
		return ""

	case http.MethodGet:
		n := 1
		if v, err := strconv.Atoi(query.Get("numofmessages")); err == nil && v > 0 {
			n = v
		}
		peek := strings.EqualFold(query.Get("peekonly"), "true")
		vis := durationParam(query, "visibilitytimeout", 30*time.Second)
		out := queueMessagesList{}
		for _, m := range q.Get(n, vis, peek) {
			item := queueMessageItem{
				MessageID:      m.ID,
				InsertionTime:  httpTime(m.Inserted),
				ExpirationTime: httpTime(m.Expires),
				DequeueCount:   m.DequeueCount,
				MessageText:    m.Text,
			}
			if !peek {
				item.PopReceipt = m.PopReceipt
				item.TimeNextVisible = httpTime(m.Visible)
			}
			out.Messages = append(out.Messages, item)
		}
		writeXML(w, 200, out)
		return ""

	case http.MethodDelete:
		if messageID == "" {
			q.Clear()
			setCommonHeaders(w)
			w.WriteHeader(204)
			return ""
		}
		if !q.Delete(messageID, query.Get("popreceipt")) {
			WriteError(w, 404, "MessageNotFound",
				"The specified message does not exist, or the pop receipt is from an earlier dequeue.")
			return ""
		}
		setCommonHeaders(w)
		w.WriteHeader(204)
		return ""
	}
	WriteError(w, 400, "InvalidUri", "Unsupported message operation.")
	return "queue: unsupported message op " + r.Method
}

func durationParam(q map[string][]string, name string, def time.Duration) time.Duration {
	v := ""
	if vals := q[name]; len(vals) > 0 {
		v = vals[0]
	}
	if v == "" {
		return def
	}
	secs, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return time.Duration(secs) * time.Second
}
