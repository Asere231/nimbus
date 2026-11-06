package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestDeleteCascadesAndPublishesLeavesFirst(t *testing.T) {
	bus := NewBus(100)
	s, _ := NewStore(bus, "")
	rg := "/subscriptions/s/resourceGroups/rg"
	vnet := rg + "/providers/Microsoft.Network/virtualNetworks/v"
	sn := vnet + "/subnets/a"
	other := "/subscriptions/s/resourceGroups/rg2"
	for _, r := range []*Resource{
		{ID: rg, ParentID: "/subscriptions/s"},
		{ID: vnet, ParentID: rg},
		{ID: sn, ParentID: vnet},
		{ID: other, ParentID: "/subscriptions/s"},
	} {
		s.Put(r)
	}

	ch, _, cancel := bus.Subscribe()
	defer cancel()
	deleted, err := s.Delete("/SUBSCRIPTIONS/s/resourcegroups/RG")
	if err != nil {
		t.Fatal(err)
	}
	// Delete reports what it removed, deepest first, so callers can run
	// per-resource cleanup for descendants and not just the addressed one.
	if len(deleted) != 3 {
		t.Errorf("Delete reported %d removed resources, want 3", len(deleted))
	}
	for i := 1; i < len(deleted); i++ {
		if len(deleted[i-1].ID) < len(deleted[i].ID) {
			t.Errorf("Delete must report deepest first; got %s before %s",
				deleted[i-1].ID, deleted[i].ID)
		}
	}
	var order []string
	for len(order) < 3 {
		select {
		case e := <-ch:
			order = append(order, e.ResourceID)
		case <-time.After(time.Second):
			t.Fatalf("got %v", order)
		}
	}
	if order[0] != sn || order[2] != rg {
		t.Errorf("delete order = %v, want leaves first", order)
	}
	if !s.Exists(other) || s.Exists(vnet) {
		t.Error("cascade deleted the wrong things")
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	s1, _ := NewStore(NewBus(10), dir)
	s1.Put(&Resource{ID: "/subscriptions/s", Name: "s"})
	s2, err := NewStore(NewBus(10), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Exists("/subscriptions/s") {
		t.Error("state was not reloaded")
	}
}

// Update is the whole point: a read-modify-write that cannot lose a
// concurrent one. Get-then-Put re-reads and therefore looks safe, but two
// overlapping writers both read the same copy and the second to write
// restores what the first had changed.
//
// This is not a hypothetical. It is why scaling an AKS node pool lost the
// new count on a loaded CI runner, while passing fifty times in a row on a
// fast machine: the pool's AfterPut and the cluster's background Provision
// were writing to the same body.
func TestUpdateDoesNotLoseConcurrentWrites(t *testing.T) {
	bus := NewBus(1000)
	store, err := NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	store.Put(&Resource{Cloud: "azure", ID: "/r", Type: "T", Name: "r", Body: []byte(`{"properties":{}}`)})

	const writers = 50
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := store.Update("/r", func(body map[string]any) error {
				props, _ := body["properties"].(map[string]any)
				props[fmt.Sprintf("field%d", i)] = i
				return nil
			})
			if err != nil {
				t.Errorf("Update %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	res, err := store.Get("/r")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body, &body); err != nil {
		t.Fatal(err)
	}
	props, _ := body["properties"].(map[string]any)
	for i := 0; i < writers; i++ {
		if _, ok := props[fmt.Sprintf("field%d", i)]; !ok {
			t.Fatalf("field%d was lost: %d of %d writes survived", i, len(props), writers)
		}
	}
}

// Update leaves the resource alone when the function says so, and reports a
// missing resource rather than creating one.
func TestUpdateAbandonsAndReportsMissing(t *testing.T) {
	bus := NewBus(10)
	store, _ := NewStore(bus, "")
	store.Put(&Resource{Cloud: "azure", ID: "/r", Type: "T", Name: "r", Body: []byte(`{"keep":true}`)})

	sentinel := errors.New("no")
	if err := store.Update("/r", func(body map[string]any) error { body["keep"] = false; return sentinel }); err != sentinel {
		t.Errorf("Update returned %v, want the function's own error", err)
	}
	res, _ := store.Get("/r")
	if string(res.Body) != `{"keep":true}` {
		t.Errorf("body = %s, want it untouched after an abandoned update", res.Body)
	}

	if err := store.Update("/nope", func(map[string]any) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update of a missing resource = %v, want ErrNotFound", err)
	}
}

// An update publishes an update event, so the visualizer and the journal
// see it like any other change.
func TestUpdatePublishes(t *testing.T) {
	bus := NewBus(10)
	store, _ := NewStore(bus, "")
	store.Put(&Resource{Cloud: "azure", ID: "/r", Type: "T", Name: "r", Body: []byte(`{}`)})
	before := len(bus.History())
	if err := store.Update("/r", func(body map[string]any) error { body["x"] = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	events := bus.History()
	if len(events) != before+1 {
		t.Fatalf("published %d events, want 1", len(events)-before)
	}
	if e := events[len(events)-1]; e.Action != "update" || e.ResourceID != "/r" {
		t.Errorf("event = %+v", e)
	}
}
