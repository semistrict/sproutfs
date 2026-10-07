package host_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/journal"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/volume"
)

// journalRing is the ring of the journal every durable flush host here writes:
// room for a few whole 2 MiB pages.
const journalRing = 32 << 20

// journalHostOf is one host with durable flush on, running one VM with memory
// and a disk, and serving a journal on a simulated disk of its own. As in
// flushHost, the checkpoint loop is an hour from its first turn.
type journalHostOf struct {
	h     *hostHarness
	clock *sim.Clock
	vm    *volume.VM
	guest *machine
	j     *journal.Journal
	disk  *sim.Disk
}

func journalHost(t *testing.T) *journalHostOf { return journalHostRing(t, journalRing) }

// journalHostRing is journalHost with a journal of a ring of its own.
func journalHostRing(t *testing.T, ring int64) *journalHostOf {
	t.Helper()
	h := newSizedHostHarness(t, 1)
	clock := sim.New(sim.Config{Seed: 1}).NewClock("host")
	h.configs[0].Clock = clock
	h.configs[0].EpochInterval = -1
	h.configs[0].CheckpointInterval = time.Hour
	h.configs[0].FlushBound = flushBound
	h.configs[0].Journal = host.JournalConfig{DurableFlush: true}
	pagers := newMixedPagers(t, h.configs[0].Resources, func(cfg *vmmemory.Config) { cfg.Clock = clock })
	h.configs[0].Pagers = pagers.pagers
	h.start(t)
	ctx := sim.WithRuntime(t.Context(), h.runtime)
	disk := h.runtime.NewDisk("journal", sim.DiskConfig{})
	file, err := disk.Open(ctx, "device", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(ctx, 2*journal.BlockBytes+ring); err != nil {
		t.Fatal(err)
	}
	// The journal keeps the wall clock: space a trim frees comes back once its
	// tail hint is written, a second after the last, and the host's clock here
	// moves only when a test moves it.
	j, err := journal.Open(ctx, file, journal.Config{Identity: rank.Identity{1},
		Lease:   journal.Lease{Assigned: 1, Member: rank.Identity{2}},
		Entropy: h.runtime.NewEntropy("journal")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := j.Close(context.WithoutCancel(ctx)); err != nil && !errors.Is(err, journal.ErrClosed) {
			t.Error(err)
		}
	})
	h.hosts[0].SetJournal(j)
	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", mixedVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	return &journalHostOf{h: h, clock: clock, vm: vm, guest: guest, j: j, disk: disk}
}

// held is what the journal holds of vm-1: its live entries' count and the
// first and last of their positions.
func (s *journalHostOf) held() journal.Held {
	for _, held := range s.j.Held() {
		if held.VM == "vm-1" {
			return held
		}
	}
	return journal.Held{}
}

// record reads vm-1's control record.
func (s *journalHostOf) record(t *testing.T) control.Record {
	t.Helper()
	client, err := control.NewClient(control.Config{ObjectStore: s.h.runtime.ObjectStore(),
		ObjectPrefix: s.h.prefix, Entropy: s.h.runtime.NewEntropy("record")})
	if err != nil {
		t.Fatal(err)
	}
	record, err := client.Read(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// A durable flush is answered once its disk's changed blocks are on the
// journal, and takes no checkpoint.
func TestADurableFlushIsAnsweredOnceItsEntryIsOnTheJournal(t *testing.T) {
	s := journalHost(t)
	before := s.vm.Status().Checkpoint
	s.guest.store("disk", 0, 7)
	if err := <-flush(s.guest); err != nil {
		t.Fatalf("a durable flush failed: %v", err)
	}
	if held := s.held(); held.Entries != 1 {
		t.Fatalf("the journal holds %+v of the VM after its flush, want one entry", held)
	}
	if after := s.vm.Status().Checkpoint; after != before {
		t.Fatalf("a durable flush took checkpoint %s", after)
	}
	// The next flush with nothing stored since has nothing to write.
	if err := <-flush(s.guest); err != nil {
		t.Fatalf("a second durable flush failed: %v", err)
	}
	if held := s.held(); held.Entries != 1 {
		t.Fatalf("the journal holds %+v of the VM after a flush with nothing to write, want one entry", held)
	}
}

// A durable flush on a host that serves no journal fails: the guest reads an
// I/O error, and no checkpoint stands in for the journal.
func TestADurableFlushWithNoJournalFails(t *testing.T) {
	s := journalHost(t)
	s.h.hosts[0].SetJournal(nil)
	s.guest.store("disk", 0, 7)
	if err := <-flush(s.guest); !errors.Is(err, host.ErrJournalUnavailable) {
		t.Fatalf("a durable flush with no journal: %v, want ErrJournalUnavailable", err)
	}
}

// A durable flush whose journal sync fails fails, and gives its pages back:
// the next flush writes them again, though the guest stored nothing since.
func TestAFlushWhoseJournalSyncFailsFailsAndIsTakenAgain(t *testing.T) {
	s := journalHost(t)
	s.guest.store("disk", 0, 7)
	s.disk.FailNext(sim.DiskSync, 1)
	if err := <-flush(s.guest); !errors.Is(err, platform.ErrInjectedFault) {
		t.Fatalf("a flush whose journal sync failed: %v, want ErrInjectedFault", err)
	}
	if err := <-flush(s.guest); err != nil {
		t.Fatalf("the flush after a failed one: %v", err)
	}
	if held := s.held(); held.Entries != 1 {
		t.Fatalf("the journal holds %+v of the VM after the second flush, want the page's entry", held)
	}
}

// A checkpoint's selection names the journal at the position its pause
// covered, and trims what it covers: the entry of the flush before the pause
// is dead, and the next flush's entry comes after the covered position.
func TestTheSelectionNamesTheJournalAtTheCoveredPosition(t *testing.T) {
	s := journalHost(t)
	s.guest.store("disk", 0, 7)
	if err := <-flush(s.guest); err != nil {
		t.Fatal(err)
	}
	first := s.held().Last
	ckpt, err := host.CaptureDisks(t.Context(), s.vm, s.guest, s.clock,
		volume.Terms{Cover: s.h.hosts[0].JournalCover("vm-1")})
	if err != nil {
		t.Fatal(err)
	}
	if err := ckpt.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := []control.Journal{{Disk: s.j.Identity(), Generation: s.j.Generation(), Epoch: s.vm.Epoch(),
		Covered: first}}
	if got := s.record(t).Journals; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("the selection names journals %+v, want %+v", got, want)
	}
	if held := s.held(); held.Entries != 0 {
		t.Fatalf("the journal holds %+v of the VM after the selection, want nothing", held)
	}
	s.guest.store("disk", 1, 8)
	if err := <-flush(s.guest); err != nil {
		t.Fatal(err)
	}
	if held := s.held(); held.Entries != 1 || held.First <= first {
		t.Fatalf("the journal holds %+v after the next flush, want one entry after %d", held, first)
	}
}

// A VM may hold at most half the journal's ring: a flush of one past that
// waits for the checkpoint its host asks for, whose selection trims what the
// VM held, and is then answered.
func TestAFlushOfAVMHoldingHalfTheRingWaitsForItsCheckpoint(t *testing.T) {
	const ring = 8 << 20
	s := journalHostRing(t, ring)
	for page, value := range []byte{1, 2} {
		s.guest.store("disk", uint64(page), value)
		if err := <-flush(s.guest); err != nil {
			t.Fatal(err)
		}
	}
	if held := s.held(); held.Bytes*2 <= ring {
		t.Fatalf("two whole pages hold %d bytes of the journal, want more than half its %d", held.Bytes, ring)
	}
	before := s.vm.Status().Checkpoint
	s.guest.store("disk", 2, 3)
	if err := <-flush(s.guest); err != nil {
		t.Fatalf("the flush past half the ring: %v", err)
	}
	if after := s.vm.Status().Checkpoint; after == before {
		t.Fatal("the flush past half the ring was answered with no checkpoint taken")
	}
	if got := s.h.runtime.Probes()[host.ProbeJournalHalfRing]; got == 0 {
		t.Fatal("the flush past half the ring never waited for its checkpoint")
	}
	if held := s.held(); held.Bytes*2 > ring {
		t.Fatalf("the VM holds %d bytes of the journal's %d after its checkpoint, want at most half", held.Bytes, ring)
	}
}

// A JOURNAL_READ from a reader at a newer epoch is answered by the host that
// holds the journal disk with the VM's entries after the covered position.
// The holder fences the VM first, so the VM's next flush there fails, and it
// gives the VM up.
func TestAJournalReadFencesAndGivesUpTheVM(t *testing.T) {
	s := journalHost(t)
	s.guest.store("disk", 0, 7)
	if err := <-flush(s.guest); err != nil {
		t.Fatal(err)
	}
	table, err := peer.NewTable(t.Context(), peer.TableConfig{
		Dial: func(ctx context.Context, address platform.Address) (platform.Conn, error) {
			return s.h.network.Dial(ctx, "", address)
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := table.Close(); err != nil {
			t.Error(err)
		}
	})
	epoch := s.vm.Epoch()
	page, err := table.Peer(s.h.pages[0]).ReadJournal(t.Context(), s.j.Identity(), journal.ReadRequest{VM: "vm-1",
		Epoch: epoch, Generation: s.j.Generation(), Reader: epoch + 1}, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || page.More || page.Entries[0].Volume != "disk" ||
		len(page.Entries[0].Data) != len(page.Entries[0].Blocks)*journal.BlockBytes {
		t.Fatalf("the read returned %d entries (more %v), want the one entry of the disk", len(page.Entries), page.More)
	}
	if got := page.Entries[0].Data[0]; got != 7 {
		t.Fatalf("the entry's first byte is %d, want the 7 the guest stored", got)
	}
	if got := s.h.hosts[0].Machines(); len(got) != 0 {
		t.Fatalf("the holder still runs %v after a newer reader read its journal", got)
	}
	if _, err := table.Peer(s.h.pages[0]).ReadJournal(t.Context(), s.j.Identity(), journal.ReadRequest{VM: "vm-1",
		Epoch: epoch, Generation: s.j.Generation() + 1, Reader: epoch + 1}, 16<<20); !errors.Is(err, journal.ErrGeneration) {
		t.Fatalf("a read of another generation: %v, want journal.ErrGeneration", err)
	}
	if _, err := table.Peer(s.h.pages[0]).ReadJournal(t.Context(), rank.Identity{9}, journal.ReadRequest{VM: "vm-1",
		Epoch: epoch, Generation: s.j.Generation(), Reader: epoch + 1}, 16<<20); !errors.Is(err, peer.ErrNoJournal) {
		t.Fatalf("a read of a disk the host does not hold: %v, want peer.ErrNoJournal", err)
	}
}
