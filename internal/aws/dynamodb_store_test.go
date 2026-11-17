package aws

import (
	"testing"
)

// Items survive a restart with their wire shape intact.
//
// The itemStore had no persistence test at all. That is the gap Milestone
// 19 exists to close: four of Milestone 18's six bugs were in the
// write-it-down-and-read-it-back path, and every one of them was found by
// hand because nothing exercised a reload.
//
// What this asserts is not merely "the item came back" but that it came
// back BYTE FOR BYTE, because two of DynamoDB's rules are only visible
// that way:
//
//   - A number travels as a decimal STRING, and can carry up to 38
//     significant digits. Anything that decodes an item and re-encodes it
//     through a float64 turns 1000000 into 1e+06 and loses a 30-digit
//     value entirely. Nimbus stores what arrived and returns it unchanged;
//     a round trip through disk has to keep that promise.
//   - An item's key is its TYPE as well as its value, so {"N":"5"} and
//     {"S":"5"} are different items. An encoding that drops the type
//     collides them, and a reload is exactly where such a thing would
//     surface.
func TestItemsSurviveARestartWithTheirWireShapeIntact(t *testing.T) {
	dir := t.TempDir()

	// A 30-digit number, which no float64 can hold.
	const big = "123456789012345678901234567890"

	before := newItemStore(dir)
	before.put("things", `{"N":"5"}`, map[string]any{
		"id":    map[string]any{"N": "5"},
		"count": map[string]any{"N": big},
		"note":  map[string]any{"S": "a number is a string here"},
	})
	before.put("things", `{"S":"5"}`, map[string]any{
		"id":   map[string]any{"S": "5"},
		"note": map[string]any{"S": "same characters, different key"},
	})
	before.flush("things")

	// A completely fresh store over the same directory: this is a restart.
	after := newItemStore(dir)
	after.load()

	n := after.get("things", `{"N":"5"}`)
	if n == nil {
		t.Fatal("the numeric-keyed item did not survive the restart")
	}
	num, _ := n["count"].(map[string]any)
	if got := num["N"]; got != big {
		t.Errorf("30-digit number came back as %v, want %s — something decoded it", got, big)
	}

	s := after.get("things", `{"S":"5"}`)
	if s == nil {
		t.Fatal("the string-keyed item did not survive the restart")
	}
	// The two keys are the same characters and different items. If a
	// reload collided them, one note would have overwritten the other.
	sn, _ := s["note"].(map[string]any)
	if got := sn["S"]; got != "same characters, different key" {
		t.Errorf("the string key collided with the numeric one: note = %v", got)
	}

	if got := len(after.all("things")); got != 2 {
		t.Errorf("table holds %d items after the restart, want 2", got)
	}
}
