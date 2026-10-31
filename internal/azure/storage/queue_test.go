package storage

import (
	"testing"
	"time"
)

// A queue is not a list: a Get hides messages rather than removing them, and
// they come back if nobody deletes them. That redelivery is the whole point,
// so it is tested rather than assumed.
func TestQueueVisibilityAndRedelivery(t *testing.T) {
	a := NewStore().CreateAccount("st")
	q, created := a.CreateQueue("work", nil)
	if !created {
		t.Fatal("queue should have been created")
	}

	q.Put("one", time.Hour, 0)
	q.Put("two", time.Hour, 0)
	if n := q.Len(); n != 2 {
		t.Fatalf("queue length = %d, want 2", n)
	}

	// A get hides what it hands out.
	got := q.Get(10, time.Minute, false)
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2", len(got))
	}
	if again := q.Get(10, time.Minute, false); len(again) != 0 {
		t.Errorf("messages still visible after being dequeued: %d", len(again))
	}
	// But they are still ON the queue until deleted.
	if n := q.Len(); n != 2 {
		t.Errorf("hidden messages should still count: %d", n)
	}

	// Deleting with the right receipt removes one.
	if !q.Delete(got[0].ID, got[0].PopReceipt) {
		t.Error("delete with the correct pop receipt failed")
	}
	if n := q.Len(); n != 1 {
		t.Errorf("after delete, length = %d, want 1", n)
	}

	// A stale receipt must NOT delete: the message may have been redelivered
	// to another consumer in the meantime.
	stale := got[1].PopReceipt
	q.Get(0, 0, false)                                  // no-op, keeps state
	got[1].Visible = time.Now().UTC().Add(-time.Second) // make it visible again
	redelivered := q.Get(10, time.Minute, false)
	if len(redelivered) != 1 {
		t.Fatalf("message was not redelivered after its visibility lapsed: %d", len(redelivered))
	}
	if redelivered[0].DequeueCount != 2 {
		t.Errorf("DequeueCount = %d, want 2", redelivered[0].DequeueCount)
	}
	if q.Delete(got[1].ID, stale) {
		t.Error("a stale pop receipt deleted a redelivered message")
	}
	if !q.Delete(redelivered[0].ID, redelivered[0].PopReceipt) {
		t.Error("the current pop receipt should delete")
	}
	if n := q.Len(); n != 0 {
		t.Errorf("queue should be empty, has %d", n)
	}
}

func TestQueuePeekDoesNotHideOrCount(t *testing.T) {
	a := NewStore().CreateAccount("st")
	q, _ := a.CreateQueue("q", nil)
	q.Put("hello", time.Hour, 0)

	peeked := q.Get(10, time.Minute, true)
	if len(peeked) != 1 {
		t.Fatalf("peek returned %d", len(peeked))
	}
	if peeked[0].DequeueCount != 0 {
		t.Errorf("a peek must not count as a dequeue: %d", peeked[0].DequeueCount)
	}
	if got := q.Get(10, time.Minute, false); len(got) != 1 {
		t.Error("a peek must leave the message visible to a real get")
	}
}

func TestQueueMessagesExpire(t *testing.T) {
	a := NewStore().CreateAccount("st")
	q, _ := a.CreateQueue("q", nil)
	q.Put("gone", -time.Second, 0) // already expired
	q.Put("stays", time.Hour, 0)
	if n := q.Len(); n != 1 {
		t.Errorf("expired message was not dropped: length %d", n)
	}
}

func TestQueueDelayedVisibility(t *testing.T) {
	a := NewStore().CreateAccount("st")
	q, _ := a.CreateQueue("q", nil)
	q.Put("later", time.Hour, time.Minute) // not visible yet
	if got := q.Get(10, time.Minute, false); len(got) != 0 {
		t.Error("a message with a visibility delay must not be returned yet")
	}
	if n := q.Len(); n != 1 {
		t.Errorf("the message should still be queued: %d", n)
	}
}

func TestCreateQueueTwiceIsNotAnError(t *testing.T) {
	a := NewStore().CreateAccount("st")
	if _, created := a.CreateQueue("q", nil); !created {
		t.Error("first create should report created")
	}
	if _, created := a.CreateQueue("q", nil); created {
		t.Error("second create should report not-created (Azure answers 204)")
	}
	if !a.DeleteQueue("q") {
		t.Error("delete should succeed")
	}
	if a.DeleteQueue("q") {
		t.Error("deleting a missing queue should report false")
	}
}
