package simtest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/knobs"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/volume"
)

// The chapters of the interactive explainer (TASK-65), each a scenario the
// real code runs. A chapter's steps are marked in the simulator's own trace,
// beside every object-store request, network exchange, disk write and process
// event they caused, and after each step the chapter takes a snapshot of the
// deployment: every page of every VM, what each host holds, and what each
// control record selects. With SPROUTFS_EXPLAIN=1 each chapter prints one line,
// "EXPLAIN " and a JSON document, which is what the explainer's page reads out
// of this test binary compiled to WebAssembly.
//
// Each chapter is also a test of what it teaches: it asserts the outcome the
// page will tell the reader to expect.

// explainVolumes is every explainer VM's shape: four 4 KiB pages of memory and
// two 2 MiB pages of disk, few enough to draw every page.
var explainVolumes = []volume.VolumeSpec{
	{Name: simtest.MemoryVolume, Size: 4 * simtest.RAMPage, PageSize: simtest.RAMPage},
	{Name: "disk", Size: 2 * simtest.PMEMPage, PageSize: simtest.PMEMPage}}

// chapter runs one scenario over two hosts, one VM on the first, and records
// its steps.
type chapter struct {
	t        *testing.T
	ctx      context.Context
	runtime  *sim.Runtime
	world    *simtest.World
	name     string
	snapshot []stepView
}

// stepView is one step of a chapter: its name, the trace sequences of its two
// markers, between which are the events it caused, and the deployment after
// it. The snapshot reads the store too, after the end marker, so its own reads
// are no step's.
type stepView struct {
	Step  string           `json:"step"`
	From  uint64           `json:"from_seq"`
	Until uint64           `json:"until_seq"`
	State simtest.Snapshot `json:"state"`
}

func newChapter(t *testing.T, name string, seed uint64) *chapter {
	t.Helper()
	runtime := retryRuntime(seed)
	ctx := sim.WithRuntime(t.Context(), runtime)
	prefix, err := platform.NewObjectPrefix("sproutfs/")
	if err != nil {
		t.Fatal(err)
	}
	// Pagers sized for six-page VMs. Each pager's spill file is sized from
	// these up front, and a browser tab holds every byte of every simulated
	// file: the suite's own sizes take a chapter to most of a gigabyte.
	k := knobs.Defaults()
	k.ResidentPages, k.DirtyPages, k.LogicalPages = 4, 4, 8
	k.ReadAheadPages, k.WriteAheadPages = 1, 1
	if err := k.Validate(); err != nil {
		t.Fatal(err)
	}
	c := &chapter{t: t, ctx: ctx, runtime: runtime, name: name}
	// The first step is the deployment starting: two hosts, and vm-1 created
	// on the first, which writes its first checkpoint and its control record.
	c.step("create vm-1", func() error {
		c.world = simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Knobs: k, Prefix: prefix, Log: t.Logf,
			Topology: simtest.Topology{Hosts: []string{"host-0", "host-1"},
				VMs: []simtest.VMSpec{{ID: "vm-1", Host: 0, Volumes: explainVolumes}}}})
		return nil
	})
	return c
}

// step runs one step between its two markers and snapshots the deployment
// after it.
func (c *chapter) step(name string, run func() error) {
	c.t.Helper()
	from := c.mark(name, "begin")
	if err := run(); err != nil {
		c.t.Fatalf("%s: %v", name, err)
	}
	until := c.mark(name, "end")
	state, err := c.world.Snapshot(c.ctx)
	if err != nil {
		c.t.Fatalf("snapshot after %s: %v", name, err)
	}
	c.snapshot = append(c.snapshot, stepView{Step: name, From: from, Until: until, State: state})
}

// mark records one of a step's markers and reports its sequence.
func (c *chapter) mark(name, outcome string) uint64 {
	return c.runtime.Trace().Record(sim.Event{Kind: "step", Resource: c.name, Operation: name, Outcome: outcome})
}

func (c *chapter) write(vm, volume string, pages []uint64, value byte) {
	c.t.Helper()
	c.step(fmt.Sprintf("write %s %s %v", vm, volume, pages), func() error {
		return c.world.StorePages(vm, volume, pages, value)
	})
}

// last is the deployment after the last step.
func (c *chapter) last() simtest.Snapshot { return c.snapshot[len(c.snapshot)-1].State }

// finish prints the chapter for the page, and verifies what the world holds.
func (c *chapter) finish() {
	c.t.Helper()
	if os.Getenv("SPROUTFS_EXPLAIN") == "1" {
		type event struct {
			Sequence  uint64 `json:"seq"`
			AtNS      int64  `json:"at_ns"`
			Kind      string `json:"kind"`
			Resource  string `json:"resource"`
			Operation string `json:"op"`
			Outcome   string `json:"outcome"`
			Bytes     int    `json:"bytes"`
		}
		var events []event
		for _, e := range c.runtime.Trace().Events() {
			events = append(events, event{e.Sequence, e.At.UnixNano(), e.Kind, e.Resource, e.Operation, e.Outcome, e.Bytes})
		}
		line, err := json.Marshal(struct {
			Chapter string     `json:"chapter"`
			Steps   []stepView `json:"steps"`
			Events  []event    `json:"events"`
		}{c.name, c.snapshot, events})
		if err != nil {
			c.t.Fatal(err)
		}
		fmt.Printf("EXPLAIN %s\n", line)
	}
	if err := c.world.Verify(c.ctx, simtest.ReadsMustSucceed); err != nil {
		c.t.Error(err)
	}
	if err := c.world.Close(c.ctx); err != nil {
		c.t.Error(err)
	}
}

// page finds one page of one VM in a snapshot, and the host running it.
func page(t *testing.T, s simtest.Snapshot, vm, volume string, index int) (simtest.PageView, string) {
	t.Helper()
	for _, h := range s.Hosts {
		for _, v := range h.VMs {
			if v.ID != vm {
				continue
			}
			for _, vol := range v.Volumes {
				if vol.Name == volume {
					return vol.Pages[index], h.Name
				}
			}
		}
	}
	t.Fatalf("%s's %s is running nowhere", vm, volume)
	return simtest.PageView{}, ""
}

// TestExplainAVMIsPages: a guest's writes land in its host's memory, page by
// page, and nothing outside the host hears of them.
func TestExplainAVMIsPages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newChapter(t, "pages", 51)
		c.write("vm-1", simtest.MemoryVolume, []uint64{0, 2}, 7)
		c.write("vm-1", "disk", []uint64{1}, 9)
		written, host := page(t, c.last(), "vm-1", simtest.MemoryVolume, 2)
		if !written.Unpublished || !written.Resident || host != "host-0" {
			t.Fatalf("a written page is %+v on %s, want resident and unpublished on host-0", written, host)
		}
		c.finish()
	})
}

// TestExplainACheckpointMakesPagesDurable: a checkpoint publishes every page
// written since the last one, and its control record selects it. A page
// written after that is unpublished again, over the published one.
func TestExplainACheckpointMakesPagesDurable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newChapter(t, "checkpoint", 52)
		c.write("vm-1", simtest.MemoryVolume, []uint64{0, 2}, 7)
		c.write("vm-1", "disk", []uint64{1}, 9)
		c.step("checkpoint vm-1", func() error { return c.world.Checkpoint(c.ctx, "vm-1") })
		published, _ := page(t, c.last(), "vm-1", simtest.MemoryVolume, 2)
		if published.Unpublished || published.VM != "vm-1" || published.Checkpoint == 0 {
			t.Fatalf("a checkpointed page is %+v, want published by vm-1", published)
		}
		c.write("vm-1", simtest.MemoryVolume, []uint64{2}, 8)
		rewritten, _ := page(t, c.last(), "vm-1", simtest.MemoryVolume, 2)
		if !rewritten.Unpublished {
			t.Fatalf("a page written after the checkpoint is %+v, want unpublished", rewritten)
		}
		c.finish()
	})
}

// TestExplainALostHostCostsOnlyWhatNoCheckpointHeld: the VM's host is lost
// with a page written after the last checkpoint. The VM comes back on the other
// host at that checkpoint: the page reads again what the checkpoint holds for
// it, and the write since is gone.
func TestExplainALostHostCostsOnlyWhatNoCheckpointHeld(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newChapter(t, "lost-host", 53)
		c.write("vm-1", simtest.MemoryVolume, []uint64{0}, 7)
		c.step("checkpoint vm-1", func() error { return c.world.Checkpoint(c.ctx, "vm-1") })
		checkpointed, _ := page(t, c.last(), "vm-1", simtest.MemoryVolume, 1)
		c.write("vm-1", simtest.MemoryVolume, []uint64{1}, 8)
		c.step("lose host-0", func() error { return c.world.LoseHost(c.ctx, 0) })
		c.step("recover vm-1", func() error { return c.world.Settle(c.ctx) })
		kept, host := page(t, c.last(), "vm-1", simtest.MemoryVolume, 0)
		rewound, _ := page(t, c.last(), "vm-1", simtest.MemoryVolume, 1)
		if host != "host-1" || kept.VM != "vm-1" || rewound.Unpublished ||
			rewound.VM != checkpointed.VM || rewound.Checkpoint != checkpointed.Checkpoint {
			t.Fatalf("after the loss vm-1 is on %s with page 0 %+v and page 1 %+v, want it on host-1 with page 1 back at %+v",
				host, kept, rewound, checkpointed)
		}
		c.finish()
	})
}

// TestExplainAForkSharesInsteadOfCopying: a child forked onto the other host
// names its parent's pages rather than copying them. The page its parent wrote
// after the last checkpoint is fetched from the parent's host, which holds it
// for the child until the child has it.
func TestExplainAForkSharesInsteadOfCopying(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newChapter(t, "fork", 54)
		c.write("vm-1", simtest.MemoryVolume, []uint64{0}, 7)
		c.step("checkpoint vm-1", func() error { return c.world.Checkpoint(c.ctx, "vm-1") })
		c.write("vm-1", simtest.MemoryVolume, []uint64{1}, 8)
		c.step("fork vm-1-a onto host-1", func() error {
			return c.world.Fork(c.ctx, simtest.VMSpec{ID: "vm-1-a", Parent: "vm-1", Host: 1, Volumes: explainVolumes})
		})
		c.step("fork vm-1-b onto host-0", func() error {
			return c.world.Fork(c.ctx, simtest.VMSpec{ID: "vm-1-b", Parent: "vm-1", Host: 0, Volumes: explainVolumes})
		})
		parent, _ := page(t, c.last(), "vm-1", simtest.MemoryVolume, 0)
		first, firstHost := page(t, c.last(), "vm-1-a", simtest.MemoryVolume, 0)
		second, secondHost := page(t, c.last(), "vm-1-b", simtest.MemoryVolume, 0)
		shared := simtest.PageView{VM: "vm-1", Checkpoint: parent.Checkpoint}
		if firstHost != "host-1" || secondHost != "host-0" ||
			first.VM != shared.VM || first.Checkpoint != shared.Checkpoint ||
			second.VM != shared.VM || second.Checkpoint != shared.Checkpoint {
			t.Fatalf("page 0 is %+v in vm-1, %+v in vm-1-a on %s and %+v in vm-1-b on %s; want both children on vm-1's checkpoint, on host-1 and host-0",
				parent, first, firstHost, second, secondHost)
		}
		// The child on the parent's host reads the fork point the parent saved;
		// the one on the other host saved the page in its own first checkpoint.
		point, _ := page(t, c.last(), "vm-1", simtest.MemoryVolume, 1)
		pulled, _ := page(t, c.last(), "vm-1-a", simtest.MemoryVolume, 1)
		local, _ := page(t, c.last(), "vm-1-b", simtest.MemoryVolume, 1)
		if point.VM != "vm-1" || point.Unpublished || local.VM != point.VM || local.Checkpoint != point.Checkpoint || pulled.VM != "vm-1-a" {
			t.Fatalf("page 1 is %+v in vm-1, %+v in vm-1-a and %+v in vm-1-b; want vm-1's fork point in vm-1 and vm-1-b, and vm-1-a's own checkpoint in vm-1-a",
				point, pulled, local)
		}
		c.finish()
	})
}

// TestExplainAMigrationMovesMemoryHostToHost: the VM moves to the other host
// with a page no checkpoint holds. It runs there at once, and that page follows
// it from the host it left, which uploads nothing.
func TestExplainAMigrationMovesMemoryHostToHost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newChapter(t, "migration", 55)
		c.write("vm-1", simtest.MemoryVolume, []uint64{0}, 7)
		c.step("checkpoint vm-1", func() error { return c.world.Checkpoint(c.ctx, "vm-1") })
		c.write("vm-1", simtest.MemoryVolume, []uint64{1}, 8)
		c.step("migrate vm-1 to host-1", func() error { return c.world.Migrate(c.ctx, "vm-1", 1) })
		moved, host := page(t, c.last(), "vm-1", simtest.MemoryVolume, 1)
		if host != "host-1" || !moved.Unpublished {
			t.Fatalf("after the migration vm-1's page 1 is %+v on %s, want it unpublished on host-1", moved, host)
		}
		c.finish()
	})
}
