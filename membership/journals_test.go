package membership

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// journalOf is journal disk n as the cloud lists it, attached to machines.
func journalOf(n byte, machines ...string) Shard {
	volume := fmt.Sprintf("sproutfs-journal-%d", n)
	return Shard{Disk: Disk{ID: JournalIdentity(volume), Volume: volume, Kind: Journal}, Machines: machines,
		Known: true}
}

// journalHost is host n on machine-n, reporting the journal disks it holds
// open, each with whether it holds no live entry.
func journalHost(n byte, held ...Disk) Host {
	host := hostOf(n)
	host.Machine = fmt.Sprintf("machine-%d", n)
	host.Disks = append(host.Disks, held...)
	return host
}

// holding is journal disk j as a host reports it open.
func holding(j Shard, empty bool) Disk {
	return Disk{ID: j.ID, Volume: j.Volume, Kind: Journal, Empty: empty}
}

// diskIn is the disk of identity id in m, failing the test where it is not
// listed.
func diskIn(t *testing.T, m Membership, id rank.Identity) Disk {
	t.Helper()
	disk, ok := m.Disk(id)
	if !ok {
		t.Fatalf("disk %s is not listed: %s", id, describe(m))
	}
	return disk
}

// A machine in the pool is reserved a free and empty journal disk, and the
// member on it is assigned that disk, attaching and then serving once its
// host reports it open. A journal disk ranks no window.
func TestAMemberIsAssignedTheJournalDiskReservedForItsMachine(t *testing.T) {
	j := journalOf(1)
	want := Want{Code: rank.Code{K: 1, M: 0}, Hosts: []Host{journalHost(1)}, Journals: []Shard{j},
		JournalsKnown: true, Pool: []string{"machine-1"}}
	m, _ := toward(t, Empty(), want)
	disk := diskIn(t, m, j.ID)
	if disk.State != Attaching || disk.Member != idOf(1) || disk.Machine != "machine-1" || disk.Empty {
		t.Fatalf("the journal disk is %+v, want attaching to member 1, reserved for machine-1, not empty", disk)
	}
	if slices.ContainsFunc(m.List().Caches(), func(cache rank.Cache) bool { return cache.Identity == j.ID }) {
		t.Fatal("a journal disk ranks windows")
	}
	want.Hosts = []Host{journalHost(1, holding(j, true))}
	m, _ = toward(t, m, want)
	if disk := diskIn(t, m, j.ID); disk.State != Serving || disk.Empty {
		t.Fatalf("the journal disk its writer reports open and empty is %+v, want serving and not empty", disk)
	}
}

// A controller creates a journal disk for a machine of the pool only where no
// free and empty disk is there to reserve, counting one the cloud lists that
// the membership does not yet.
func TestAJournalDiskIsCreatedOnlyWhereNoneIsFree(t *testing.T) {
	m, _ := toward(t, Empty(), Want{Code: rank.Code{K: 1, M: 0}, Journals: []Shard{journalOf(1)},
		JournalsKnown: true})
	pool := Want{Pool: []string{"machine-1", "machine-2"}}
	if got := NeedsJournal(m, pool); !slices.Equal(got, []string{"machine-2"}) {
		t.Fatalf("with one free disk for two machines the controller creates for %v, want [machine-2]", got)
	}
	pool.Journals = []Shard{journalOf(2)}
	if got := NeedsJournal(m, pool); len(got) != 0 {
		t.Fatalf("with a disk the cloud lists and the membership does not yet, it creates for %v, want none", got)
	}
}

// A draining host's journal disk is released, marked empty once its host
// reports no live entry, and let go once the host closed it and the cloud has
// it on no machine. Its reservation goes with the machine.
func TestADrainingHostsJournalIsLetGoOnceEmptyAndDetached(t *testing.T) {
	j := journalOf(1)
	want := Want{Code: rank.Code{K: 1, M: 0}, Hosts: []Host{journalHost(1, holding(j, false))},
		Journals: []Shard{journalOf(1, "machine-1")}, JournalsKnown: true, Pool: []string{"machine-1"}}
	m, _ := toward(t, Empty(), want)
	if disk := diskIn(t, m, j.ID); disk.State != Serving {
		t.Fatalf("the journal disk is %s, want serving", disk.State)
	}
	leaving := journalHost(1, holding(j, false))
	leaving.Leaving = true
	want.Hosts = []Host{leaving}
	m, _ = toward(t, m, want)
	if disk := diskIn(t, m, j.ID); disk.State != Releasing || disk.Empty {
		t.Fatalf("a draining host's journal disk with live entries is %+v, want releasing and not empty", disk)
	}
	leaving.Disks = []Disk{diskOf(1), holding(j, true)}
	want.Hosts = []Host{leaving}
	m, _ = toward(t, m, want)
	if disk := diskIn(t, m, j.ID); disk.State != Releasing || !disk.Empty {
		t.Fatalf("a draining host's journal disk reported empty is %+v, want releasing and empty", disk)
	}
	// A flush wrote it since: its host found live entries at the close and
	// opened it again.
	leaving.Disks = []Disk{diskOf(1), holding(j, false)}
	want.Hosts = []Host{leaving}
	m, _ = toward(t, m, want)
	if disk := diskIn(t, m, j.ID); disk.State != Releasing || disk.Empty {
		t.Fatalf("a disk marked empty whose host reports live entries is %+v, want releasing and unmarked", disk)
	}
	leaving.Disks = []Disk{diskOf(1), holding(j, true)}
	want.Hosts = []Host{leaving}
	m, _ = toward(t, m, want)
	leaving.Disks = []Disk{diskOf(1)}
	want.Hosts, want.Journals, want.Pool = []Host{leaving}, []Shard{journalOf(1)}, nil
	m, _ = toward(t, m, want)
	if disk := diskIn(t, m, j.ID); !disk.free() || !disk.Empty {
		t.Fatalf("the journal disk of a host gone with its machine is %+v, want free and empty", disk)
	}
}

// A lost host's journal disk, which may hold live entries, is assigned to a
// surviving member for reading once the cloud has detached it, and released
// once that member reports it holds none; it is then free and empty.
func TestALostHostsJournalIsReadOnASurvivorUntilEmpty(t *testing.T) {
	want := Want{Code: rank.Code{K: 2, M: 0}, Hosts: []Host{journalHost(1), journalHost(2)},
		Journals: []Shard{journalOf(1), journalOf(2)}, JournalsKnown: true, Pool: []string{"machine-1", "machine-2"}}
	m, _ := toward(t, Empty(), want)
	// Each host opens the disk reserved for its machine, which the cloud
	// has attached there.
	own := map[string]Shard{}
	for _, j := range want.Journals {
		disk := diskIn(t, m, j.ID)
		j.Machines = []string{disk.Machine}
		own[disk.Machine] = j
	}
	j1, j2 := own["machine-1"], own["machine-2"]
	want.Hosts = []Host{journalHost(1, holding(j1, false)), journalHost(2, holding(j2, false))}
	want.Journals = []Shard{j1, j2}
	m, _ = toward(t, m, want)
	// Host 1 and its machine are gone; the cloud detached its disk.
	lost := j1
	lost.Machines = nil
	want.Hosts = []Host{journalHost(2, holding(j2, false))}
	want.Journals, want.Pool = []Shard{lost, j2}, []string{"machine-2"}
	m, _ = toward(t, m, want)
	disk := diskIn(t, m, j1.ID)
	if disk.State != Attaching || disk.Member != idOf(2) || disk.Empty || disk.Machine != "" {
		t.Fatalf("the lost host's journal disk is %+v, want attaching to member 2 for reading, not empty", disk)
	}
	want.Hosts = []Host{journalHost(2, holding(j2, false), holding(j1, false))}
	m, _ = toward(t, m, want)
	if disk := diskIn(t, m, j1.ID); disk.State != Serving {
		t.Fatalf("the read disk its holder reports open is %s, want serving", disk.State)
	}
	want.Hosts = []Host{journalHost(2, holding(j2, false), holding(j1, true))}
	m, _ = toward(t, m, want)
	if disk := diskIn(t, m, j1.ID); disk.State != Releasing || !disk.Empty {
		t.Fatalf("the read disk reported empty is %+v, want releasing and empty", disk)
	}
	want.Hosts = []Host{journalHost(2, holding(j2, false))}
	m, _ = toward(t, m, want)
	if disk := diskIn(t, m, j1.ID); !disk.free() || !disk.Empty {
		t.Fatalf("the read disk once closed and detached is %+v, want free and empty", disk)
	}
	if disk := diskIn(t, m, j2.ID); disk.State != Serving || disk.Member != idOf(2) {
		t.Fatalf("the survivor's own journal disk is %+v, want serving for member 2", disk)
	}
}

// A free and empty journal disk that has expired is marked deleting, and
// removed once the cloud no longer lists it. Step refuses to delete a disk
// that is not empty, or to bring one back from deleting.
func TestAnExpiredJournalDiskIsDeletedAndRemoved(t *testing.T) {
	j := journalOf(1)
	want := Want{Code: rank.Code{K: 1, M: 0}, Journals: []Shard{j}, JournalsKnown: true}
	m, _ := toward(t, Empty(), want)
	want.Expired = []rank.Identity{j.ID}
	m, _ = toward(t, m, want)
	if disk := diskIn(t, m, j.ID); disk.State != Deleting {
		t.Fatalf("an expired free and empty journal disk is %s, want deleting", disk.State)
	}
	want.Journals, want.Expired = nil, nil
	m, _ = toward(t, m, want)
	if _, listed := m.Disk(j.ID); listed {
		t.Fatal("a deleted journal disk the cloud no longer lists is still listed")
	}
	live := built(t)(Empty().Add(Disk{ID: j.ID, Volume: j.Volume, Kind: Journal}))
	deleting := built(t)(live.Delete(j.ID))
	ctx := sim.WithRuntime(t.Context(), sim.New(sim.Config{}))
	if err := Step(ctx, live, deleting); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Step deleted a journal disk that is not empty: %v", err)
	}
}

// Carry attaches a journal disk reserved for a machine of the pool to that
// machine before any member there is assigned it, and detaches a free one.
func TestCarryAttachesAReservedJournalDiskAtOnce(t *testing.T) {
	j := journalOf(1, "machine-9")
	m := built(t)(Empty().Add(Disk{ID: j.ID, Volume: j.Volume, Kind: Journal, Empty: true}))
	m = built(t)(m.Reserve(j.ID, "machine-1"))
	want := Want{Journals: []Shard{j}, JournalsKnown: true, Pool: []string{"machine-1"}}
	ctx := sim.WithRuntime(t.Context(), sim.New(sim.Config{}))
	got := Carry(ctx, m, want)
	wantActions := []Action{{Volume: j.Volume, Machine: "machine-9"}, {Volume: j.Volume, Machine: "machine-1", Attach: true}}
	if !slices.Equal(got, wantActions) {
		t.Fatalf("Carry asked %+v, want %+v", got, wantActions)
	}
	m = built(t)(m.Reserve(j.ID, ""))
	if got := Carry(ctx, m, want); !slices.Equal(got, []Action{{Volume: j.Volume, Machine: "machine-9"}}) {
		t.Fatalf("Carry asked %+v of a free journal disk, want it detached", got)
	}
}

// journalWorld is a deployment with durable flush on, as a controller and its
// hosts see it: hosts each on a machine of their own, a cloud whose disks
// the controller makes and deletes, and hosts that open the journal disks
// the membership assigns them once the cloud has them on their machine. A
// disk holds live entries while live names it, which only its writer or the
// test changes; a host reports a disk it holds empty when it holds none.
type journalWorld struct {
	t       *testing.T
	ctx     context.Context
	cloud   *sim.NetworkDisks
	control *ShardControl
	m       Membership
	hosts   map[rank.Identity]*journalWorldHost
	live    map[string]bool
}

type journalWorldHost struct {
	id      rank.Identity
	machine string
	present bool
	leaving bool
	held    map[rank.Identity]string
}

func newJournalWorld(t *testing.T, ctx context.Context, runtime *sim.Runtime) *journalWorld {
	t.Helper()
	cloud := runtime.NewNetworkDisks(sim.NetworkDisksConfig{})
	store, err := NewStore(Config{ObjectStore: runtime.ObjectStore(), Entropy: runtime.NewEntropy("controller")})
	if err != nil {
		t.Fatal(err)
	}
	control := &ShardControl{Store: store, Disks: cloud, Journals: &JournalControl{Disks: cloud,
		Deployment: "d", Bytes: 1 << 30, Entropy: runtime.NewEntropy("journals")}}
	return &journalWorld{t: t, ctx: ctx, cloud: cloud, control: control, m: Empty(),
		hosts: map[rank.Identity]*journalWorldHost{}, live: map[string]bool{}}
}

// add starts host n on machine-n.
func (w *journalWorld) add(n byte) *journalWorldHost {
	h := &journalWorldHost{id: idOf(n), machine: fmt.Sprintf("machine-%d", n), present: true,
		held: map[rank.Identity]string{}}
	w.hosts[h.id] = h
	return h
}

// lose takes host n away with its machine, as a node the cloud deleted: its
// disks are detached, whatever they hold.
func (w *journalWorld) lose(n byte) {
	h := w.hosts[idOf(n)]
	h.present = false
	for _, volume := range h.held {
		for {
			if err := w.cloud.Detach(w.ctx, volume, h.machine); err == nil {
				break
			}
		}
	}
	h.held = map[rank.Identity]string{}
}

func (w *journalWorld) want() Want {
	want := Want{Code: rank.CodeFor(1)}
	for _, id := range slices.SortedFunc(maps.Keys(w.hosts), compareIdentities) {
		h := w.hosts[id]
		if !h.present {
			continue
		}
		host := Host{ID: h.id, Address: platform.Address("host-" + h.machine), Machine: h.machine, Leaving: h.leaving}
		for _, disk := range slices.SortedFunc(maps.Keys(h.held), compareIdentities) {
			volume := h.held[disk]
			host.Disks = append(host.Disks, Disk{ID: disk, Volume: volume, Kind: Journal, Empty: !w.live[volume]})
		}
		want.Hosts = append(want.Hosts, host)
		want.Pool = append(want.Pool, h.machine)
	}
	return want
}

// pass is one pass of the controller and then of every host. It reports
// whether anything changed.
func (w *journalWorld) pass() bool {
	w.t.Helper()
	m, changed, err := w.control.Pass(w.ctx, w.want())
	if err == nil || m.Generation() > 0 {
		w.m = m
	}
	if err != nil {
		changed = true
	}
	for _, id := range slices.SortedFunc(maps.Keys(w.hosts), compareIdentities) {
		h := w.hosts[id]
		if !h.present {
			continue
		}
		for disk, volume := range h.held {
			assigned, _ := w.m.Disk(disk)
			mine := assigned.Member == h.id &&
				(assigned.State == Attaching || assigned.State == Serving || assigned.State == Releasing)
			// A host that finds live entries at the close opens the disk again.
			if !mine || assigned.State == Releasing && assigned.Empty && !w.live[volume] {
				delete(h.held, disk)
				changed = true
			}
		}
		for _, disk := range w.m.Disks() {
			if disk.Kind != Journal || disk.Member != h.id || disk.State != Attaching && disk.State != Serving {
				continue
			}
			if _, held := h.held[disk.ID]; held || w.cloud.Attached(disk.Volume) != h.machine {
				continue
			}
			for _, other := range w.hosts {
				if _, also := other.held[disk.ID]; also && other != h {
					w.t.Fatalf("journal disk %s would be opened by two hosts: %s", disk.ID, describe(w.m))
				}
			}
			h.held[disk.ID] = disk.Volume
			changed = true
		}
	}
	w.check()
	return changed
}

// check holds after every pass: a disk a host holds is on its machine, and a
// disk with live entries is never deleted.
func (w *journalWorld) check() {
	w.t.Helper()
	for _, h := range w.hosts {
		for _, volume := range h.held {
			if got := w.cloud.Attached(volume); got != h.machine {
				w.t.Fatalf("%s holds journal disk %s attached to %q", h.machine, volume, got)
			}
		}
	}
	listed := w.listed()
	for volume, live := range w.live {
		if live && !slices.Contains(listed, volume) {
			w.t.Fatalf("journal disk %s with live entries was deleted: %s", volume, describe(w.m))
		}
	}
}

// listed is the journal disks the cloud has.
func (w *journalWorld) listed() []string {
	for {
		disks, err := w.cloud.List(w.ctx, JournalLabel, "d")
		if err == nil {
			var names []string
			for _, disk := range disks {
				names = append(names, disk.Name)
			}
			return names
		}
	}
}

// settle passes until nothing changes.
func (w *journalWorld) settle() {
	w.t.Helper()
	quiet := 0
	for range 2000 {
		if w.pass() {
			quiet = 0
			continue
		}
		if quiet++; quiet == 3 {
			return
		}
	}
	w.t.Fatalf("the journal disks never settled: %s", describe(w.m))
}

// own is the journal disk host n writes: the one reserved for its machine,
// which it holds and the membership has serving for it.
func (w *journalWorld) own(n byte) Disk {
	w.t.Helper()
	h := w.hosts[idOf(n)]
	for _, disk := range w.m.Disks() {
		if disk.Kind == Journal && disk.Machine == h.machine {
			if _, held := h.held[disk.ID]; !held || disk.State != Serving || disk.Member != h.id {
				w.t.Fatalf("%s's journal disk is %+v, held %v: %s", h.machine, disk, held, describe(w.m))
			}
			return disk
		}
	}
	w.t.Fatalf("no journal disk is reserved for %s: %s", h.machine, describe(w.m))
	return Disk{}
}

// The journal disks follow the machines, whatever the cloud and the store do:
// each machine of the pool is made a disk and writes it; a drained host's disk
// is kept until it holds nothing and reused by the next machine; a lost host's
// disk is read on a survivor until it holds nothing; and a disk free for an
// hour is deleted. No disk is held by two hosts or deleted with live entries.
func TestJournalDisksFollowTheMachines(t *testing.T) {
	for seed := range uint64(8) {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime := sim.New(sim.Config{Seed: seed, Buggify: seed > 0})
				ctx := sim.WithRuntime(t.Context(), runtime)
				w := newJournalWorld(t, ctx, runtime)
				w.add(1)
				w.add(2)
				w.settle()
				one, two := w.own(1), w.own(2)
				if listed := w.listed(); len(listed) != 2 {
					t.Fatalf("two machines are made %v, want two journal disks", listed)
				}
				w.live[one.Volume], w.live[two.Volume] = true, true

				// Host 2 drains: its disk is kept while it holds entries.
				w.hosts[idOf(2)].leaving = true
				w.settle()
				if disk := diskIn(t, w.m, two.ID); disk.State != Releasing || disk.Empty {
					t.Fatalf("a draining host's disk with live entries is %+v", disk)
				}
				w.live[two.Volume] = false
				w.settle()
				w.hosts[idOf(2)].present = false
				w.add(3)
				w.settle()
				if three := w.own(3); three.ID != two.ID {
					t.Fatalf("machine-3 was given %s, want the drained host's disk %s back: %s", three.Volume, two.Volume, describe(w.m))
				}
				if listed := w.listed(); len(listed) != 2 {
					t.Fatalf("after a drain and a join the cloud has %v, want the two disks reused", listed)
				}

				// Host 1 is lost with live entries: host 3 reads them.
				w.lose(1)
				w.settle()
				read := diskIn(t, w.m, one.ID)
				if read.State != Serving || read.Member != idOf(3) || read.Empty {
					t.Fatalf("the lost host's disk is %+v, want serving on host 3 for reading", read)
				}
				w.live[one.Volume] = false
				w.settle()
				if disk := diskIn(t, w.m, one.ID); !disk.free() || !disk.Empty {
					t.Fatalf("the read disk once it holds nothing is %+v, want free and empty", disk)
				}

				// A disk free for an hour is deleted.
				time.Sleep(DefaultJournalExpiry)
				w.settle()
				if _, listed := w.m.Disk(one.ID); listed || slices.Contains(w.listed(), one.Volume) {
					t.Fatalf("the disk free for an hour is still there: %s", describe(w.m))
				}
				w.own(3)
			})
		})
	}
}
