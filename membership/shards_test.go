package membership

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// shardWorld is a deployment of hosts and network disks as a controller sees
// it, small enough to step by hand: each host runs on a machine of its own,
// the cloud attaches each disk to at most one machine, and each host holds
// open the shards the membership it holds assigns it once the cloud has
// them on its machine, and closes them as soon as it does not. The
// controller takes Next's step and carries out what Carry asks, once a pass.
type shardWorld struct {
	t      *testing.T
	ctx    context.Context
	m      Membership
	code   rank.Code
	hosts  map[rank.Identity]*shardHost
	shards []Shard
	// attached is the machine each volume is on, and events what happened to
	// each shard, in order.
	attached map[string]string
	events   map[rank.Identity][]string
}

// shardHost is one host: its machine, whether it is leaving, whether it is
// there at all, and the shards it holds open.
type shardHost struct {
	id      rank.Identity
	machine string
	leaving bool
	present bool
	held    []rank.Identity
}

func newShardWorld(t *testing.T, ctx context.Context, shards int) *shardWorld {
	w := &shardWorld{t: t, ctx: ctx, m: Empty(), code: rank.Code{K: 2, M: 1},
		hosts: map[rank.Identity]*shardHost{}, attached: map[string]string{}, events: map[rank.Identity][]string{}}
	for n := range shards {
		volume := fmt.Sprintf("projects/p/zones/z/disks/shard-%d", n)
		w.shards = append(w.shards, Shard{Disk: Disk{ID: ShardIdentity(volume), Volume: volume, Weight: 4}})
	}
	return w
}

// add starts host n on machine-n.
func (w *shardWorld) add(n byte) *shardHost {
	h := &shardHost{id: idOf(n), machine: fmt.Sprintf("machine-%d", n), present: true}
	w.hosts[h.id] = h
	return h
}

func (w *shardWorld) want() Want {
	want := Want{Code: w.code}
	for _, id := range slices.SortedFunc(maps.Keys(w.hosts), compareIdentities) {
		h := w.hosts[id]
		if !h.present {
			continue
		}
		host := Host{ID: h.id, Address: platform.Address("host-" + h.machine), Machine: h.machine, Leaving: h.leaving}
		for _, held := range h.held {
			shard := w.shard(held)
			host.Disks = append(host.Disks, shard.Disk)
		}
		want.Hosts = append(want.Hosts, host)
	}
	for _, shard := range w.shards {
		shard.Known = true
		shard.Machines = nil
		if machine := w.attached[shard.Volume]; machine != "" {
			shard.Machines = []string{machine}
		}
		want.Shards = append(want.Shards, shard)
	}
	return want
}

func (w *shardWorld) shard(id rank.Identity) Shard {
	for _, shard := range w.shards {
		if shard.ID == id {
			return shard
		}
	}
	w.t.Fatalf("no shard %s", id)
	return Shard{}
}

func (w *shardWorld) note(id rank.Identity, event string) {
	w.events[id] = append(w.events[id], event)
}

// pass is one pass of the controller and then of every host: Next's step,
// what Carry asks of the cloud, and each host opening and closing. It
// reports whether anything changed.
func (w *shardWorld) pass() bool {
	w.t.Helper()
	changed := false
	if change, ok := Next(w.ctx, w.m, w.want()); ok {
		before := w.m
		w.m = stepped(w.t, w.m, change)
		for _, disk := range w.m.Disks() {
			was, listed := before.Disk(disk.ID)
			if !listed || was.State != disk.State || was.Member != disk.Member {
				w.note(disk.ID, disk.State.String())
			}
		}
		changed = true
	}
	for _, action := range Carry(w.ctx, w.m, w.want()) {
		id := ShardIdentity(action.Volume)
		switch {
		case action.Attach && w.attached[action.Volume] != "":
			// The cloud refuses a single-writer disk on a second machine.
			continue
		case action.Attach:
			w.attached[action.Volume] = action.Machine
			w.note(id, "attached to "+action.Machine)
		case w.attached[action.Volume] == action.Machine:
			// A detach takes the device from whoever holds it there.
			for _, h := range w.hosts {
				if h.machine == action.Machine && slices.Contains(h.held, id) {
					w.t.Fatalf("shard %s was detached from %s while its host held it: %s", id, action.Machine,
						describe(w.m))
				}
			}
			delete(w.attached, action.Volume)
			w.note(id, "detached")
		}
		changed = true
	}
	for _, id := range slices.SortedFunc(maps.Keys(w.hosts), compareIdentities) {
		h := w.hosts[id]
		if !h.present {
			continue
		}
		for _, held := range slices.Clone(h.held) {
			if disk, _ := w.m.Disk(held); disk.Member != h.id || disk.State != Attaching && disk.State != Serving {
				h.held = slices.DeleteFunc(h.held, func(shard rank.Identity) bool { return shard == held })
				w.note(held, "closed by "+h.machine)
				changed = true
			}
		}
		if h.leaving {
			continue
		}
		for _, disk := range w.m.Disks() {
			shard := w.shard(disk.ID)
			if disk.Member != h.id || disk.State != Attaching && disk.State != Serving ||
				w.attached[shard.Volume] != h.machine || slices.Contains(h.held, disk.ID) {
				continue
			}
			for _, other := range w.hosts {
				if other != h && slices.Contains(other.held, disk.ID) {
					w.t.Fatalf("shard %s would be opened by two hosts: %s", disk.ID, describe(w.m))
				}
			}
			h.held = append(h.held, disk.ID)
			w.note(disk.ID, "opened by "+h.machine)
			changed = true
		}
	}
	w.check()
	return changed
}

// check holds what must hold after every pass: no shard held by two hosts, a
// shard a host holds is on its machine and assigned to it, and a serving
// shard is held by its member alone.
func (w *shardWorld) check() {
	w.t.Helper()
	holders := map[rank.Identity][]rank.Identity{}
	for _, h := range w.hosts {
		for _, held := range h.held {
			holders[held] = append(holders[held], h.id)
			if w.attached[w.shard(held).Volume] != h.machine {
				w.t.Fatalf("%s holds shard %s attached to %q", h.machine, held, w.attached[w.shard(held).Volume])
			}
		}
	}
	for id, by := range holders {
		if len(by) > 1 {
			w.t.Fatalf("shard %s is held by %v", id, by)
		}
	}
	for _, disk := range w.m.Disks() {
		if by := holders[disk.ID]; disk.State == Serving && len(by) == 1 && by[0] != disk.Member {
			w.t.Fatalf("shard %s serves on %s and is held by %s", disk.ID, disk.Member, by[0])
		}
	}
}

// settle passes until nothing changes, and reports how many passes it took.
func (w *shardWorld) settle() int {
	w.t.Helper()
	for passes := 1; passes <= 400; passes++ {
		if !w.pass() {
			return passes
		}
	}
	w.t.Fatalf("the shards never settled: %s", describe(w.m))
	return 0
}

// load is how many shards each present host holds open.
func (w *shardWorld) load() map[string]int {
	load := map[string]int{}
	for _, h := range w.hosts {
		if h.present {
			load[h.machine] = len(h.held)
		}
	}
	return load
}

// everyShardServes fails unless every shard is serving, held by its member.
func (w *shardWorld) everyShardServes() {
	w.t.Helper()
	for _, shard := range w.shards {
		disk, listed := w.m.Disk(shard.ID)
		if !listed || disk.State != Serving {
			w.t.Fatalf("shard %s is not serving: %s", shard.ID, describe(w.m))
		}
		if h := w.hosts[disk.Member]; h == nil || !slices.Contains(h.held, shard.ID) {
			w.t.Fatalf("shard %s serves on %s, which does not hold it", shard.ID, disk.Member)
		}
	}
}

// A deployment's shards are listed released, assigned one at a time to the
// member serving the fewest, attached to its machine, opened there and
// served: six shards over three hosts serve two each.
func TestShardsJoinTheMembershipAndSpreadOverItsMembers(t *testing.T) {
	w := newShardWorld(t, t.Context(), 6)
	for n := byte(1); n <= 3; n++ {
		w.add(n)
	}
	w.settle()
	w.everyShardServes()
	if load := w.load(); !maps.Equal(load, map[string]int{"machine-1": 2, "machine-2": 2, "machine-3": 2}) {
		t.Fatalf("the shards are held %v, want two on each machine", load)
	}
	if got := w.m.List().Len(); got != 6 {
		t.Fatalf("windows are ranked over %d disks, want the six shards", got)
	}
}

// A host the autoscaler removes drains: each of its shards is released, its
// host closes it, the cloud detaches it, it is let go, assigned to another
// member, attached there, opened and served, in that order. The windows are
// ranked over the same shards throughout.
func TestAShardMovesOffAHostTheAutoscalerRemoves(t *testing.T) {
	w := newShardWorld(t, t.Context(), 6)
	for n := byte(1); n <= 3; n++ {
		w.add(n)
	}
	w.settle()
	ranks := rankedOver(w.m)
	leaving := w.hosts[idOf(2)]
	moved := slices.Clone(leaving.held)
	clear(w.events)
	leaving.leaving = true
	w.settle()
	for _, id := range moved {
		events := w.events[id]
		disk, _ := w.m.Disk(id)
		target := w.hosts[disk.Member].machine
		want := []string{"releasing", "closed by machine-2", "detached", "released", "attaching",
			"attached to " + target, "opened by " + target, "serving"}
		if !slices.Equal(events, want) {
			t.Fatalf("shard %s moved %v, want %v", id, events, want)
		}
	}
	leaving.present = false
	w.settle()
	w.everyShardServes()
	if _, listed := w.m.Member(idOf(2)); listed {
		t.Fatalf("the removed host is still a member: %s", describe(w.m))
	}
	if load := w.load(); !maps.Equal(load, map[string]int{"machine-1": 3, "machine-3": 3}) {
		t.Fatalf("the shards are held %v, want three on each remaining machine", load)
	}
	if !slices.Equal(rankedOver(w.m), ranks) {
		t.Fatal("removing a host changed the disks windows are ranked over")
	}
}

// rankedOver is the disks a membership ranks windows over, and their weights,
// wherever they are served.
func rankedOver(m Membership) []string {
	var disks []string
	for _, cache := range m.List().Caches() {
		disks = append(disks, fmt.Sprintf("%s/%d", cache.Identity, cache.Weight))
	}
	return disks
}

// A host that dies with its shards attached to its machine: its member
// drains and each shard is let go only once the cloud has detached it, then
// served elsewhere.
func TestADeadHostsShardsAreLetGoOnlyOnceDetached(t *testing.T) {
	w := newShardWorld(t, t.Context(), 4)
	for n := byte(1); n <= 2; n++ {
		w.add(n)
	}
	w.settle()
	dead := w.hosts[idOf(2)]
	moved := slices.Clone(dead.held)
	dead.present, dead.held = false, nil
	clear(w.events)
	w.settle()
	for _, id := range moved {
		events := w.events[id]
		if at := slices.Index(events, "released"); at < 1 || events[at-1] != "detached" {
			t.Fatalf("shard %s of a dead host went %v, want it detached before it was released", id, events)
		}
	}
	w.everyShardServes()
	if load := w.load(); !maps.Equal(load, map[string]int{"machine-1": 4}) {
		t.Fatalf("the shards are held %v, want all four on the host left", load)
	}
}

// Shards spread when a host joins: one at a time, released by the member
// serving the most, until no member serves two more than another.
func TestShardsSpreadOverAHostThatJoins(t *testing.T) {
	w := newShardWorld(t, t.Context(), 4)
	w.add(1)
	w.settle()
	w.add(2)
	for w.pass() {
		moving := 0
		for _, disk := range w.m.Disks() {
			if disk.State == Attaching || disk.State == Releasing {
				moving++
			}
		}
		if moving > 1 {
			t.Fatalf("%d shards move at once: %s", moving, describe(w.m))
		}
	}
	w.everyShardServes()
	if load := w.load(); !maps.Equal(load, map[string]int{"machine-1": 2, "machine-2": 2}) {
		t.Fatalf("the shards are held %v, want two on each machine", load)
	}
}

// releasingShard is a membership in which one shard is releasing from member
// 1, and the want of a host that no longer holds it.
func releasingShard(t *testing.T) (Membership, Want, Shard) {
	t.Helper()
	shard := Shard{Disk: Disk{ID: ShardIdentity("shard-0"), Volume: "shard-0", Weight: 1}, Known: true}
	m := Empty()
	m = stepped(t, m, func(m Membership) (Membership, error) { return m.Join(memberOf(1)) })
	m = stepped(t, m, func(m Membership) (Membership, error) { return m.Add(shard.Disk) })
	m = stepped(t, m, func(m Membership) (Membership, error) { return m.Assign(shard.ID, idOf(1)) })
	m = stepped(t, m, func(m Membership) (Membership, error) { return m.Serve(shard.ID, idOf(1)) })
	m = stepped(t, m, func(m Membership) (Membership, error) { return m.Release(shard.ID) })
	host := Host{ID: idOf(1), Address: memberOf(1).Address, Machine: "machine-1"}
	return m, Want{Code: m.Code(), Hosts: []Host{host}}, shard
}

// A releasing shard whose host has closed it is let go only once the cloud
// says it is attached to no machine: not while it is attached, and not while
// the cloud's answer is unknown.
func TestAShardIsLetGoOnlyOnceTheCloudHasItOnNoMachine(t *testing.T) {
	ctx := sim.WithRuntime(t.Context(), sim.New(sim.Config{}))
	m, want, shard := releasingShard(t)
	for _, cloud := range []Shard{
		{Disk: shard.Disk, Known: true, Machines: []string{"machine-1"}},
		{Disk: shard.Disk, Known: false},
	} {
		want.Shards = []Shard{cloud}
		if change, ok := Next(ctx, m, want); ok {
			next := built(t)(change(m))
			if disk, _ := next.Disk(shard.ID); disk.State == Released {
				t.Fatalf("a shard the cloud has on %v (known %v) was let go", cloud.Machines, cloud.Known)
			}
		}
	}
	want.Shards = []Shard{{Disk: shard.Disk, Known: true}}
	change, ok := Next(ctx, m, want)
	if !ok {
		t.Fatal("a closed and detached shard was not let go")
	}
	if disk, _ := built(t)(change(m)).Disk(shard.ID); disk.State != Released {
		t.Fatalf("the step taken left the shard %s, want released", disk.State)
	}
}

// Carry leaves a releasing shard on its machine while its host holds it
// open, and detaches it from every machine the cloud lists once the host has
// closed it.
func TestCarryDetachesAReleasingShardOnlyOnceItsHostClosedIt(t *testing.T) {
	ctx := sim.WithRuntime(t.Context(), sim.New(sim.Config{}))
	m, want, shard := releasingShard(t)
	want.Shards = []Shard{{Disk: shard.Disk, Known: true, Machines: []string{"machine-1"}}}
	want.Hosts[0].Disks = []Disk{shard.Disk}
	if actions := Carry(ctx, m, want); len(actions) != 0 {
		t.Fatalf("Carry asked %v of a shard its host still holds", actions)
	}
	want.Hosts[0].Disks = nil
	if actions := Carry(ctx, m, want); !slices.Equal(actions, []Action{{Volume: "shard-0", Machine: "machine-1"}}) {
		t.Fatalf("Carry asked %v of a closed shard, want it detached from machine-1", actions)
	}
}

// Whatever a crash left the cloud holding, Carry asks for what the membership
// says: a released shard still attached is detached, and a shard assigned to
// a member while attached to another machine is detached there and attached
// to its member's.
func TestCarryConvergesWhateverACrashLeftAttached(t *testing.T) {
	ctx := t.Context()
	shard := Shard{Disk: Disk{ID: ShardIdentity("shard-0"), Volume: "shard-0", Weight: 1}, Known: true,
		Machines: []string{"machine-9"}}
	m := Empty()
	m = stepped(t, m, func(m Membership) (Membership, error) { return m.Join(memberOf(1)) })
	m = stepped(t, m, func(m Membership) (Membership, error) { return m.Add(shard.Disk) })
	want := Want{Code: m.Code(), Hosts: []Host{{ID: idOf(1), Address: memberOf(1).Address, Machine: "machine-1"}},
		Shards: []Shard{shard}}
	if actions := Carry(ctx, m, want); !slices.Equal(actions, []Action{{Volume: "shard-0", Machine: "machine-9"}}) {
		t.Fatalf("a released shard attached to machine-9 is asked %v, want detached", actions)
	}
	m = stepped(t, m, func(m Membership) (Membership, error) { return m.Assign(shard.ID, idOf(1)) })
	want2 := []Action{{Volume: "shard-0", Machine: "machine-9"}, {Volume: "shard-0", Machine: "machine-1", Attach: true}}
	if actions := Carry(ctx, m, want); !slices.Equal(actions, want2) {
		t.Fatalf("a shard assigned to machine-1 and attached to machine-9 is asked %v, want %v", actions, want2)
	}
	want.Shards[0].Machines = []string{"machine-1"}
	if actions := Carry(ctx, m, want); len(actions) != 0 {
		t.Fatalf("a shard where its member is is asked %v", actions)
	}
	want.Shards[0].Known = false
	want.Shards[0].Machines = nil
	if actions := Carry(ctx, m, want); len(actions) != 0 {
		t.Fatalf("a shard whose attachments the cloud did not report is asked %v", actions)
	}
}

// A pass of the controller reads where the cloud has each shard and its size,
// takes one step, and asks the cloud for what the membership it leaves calls
// for: a shard listed, assigned and attached in three passes. A shard the
// cloud could not describe keeps the weight the membership lists it with,
// and one not listed yet is left out until it can be described.
func TestAShardControlPassStepsAndCarriesTheStepOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		cloud := runtime.NewNetworkDisks(sim.NetworkDisksConfig{})
		if err := cloud.Create(ctx, "shard-0", 64<<30); err != nil {
			t.Fatal(err)
		}
		store, err := NewStore(Config{ObjectStore: runtime.ObjectStore()})
		if err != nil {
			t.Fatal(err)
		}
		control := &ShardControl{Store: store, Disks: cloud, Volumes: []string{"shard-0", "gone"}}
		host := Host{ID: idOf(1), Address: memberOf(1).Address, Machine: "machine-1"}
		want := Want{Code: rank.CodeFor(1), Hosts: []Host{host}}
		id := ShardIdentity("shard-0")
		var m Membership
		for range 4 {
			if m, _, err = control.Pass(ctx, want); err != nil {
				t.Fatal(err)
			}
		}
		disk, listed := m.Disk(id)
		if !listed || disk.State != Attaching || disk.Member != idOf(1) || disk.Weight != 4 {
			t.Fatalf("after four passes the shard is %+v (listed %v), want attaching to member 1 at weight 4", disk,
				listed)
		}
		if _, listed := m.Disk(ShardIdentity("gone")); listed {
			t.Fatal("a shard the cloud does not have was listed")
		}
		if got := cloud.Attached("shard-0"); got != "machine-1" {
			t.Fatalf("the pass left the shard attached to %q, want machine-1", got)
		}
		// Nothing more is called for until the host reports the shard open.
		if _, changed, err := control.Pass(ctx, want); err != nil || changed {
			t.Fatalf("a pass with nothing to do changed something (%v): %v", changed, err)
		}
		described := control.Describe(ctx, m)
		if len(described) != 1 || !described[0].Known || described[0].Weight != 4 ||
			!slices.Equal(described[0].Machines, []string{"machine-1"}) {
			t.Fatalf("the shards are described as %+v", described)
		}
		flaky := &ShardControl{Store: store, Disks: describeFails{NetworkDisks: cloud}, Volumes: []string{"shard-0"}}
		described = flaky.Describe(ctx, m)
		if len(described) != 1 || described[0].Known || described[0].Weight != 4 || described[0].Machines != nil {
			t.Fatalf("a listed shard the cloud did not describe is %+v, want unknown at its listed weight 4",
				described)
		}
		if described = flaky.Describe(ctx, Empty()); len(described) != 0 {
			t.Fatalf("an unlisted shard the cloud did not describe is %+v, want it left out", described)
		}
	})
}

// describeFails is a cloud that describes no disk.
type describeFails struct {
	platform.NetworkDisks
}

func (describeFails) Describe(context.Context, string) (platform.NetworkDisk, error) {
	return platform.NetworkDisk{}, platform.ErrUnavailable
}
