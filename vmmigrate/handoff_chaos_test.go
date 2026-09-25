package vmmigrate_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/vmmigrate"
)

// A migration publishes nothing, so the source's pages are the only copy of
// everything the guest wrote since its last checkpoint. Losing that host before
// the destination fetched them rewinds the VM to the checkpoint, and no
// further: the control record still selects it and an ordinary open brings it
// back.
func TestSourceLostAfterHandoffRewindsToTheLastCheckpoint(t *testing.T) {
	for _, seed := range []uint64{1, 7, 23} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newSeededCluster(t, seed)
				random := rand.New(rand.NewPCG(seed, 91))
				manager := c.manager(t, "source")
				vm, err := manager.Create(t.Context(), "migrant", vmSpec)
				if err != nil {
					t.Fatal(err)
				}
				guest, err := newMachine(t, newPager(t, c, "source"), vm, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				stores(random, guest)
				if err := guest.checkpoint(t.Context(), vm); err != nil {
					t.Fatal(err)
				}
				guest.start(1)
				// Everything the guest stores from here lives only in this host's
				// pages until the destination fetches it or the next checkpoint lands.
				durable, selected := guest.snapshot(), vm.Status().Checkpoint
				stores(random, guest)
				source, err := vmmigrate.NewPageSource(t.Context(), vmmigrate.SourceConfig{PageSize: pageSize,
					Network: c.runtime.Network(), Address: sourceAddress})
				if err != nil {
					t.Fatal(err)
				}
				defer source.Close()
				process := &savedStop{machine: guest}
				if _, err = vmmigrate.Migrate(t.Context(), vm, process, source, vmmigrate.Options{}); err != nil {
					t.Fatal(err)
				}
				if !vm.Status().HandedOff || guest.running {
					t.Fatalf("the handoff left the source running: %+v", vm.Status())
				}
				for name, memoryRegion := range guest.memoryRegions {
					if err := memoryRegion.Fault(t.Context(), 0, true); !errors.Is(err, vmmemory.ErrHandedOff) {
						t.Fatalf("%s remained writable after the handoff: %v", name, err)
					}
				}
				// Drop every source page before anything fetched them, which is
				// the source host dying mid post-copy.
				source.Release(vm.ID())
				guest.close()

				destination := c.manager(t, "destination")
				opened, err := destination.Open(t.Context(), vm.ID())
				if err != nil {
					t.Fatal(err)
				}
				if got := opened.Status().Checkpoint; got != selected {
					t.Fatalf("reopened at %v, want the last selected checkpoint %v", got, selected)
				}
				restarted, err := newMachine(t, newPager(t, c, "destination"), opened, nil, process.state)
				if err != nil {
					t.Fatal(err)
				}
				if err := restarted.verify(t.Context(), durable); err != nil {
					t.Fatal(err)
				}
				restarted.adopt(durable)
				stores(random, restarted)
				if err := restarted.checkpoint(t.Context(), opened); err != nil {
					t.Fatal(err)
				}
				if err := restarted.verify(t.Context(), restarted.snapshot()); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

// A supervisor retains the snapshot it produced even if the coordinator fails
// after Stop returned it; recovering volume bytes alone cannot restore vCPUs.
type savedStop struct {
	*machine
	state []byte
}

func (p *savedStop) Stop(ctx context.Context) ([]byte, error) {
	state, err := p.machine.Stop(ctx)
	p.state = state
	return state, err
}

// stores writes a few of the guest's pages, which is the work a handoff has to
// carry across.
func stores(random *rand.Rand, m *machine) {
	for range 1 + random.IntN(12) {
		name := m.names[random.IntN(len(m.names))]
		m.write(name, uint64(random.IntN(m.pages[name])))
	}
}

// Failure to resume is an explicit error, never a successful handoff. This is
// the supervisor contract when a canceled/failed stop cannot safely run again.
func TestMigrationReportsFailedResumption(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMigration(t)
		m.machine.failStop = errInjected
		resumeFailure := errors.New("supervisor cannot resume")
		process := &failedRelease{machine: m.machine, err: resumeFailure}
		handoff, err := vmmigrate.Migrate(t.Context(), m.vm, process, m.pages, vmmigrate.Options{})
		if !errors.Is(err, errInjected) || !errors.Is(err, resumeFailure) {
			t.Fatalf("lost a migration/resumption error: %v", err)
		}
		if handoff.VMID != "" || m.vm.Status().HandedOff || m.machine.running {
			t.Fatal("failed resumption was reported as a running or handed-off guest")
		}
	})
}

type failedRelease struct {
	*machine
	err error
}

func (p *failedRelease) Release(context.Context) error { return p.err }
