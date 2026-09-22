package host

import (
	"testing"

	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// deploymentConfig is the budgets deploy/10-host.yaml gives a host, divided
// between the two pagers as SPROUTFS_RAM_SHARE_PERCENT divides them: a 5 GiB
// arena three quarters to RAM, 9 GiB of dirty RAM in 4 KiB pages and 3 GiB of
// dirty PMEM in 2 MiB ones.
func deploymentConfig() SupervisorConfig {
	return SupervisorConfig{
		ArenaBytes:   KindBytes{RAM: 3 << 30, PMEM: 5<<30 - 3<<30},
		LogicalPages: KindPages{RAM: 1 << 22, PMEM: 1 << 13},
		DirtyPages:   KindPages{RAM: 2359296, PMEM: 1536},
	}
}

// Both runs are stated in bytes and converted by each pager, so the two get a
// run of the same size rather than the same number of pages: 8 MiB is four
// 2 MiB pages and 2,048 4 KiB ones.
func TestBothPagersGetRunsOfTheSameSizeInTheirOwnPages(t *testing.T) {
	config := deploymentConfig()
	ram := pagerConfig(config, vmmemory.Ram)
	pmem := pagerConfig(config, vmmemory.Pmem)
	if ram.PageSize != RAMPageSize || pmem.PageSize != PMEMPageSize {
		t.Fatalf("pages %d and %d, want %d and %d", ram.PageSize, pmem.PageSize, RAMPageSize, PMEMPageSize)
	}
	for _, tc := range []struct {
		what string
		got  int
		want int
	}{
		{"RAM read-ahead", ram.ReadAheadPages, readAheadBytes / RAMPageSize},
		{"PMEM read-ahead", pmem.ReadAheadPages, readAheadBytes / PMEMPageSize},
		{"RAM write-ahead", ram.WriteAheadPages, writeAheadBytes / RAMPageSize},
		{"PMEM write-ahead", pmem.WriteAheadPages, writeAheadBytes / PMEMPageSize},
	} {
		if tc.got != tc.want {
			t.Errorf("%s is %d pages, want %d", tc.what, tc.got, tc.want)
		}
	}
}

// An offset is an address and a page is memory. RAM places a private page at
// the offset it has within its range, so every 2 MiB range a region may write
// into owns 512 consecutive offsets of which only the stored pages hold memory:
// the offsets a pager needs are one extent per range of everything it may map —
// which is `LogicalPages`, since a range is 512 pages and an extent 512 offsets
// — plus the read-ahead runs, which come out of the offset space too and are
// bounded by what the arena can hold at once. PMEM places nothing, so its
// offsets and its pages stay one number.
func TestRAMsOffsetSpaceCoversAnExtentPerRangeItMayWriteInto(t *testing.T) {
	config := deploymentConfig()
	ram := pagerConfig(config, vmmemory.Ram)
	pmem := pagerConfig(config, vmmemory.Pmem)
	if want := config.LogicalPages.RAM + ram.ResidentPages; ram.ArenaOffsets != want {
		t.Errorf("RAM's arena has %d offsets, want %d: an extent per range it may write into,"+
			" and the runs it may hold at once", ram.ArenaOffsets, want)
	}
	if pmem.ArenaOffsets != pmem.ResidentPages {
		t.Errorf("PMEM's arena has %d offsets for %d pages, want the two to be one number",
			pmem.ArenaOffsets, pmem.ResidentPages)
	}
	// The address space is the point: it is far larger than the memory behind
	// it, and the memory is what the deployment budgeted.
	if ram.ArenaOffsets <= ram.ResidentPages {
		t.Errorf("RAM's arena has %d offsets for %d pages, want more addresses than memory",
			ram.ArenaOffsets, ram.ResidentPages)
	}
}

// A write-ahead run is charged a dirty reservation per page whether the guest
// uses it or not, so a pager whose dirty budget cannot hold 64 of them keeps
// one page. It is the budget that decides, not the kind: RAM writes ahead for
// the same reason PMEM does, because fresh zeros are shared with nobody.
func TestASmallDirtyBudgetKeepsWriteAheadAtOnePage(t *testing.T) {
	for _, kind := range []vmmemory.RegionKind{vmmemory.Ram, vmmemory.Pmem} {
		pageSize := PMEMPageSize
		if kind == vmmemory.Ram {
			pageSize = int(RAMPageSize)
		}
		run := writeAheadBytes / pageSize
		config := deploymentConfig()
		for _, tc := range []struct {
			dirty int
			want  int
		}{
			{run * writeAheadDirtyShare, run},
			{run*writeAheadDirtyShare - 1, 1},
			{run, 1},
		} {
			if kind == vmmemory.Ram {
				config.DirtyPages.RAM = tc.dirty
			} else {
				config.DirtyPages.PMEM = tc.dirty
			}
			if got := pagerConfig(config, kind).WriteAheadPages; got != tc.want {
				t.Errorf("%s with a dirty budget of %d pages writes %d ahead, want %d",
					kind, tc.dirty, got, tc.want)
			}
		}
	}
}

// The deployment's own budgets give each pager the whole run: nothing in
// deploy/10-host.yaml lands a production host on the one-page fallback.
func TestTheDeploymentsBudgetsAffordBothWriteAheadRuns(t *testing.T) {
	config := deploymentConfig()
	if got := pagerConfig(config, vmmemory.Ram).WriteAheadPages; got != writeAheadBytes/RAMPageSize {
		t.Errorf("the deployment's RAM dirty budget of %d pages writes %d ahead, want %d",
			config.DirtyPages.RAM, got, writeAheadBytes/RAMPageSize)
	}
	if got := pagerConfig(config, vmmemory.Pmem).WriteAheadPages; got != writeAheadBytes/PMEMPageSize {
		t.Errorf("the deployment's PMEM dirty budget of %d pages writes %d ahead, want %d",
			config.DirtyPages.PMEM, got, writeAheadBytes/PMEMPageSize)
	}
}
