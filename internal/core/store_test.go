package core

import (
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
