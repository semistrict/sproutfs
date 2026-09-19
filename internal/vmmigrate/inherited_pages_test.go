package vmmigrate_test

import (
	"testing"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/host"
	"github.com/semistrict/sproutfs/internal/vmmigrate"
	"github.com/semistrict/sproutfs/internal/volume"
)

// fanOut seals one pause on the running parent and hands every child of it to
// the same destination, which is what an orchestrator's cross-host fork does
// with a count above one: one pause, one pin, one hold per child.
func (m *migration) fanOut(t *testing.T, children ...string) []vmmigrate.Handoff {
	t.Helper()
	point, err := host.Seal(t.Context(), m.vm, m.machine)
	if err != nil {
		t.Fatal(err)
	}
	// The fan-out's own hold keeps the point while its children are described.
	point.Hold()
	if err := point.Pin(t.Context()); err != nil {
		t.Fatal(err)
	}
	handoffs := make([]vmmigrate.Handoff, 0, len(children))
	for _, child := range children {
		point.Hold()
		handoff, err := vmmigrate.Fork(t.Context(), child, point, m.pages, vmmigrate.Options{})
		if err != nil {
			t.Fatal(err)
		}
		handoffs = append(handoffs, handoff)
	}
	if err := point.Retire(t.Context()); err != nil {
		t.Fatal(err)
	}
	return handoffs
}

// TestForkFanOutChildrenShareThePagesTheyInherit. Two children of one fork point
// on one host read the same parent checkpoint, so every page of it that neither
// has diverged from is one page between them. That sharing is the reason a
// fan-out puts children on one host at all: without it the host pays a page
// and a load per child per page of checkpoints they agree on completely.
//
// What has to hold for it is that the page keeps the parent's name. The child's
// root index is published over the parent's checkpoints rather than in place of them — it
// carries the pages the child actually pulled and nothing else — so a page
// neither child has written still names the object the parent put it in, and
// the pager keys one page by it for both of them.
//
// It is the pages the parent published that this is about. A page no checkpoint
// of the parent holds is fetched per child over the wire and is that child's
// own dirty state from the moment it lands, which two guests that may diverge
// from it the next moment have no business sharing.
func TestForkFanOutChildrenShareThePagesTheyInherit(t *testing.T) {
	m := newMigration(t)
	// The parent publishes what its pager holds, so what the children inherit
	// is in object storage rather than in this host's pages.
	if err := m.machine.checkpoint(t.Context(), m.vm); err != nil {
		t.Fatal(err)
	}
	inherited := pageIdentity(t, m.vm, sharedPage)
	// One page written since that checkpoint, so the point carries an
	// unpublished set as well as the published checkpoint under it.
	m.machine.write("ram0", 0)
	handoffs := m.fanOut(t, "vm-a", "vm-b")

	children := make([]*machine, 0, len(handoffs))
	for _, handoff := range handoffs {
		received, child := m.receive(t, handoff)
		if err := received.Done(t.Context()); err != nil {
			t.Fatal(err)
		}
		// The root index is what the destination publishes once the child holds
		// every page it inherited, and it is the one thing that could rename the
		// pages under it.
		if err := child.checkpoint(t.Context(), received.VM()); err != nil {
			t.Fatal(err)
		}
		if named := pageIdentity(t, received.VM(), sharedPage); named != inherited {
			t.Errorf("%s renamed a page it never wrote: %+v, want the parent's %+v",
				handoff.VMID, named, inherited)
		}
		received.Close()
		children = append(children, child)
	}
	if inherited.Zero || inherited.Ref.IsZero() {
		t.Fatalf("the page this compares is not one the parent published: %+v", inherited)
	}

	// And what the pager makes of that: the second child to read the page maps
	// the page the first one loaded, without a load of its own.
	before, err := m.destPager.host.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		if _, err := child.read(child.ctx(), "ram0", sharedPage); err != nil {
			t.Fatal(err)
		}
	}
	after, err := m.destPager.host.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	hits, loads := after.IdentityHits-before.IdentityHits, after.Loads-before.Loads
	if hits != 1 || loads != 1 {
		t.Errorf("two children reading one page they both inherited: identity_hits=%d loads=%d, want 1 and 1",
			hits, loads)
	}
}

// sharedPage is a page of the parent's RAM that its checkpoint published and
// that neither child writes, which is what they must agree on the name of.
const sharedPage = uint64(5)

// pageIdentity is the identity one VM's own volume gives one page of its RAM,
// which is what the pager keys a shared page by.
func pageIdentity(t *testing.T, vm *volume.VM, page uint64) control.Identity {
	t.Helper()
	extents, err := vm.Volume("ram0").Locate(t.Context(), page*pageSize, pageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(extents) != 1 {
		t.Fatalf("%s page %d is %d extents, want one", vm.ID(), page, len(extents))
	}
	return extents[0].Identity
}
