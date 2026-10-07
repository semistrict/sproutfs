package host_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/journal"
	"github.com/semistrict/sproutfs/membership"
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
	// source is the journal a migration's source named, for a VM that
	// arrived here.
	source control.Journal
}

func journalHost(t *testing.T) *journalHostOf { return journalHostRing(t, journalRing) }

// journalHostRing is journalHost with a journal of a ring of its own. Its VM's
// record names the journal, as it does once the VM's first checkpoint here
// is selected.
func journalHostRing(t *testing.T, ring int64) *journalHostOf {
	t.Helper()
	s := unnamedJournalHost(t, ring)
	s.checkpoint(t)
	return s
}

// unnamedJournalHost is journalHostRing before any checkpoint of its VM: the
// record names no journal yet.
func unnamedJournalHost(t *testing.T, ring int64) *journalHostOf {
	t.Helper()
	return journalHosts(t, ring, 1)
}

// journalHosts is unnamedJournalHost among count hosts, all with durable flush
// on; the VM and the journal are the first one's.
func journalHosts(t *testing.T, ring int64, count int) *journalHostOf {
	t.Helper()
	return journalHostsOf(t, ring, count, false)
}

// arrivedJournalHost is unnamedJournalHost whose VM a migration brought here:
// its record names the source's journal at the epoch before, and this host's
// at the epoch its open took, and its post-copy has not ended.
func arrivedJournalHost(t *testing.T) *journalHostOf {
	t.Helper()
	return journalHostsOf(t, journalRing, 1, true)
}

func journalHostsOf(t *testing.T, ring int64, count int, arrived bool) *journalHostOf {
	t.Helper()
	h := newSizedHostHarness(t, count)
	clock := sim.New(sim.Config{Seed: 1}).NewClock("host")
	var pagers *hostPagers
	for n := range count {
		h.configs[n].Clock = clock
		h.configs[n].EpochInterval = -1
		h.configs[n].CheckpointInterval = time.Hour
		h.configs[n].FlushBound = flushBound
		h.configs[n].Journal = host.JournalConfig{DurableFlush: true}
		these := newMixedPagers(t, h.configs[n].Resources, func(cfg *vmmemory.Config) { cfg.Clock = clock })
		h.configs[n].Pagers = these.pagers
		if n == 0 {
			pagers = these
		}
	}
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
	var source control.Journal
	if arrived {
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		// The source's open named its journal; this host's names its own.
		records := h.hosts[0].Control()
		opened, err := records.OpenMigration(t.Context(), "vm-1",
			control.Journal{Disk: [16]byte{0x50}, Generation: 9, Covered: 4096})
		if err != nil {
			t.Fatal(err)
		}
		opened.Close()
		source = control.Journal{Disk: [16]byte{0x50}, Generation: 9, Epoch: opened.Epoch(), Covered: 4096}
		if vm, err = h.hosts[0].Volumes().OpenMigration(t.Context(), "vm-1",
			&control.Journal{Disk: j.Identity(), Generation: j.Generation()}); err != nil {
			t.Fatal(err)
		}
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[0].AddMachineWith("vm-1", guest, host.MachineTerms{PostCopy: arrived}); err != nil {
		t.Fatal(err)
	}
	return &journalHostOf{h: h, clock: clock, vm: vm, guest: guest, j: j, disk: disk, source: source}
}

// checkpoint takes and selects a checkpoint of vm-1 with its VMM state,
// which names the journal: a flush is journaled only once the record names it.
func (s *journalHostOf) checkpoint(t *testing.T) {
	t.Helper()
	ckpt, err := host.Capture(sim.WithRuntime(t.Context(), s.h.runtime), s.vm, s.guest, s.clock,
		volume.Terms{Cover: s.h.hosts[0].JournalCover("vm-1")})
	if err != nil {
		t.Fatal(err)
	}
	if err := ckpt.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
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

// A commit needs room on the ring for its entries and for the pad that may
// come before them, which may be as large. A flush of one page beside the
// two its VM holds fits neither, though the VM holds no more than half the
// ring and the ring would not be three quarters full: the host asks for the
// checkpoint that frees the room, and the flush is answered once it has.
func TestAFlushTheRingHasNoRoomForAsksForTheCheckpointThatFreesIt(t *testing.T) {
	blocks := func(bytes int64) int64 {
		return (bytes + journal.BlockBytes - 1) / journal.BlockBytes * journal.BlockBytes
	}
	// A page's entry is one batch, which ends on a block. The ring takes two
	// and is three quarters full with a third.
	page := host.RoomFor("vm-1", "disk", int(checkpoint.PageSize2MiB/journal.BlockBytes))
	ring := blocks((4*(2*blocks(page)+page) + 2) / 3)
	s := journalHostRing(t, ring)
	for page, value := range []byte{1, 2} {
		s.guest.store("disk", uint64(page), value)
		if err := <-flush(s.guest); err != nil {
			t.Fatal(err)
		}
	}
	if held := s.held(); held.Bytes*2 > ring {
		t.Fatalf("two whole pages hold %d bytes of the journal, want at most half its %d", held.Bytes, ring)
	}
	if usage := s.j.Usage(); (usage.Used+page)*4 > usage.Ring*3 {
		t.Fatalf("the ring holds %d of %d bytes, and a page more is past three quarters", usage.Used, usage.Ring)
	}
	if short := s.j.Shortfall(page); short == 0 {
		t.Fatalf("a page fits on a ring of %d holding %d bytes", ring, s.j.Usage().Used)
	}
	before := s.vm.Status().Checkpoint
	s.guest.store("disk", 2, 3)
	if err := <-flush(s.guest); err != nil {
		t.Fatalf("the flush the ring had no room for: %v", err)
	}
	if after := s.vm.Status().Checkpoint; after == before {
		t.Fatal("the flush the ring had no room for was answered with no checkpoint taken")
	}
	if got := s.h.runtime.Probes()[host.ProbeJournalNoRoom]; got == 0 {
		t.Fatal("the flush the ring had no room for never asked for room")
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

// A flush is journaled only once the VM's record names the journal at its
// epoch: a recovery replays only what the record names. The first flush after
// an open asks for a checkpoint out of the interval's turn, whose selection
// names it, and is answered after that.
func TestAFlushIsJournaledOnlyOnceTheRecordNamesTheJournal(t *testing.T) {
	s := unnamedJournalHost(t, journalRing)
	if got := s.record(t).Journals; len(got) != 0 {
		t.Fatalf("before any checkpoint the record names %v", got)
	}
	before := s.vm.Status().Checkpoint
	s.guest.store("disk", 0, 7)
	if err := <-flush(s.guest); err != nil {
		t.Fatalf("the first durable flush failed: %v", err)
	}
	got := s.record(t).Journals
	if len(got) != 1 || got[0].Disk != s.j.Identity() || got[0].Epoch != s.vm.Epoch() {
		t.Fatalf("after the first flush the record names %v, want this host's journal at epoch %d", got,
			s.vm.Epoch())
	}
	if after := s.vm.Status().Checkpoint; after == before {
		t.Fatal("the first flush was answered with no checkpoint naming the journal")
	}
}

// A VM another host opens after its host stopped answering reads back what
// it flushed there: the open finds the journal's holder in the membership and
// reads its entries over the network, which fences the old instance and gives
// it up, and the new instance's disk holds the flushed byte. Its cold boot
// publishes it and leaves the record naming no journal.
func TestAnotherHostsOpenReplaysWhatTheVMFlushed(t *testing.T) {
	s := journalHosts(t, journalRing, 2)
	s.checkpoint(t)
	published := s.vm.Status().Checkpoint
	s.guest.store("disk", 0, 7)
	if err := <-flush(s.guest); err != nil {
		t.Fatal(err)
	}
	if got := s.vm.Status().Checkpoint; got != published {
		t.Fatalf("the flush took checkpoint %s", got)
	}
	ctx := sim.WithRuntime(t.Context(), s.h.runtime)
	other := s.h.hosts[1]
	if _, err := other.Volumes().Open(ctx, "vm-1"); !errors.Is(err, volume.ErrJournalPending) {
		t.Fatalf("an open while no member serves the journal = %v, want ErrJournalPending", err)
	}
	// The membership says the first host serves its journal disk.
	s.servedAt(t, s.h.pages[0])
	reopened, err := other.Volumes().Open(ctx, "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	}()
	if !reopened.Replayed() {
		t.Fatal("an open over a journal holding a flush of the VM replayed nothing")
	}
	if got := s.h.hosts[0].Machines(); len(got) != 0 {
		t.Fatalf("the old instance still runs as %v after the replay fenced it", got)
	}
	read := make([]byte, 1)
	if err := reopened.Volume("disk").Read(t.Context(), 0, read); err != nil {
		t.Fatal(err)
	}
	if read[0] != 7 {
		t.Fatalf("the replayed disk reads %d, want the 7 the guest flushed", read[0])
	}
	state, err := other.Starting(ctx, reopened, "ram0")
	if err != nil {
		t.Fatal(err)
	}
	if state != nil {
		t.Fatal("a replayed VM is resumed from its checkpoint's VMM state rather than cold booted")
	}
	record := s.record(t)
	if record.Selected == published.Sequence || len(record.Journals) != 0 {
		t.Fatalf("after the cold boot the record selects %d naming %v, want a new checkpoint naming none",
			record.Selected, record.Journals)
	}
}

// servedAt has the membership say that a member at address serves the
// journal disk, as a holder that opened it does.
func (s *journalHostOf) servedAt(t *testing.T, address platform.Address) {
	t.Helper()
	members, err := membership.NewStore(membership.Config{ObjectStore: s.h.runtime.ObjectStore(),
		ObjectPrefix: s.h.prefix, Entropy: s.h.runtime.NewEntropy("membership")})
	if err != nil {
		t.Fatal(err)
	}
	holder := rank.Identity{2}
	for _, change := range []func(membership.Membership) (membership.Membership, error){
		func(m membership.Membership) (membership.Membership, error) {
			return m.Join(membership.Member{ID: holder, Address: address})
		},
		func(m membership.Membership) (membership.Membership, error) {
			return m.Add(membership.Disk{ID: s.j.Identity(), Volume: "journal-0", Kind: membership.Journal})
		},
		func(m membership.Membership) (membership.Membership, error) { return m.Assign(s.j.Identity(), holder) },
		func(m membership.Membership) (membership.Membership, error) { return m.Serve(s.j.Identity(), holder) },
	} {
		if _, err := members.Update(t.Context(), change); err != nil {
			t.Fatal(err)
		}
	}
}

// A holder the membership still lists and nothing can reach, as a host that
// died before its member was drained, holds the open back as pending: the
// disk is read once a survivor serves it, and the open publishes nothing.
func TestAnOpenWhoseJournalHolderCannotBeReachedIsPending(t *testing.T) {
	s := journalHosts(t, journalRing, 2)
	s.checkpoint(t)
	published := s.vm.Status().Checkpoint
	s.guest.store("disk", 0, 7)
	if err := <-flush(s.guest); err != nil {
		t.Fatal(err)
	}
	s.servedAt(t, "gone-pages")
	ctx := sim.WithRuntime(t.Context(), s.h.runtime)
	if _, err := s.h.hosts[1].Volumes().Open(ctx, "vm-1"); !errors.Is(err, volume.ErrJournalPending) {
		t.Fatalf("an open whose journal's holder cannot be reached = %v, want ErrJournalPending", err)
	}
	if record := s.record(t); record.Selected != published.Sequence {
		t.Fatalf("the pending open left the record selecting %d, want %d", record.Selected, published.Sequence)
	}
}

// A migration's destination journals no flush until its post-copy ends: the
// pages the guest stored into on the source since their last capture reach it
// only in the post-copy. A flush that arrives meanwhile is answered after it
// ends, and a checkpoint paused before then
// keeps the source's journal named, since it may not hold every page the
// source held. The first paused after it names this host's journal alone.
func TestADestinationJournalsNothingUntilItsPostCopyEnds(t *testing.T) {
	s := arrivedJournalHost(t)
	own := control.Journal{Disk: s.j.Identity(), Generation: s.j.Generation(), Epoch: s.vm.Epoch()}
	if got := s.record(t).Journals; !slices.Equal(got, []control.Journal{s.source, own}) {
		t.Fatalf("the destination's open left the record naming %v, want %v", got, []control.Journal{s.source, own})
	}
	s.guest.store("disk", 0, 7)
	flushed := flush(s.guest)
	// A page the post-copy brings, stored into on the source.
	s.guest.store("disk", 1, 8)
	s.checkpoint(t)
	if got := s.record(t).Journals; len(got) != 2 || got[0] != s.source || got[1].Disk != own.Disk {
		t.Fatalf("a checkpoint paused during the post-copy names %v, want the source's journal and then this one", got)
	}
	if held := s.held(); held.Entries != 0 {
		t.Fatalf("the journal holds %+v of the VM during its post-copy, want nothing", held)
	}
	select {
	case err := <-flushed:
		t.Fatalf("a flush was answered during the post-copy: %v", err)
	default:
	}
	s.h.hosts[0].PostCopied("vm-1")
	if err := <-flushed; err != nil {
		t.Fatalf("the flush that waited for the post-copy: %v", err)
	}
	s.checkpoint(t)
	if got := s.record(t).Journals; len(got) != 1 || got[0].Disk != own.Disk || got[0].Epoch != own.Epoch {
		t.Fatalf("the first checkpoint paused after the post-copy names %v, want this host's journal alone", got)
	}
}

// A VM whose record still names its last source's journal is not handed over
// again: the destination's open would name a third. The refusal comes before
// the guest stops, and asks for the checkpoint that drops that journal.
func TestAHandoffIsRefusedWhileTheRecordNamesTwoJournals(t *testing.T) {
	s := arrivedJournalHost(t)
	_, err := s.h.hosts[0].Migrate(t.Context(), "vm-1", "elsewhere-pages")
	if !errors.Is(err, host.ErrNotMigratable) || !errors.Is(err, control.ErrTooManyJournals) {
		t.Fatalf("a handoff of a VM naming two journals = %v, want ErrNotMigratable for ErrTooManyJournals", err)
	}
	if got := s.h.hosts[0].Machines(); !slices.Equal(got, []string{"vm-1"}) {
		t.Fatalf("the refused handoff left this host running %v", got)
	}
}

// A drain's last step waits for the journal to hold no live entry: once the
// VM is stopped, its record names no journal, and the drain trims the entry
// away at once rather than at the trimming loop's interval.
func TestADrainWaitsForTheJournalToEmpty(t *testing.T) {
	s := journalHost(t)
	s.guest.store("disk", 0, 7)
	if err := <-flush(s.guest); err != nil {
		t.Fatal(err)
	}
	if held := s.held(); held.Entries != 1 {
		t.Fatalf("the journal holds %+v of the VM after its flush, want one entry", held)
	}
	if _, err := s.h.hosts[0].Stop(t.Context(), "vm-1", hostapi.StopRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := s.h.hosts[0].DrainJournal(t.Context(), time.Second); err != nil {
		t.Fatal(err)
	}
	if held := s.j.Held(); len(held) != 0 {
		t.Fatalf("the journal holds %+v after the drain waited for it", held)
	}
}
