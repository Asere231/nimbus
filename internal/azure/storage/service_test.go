package storage

import "testing"

// ARM and the data plane share one listener and one port; the Host header is
// the only thing that tells them apart. A false positive here would swallow an
// ARM request, so the unhappy cases matter as much as the happy ones.
func TestParseHost(t *testing.T) {
	const suffix = "storage.nimbus.localhost:4701"

	for _, tc := range []struct {
		host    string
		want    Target
		wantOK  bool
		comment string
	}{
		{host: "st1.blob.storage.nimbus.localhost:4701", want: Target{"st1", Blob}, wantOK: true},
		{host: "st1.queue.storage.nimbus.localhost:4701", want: Target{"st1", Queue}, wantOK: true},
		{host: "st1.table.storage.nimbus.localhost:4701", want: Target{"st1", Table}, wantOK: true},
		{host: "st1.file.storage.nimbus.localhost:4701", want: Target{"st1", File}, wantOK: true},

		// The Host header may omit the port even when the suffix carries it,
		// and vice versa: both are compared without it.
		{host: "st1.blob.storage.nimbus.localhost", want: Target{"st1", Blob}, wantOK: true},

		// Casing is not significant in host names.
		{host: "ST1.Blob.Storage.Nimbus.LOCALHOST:4701", want: Target{"st1", Blob}, wantOK: true},

		// Everything below is an ARM request or nonsense and must fall through.
		{host: "localhost:4701", comment: "plain ARM host"},
		{host: "storage.nimbus.localhost:4701", comment: "the suffix itself, no account"},
		{host: "blob.storage.nimbus.localhost:4701", comment: "service but no account"},
		{host: "st1.disk.storage.nimbus.localhost:4701", comment: "not a storage service"},
		{host: "a.b.blob.storage.nimbus.localhost:4701", comment: "account cannot contain a dot"},
		{host: "st1.blob.storage.nimbus.localhost.evil.com", comment: "suffix must be at the end"},
		{host: "", comment: "empty"},
	} {
		got, ok := ParseHost(tc.host, suffix)
		if ok != tc.wantOK {
			t.Errorf("ParseHost(%q) ok = %v, want %v  (%s)", tc.host, ok, tc.wantOK, tc.comment)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("ParseHost(%q) = %+v, want %+v", tc.host, got, tc.want)
		}
	}
}

func TestAccountKeysAreStableAndDistinct(t *testing.T) {
	s := NewStore()
	a := s.CreateAccount("st1")
	if a.Keys[0] == a.Keys[1] {
		t.Error("the two account keys must differ")
	}
	if len(a.Keys[0]) < 80 {
		t.Errorf("key looks too short to be 64 base64-encoded bytes: %d chars", len(a.Keys[0]))
	}

	// Creating again must not rotate the keys: clients cache connection strings.
	if again := s.CreateAccount("st1"); again.Keys != a.Keys {
		t.Error("re-creating an account rotated its keys")
	}
	// Account names are matched case-insensitively.
	if s.Account("ST1") == nil {
		t.Error("account lookup must be case-insensitive")
	}

	before := a.Keys
	s.RegenerateKey("st1", 1)
	if a.Keys[0] != before[0] {
		t.Error("regenerating key2 changed key1")
	}
	if a.Keys[1] == before[1] {
		t.Error("regenerating key2 did not change it")
	}

	s.DeleteAccount("st1")
	if s.Account("st1") != nil {
		t.Error("account survived deletion")
	}
}
