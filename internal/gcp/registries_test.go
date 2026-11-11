package gcp

import "testing"

// The invariant five races have now taught, asserted directly instead of
// waited for.
//
// Every one of them was the same shape: a registry of operations hands out
// its own pointer, a background goroutine finishes the operation, and a
// reader serialises the struct while it is being written. The race
// detector reports it from inside encoding/json, naming neither end, and
// plain `go test` never sees it. Four were fixed by copying on the way
// OUT; the fifth was `lros.put` keeping the caller's pointer on the way
// IN.
//
// A timing test would only catch this when the scheduler cooperated --
// exactly the flake that let it through CI twice. These assert the
// property: whatever a registry hands back shares nothing with what it
// holds. Deterministic, and it fails the moment the invariant breaks.

func TestAnOperationRegistrySharesNothingWithItsCallers(t *testing.T) {
	s := newTestServer(t)

	op := s.newLRO("us-central1", "type.googleapis.com/test", false)

	// The real check: finishing the operation must leave the caller's copy
	// alone. That is what makes it safe to serialise one after handing it
	// over, and it is the assertion that fails when put keeps the
	// caller's pointer -- verified by reverting the fix.
	//
	// (A pointer-identity check alone would NOT catch it, because get
	// already returns a copy: the two would differ either way. Both ends
	// have to be checked by what they do, not by what they are.)
	s.lros.finish(op.Name, "type.googleapis.com/test", map[string]any{"name": "done"})
	if op.Done {
		t.Error("finishing the operation mutated the caller's copy")
	}
	if stored := s.lros.get(op.Name); !stored.Done {
		t.Error("the stored operation was not finished")
	}

	// Two reads must not share either: both get serialised.
	first, second := s.lros.get(op.Name), s.lros.get(op.Name)
	if first == second {
		t.Error("two reads returned the same pointer")
	}
}

func TestAComputeOperationRegistrySharesNothingWithItsCallers(t *testing.T) {
	s := newTestServer(t)

	op := s.newOperation("global", "insert", "https://example.test/thing")
	if stored := s.operations.get(op.SelfLink); stored == op {
		t.Fatal("newOperation returned the registry's own pointer")
	}
	first, second := s.operations.get(op.SelfLink), s.operations.get(op.SelfLink)
	if first == second {
		t.Error("two reads returned the same pointer")
	}
}
