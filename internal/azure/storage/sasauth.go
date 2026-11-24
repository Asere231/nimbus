package storage

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// What a SAS DECIDES, once its signature has been verified.
//
// A SAS is the one Azure storage credential that carries its own
// authorization: sp says what may be done, se until when, sr/ss/srt to what.
// Shared Key carries no identity at all and a bearer token here is unsigned,
// so this is the only scheme where "would these permissions work" is a
// question the emulator can answer -- which is the whole reason to have one
// locally, and it is what was being waved through.
//
// Enforcement is unconditional rather than behind --enforce-iam, unlike the
// three identity-policy languages. Three reasons: a SAS needs no principal, so
// there is no "unmapped caller is root" fallback to fall into; real Azure has
// no mode in which a SAS does not decide; and nothing in this repository
// generates a SAS, so the blast radius was measured at zero rather than
// hoped for.

// sasOp is what a request is asking to do, as a SAS sees it.
type sasOp struct {
	name  string // named in a refusal, so the message says what was attempted
	perms string // any ONE of these letters permits it
	level byte   // 's' service, 'c' container, 'o' object -- the srt required
}

// blobSASOperation classifies a blob data-plane request.
//
// It mirrors serveBlob, containerOp and blobOp, and it lives here BESIDE the
// routing it mirrors for the reason the AWS authorization work wrote down: a
// classifier that disagrees with its router authorizes one thing and serves
// another. When one of those switches gains a case, this one has to gain it
// too, and the test that pins them together is what says so.
//
// An operation it cannot place is REFUSED by the caller, never waved through.
// Treating an unclassified request as permitted turns every gap here into a
// permission nobody granted -- the FilterExpression rule, which this project
// has now paid for in four subsystems.
func blobSASOperation(r *http.Request, container, blob string) (sasOp, bool) {
	q := r.URL.Query()
	restype, comp := q.Get("restype"), q.Get("comp")
	get := r.Method == http.MethodGet || r.Method == http.MethodHead

	switch {
	// --- account level ---
	case container == "" && restype == "service":
		if get {
			return sasOp{"get service properties", "r", 's'}, true
		}
		return sasOp{"set service properties", "w", 's'}, true
	case container == "" && comp == "list":
		return sasOp{"list containers", "l", 's'}, true
	case container == "":
		return sasOp{}, false

	// --- container level ---
	case blob == "" && restype == "container":
		switch {
		case r.Method == http.MethodPut && comp == "":
			return sasOp{"create container", "c", 'c'}, true
		case r.Method == http.MethodPut && (comp == "metadata" || comp == "acl"):
			return sasOp{"set container " + comp, "w", 'c'}, true
		case r.Method == http.MethodDelete:
			return sasOp{"delete container", "d", 'c'}, true
		case get && comp == "list":
			return sasOp{"list blobs", "l", 'c'}, true
		case get && (comp == "" || comp == "metadata" || comp == "acl"):
			return sasOp{"read container properties", "r", 'c'}, true
		}
		return sasOp{}, false
	case blob == "":
		return sasOp{}, false

	// --- blob level ---
	default:
		switch {
		case r.Method == http.MethodPut && (comp == "block" || comp == "blocklist"):
			// Put Block and Put Block List are the multi-part upload path, and
			// real Azure accepts add or create for them as well as write.
			return sasOp{"put " + comp, "wac", 'o'}, true
		case r.Method == http.MethodPut && (comp == "metadata" || comp == "properties"):
			return sasOp{"set blob " + comp, "w", 'o'}, true
		case r.Method == http.MethodPut && comp == "":
			// Put Blob creates or overwrites, so either letter permits it.
			return sasOp{"put blob", "wc", 'o'}, true
		case r.Method == http.MethodDelete:
			return sasOp{"delete blob", "d", 'o'}, true
		case get:
			return sasOp{"read blob", "r", 'o'}, true
		}
		return sasOp{}, false
	}
}

// authorizeSAS decides a request that arrived with a SAS.
//
// The order is deliberate. The token's SHAPE is settled first, so an
// unimplemented kind is named as such rather than reported as a bad signature.
// Scope comes before the signature because sr decides what the signature is
// computed over: a sr=b token used on a container would otherwise fail with a
// mismatch whose string-to-sign looks right. Then the signature, then the
// window, then the request-bound conditions, then the permission itself --
// which is last because it is the only one whose message is about what the
// caller asked for rather than about the token.
func authorizeSAS(r *http.Request, a *Account, svc Service) *accessRefusal {
	tok, kind, ref := parseSAS(r.URL.Query())
	if ref != nil {
		return ref
	}

	// Only the blob service's SAS has been measured against a real client.
	// Queue, table and file have documented layouts and operation sets that
	// nothing here has ever exercised, and an unproven layout is a claim this
	// project has learned not to make. Refused by name on both kinds, because
	// an account SAS's ss would otherwise appear to cover a service whose
	// operations nothing can classify.
	if !sasServiceSupported(svc) {
		return authFailed("Nimbus enforces a SAS for the blob service only; this request is for " +
			serviceLabel(svc) + ". Use Shared Key, or an account key, for that service.")
	}

	container, blob := splitBlobPath(r.URL.Path)
	op, known := blobSASOperation(r, container, blob)
	if !known {
		// A gap in the classifier, stated as one. It names the request so the
		// missing case can be added, rather than granting it by silence.
		return authFailed("Nimbus cannot classify this operation for a SAS, so it is refused: " +
			r.Method + " " + strconv.Quote(r.URL.Path) + " " + strconv.Quote(r.URL.RawQuery) +
			". A request a SAS cannot be reasoned about must not be permitted by default.")
	}

	if ref := tok.checkScope(kind, svc, op); ref != nil {
		return ref
	}
	if ref := tok.verifySignature(a, kind, svc, container, blob); ref != nil {
		return ref
	}
	if ref := tok.checkWindow(time.Now().UTC()); ref != nil {
		return ref
	}
	if ref := tok.checkRequestBound(r); ref != nil {
		return ref
	}
	return tok.checkPermission(op)
}

// checkScope is what the token is scoped TO, against what the request touches.
func (t *sasToken) checkScope(kind sasKind, svc Service, op sasOp) *accessRefusal {
	if kind == sasAccount {
		// ss is the services the token covers; the letter is the service's own.
		letter := serviceLabel(svc)[:1]
		if !strings.Contains(t.services, letter) {
			return refuse("AuthorizationServiceMismatch",
				"This account SAS covers services "+strconv.Quote(t.services)+
					", which does not include "+serviceLabel(svc)+" ("+letter+").")
		}
		if !strings.ContainsRune(t.resTypes, rune(op.level)) {
			return refuse("AuthorizationResourceTypeMismatch",
				"This account SAS covers resource types "+strconv.Quote(t.resTypes)+
					", and "+op.name+" is a "+sasLevelName(op.level)+
					"-level operation (srt="+string(op.level)+").")
		}
		return nil
	}

	// A service SAS's scope is its signed resource. sr=b is one blob, so it
	// cannot reach a container-level operation; sr=c is a container, so it
	// reaches the container and the blobs inside it. Checked here rather than
	// left to the signature, which would also refuse it but would blame the
	// signature for what is really a scope error.
	switch t.resource {
	case "b":
		if op.level != 'o' {
			return refuse("AuthorizationResourceTypeMismatch",
				"This SAS is scoped to a single blob (sr=b) and "+op.name+
					" is a "+sasLevelName(op.level)+"-level operation.")
		}
	case "c":
		if op.level == 's' {
			return refuse("AuthorizationResourceTypeMismatch",
				"This SAS is scoped to one container (sr=c) and "+op.name+
					" is an account-level operation.")
		}
	}
	return nil
}

func sasLevelName(level byte) string {
	switch level {
	case 's':
		return "service"
	case 'c':
		return "container"
	default:
		return "object"
	}
}

// checkWindow enforces se, and st when present.
//
// The expiry is QUOTED in the refusal. "403" on its own sends people to look
// at their permissions, which is the one thing that is not wrong -- and a
// clock that is a few minutes out is otherwise invisible.
func (t *sasToken) checkWindow(now time.Time) *accessRefusal {
	if t.expiry == "" {
		return authFailed("SAS token has no expiry (se), which is required.")
	}
	exp, err := parseSASTime(t.expiry)
	if err != nil {
		return authFailed("SAS expiry (se) " + strconv.Quote(t.expiry) + " is not a time Nimbus can read.")
	}
	if now.After(exp) {
		return authFailed("This SAS expired at " + exp.Format(time.RFC3339) +
			" and it is now " + now.Format(time.RFC3339) + ".")
	}
	if t.start != "" {
		st, err := parseSASTime(t.start)
		if err != nil {
			return authFailed("SAS start (st) " + strconv.Quote(t.start) + " is not a time Nimbus can read.")
		}
		if now.Before(st) {
			return authFailed("This SAS is not valid until " + st.Format(time.RFC3339) +
				" and it is now " + now.Format(time.RFC3339) + ".")
		}
	}
	return nil
}

// parseSASTime reads the ISO 8601 forms Azure accepts in st and se.
//
// Clients differ in how much precision they write -- az sends seconds, the
// SDKs sometimes send only a date -- and all of them are legal, so the layouts
// are tried in order rather than one being assumed.
func parseSASTime(s string) (time.Time, error) {
	layouts := []string{
		time.RFC3339Nano, time.RFC3339,
		"2006-01-02T15:04Z07:00", "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02",
	}
	var err error
	for _, l := range layouts {
		var ts time.Time
		if ts, err = time.Parse(l, s); err == nil {
			return ts.UTC(), nil
		}
	}
	return time.Time{}, err
}

// checkRequestBound enforces sip and spr.
//
// Both are honoured because both are answerable from the request in hand --
// the same test that took AWS's condition keys from four to ten. A field
// Nimbus could not answer would have to be refused instead, not ignored.
func (t *sasToken) checkRequestBound(r *http.Request) *accessRefusal {
	if t.protocol != "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		// spr is one protocol or the pair, in either order.
		allowed := false
		for _, p := range strings.Split(t.protocol, ",") {
			if strings.EqualFold(strings.TrimSpace(p), scheme) {
				allowed = true
			}
		}
		if !allowed {
			return refuse("AuthorizationProtocolMismatch",
				"This SAS allows protocol "+strconv.Quote(t.protocol)+
					" and the request arrived over "+scheme+".")
		}
	}
	if t.ip != "" {
		caller := net.ParseIP(clientIP(r))
		if caller == nil || !sasIPAllowed(t.ip, caller) {
			return refuse("AuthorizationSourceIPMismatch",
				"This SAS allows source IP "+strconv.Quote(t.ip)+
					" and the request came from "+clientIP(r)+".")
		}
	}
	return nil
}

// sasIPAllowed matches sip, which is a single address or an inclusive range
// written as "first-last".
func sasIPAllowed(sip string, caller net.IP) bool {
	first, last, isRange := strings.Cut(sip, "-")
	lo := net.ParseIP(strings.TrimSpace(first))
	if lo == nil {
		return false
	}
	if !isRange {
		return lo.Equal(caller)
	}
	hi := net.ParseIP(strings.TrimSpace(last))
	if hi == nil {
		return false
	}
	// Compare as 16-byte forms so a v4 address written either way still
	// orders correctly against the bounds.
	c, l, h := caller.To16(), lo.To16(), hi.To16()
	if c == nil || l == nil || h == nil {
		return false
	}
	return string(c) >= string(l) && string(c) <= string(h)
}

// checkPermission is the check the whole milestone exists for.
//
// The refusal names the letter it wanted and the operation it was for,
// because "403" tells you nothing about which of sp's letters was missing.
func (t *sasToken) checkPermission(op sasOp) *accessRefusal {
	for _, want := range op.perms {
		if strings.ContainsRune(t.perms, want) {
			return nil
		}
	}
	return refuse("AuthorizationPermissionMismatch",
		"This SAS grants sp="+strconv.Quote(t.perms)+" and "+op.name+
			" needs "+sasPermsPhrase(op.perms)+".")
}

func sasPermsPhrase(perms string) string {
	names := map[rune]string{
		'r': "r (read)", 'w': "w (write)", 'a': "a (add)",
		'c': "c (create)", 'd': "d (delete)", 'l': "l (list)",
	}
	var out []string
	for _, p := range perms {
		if n, ok := names[p]; ok {
			out = append(out, n)
		} else {
			out = append(out, string(p))
		}
	}
	if len(out) == 1 {
		return out[0]
	}
	return "one of " + strings.Join(out, ", ")
}
