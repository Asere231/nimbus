package storage

import (
	"net/http"
	"strings"
	"testing"
)

// These vectors were produced by Microsoft's own azure-storage-blob Python SDK
// (12.31.1) signing real requests -- not by a second hand-written copy of the
// documented algorithm, which could easily repeat the same misreading. If our
// implementation agrees with these, it agrees with what every real client
// actually sends.
//
// Regenerate with scratch script vectors.py if the case list ever needs to grow.

const (
	vectorAccount = "stnimbus"
	vectorKey     = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8gISIjJCUmJygpKissLS4vMDEyMzQ1Njc4OTo7PD0+Pw=="
)

type sharedKeyVector struct {
	name    string
	method  string
	url     string
	headers map[string]string
	body    string
	want    string // the signature the SDK produced
}

var sharedKeyVectors = []sharedKeyVector{
	{
		name:    "get service properties",
		method:  "GET",
		url:     "https://stnimbus.blob.storage.nimbus.localhost:4701/?comp=properties&restype=service",
		headers: map[string]string{"x-ms-date": "Fri, 30 Oct 2026 21:00:00 GMT", "x-ms-version": "2025-05-05"},
		body:    "",
		want:    "nga9IQ1hVrtgNjfbnIjhzpO66Pb8kM8bbZY7wTIkSzU=",
	},
	{
		name:    "list containers",
		method:  "GET",
		url:     "https://stnimbus.blob.storage.nimbus.localhost:4701/?comp=list",
		headers: map[string]string{"x-ms-date": "Fri, 30 Oct 2026 21:00:00 GMT", "x-ms-version": "2025-05-05"},
		body:    "",
		want:    "vMcYkfgrJkCx0/tNr+h7lIBWrk2AuEPWNjeTwiusZ84=",
	},
	{
		name:    "create container",
		method:  "PUT",
		url:     "https://stnimbus.blob.storage.nimbus.localhost:4701/data?restype=container",
		headers: map[string]string{"Content-Length": "0", "x-ms-date": "Fri, 30 Oct 2026 21:00:00 GMT", "x-ms-version": "2025-05-05"},
		body:    "",
		want:    "1RwW2AexFATNwEcBLzssT9EQnZrg4NFoDBQfONgsDsc=",
	},
	{
		name:    "put blob with body and type",
		method:  "PUT",
		url:     "https://stnimbus.blob.storage.nimbus.localhost:4701/data/hello.txt",
		headers: map[string]string{"Content-Length": "12", "Content-Type": "text/plain", "x-ms-blob-type": "BlockBlob", "x-ms-date": "Fri, 30 Oct 2026 21:00:00 GMT", "x-ms-version": "2025-05-05"},
		body:    "hello nimbus",
		want:    "lwrbkuUQHnuH+DAeuAH4/gX7BTJQjW/sDu56GqKCQMM=",
	},
	{
		name:    "list blobs, several query params",
		method:  "GET",
		url:     "https://stnimbus.blob.storage.nimbus.localhost:4701/data?restype=container&comp=list&prefix=a/&delimiter=/&maxresults=10",
		headers: map[string]string{"x-ms-date": "Fri, 30 Oct 2026 21:00:00 GMT", "x-ms-version": "2025-05-05"},
		body:    "",
		want:    "IIZ44Zoo9xvY7bkh6DCodL1dC6YKJHu338aFELd6u9c=",
	},
	{
		name:    "metadata headers sort into the signature",
		method:  "PUT",
		url:     "https://stnimbus.blob.storage.nimbus.localhost:4701/data/m.txt?comp=metadata",
		headers: map[string]string{"Content-Length": "0", "x-ms-date": "Fri, 30 Oct 2026 21:00:00 GMT", "x-ms-meta-alpha": "first", "x-ms-meta-zebra": "last", "x-ms-version": "2025-05-05"},
		body:    "",
		want:    "lEG9oSbgmrilzsrsCRqzsFAAP76UWOJEBt4v4o6ql9Q=",
	},
}

func TestSharedKeySignatureMatchesTheRealSDK(t *testing.T) {
	for _, v := range sharedKeyVectors {
		t.Run(v.name, func(t *testing.T) {
			var body *strings.Reader
			if v.body != "" {
				body = strings.NewReader(v.body)
			} else {
				body = strings.NewReader("")
			}
			req, err := http.NewRequest(v.method, v.url, body)
			if err != nil {
				t.Fatal(err)
			}
			for k, val := range v.headers {
				req.Header.Set(k, val)
			}
			// net/http sets ContentLength from the reader; mirror what the
			// server sees for a request arriving off the wire.
			req.ContentLength = int64(len(v.body))

			got, err := Sign(vectorKey, StringToSign(req, vectorAccount, ServiceBlob, false))
			if err != nil {
				t.Fatal(err)
			}
			if got != v.want {
				t.Errorf("signature mismatch\n got %s\nwant %s\nstring-to-sign we built:\n%q",
					got, v.want, StringToSign(req, vectorAccount, ServiceBlob, false))
			}
		})
	}
}

// A request signed with the wrong key must fail, or an emulator teaches you
// that a bad connection string is fine.
func TestWrongKeyIsRejected(t *testing.T) {
	store := NewStore()
	a := store.CreateAccount(vectorAccount)
	a.Keys = [2]string{vectorKey, vectorKey}

	v := sharedKeyVectors[0]
	req, _ := http.NewRequest(v.method, v.url, strings.NewReader(""))
	for k, val := range v.headers {
		req.Header.Set(k, val)
	}

	req.Header.Set("Authorization", "SharedKey "+vectorAccount+":"+v.want)
	if res, msg := Authenticate(req, a, ServiceBlob); res != AuthSharedKey {
		t.Errorf("correct signature rejected: %s", msg)
	}

	// Same request, different key on the account.
	a.Keys = [2]string{newKey(), newKey()}
	if res, _ := Authenticate(req, a, ServiceBlob); res != AuthFailed {
		t.Error("a signature made with a key the account does not have was accepted")
	}

	// Malformed header.
	req.Header.Set("Authorization", "SharedKey nocolon")
	if res, _ := Authenticate(req, a, ServiceBlob); res != AuthFailed {
		t.Error("malformed Authorization header was accepted")
	}

	// Bearer and anonymous are recognised without a signature to check.
	req.Header.Set("Authorization", "Bearer abc.def.ghi")
	if res, _ := Authenticate(req, a, ServiceBlob); res != AuthBearer {
		t.Error("bearer token not recognised")
	}
	req.Header.Del("Authorization")
	if res, _ := Authenticate(req, a, ServiceBlob); res != AuthAnonymous {
		t.Error("unauthenticated request should be reported as anonymous")
	}
}

// The Table service signs differently from blob and queue: a shorter header
// list, x-ms-date instead of Date, no x-ms-* block at all, and a canonicalised
// resource that carries only the comp parameter. These vectors come from
// Microsoft's azure-data-tables SDK.
var tableVectors = []sharedKeyVector{
	{
		name:    "table service properties",
		method:  "GET",
		url:     "https://stnimbus.table.storage.nimbus.localhost:4701/?restype=service&comp=properties",
		headers: map[string]string{"x-ms-date": "Fri, 30 Oct 2026 21:00:00 GMT", "x-ms-version": "2025-05-05"},
		body:    "",
		want:    "3twwvthvHfgoUPPHJdKvWacwbxE9CZZVYQFJSUirUsg=",
	},
	{
		name:    "list tables",
		method:  "GET",
		url:     "https://stnimbus.table.storage.nimbus.localhost:4701/Tables",
		headers: map[string]string{"x-ms-date": "Fri, 30 Oct 2026 21:00:00 GMT", "x-ms-version": "2025-05-05"},
		body:    "",
		want:    "iz3AmbIpD+Ra8hZ3JsbntfYmIaYN0uvwxVmgT2NU7PE=",
	},
	{
		name:    "create table",
		method:  "POST",
		url:     "https://stnimbus.table.storage.nimbus.localhost:4701/Tables",
		headers: map[string]string{"Content-Length": "18", "Content-Type": "application/json", "x-ms-date": "Fri, 30 Oct 2026 21:00:00 GMT", "x-ms-version": "2025-05-05"},
		body:    "{\"TableName\":\"t1\"}",
		want:    "KAra5SshgmbJiz6ptWqu3QeYeCzXKpRg+BAvnQxGsTY=",
	},
	{
		name:    "query entities with filter",
		method:  "GET",
		url:     "https://stnimbus.table.storage.nimbus.localhost:4701/t1()?$filter=PartitionKey%20eq%20%27p%27",
		headers: map[string]string{"x-ms-date": "Fri, 30 Oct 2026 21:00:00 GMT", "x-ms-version": "2025-05-05"},
		body:    "",
		want:    "zzT8IV5x9AbRRm+7FXldG3h4lLHx3HUcy0b7Ikqng4s=",
	},
}

func TestTableSharedKeySignatureMatchesTheRealSDK(t *testing.T) {
	for _, v := range tableVectors {
		t.Run(v.name, func(t *testing.T) {
			req, err := http.NewRequest(v.method, v.url, strings.NewReader(v.body))
			if err != nil {
				t.Fatal(err)
			}
			for k, val := range v.headers {
				req.Header.Set(k, val)
			}
			req.ContentLength = int64(len(v.body))
			got, err := Sign(vectorKey, StringToSign(req, vectorAccount, ServiceTable, false))
			if err != nil {
				t.Fatal(err)
			}
			if got != v.want {
				t.Errorf("signature mismatch\n got %s\nwant %s\nstring-to-sign we built: %q",
					got, v.want, StringToSign(req, vectorAccount, ServiceTable, false))
			}
		})
	}
}
