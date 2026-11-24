package storage

import (
	"crypto/hmac"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// A Shared Access Signature is how Azure delegates storage access without
// handing over the account key. The client builds a string from the
// permissions, the validity window and the scope, signs it with the account
// key and sends the whole lot as query parameters:
//
//	?se=2030-01-01T00:00:00Z&sp=r&sv=2026-04-06&sr=b&sig={base64 signature}
//
// Nimbus verifies it for real, which it can because Nimbus ISSUED the account
// key. That is the half that makes a local SAS worth anything: it needs no
// principal and no OAuth, unlike Shared Key, which carries no identity at all.
//
// # Where the layouts came from
//
// The string-to-sign is unforgiving and NOT guessable, and the documentation
// runs out before the clients do: `az` 2.90.0 sends sv=2026-04-06, and
// Microsoft's create-service-sas page documents no layout newer than
// 2020-12-06. So the docs are not the spec here.
//
// What settled it is that a token from a real client, signed with a key we
// chose, is an ORACLE: the signature is a checkable answer, so a candidate
// layout can be confirmed offline by reproducing it byte for byte. All three
// layouts below were measured that way rather than read, and the tokens are
// kept as fixtures in TestSASStringToSign so the measurement is pinned instead
// of remembered. Two findings that only an oracle could give:
//
//   - sv=2026-04-06 adds NO fields over 2020-12-06. Candidates with 17 to 22
//     fields were all tried and all failed.
//   - the ACCOUNT layout ends in a trailing newline and the service layout
//     does not. The same account string without it signs differently, which is
//     a 403 explaining nothing.
//
// The version ladder below compares sv lexicographically, which is correct
// because a service version IS an ISO date -- and it means a version newer
// than anything known today takes the newest layout rather than being refused,
// which is what 2026-04-06 proved is the right guess.

// The service versions whose layouts are implemented. Older than the last is
// refused BY NAME rather than attempted: a wrong field count is a signature
// mismatch that names nothing, and guessing is how you get one.
const (
	sasVerEncryptionScope = "2020-12-06" // adds ses
	sasVerSignedResource  = "2018-11-09" // adds sr, sst
	sasVerOldest          = "2015-04-05" // adds sip, spr
)

// sasToken is the parsed query-string credentials of a SAS.
//
// Every field is signed, including the ones Nimbus does not act on -- a
// response-header override that is dropped from the string-to-sign is a
// mismatch, so they are parsed whether or not they are honoured.
type sasToken struct {
	sig        string // sig: the signature itself, not part of what is signed
	version    string // sv
	perms      string // sp
	start      string // st
	expiry     string // se
	ip         string // sip
	protocol   string // spr
	identifier string // si: a stored access policy, not implemented

	// Service SAS only.
	resource string // sr: "b" one blob, "c" one container
	snapshot string // sst
	encScope string // ses

	// Account SAS only.
	services string // ss: which services, e.g. "bqtf"
	resTypes string // srt: service / container / object, e.g. "sco"

	// Response-header overrides: signed, not acted on.
	rscc, rscd, rsce, rscl, rsct string

	// User delegation SAS, which is a different mechanism.
	keyObjectID, keyTenantID string // skoid, sktid
}

// sasKind says which of the two schemes a token is. They sign different
// strings of different lengths, so a single implementation that assumes one
// silently fails the other.
type sasKind int

const (
	sasService sasKind = iota
	sasAccount
)

// accessRefusal is a refusal carrying the Azure error code for it.
//
// There is more than one way for a credential to be wrong and clients match on
// the code, not the status: a bad signature is AuthenticationFailed, a
// signature that verifies over permissions not covering the request is
// AuthorizationPermissionMismatch, and an anonymous request to a private
// container is a 404 ResourceNotFound -- because Azure will not tell an
// unauthenticated caller that a container it cannot reach exists.
//
// Status is the HTTP status, and zero means 403, which is what nearly all of
// these are.
type accessRefusal struct {
	Code   string
	Msg    string
	Status int
}

func (e *accessRefusal) Error() string { return e.Code + ": " + e.Msg }

// status is the HTTP status to answer with, defaulting to 403.
func (e *accessRefusal) status() int {
	if e.Status == 0 {
		return http.StatusForbidden
	}
	return e.Status
}

func authFailed(msg string) *accessRefusal {
	return refuse("AuthenticationFailed", msg)
}

// refuse is a 403 with an Azure error code. Named fields and a constructor
// rather than positional literals, because adding Status to this struct broke
// every one of the seven positional literals that existed -- and the next
// field would do it again.
func refuse(code, msg string) *accessRefusal {
	return &accessRefusal{Code: code, Msg: msg}
}

// hasSAS reports whether a request carries SAS credentials at all.
//
// It reads the RAW query rather than the parsed values, which matters more than
// it looks. url.Values comes from URL.Query(), which DISCARDS its parse error
// and returns only the pairs it could decode -- so a signature containing a
// malformed percent-escape disappears entirely, the request presents as having
// no credentials, and it is judged as ANONYMOUS. Measured against a real
// token: corrupting the last character of "...sig=abc%3D" gives "%3X", and the
// read of a private blob came back 404 "the specified resource does not
// exist", which is true of nothing and says nothing about the real problem.
//
// Demoting a broken credential to no credential is the quiet kind of wrong
// this project keeps writing rules about. A request that MEANT to present a
// SAS is judged as one, and a signature that cannot be read is refused saying
// so.
func hasSAS(r *http.Request) bool {
	for _, pair := range strings.Split(r.URL.RawQuery, "&") {
		if name, _, _ := strings.Cut(pair, "="); name == "sig" {
			return true
		}
	}
	return false
}

// parseSAS reads a SAS out of the query string.
//
// It refuses, by name, every token whose shape Nimbus does not implement
// rather than attempting one and reporting a signature mismatch. The
// FilterExpression rule: a refusal that names the construct is debuggable and
// a wrong answer is not.
func parseSAS(q url.Values) (*sasToken, sasKind, *accessRefusal) {
	t := &sasToken{
		sig: q.Get("sig"), version: q.Get("sv"), perms: q.Get("sp"),
		start: q.Get("st"), expiry: q.Get("se"),
		ip: q.Get("sip"), protocol: q.Get("spr"), identifier: q.Get("si"),
		resource: q.Get("sr"), snapshot: q.Get("sst"), encScope: q.Get("ses"),
		services: q.Get("ss"), resTypes: q.Get("srt"),
		rscc: q.Get("rscc"), rscd: q.Get("rscd"), rsce: q.Get("rsce"),
		rscl: q.Get("rscl"), rsct: q.Get("rsct"),
		keyObjectID: q.Get("skoid"), keyTenantID: q.Get("sktid"),
	}

	// A user delegation SAS is signed with a key obtained from OAuth, not with
	// the account key, so there is nothing here to verify it against. Said
	// plainly rather than reported as a bad signature.
	if t.keyObjectID != "" || t.keyTenantID != "" {
		return nil, 0, authFailed("This is a user delegation SAS (skoid/sktid), which Nimbus does not implement. " +
			"Use a service or account SAS signed with an account key.")
	}
	// An empty sig here means the parameter was PRESENT -- hasSAS said so from
	// the raw query -- and could not be decoded. Almost always a mangled
	// percent-escape from a token that was edited or re-quoted somewhere.
	if t.sig == "" {
		return nil, 0, authFailed("The SAS signature (sig) could not be read. " +
			"It is present but did not decode, which usually means a malformed percent-escape " +
			"-- check that the token was not truncated or re-quoted.")
	}
	if t.version == "" {
		return nil, 0, authFailed("SAS token is missing the service version (sv).")
	}
	if t.version < sasVerOldest {
		return nil, 0, authFailed("SAS service version " + strconv.Quote(t.version) +
			" is older than " + sasVerOldest + ", which Nimbus does not implement.")
	}

	// A stored access policy supplies the permissions and window server-side,
	// so a token naming one signs different fields and cannot be checked
	// against what is in the query string.
	if t.identifier != "" {
		return nil, 0, authFailed("This SAS names a stored access policy (si=" +
			strconv.Quote(t.identifier) + "), which Nimbus does not implement.")
	}

	// sr marks a service SAS, ss/srt an account SAS. Neither or both is not a
	// token any client produces.
	hasAcct := t.services != "" || t.resTypes != ""
	switch {
	case t.resource != "" && hasAcct:
		return nil, 0, authFailed("SAS token carries both a signed resource (sr) and account fields (ss/srt).")
	case t.resource != "":
		switch t.resource {
		case "b", "c":
		default:
			return nil, 0, authFailed("SAS signed resource sr=" + strconv.Quote(t.resource) +
				" is not implemented; Nimbus supports sr=b (one blob) and sr=c (one container).")
		}
		return t, sasService, nil
	case hasAcct:
		if t.services == "" || t.resTypes == "" {
			return nil, 0, authFailed("An account SAS needs both signed services (ss) and resource types (srt).")
		}
		return t, sasAccount, nil
	}
	return nil, 0, authFailed("SAS token has neither a signed resource (sr) nor account fields (ss/srt).")
}

// stringToSign builds the exact string the client signed.
//
// The field ORDER and the exact field COUNT are the whole contract: an unset
// optional field is an empty string that still takes its newline, so a dropped
// field shifts everything after it and produces a mismatch that names nothing.
// Each layout's count is asserted by a test for that reason.
//
// It returns the layout's name alongside the string so a refusal can say which
// one it chose -- with three layouts in play, "the signature did not match" is
// only half an answer.
func (t *sasToken) stringToSign(kind sasKind, account string, svc Service, container, blob string) (sts, layout string) {
	if kind == sasAccount {
		// account, sp, ss, srt, st, se, sip, spr, sv [, ses] and then a
		// TRAILING newline -- measured, and load-bearing.
		fields := []string{
			strings.ToLower(account),
			t.perms, t.services, t.resTypes,
			t.start, t.expiry, t.ip, t.protocol, t.version,
		}
		layout = "account/" + sasVerOldest
		if t.version >= sasVerEncryptionScope {
			fields = append(fields, t.encScope)
			layout = "account/" + sasVerEncryptionScope
		}
		return strings.Join(fields, "\n") + "\n", layout
	}

	// A service SAS's resource is the path the SIGNATURE is scoped to, which
	// is not the same thing as the path the request addresses. sr says which:
	// sr=c signs the CONTAINER and is then used on the blobs inside it, so a
	// container SAS reading a blob still signs the container. Appending the
	// addressed blob regardless would make every such request fail with a
	// signature mismatch -- and the mismatch would name the right string for
	// the wrong scope, which is the least debuggable combination there is.
	//
	// The service name goes in front and there is no trailing slash. It is the
	// URL-DECODED path, which is what r.URL.Path already holds.
	res := "/" + serviceLabel(svc) + "/" + account + "/" + container
	if t.resource == "b" && blob != "" {
		res += "/" + blob
	}

	// sp, st, se, resource, si, sip, spr, sv [, sr, sst [, ses]] then the five
	// response-header overrides. No trailing newline.
	fields := []string{t.perms, t.start, t.expiry, res, t.identifier, t.ip, t.protocol, t.version}
	layout = "service/" + sasVerOldest
	if t.version >= sasVerSignedResource {
		fields = append(fields, t.resource, t.snapshot)
		layout = "service/" + sasVerSignedResource
	}
	if t.version >= sasVerEncryptionScope {
		fields = append(fields, t.encScope)
		layout = "service/" + sasVerEncryptionScope
	}
	fields = append(fields, t.rscc, t.rscd, t.rsce, t.rscl, t.rsct)
	return strings.Join(fields, "\n"), layout
}

// serviceLabel is the service's name as it appears in a signed resource.
func serviceLabel(svc Service) string {
	switch svc {
	case ServiceQueue:
		return "queue"
	case ServiceTable:
		return "table"
	case ServiceFile:
		return "file"
	default:
		return "blob"
	}
}

// verifySignature checks the token's sig against both account keys.
//
// Both, because an account has two and either may be the one that signed --
// the same reason the Shared Key path tries both. hmac.Equal rather than ==
// for the usual reason, even though nothing here is a secret worth timing.
func (t *sasToken) verifySignature(a *Account, kind sasKind, svc Service, container, blob string) *accessRefusal {
	sts, layout := t.stringToSign(kind, a.Name, svc, container, blob)
	for _, k := range a.Keys {
		if want, err := Sign(k, sts); err == nil && hmac.Equal([]byte(want), []byte(t.sig)) {
			return nil
		}
	}
	// The string-to-sign is quoted because it is the single most useful thing
	// in this error, exactly as it is for Shared Key. There is no secret in
	// it: it carries the permissions, the window and the scope, never the key.
	return authFailed("SAS signature did not match. Layout " + layout +
		", string-to-sign used was: " + strconv.Quote(sts))
}

// sasServiceOf reports whether a service SAS is implemented for this service.
//
// Queue and table service SAS have documented layouts of their own, and no
// client here has ever been measured against them -- so they are refused by
// name rather than attempted. An unproven layout is a claim, and this project
// has learned what an untested claim is worth. An ACCOUNT SAS covers every
// service, because its layout is service-independent and ss gates them.
func sasServiceSupported(svc Service) bool { return svc == ServiceBlob }

// clientIP is the caller's address without its port, for sip.
func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i+1:], "]") {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}
