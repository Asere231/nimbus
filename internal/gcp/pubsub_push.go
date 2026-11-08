package gcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/Asere231/nimbus/internal/core"
)

// Push delivery: the first thing Nimbus does OUTBOUND.
//
// Everything in this project so far has answered requests. A push
// subscription inverts that: Nimbus becomes an HTTP client, posting to a
// URL that came out of a Terraform config. That is worth being careful
// about, so three things are deliberate here.
//
//  1. **The endpoint must be local.** A push URL is configuration, and a
//     local emulator has no business reaching the internet on its own --
//     it is the same line "never call real cloud endpoints" draws. A
//     non-local endpoint is recorded and reported in the activity feed
//     instead of being called.
//  2. **Retries are bounded**, with a short timeout. Real Pub/Sub retries
//     for days; a local apply that hangs on a dead endpoint teaches
//     nothing, so this gives up and says so.
//  3. **A delivery is an event on the bus**, so the feed shows the whole
//     chain: the publish, the POST to the service, and the service's own
//     request. That is the thing a visualizer can show and a log cannot.
//
// The envelope is Pub/Sub's own, which is what any handler written for
// real Pub/Sub expects:
//
//	{"message": {"data": "<base64>", "messageId": "...",
//	             "publishTime": "...", "attributes": {...}},
//	 "subscription": "projects/p/subscriptions/s"}
//
// An `oidcToken` in the push config is recorded and never minted or
// verified, like every other credential here.

const (
	pushTimeout  = 10 * time.Second
	pushAttempts = 3
)

// deliverPush posts one message to a push endpoint, retrying a few times.
func (s *Server) deliverPush(subscription, endpoint string, msg message) {
	if err := localEndpoint(endpoint); err != nil {
		s.notePush(subscription, endpoint, 0, err)
		return
	}
	envelope, err := json.Marshal(map[string]any{
		"message":      msg,
		"subscription": subscription,
	})
	if err != nil {
		s.notePush(subscription, endpoint, 0, err)
		return
	}

	client := &http.Client{Timeout: pushTimeout}
	var lastErr error
	for attempt := 1; attempt <= pushAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(envelope))
		if err != nil {
			cancel()
			s.notePush(subscription, endpoint, 0, err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		// Real Pub/Sub identifies itself, and a handler may log it.
		req.Header.Set("User-Agent", "APIs-Google; (+https://developers.google.com/webmasters/APIs-Google.html)")
		resp, err := client.Do(req)
		cancel()
		if err != nil {
			lastErr = err
		} else {
			status := resp.StatusCode
			resp.Body.Close()
			// 2xx means delivered and acknowledged, which for a push
			// subscription is the same thing.
			if status >= 200 && status < 300 {
				s.notePush(subscription, endpoint, status, nil)
				return
			}
			lastErr = fmt.Errorf("the endpoint answered %d", status)
		}
		if attempt < pushAttempts {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}
	s.notePush(subscription, endpoint, 0, lastErr)
}

// notePush records a delivery on the event bus so the activity feed shows
// it beside the requests that caused it.
func (s *Server) notePush(subscription, endpoint string, status int, err error) {
	note := "push to " + endpoint
	if err != nil {
		note = "push to " + endpoint + " failed: " + err.Error()
		status = 0
	}
	s.bus.Publish(core.Event{
		Kind:   "request",
		Action: "push",
		Cloud:  "gcp",
		Method: "POST",
		Path:   subscription,
		Status: status,
		Note:   note,
	})
}

// localEndpoint refuses a push URL that is not on this machine.
//
// A hostname is resolved before being trusted, because a name that looks
// local is not the same as a name that IS local -- and a config is not a
// thing to take on faith. Docker's own host alias counts as local: a
// handler running in a container pushing back to the host is exactly the
// case this has to allow.
func localEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("the push endpoint %q is not a URL: %w", endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("the push endpoint %q is not HTTP", endpoint)
	}
	host := u.Hostname()
	if host == "host.docker.internal" {
		return nil
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("the push endpoint %q does not resolve: %w", endpoint, err)
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("Nimbus only pushes to endpoints on this machine, and %q "+
				"resolves to %s. The subscription is recorded; nothing was sent", endpoint, a)
		}
	}
	return nil
}
