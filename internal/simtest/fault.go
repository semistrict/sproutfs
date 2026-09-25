package simtest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// Fault is one thing that goes wrong to a deployment: it begins at an offset in
// the schedule, it ends at a later one, and something must be true once it has.
//
// A fault is ambient rather than attached to an operation. The campaigns before
// this one ran one fault per migration and asserted that migration's exact
// outcome, which is why no two faults ever overlapped; a fault that is a
// condition of the world instead — this link is blocked, that host is gone,
// this store does not answer — can be started beside two others and the
// operations underneath it go on happening. What an operation owes under a
// fault is not success: it is that the deployment is still a deployment, that
// no guest reads bytes it never wrote, and that every VM's selected checkpoint
// is one a writer of it published.
//
// Holds is what this fault in particular must leave behind once it is over and
// the world has quiesced. It is deliberately modest — the campaign's invariants
// are the assertions that matter — but it is what says a fault ended rather
// than merely stopped being injected: a partition that healed carries traffic,
// a host that came back runs its VMs, a store that recovered answers.
type Fault interface {
	// Name identifies the fault in the trace and in a failure. It names the
	// fault rather than its target, so a sweep can count the seeds that reached
	// each one.
	Name() string
	// Begin injects the fault.
	Begin(ctx context.Context, w *World) error
	// End removes it. It runs even when Begin failed, so it must tolerate a
	// fault that was never injected.
	End(ctx context.Context, w *World) error
	// Holds is what the fault must leave true once it has ended.
	Holds(ctx context.Context, w *World) error
}

// Faults draws the faults one seed runs. Every one of the migration chaos
// campaign's faults is here, as a condition of the world rather than a phase of
// one migration, plus the ones only a generated topology can express: a whole
// host lost and started again, a store one host cannot reach while another can,
// and a swizzle across every link at once.
//
// The targets are drawn here, from the seed, so the schedule below decides only
// when each fault happens.
func Faults(r sim.Random, t Topology) []Fault {
	hosts := len(t.Hosts)
	pick := func(id string) int { return r.Intn(id, hosts) }
	other := func(id string, from int) int {
		to := r.Intn(id, hosts-1)
		if to >= from {
			to++
		}
		return to
	}
	partitioned := pick("partition/from")
	return []Fault{
		StoreUnavailable(),
		HostLosesStore(pick("store/host")),
		PartitionedPages(partitioned, other("partition/to", partitioned)),
		SwizzledLinks(time.Duration(1+r.Intn("swizzle/window", 8)) * time.Second),
		LostPageReplies(pick("lost-reply/host"), 1+r.Intn("lost-reply/after", 2)),
		StalledStream(pick("stall/host")),
		LostHost(pick("lost-host/host")),
		RefusedStop(),
		RefusedStart(pick("refused-start/host")),
		DegradedLinks(),
	}
}

// The faults, one constructor each. They are exported so that a test can name a
// combination rather than wait for a seed to draw it: the combination this
// package exists for is a source partitioned while the store is unavailable
// while a second host takes the VM over, and a campaign that could only reach
// it by luck could not say it had.
func StoreUnavailable() Fault { return &storeUnavailable{} }

// HostLosesStore takes object storage away from one host and leaves it with
// every other.
func HostLosesStore(host int) Fault { return &hostLosesStore{host: host} }

// PartitionedPages separates two hosts from each other's page servers.
func PartitionedPages(from, to int) Fault { return &partitionedPages{from: from, to: to} }

// SwizzledLinks blocks and heals every link of the deployment over window.
func SwizzledLinks(window time.Duration) Fault { return &swizzledLinks{window: window} }

// LostPageReplies drops the reply to one host's page request after the source
// has answered it.
func LostPageReplies(host, after int) Fault { return &lostPageReplies{host: host, after: after} }

// StalledStream holds the first frame one host's post-copy receives.
func StalledStream(host int) Fault { return &stalledStream{host: host} }

// LostHost takes a whole host away and starts it again when the fault ends.
func LostHost(host int) Fault { return &lostHost{host: host} }

// RefusedStop fails every migration pause that begins while it is on, after the
// guest has already stopped.
func RefusedStop() Fault { return &refusedStop{} }

// RefusedStart fails one host's half of a receive before the guest is started.
func RefusedStart(host int) Fault { return &refusedStart{host: host} }

// RefusedStartAfter is RefusedStart from the after-th guest on: a destination
// that takes that many and can take no more, which is what a fan-out whose
// receive fails part way through meets.
func RefusedStartAfter(host, after int) Fault { return &refusedStart{host: host, after: after} }

// DegradedLinks duplicates, delays and slows what the page-server links
// carry.
func DegradedLinks() Fault { return &degradedLinks{} }

// ErrInjected is the failure a fault makes one phase report. It is this
// package's own error, so a test can tell a fault it injected from a failure it
// did not.
var ErrInjected = errors.New("simtest: injected failure")

// storeUnavailable takes object storage away from the whole deployment, which
// is what the migration campaign's metadata-unavailable fault did to one
// destination's open. Nothing can publish, open, take over or delete while it
// is on; every VM that is running goes on running out of its own pages.
type storeUnavailable struct{}

func (f *storeUnavailable) Name() string { return "store-unavailable" }

func (f *storeUnavailable) Begin(_ context.Context, w *World) error {
	w.runtime.ObjectStore().Fail()
	return nil
}

func (f *storeUnavailable) End(_ context.Context, w *World) error {
	w.runtime.ObjectStore().Recover()
	return nil
}

func (f *storeUnavailable) Holds(ctx context.Context, w *World) error {
	return storeAnswers(ctx, w.runtime.ObjectStore(), w.config.Prefix)
}

// hostLosesStore takes object storage away from one host while every other host
// still has it. That is the half of a store outage a deployment actually sees —
// one host in the dark while another takes its VMs over — and it is
// unreachable while the store is a single switch.
type hostLosesStore struct{ host int }

func (f *hostLosesStore) Name() string { return "host-loses-store" }

func (f *hostLosesStore) Begin(_ context.Context, w *World) error {
	w.hosts[f.host].objects.setFailed(true)
	return nil
}

func (f *hostLosesStore) End(_ context.Context, w *World) error {
	w.hosts[f.host].objects.setFailed(false)
	return nil
}

func (f *hostLosesStore) Holds(ctx context.Context, w *World) error {
	return storeAnswers(ctx, w.hosts[f.host].objects, w.config.Prefix)
}

// storeAnswers reports whether a store is carrying operations again, which is
// what a store fault has to leave behind.
func storeAnswers(ctx context.Context, store platform.ObjectStore, prefix platform.ObjectPrefix) error {
	if _, err := store.List(ctx, platform.ListRequest{Prefix: prefix, Limit: 1}); err != nil {
		return fmt.Errorf("the store does not answer after the fault ended: %w", err)
	}
	return nil
}

// partitionedPages separates one host from another's page server in both
// directions: a destination that cannot reach the source it is post-copying
// from, and a source that cannot answer. It is the migration campaign's
// source-partition, with the difference that it can be on while the store is
// away and a third host is taking the VM over.
type partitionedPages struct{ from, to int }

func (f *partitionedPages) Name() string { return "partitioned-pages" }

func (f *partitionedPages) Begin(_ context.Context, w *World) error {
	w.runtime.Network().PartitionBoth(w.hosts[f.from].address, w.hosts[f.to].pages)
	w.runtime.Network().PartitionBoth(w.hosts[f.to].address, w.hosts[f.from].pages)
	return nil
}

func (f *partitionedPages) End(_ context.Context, w *World) error {
	w.runtime.Network().HealBoth(w.hosts[f.from].address, w.hosts[f.to].pages)
	w.runtime.Network().HealBoth(w.hosts[f.to].address, w.hosts[f.from].pages)
	return nil
}

func (f *partitionedPages) Holds(_ context.Context, w *World) error {
	if w.runtime.Network().Clogged(w.hosts[f.from].address, w.hosts[f.to].pages) {
		return fmt.Errorf("%s still cannot reach %s", w.hosts[f.from].address, w.hosts[f.to].pages)
	}
	return nil
}

// swizzledLinks blocks every link among the hosts, their page servers and the
// store at its own seeded moment and heals each of them at another, so the
// order they come back in is not the order they went away in. It is
// FoundationDB's champion bug finder, and the one fault here that separates
// every pair of a generated topology at once.
type swizzledLinks struct {
	window time.Duration
	addrs  []platform.Address
}

func (f *swizzledLinks) Name() string { return "swizzled-links" }

func (f *swizzledLinks) Begin(_ context.Context, w *World) error {
	f.addrs = nil
	for _, h := range w.hosts {
		f.addrs = append(f.addrs, h.address, h.pages)
	}
	f.addrs = append(f.addrs, StoreAddress)
	w.runtime.Network().Swizzle(f.addrs, f.window, w.runtime.Random("simtest/swizzle"))
	return nil
}

// End heals what the schedule has not healed by itself. A swizzle is a
// schedule read through the clock, so most of it is over before this runs; the
// heal is what makes the end of the fault the end of the fault whatever the
// window was.
func (f *swizzledLinks) End(_ context.Context, w *World) error {
	for _, from := range f.addrs {
		for _, to := range f.addrs {
			if from != to {
				w.runtime.Network().Heal(from, to)
			}
		}
	}
	return nil
}

func (f *swizzledLinks) Holds(_ context.Context, w *World) error {
	for _, from := range f.addrs {
		for _, to := range f.addrs {
			if from != to && w.runtime.Network().Clogged(from, to) {
				return fmt.Errorf("%s still cannot reach %s", from, to)
			}
		}
	}
	return nil
}

// lostPageReplies drops the reply to one host's page request after the source
// has already read the pages out of its memory. It is the migration campaign's
// lost-page-reply: a connection that dies with the request answered, which the
// destination must survive by asking again.
type lostPageReplies struct {
	host  int
	after int
}

func (f *lostPageReplies) Name() string { return "lost-page-replies" }

func (f *lostPageReplies) Begin(_ context.Context, w *World) error {
	w.hosts[f.host].faults.dropAfter(f.after)
	return nil
}

func (f *lostPageReplies) End(_ context.Context, w *World) error {
	w.hosts[f.host].faults.clear()
	return nil
}

func (f *lostPageReplies) Holds(_ context.Context, w *World) error {
	return w.hosts[f.host].faults.quiet()
}

// stalledStream holds the first frame one host's post-copy receives until
// whatever asked for it is cancelled. It is the migration campaign's
// stream-canceled, and what it proves is that a destination whose stream is
// stopped reads its pages from its own checkpoint rather than from a source it
// is no longer talking to.
type stalledStream struct{ host int }

func (f *stalledStream) Name() string { return "stalled-stream" }

func (f *stalledStream) Begin(_ context.Context, w *World) error {
	w.hosts[f.host].faults.stall()
	return nil
}

func (f *stalledStream) End(_ context.Context, w *World) error {
	w.hosts[f.host].faults.clear()
	return nil
}

func (f *stalledStream) Holds(_ context.Context, w *World) error {
	return w.hosts[f.host].faults.quiet()
}

// lostHost takes a whole host away at a moment and starts it again when the
// fault ends: its guests stop existing, the pages they held are gone, its page
// server stops answering and its handles publish nothing ever again. It is the
// migration campaign's source-gone raised to the whole machine, and it is the
// only fault here that costs a VM anything it may legitimately lose: every VM
// that host was running comes back at the checkpoint its record selects.
type lostHost struct{ host int }

func (f *lostHost) Name() string { return "lost-host" }

func (f *lostHost) Begin(ctx context.Context, w *World) error { return w.LoseHost(ctx, f.host) }

func (f *lostHost) End(ctx context.Context, w *World) error { return w.RestartHost(ctx, f.host) }

func (f *lostHost) Holds(_ context.Context, w *World) error {
	if w.hosts[f.host].down {
		return fmt.Errorf("%s never came back", w.hosts[f.host].name)
	}
	return nil
}

// refusedStop fails a migration pause after the guest has already stopped and
// one memory region has been sealed. It is the migration campaign's stop-failed: the
// release must unseal that memory region and leave the guest running on the host it
// was already on, with nothing handed over.
//
// It is on every VM rather than one drawn from the seed, because what it is
// about is what an abandoned migration owes a guest, and a schedule that
// happened to migrate a different VM during the fault's window would have
// proved nothing about that.
type refusedStop struct{}

func (f *refusedStop) Name() string { return "refused-stop" }

func (f *refusedStop) Begin(_ context.Context, w *World) error {
	w.eachGuest(func(g *guest) {
		g.setRefuseStop(fmt.Errorf("%w: the guest could not be stopped", ErrInjected))
	})
	return nil
}

func (f *refusedStop) End(_ context.Context, w *World) error {
	w.eachGuest(func(g *guest) { g.setRefuseStop(nil) })
	return nil
}

// Holds requires every VM to be storing again. A pause that failed owes the
// guest its memory back: the vCPUs running, every memory region unsealed and every
// page writable.
func (f *refusedStop) Holds(_ context.Context, w *World) error {
	var errs []error
	for _, id := range w.Running() {
		_, g := w.runningVM(id)
		if g == nil {
			continue
		}
		if err := g.store(g.names[0], 0); err != nil {
			errs = append(errs, fmt.Errorf("%s cannot store after a refused stop: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// refusedStart fails one host's half of a receive before the guest is started.
// It is the migration campaign's start-failed: the destination could not start
// the VMM it was handed, so the VM is nobody's until somebody opens it.
type refusedStart struct {
	host int
	// after is how many guests this host still starts before it refuses, which
	// is what puts a refusal inside a fan-out rather than at its head.
	after int
}

func (f *refusedStart) Name() string { return "refused-start" }

func (f *refusedStart) Begin(_ context.Context, w *World) error {
	w.hosts[f.host].refuseStart = fmt.Errorf("%w: the destination could not start the guest", ErrInjected)
	w.hosts[f.host].startsBeforeRefusal = f.after
	return nil
}

func (f *refusedStart) End(_ context.Context, w *World) error {
	w.hosts[f.host].refuseStart = nil
	w.hosts[f.host].startsBeforeRefusal = 0
	return nil
}

func (f *refusedStart) Holds(_ context.Context, w *World) error {
	if w.hosts[f.host].refuseStart != nil {
		return fmt.Errorf("%s still refuses to start a guest", w.hosts[f.host].name)
	}
	return nil
}

// degradedLinks duplicates, delays and slows what the page-server links carry,
// which is the rest of the simulated network's kit that one fault at a time
// never reached alongside anything else.
//
// It does not drop a frame. A page server is reached over a reliable
// message-framed connection, on which a frame that vanishes while the
// connection stays open is not something a transport can do — and it has
// exactly one outcome here, because a guest's demand fault against the peer
// that holds the only copy of an unpublished page waits for its reply rather
// than giving up: the guest hangs for ever. Waiting is the right answer for
// that page, since giving up on it loses the guest's memory; what a lost reply
// really costs is the connection, which is what lostPageReplies models by
// ending it.
type degradedLinks struct{ links [][2]platform.Address }

func (f *degradedLinks) Name() string { return "degraded-links" }

func (f *degradedLinks) Begin(_ context.Context, w *World) error {
	network := w.runtime.Network()
	r := w.runtime.Random("simtest/degraded")
	f.links = nil
	for i, from := range w.hosts {
		for j, to := range w.hosts {
			if i == j {
				continue
			}
			id := fmt.Sprintf("%s->%s", from.address, to.pages)
			network.DuplicateNext(from.address, to.pages, 1+r.Intn(id+"/duplicate", 2))
			network.DelayNext(from.address, to.pages, r.Duration(id+"/delay", time.Second))
			network.SetLink(from.address, to.pages, sim.LinkConfig{
				Latency: r.Duration(id+"/latency", 50*time.Millisecond), Jitter: 10 * time.Millisecond})
			f.links = append(f.links, [2]platform.Address{from.address, to.pages})
		}
	}
	return nil
}

func (f *degradedLinks) End(_ context.Context, w *World) error {
	for _, link := range f.links {
		w.runtime.Network().ClearLink(link[0], link[1])
		// A duplicate or a delay still armed when the fault ends would land on
		// whatever the link carries next, which belongs to another step.
		w.runtime.Network().ClearFaults(link[0], link[1])
	}
	return nil
}

func (f *degradedLinks) Holds(_ context.Context, w *World) error {
	for _, link := range f.links {
		if w.runtime.Network().Clogged(link[0], link[1]) {
			return fmt.Errorf("%s still cannot reach %s", link[0], link[1])
		}
	}
	return nil
}

// connFaults is what one host's connections to a page server do while a fault
// is on it: nothing, drop the reply to a request the source has already
// answered, or hold the first frame until whatever asked for it gives up.
//
// It is consulted per frame rather than installed per connection, so a fault
// that begins while a post-copy is already running reaches that post-copy.
type connFaults struct {
	mu sync.Mutex
	// remaining counts the payload-bearing frames still to be delivered before
	// one is dropped. Zero is off.
	remaining int
	// stalling holds the first frame of any kind rather than delivering it.
	// Any frame will do: a destination whose source holds nothing it needs is
	// answered with an empty listing, and that listing is the only frame it
	// ever receives.
	stalling bool
	// held counts the frames being held right now, and released is closed when
	// the fault ends so that they stop being held. The fault does not end until
	// every one of them has let go, so that what it left behind is settled by
	// the time anything is required of it.
	held     sync.WaitGroup
	released chan struct{}
}

func (f *connFaults) dropAfter(count int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remaining = max(count, 1)
}

func (f *connFaults) stall() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stalling = true
	if f.released == nil {
		f.released = make(chan struct{})
	}
}

// clear ends the fault and lets go of every frame it is holding. A held frame
// whose reader is not cancelled would otherwise be held for ever, which is a
// harness that stops rather than a fault that ended.
func (f *connFaults) clear() {
	f.mu.Lock()
	f.remaining, f.stalling = 0, false
	if f.released != nil {
		close(f.released)
		f.released = nil
	}
	f.mu.Unlock()
	f.held.Wait()
}

// quiet reports that this host's connections are carrying frames again.
func (f *connFaults) quiet() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stalling || f.remaining != 0 {
		return errors.New("the connection fault is still installed")
	}
	return nil
}

type frameAction int

const (
	deliverFrame frameAction = iota
	dropFrame
	stallFrame
)

// classify decides what happens to one frame, and hands a stalled one the
// channel that says when the fault is over.
func (f *connFaults) classify(payload bool) (frameAction, chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stalling {
		f.stalling = false
		f.held.Add(1)
		return stallFrame, f.released
	}
	if payload && f.remaining > 0 {
		f.remaining--
		if f.remaining == 0 {
			return dropFrame, nil
		}
	}
	return deliverFrame, nil
}

func (f *connFaults) done() { f.held.Done() }

func (f *connFaults) wrap(conn platform.Conn) platform.Conn {
	return &faultyConn{Conn: conn, faults: f}
}

// faultyConn is one connection to a page server under this host's fault.
type faultyConn struct {
	platform.Conn
	faults *connFaults
}

func (c *faultyConn) Receive(ctx context.Context) (platform.ReceivedFrame, error) {
	frame, err := c.Conn.Receive(ctx)
	if err != nil {
		return frame, err
	}
	action, released := c.faults.classify(frame.PayloadSize > 0)
	switch action {
	case dropFrame:
		// The source read the pages out of its memory and this host never sees
		// them: the connection died with the request answered.
		if frame.Payload != nil {
			_ = frame.Payload.Close()
		}
		return platform.ReceivedFrame{}, platform.ErrClosed
	case stallFrame:
		defer c.faults.done()
		// The hold ends when whatever asked for this frame gives up, when the
		// fault ends, or when the connection has been silent long enough to be
		// dead. The last of those is what keeps a stall a fault rather than a
		// stopped harness: the caller may be a guest's own page fault, which
		// has no deadline of its own and would otherwise wait for a frame until
		// the end of the world.
		dead := time.NewTimer(Deadline)
		defer dead.Stop()
		select {
		case <-ctx.Done():
		case <-released:
		case <-dead.C:
		}
		if frame.Payload != nil {
			_ = frame.Payload.Close()
		}
		if err := context.Cause(ctx); err != nil {
			return platform.ReceivedFrame{}, err
		}
		return platform.ReceivedFrame{}, platform.ErrClosed
	}
	return frame, nil
}

// DroppedPageServerFrames drops the next few frames each page-server link
// carries, on top of duplicating, delaying and slowing them. It is the last of the
// simulated network's kit, and the one fault here the generated campaign does
// not draw: a frame dropped on an open connection has exactly one outcome for a
// guest's demand fault against the peer holding the only copy of an unpublished
// page, which is to wait for a reply that never comes. Waiting is the right
// answer for that page — giving up on it loses the guest's memory — so this is
// for a campaign whose every fetch is a bounded attempt that is retried, which
// is what a drain of a host that is going away does.
func DroppedPageServerFrames(after int) Fault { return &droppedPageServerFrames{after: after} }

type droppedPageServerFrames struct {
	after int
	links [][2]platform.Address
}

func (f *droppedPageServerFrames) Name() string { return "dropped-page-server-frames" }

func (f *droppedPageServerFrames) Begin(_ context.Context, w *World) error {
	network := w.runtime.Network()
	r := w.runtime.Random("simtest/dropped")
	f.links = nil
	for i, from := range w.hosts {
		for j, to := range w.hosts {
			if i == j {
				continue
			}
			id := fmt.Sprintf("%s->%s", from.address, to.pages)
			network.DropNext(from.address, to.pages, f.after+r.Intn(id+"/drop", 2))
			network.DuplicateNext(from.address, to.pages, 1+r.Intn(id+"/duplicate", 2))
			network.DelayNext(from.address, to.pages, r.Duration(id+"/delay", time.Second))
			network.SetLink(from.address, to.pages, sim.LinkConfig{
				Latency: r.Duration(id+"/latency", 50*time.Millisecond), Jitter: 10 * time.Millisecond})
			f.links = append(f.links, [2]platform.Address{from.address, to.pages})
		}
	}
	return nil
}

func (f *droppedPageServerFrames) End(_ context.Context, w *World) error {
	for _, link := range f.links {
		w.runtime.Network().ClearLink(link[0], link[1])
		// A drop, a duplicate or a delay still armed when the fault ends would
		// land on whatever the link carries next, which belongs to another step.
		w.runtime.Network().ClearFaults(link[0], link[1])
	}
	return nil
}

func (f *droppedPageServerFrames) Holds(_ context.Context, w *World) error {
	for _, link := range f.links {
		if w.runtime.Network().Clogged(link[0], link[1]) {
			return fmt.Errorf("%s still cannot reach %s", link[0], link[1])
		}
	}
	return nil
}
