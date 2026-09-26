package simtest

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/semistrict/sproutfs/internal/testpager"
)

// Sharing is what the running guests' mappings share, by tenant. Within counts
// the pages each tenant's guests share among themselves: slots two or more of
// them map at once. Across describes every slot the guests of two tenants map
// at once, and in an isolated arena every file they both map from. No page
// crosses between tenants, so a campaign requires Across to be empty.
func (w *World) Sharing() (within map[string]int, across []string) {
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
	for _, id := range w.Running() {
		_, g := w.runningVM(id)
		if g == nil {
			continue
		}
		for _, name := range g.names {
			mp := g.mappings[name]
			for _, p := range mp.Mapped() {
				f := file{mp.Arena(), p.File}
				s := slot{f, p.Slot}
				addTo(fileTenants, f, mp.Tenant())
				addTo(slotGuests, s, id)
				addTo(slotTenants, s, mp.Tenant())
			}
		}
	}
	within = map[string]int{}
	for s, guests := range slotGuests {
		tenants := slotTenants[s]
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
		if f.arena.Isolated() && len(tenants) > 1 {
			across = append(across, fmt.Sprintf("file %d of an isolated arena is mapped from by tenants %s",
				f.id, sortedKeys(tenants)))
		}
	}
	slices.Sort(across)
	return within, across
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
