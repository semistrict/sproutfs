package simtest

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/testpager"
)

// Sharing is what the running guests' mappings share, by tenant. Within counts
// the pages each tenant's guests share among themselves: slots two or more of
// them map at once. Public counts the slots of an isolated arena's public file
// the guests of two or more tenants map at once: the pages of public
// templates, the one thing tenants share. Across describes every other slot
// the guests of two tenants map at once, and in an isolated arena every other
// file they both map from. No other page crosses between tenants, so a
// campaign requires Across to be empty.
func (w *World) Sharing() (within map[string]int, public int, across []string) {
	type file struct {
		arena *testpager.Arena
		id    int
	}
	type slot struct {
		file
		slot int
	}
	fileTenants := map[file]map[string]bool{}
	slotGuests := map[slot]map[string]bool{}
	slotTenants := map[slot]map[string]bool{}
	// private is every slot some guest maps at a page that is no public
	// template's: a page that is its tenant's alone.
	private := map[slot]bool{}
	for _, id := range w.Running() {
		_, g := w.runningVM(id)
		if g == nil {
			continue
		}
		for _, name := range g.names {
			mp := g.mappings[name]
			for page, p := range mp.Mapped() {
				f := file{mp.Arena(), p.File}
				s := slot{f, p.Slot}
				addTo(fileTenants, f, mp.Tenant())
				addTo(slotGuests, s, id)
				addTo(slotTenants, s, mp.Tenant())
				if !g.publicPage(name, page) {
					private[s] = true
				}
			}
		}
	}
	within = map[string]int{}
	for s, guests := range slotGuests {
		tenants := slotTenants[s]
		if s.arena.Public(s.id) && private[s] {
			across = append(across, fmt.Sprintf("slot %d of the public file %d holds a page of tenants %s",
				s.slot, s.id, sortedKeys(tenants)))
			continue
		}
		if len(tenants) > 1 && !private[s] {
			public++
			continue
		}
		if len(tenants) > 1 {
			across = append(across, fmt.Sprintf("slot %d of file %d is mapped by tenants %s",
				s.slot, s.id, sortedKeys(tenants)))
			continue
		}
		if len(guests) > 1 {
			for tenant := range tenants {
				within[tenant]++
			}
		}
	}
	for f, tenants := range fileTenants {
		if f.arena.Isolated() && len(tenants) > 1 && !f.arena.Public(f.id) {
			across = append(across, fmt.Sprintf("file %d of an isolated arena is mapped from by tenants %s",
				f.id, sortedKeys(tenants)))
		}
	}
	slices.Sort(across)
	return within, public, across
}

// publicPage reports a page of one of this guest's volumes whose bytes are a
// public template's: the volume names it by a checkpoint of one.
func (g *guest) publicPage(name string, page uint64) bool {
	size := uint64(g.pageBytes[name])
	extents, err := g.backings[name].Backing.Locate(g.ctx, page*size, size)
	return err == nil && len(extents) == 1 && control.Public(extents[0].Identity.Ref.VM)
}

// addTo adds one member to the set a key names.
func addTo[K comparable](sets map[K]map[string]bool, key K, member string) {
	if sets[key] == nil {
		sets[key] = map[string]bool{}
	}
	sets[key][member] = true
}

// sortedKeys is a set of names as one sorted list.
func sortedKeys(set map[string]bool) string {
	var quoted []string
	for _, name := range slices.Sorted(maps.Keys(set)) {
		quoted = append(quoted, strconv.Quote(name))
	}
	return strings.Join(quoted, ", ")
}
