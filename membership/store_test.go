package membership

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// history is every generation the object store applied, in its order.
type history struct {
	mu      sync.Mutex
	applied []Membership
	broken  []error
}

// watch records every write of the membership the store applies.
func watch(store *sim.ObjectStore) *history {
	h := &history{}
	store.Observe(func(change sim.ObjectChange) {
		if change.Key != ObjectName {
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		m, err := Unmarshal(change.Value)
		if err != nil {
			h.broken = append(h.broken, err)
			return
		}
		h.applied = append(h.applied, m)
	})
	return h
}

// check requires the history to be one line of generations from 1, each
// admitted by Step after the one before it, and each written by one write.
func (h *history) check(t *testing.T) []Membership {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, err := range h.broken {
		t.Errorf("the store applied a membership that does not parse: %v", err)
	}
	previous := Empty()
	nonces := map[string]bool{}
	for _, m := range h.applied {
		if err := Step(context.Background(), previous, m); err != nil {
			t.Errorf("generation %d followed %d against the rules: %v\nbefore: %s\nafter: %s", m.Generation(),
				previous.Generation(), err, describe(previous), describe(m))
		}
		if nonces[string(m.nonce)] {
			t.Errorf("generation %d repeats a nonce", m.Generation())
		}
		nonces[string(m.nonce)] = true
		previous = m
	}
	return slices.Clone(h.applied)
}

// countingStore counts the reads that reach the store beneath it.
type countingStore struct {
	platform.ObjectStore
	gets atomic.Int64
}

func (s *countingStore) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	s.gets.Add(1)
	return s.ObjectStore.Get(ctx, request)
}

// storeOver is a store over objects that draws its nonces from the seed.
func storeOver(t *testing.T, runtime *sim.Runtime, objects platform.ObjectStore, name string) *Store {
	t.Helper()
	store, err := NewStore(Config{ObjectStore: objects, Entropy: runtime.NewEntropy(name)})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// join is the change that joins member n with its disk.
func join(n byte) func(Membership) (Membership, error) {
	return func(m Membership) (Membership, error) { return m.Join(memberOf(n), diskOf(n)) }
}

// A write whose reply was lost is read back, found by its nonce, and is done:
// the object holds it once, at the generation after the one read.
func TestALostReplyIsFoundByItsNonce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		applied := watch(runtime.ObjectStore())
		store := storeOver(t, runtime, runtime.ObjectStore(), "writer")
		runtime.ObjectStore().FailNextAfterApply(sim.ObjectPut, 1)
		m, err := store.Update(ctx, join(1))
		if err != nil {
			t.Fatal(err)
		}
		if got := applied.check(t); len(got) != 1 || !got[0].Equal(m) || m.Generation() != 1 {
			t.Fatalf("the store applied %d generations, and the update returned %s", len(got), describe(m))
		}
		if runtime.Probes()[ProbeReplyReconciled] != 1 {
			t.Fatalf("probes %v, want the lost reply reconciled once", runtime.Probes())
		}
	})
}

// Two writers that read one generation both change it: one wins, and the
// other reads again and writes the generation after, with both changes.
func TestTwoWritersOfOneGenerationEachWriteOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		applied := watch(runtime.ObjectStore())
		first := storeOver(t, runtime, runtime.ObjectStore(), "first")
		second := storeOver(t, runtime, runtime.ObjectStore(), "second")
		var wg sync.WaitGroup
		for n, store := range []*Store{first, second} {
			wg.Go(func() {
				if _, err := store.Update(ctx, join(byte(n+1))); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		got := applied.check(t)
		if len(got) != 2 || len(got[1].Members()) != 2 {
			t.Fatalf("the store applied %d generations, the last %s", len(got), describe(got[len(got)-1]))
		}
		if runtime.Probes()[ProbeWriteRaced] != 1 {
			t.Fatalf("probes %v, want one write that lost the race", runtime.Probes())
		}
	})
}

// A write the store refuses writes nothing, and the update says so; a change
// with nothing to do writes nothing either; and a change Step refuses is
// never written.
func TestAnUpdateWritesNothingItCannotWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		applied := watch(runtime.ObjectStore())
		store := storeOver(t, runtime, runtime.ObjectStore(), "writer")
		runtime.ObjectStore().FailNext(sim.ObjectPut, 1)
		if _, err := store.Update(ctx, join(1)); !errors.Is(err, platform.ErrInjectedFault) {
			t.Fatalf("an update the store refused said %v", err)
		}
		if len(applied.check(t)) != 0 {
			t.Fatal("a refused write was applied")
		}
		if _, err := store.Update(ctx, join(1)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Update(ctx, join(1)); !errors.Is(err, ErrUnchanged) {
			t.Fatalf("a join of a member listed said %v, want ErrUnchanged", err)
		}
		moved := func(m Membership) (Membership, error) { return m.Assign(diskOf(1).ID, idOf(2)) }
		if _, err := store.Update(ctx, join(2)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Update(ctx, moved); !errors.Is(err, ErrInvalid) {
			t.Fatalf("assigning an attaching disk to another member said %v, want ErrInvalid", err)
		}
		if got := applied.check(t); len(got) != 2 {
			t.Fatalf("the store applied %d generations, want 2", len(got))
		}
	})
}

// Two members both believe they should hold one disk: the second asks for it
// while the first serves it. The store assigns it to the second only once the
// first has released it and said it let it go, so the disk is never assigned
// to two members, and the generation that assigns it to the second is later
// than every generation of the first's.
func TestADiskIsAssignedToASecondMemberOnlyOnceTheFirstLetItGo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		applied := watch(runtime.ObjectStore())
		first := storeOver(t, runtime, runtime.ObjectStore(), "first")
		second := storeOver(t, runtime, runtime.ObjectStore(), "second")
		for n := byte(1); n <= 2; n++ {
			if _, err := first.Update(ctx, join(n)); err != nil {
				t.Fatal(err)
			}
			serve := func(m Membership) (Membership, error) { return m.Serve(diskOf(n).ID, idOf(n)) }
			if _, err := first.Update(ctx, serve); err != nil {
				t.Fatal(err)
			}
		}
		disk := diskOf(1).ID
		steal := func(m Membership) (Membership, error) { return m.Assign(disk, idOf(2)) }
		if _, err := second.Update(ctx, steal); !errors.Is(err, ErrInvalid) {
			t.Fatalf("assigning a disk its member serves to another said %v, want ErrInvalid", err)
		}
		if _, err := second.Update(ctx, func(m Membership) (Membership, error) { return m.Release(disk) }); err != nil {
			t.Fatal(err)
		}
		if _, err := second.Update(ctx, steal); !errors.Is(err, ErrInvalid) {
			t.Fatalf("assigning a releasing disk said %v, want ErrInvalid", err)
		}
		before, err := first.Update(ctx, func(m Membership) (Membership, error) { return m.Let(disk, idOf(1)) })
		if err != nil {
			t.Fatal(err)
		}
		after, err := second.Update(ctx, steal)
		if err != nil {
			t.Fatal(err)
		}
		found, _ := after.Disk(disk)
		if found.Member != idOf(2) || found.Assigned <= before.Generation() {
			t.Fatalf("the disk moved to %s at %d, after generation %d", found.Member, found.Assigned,
				before.Generation())
		}
		applied.check(t)
	})
}

// A view reads the object only when a request names a generation it does
// not hold, once for all the requests that wait on that read, and never
// moves back to an older generation.
func TestAViewReadsOnlyWhenBehindAndNeverGoesBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		counted := &countingStore{ObjectStore: runtime.ObjectStore()}
		writer := storeOver(t, runtime, runtime.ObjectStore(), "writer")
		reader := storeOver(t, runtime, counted, "reader")
		view := NewView(ctx, ViewConfig{Store: reader, Initial: Alone(memberOf(1), diskOf(1)), Interval: -1})
		defer view.Close()
		if _, err := writer.Update(ctx, join(1)); err != nil {
			t.Fatal(err)
		}
		if got, err := view.Catch(ctx, 0); err != nil || got.Generation() != 0 || counted.gets.Load() != 0 {
			t.Fatalf("a view asked for a generation it holds read %d times and holds %d", counted.gets.Load(),
				got.Generation())
		}
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				if got, err := view.Catch(ctx, 1); err != nil || got.Generation() != 1 {
					t.Errorf("a view caught up to %d with %v", got.Generation(), err)
				}
			})
		}
		wg.Wait()
		if counted.gets.Load() != 1 {
			t.Fatalf("four requests behind the view read the object %d times, want once", counted.gets.Load())
		}
		runtime.ObjectStore().FailNext(sim.ObjectGet, 1)
		if got, err := view.Catch(ctx, 2); err == nil || got.Generation() != 1 {
			t.Fatalf("a view whose read failed holds %d with %v, want 1 and the failure", got.Generation(), err)
		}
		if status := view.Status(); status.Failures != 1 || status.Reads != 1 || status.Error == "" {
			t.Fatalf("status %+v", status)
		}
		select {
		case <-view.Changed():
			t.Fatal("a view that read nothing new says it changed")
		default:
		}
		changed := view.Changed()
		if _, err := writer.Update(ctx, join(2)); err != nil {
			t.Fatal(err)
		}
		if got, err := view.Refresh(ctx); err != nil || got.Generation() != 2 {
			t.Fatalf("a refresh holds %d with %v", got.Generation(), err)
		}
		<-changed
	})
}

// A view reads the object as it starts and then on its timer.
func TestAViewReadsOnItsTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		clock := runtime.NewClock("host")
		writer := storeOver(t, runtime, runtime.ObjectStore(), "writer")
		if _, err := writer.Update(ctx, join(1)); err != nil {
			t.Fatal(err)
		}
		view := NewView(ctx, ViewConfig{Store: writer, Initial: Empty(), Clock: clock})
		defer view.Close()
		// A read of the store takes the store's latency on the bubble's
		// clock; the view's timer is on its own.
		settle := func() {
			time.Sleep(time.Second)
			synctest.Wait()
		}
		settle()
		if got := view.Current().Generation(); got != 1 {
			t.Fatalf("a view that started holds %d, want 1", got)
		}
		if _, err := writer.Update(ctx, join(2)); err != nil {
			t.Fatal(err)
		}
		clock.Advance(DefaultInterval - 1)
		settle()
		if got := view.Current().Generation(); got != 1 {
			t.Fatalf("a view holds %d before its interval, want 1", got)
		}
		clock.Advance(1)
		settle()
		if got := view.Current().Generation(); got != 2 {
			t.Fatalf("a view holds %d after its interval, want 2", got)
		}
	})
}

// The registered probes and sites of a store and a view, which the campaign
// must reach.
var (
	storeProbes = []string{ProbeReplyReconciled, ProbeWriteRaced, ProbeWriteUnsettled, ProbeViewAdopted,
		ProbeViewKept}
	storeSites = []string{buggifyReadFails, buggifyWriteFails, buggifyReplyLost}
)

// shared is the disk the campaign's movers take from each other.
var shared = Disk{ID: idOf(200), Volume: "shared", Weight: 2}

// take is one step of a mover towards holding the shared disk: release it
// from whoever holds it, say it is released, assign it, and serve it.
func take(mover byte) func(Membership) (Membership, error) {
	return func(m Membership) (Membership, error) {
		disk, ok := m.Disk(shared.ID)
		switch {
		case !ok:
			return m.Add(shared)
		case disk.State == Released:
			return m.Assign(shared.ID, idOf(mover))
		case disk.Member == idOf(mover) && disk.State == Attaching:
			return m.Serve(shared.ID, idOf(mover))
		case disk.Member == idOf(mover):
			return Membership{}, ErrUnchanged
		case disk.State == Releasing:
			return m.Let(shared.ID, disk.Member)
		}
		return m.Release(shared.ID)
	}
}

// letGo says member has let disk go, if disk is releasing for it.
func letGo(disk, member rank.Identity) func(Membership) (Membership, error) {
	return func(m Membership) (Membership, error) {
		if found, ok := m.Disk(disk); !ok || found.Member != member || found.State != Releasing {
			return Membership{}, ErrUnchanged
		}
		return m.Let(disk, member)
	}
}

// campaignSeeds are the seeds the campaign runs, which between them fire
// every site and reach every probe.
var campaignSeeds = []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

// Several writers change one membership at once: four members each join with
// a disk, serve it, set its weight three times, and two of them drain and
// leave, while two of them also take a shared disk from each other again and
// again. Replies are lost after the store applied a write, writes and reads
// fail, and the store goes down and comes back, as a seeded scheduler and the
// sites choose. Whatever they choose, the store applies one line of
// generations from 1, each one write that Step admits after the one before;
// every update that returned is the generation the store holds at its
// number; and no update is lost: at the end each member's disk has its last
// weight, or the member has left. Across the seeds every site fires and
// every probe is reached.
func TestConcurrentWritersNeverLoseAnUpdateOrGoBack(t *testing.T) {
	probes, fired := map[string]uint64{}, map[string]uint64{}
	for _, seed := range campaignSeeds {
		t.Run("seed-"+string(rune('a'+seed)), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime := writersCampaign(t, seed)
				for name, count := range runtime.Probes() {
					probes[name] += count
				}
				for name, count := range runtime.FiredSites() {
					fired[name] += count
				}
			})
		})
	}
	var missed []string
	for _, name := range slices.Concat(storeProbes, storeSites) {
		if probes[name]+fired[name] == 0 {
			missed = append(missed, name)
		}
	}
	if len(missed) != 0 {
		t.Fatalf("the campaign never reached %v; it reached probes %v and fired %v", missed, probes, fired)
	}
}

// acked is one update that returned, and what it returned.
type acked struct {
	writer string
	m      Membership
}

// writersCampaign runs one seed of the campaign and returns its runtime.
func writersCampaign(t *testing.T, seed uint64) *sim.Runtime {
	scheduler := sim.NewScheduler(seed)
	runtime := sim.New(sim.Config{Seed: seed, Wait: scheduler.Wait, Buggify: true})
	random := runtime.Random("writers-campaign")
	objects := runtime.ObjectStore()
	applied := watch(objects)
	var mu sync.Mutex
	var acks []acked
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx := sim.WithRuntime(t.Context(), runtime)
		// update makes one change until it is made, as a writer that is told
		// nothing it can act on does: every change here is idempotent, so a
		// write whose outcome was lost is made again or found done.
		update := func(ctx context.Context, name string, store *Store, change func(Membership) (Membership, error)) {
			for {
				m, err := store.Update(ctx, change)
				switch {
				case err == nil:
					mu.Lock()
					acks = append(acks, acked{writer: name, m: m})
					mu.Unlock()
					return
				case errors.Is(err, ErrUnchanged):
					return
				case errors.Is(err, ErrInvalid), errors.Is(err, ErrCorrupt), ctx.Err() != nil:
					t.Errorf("%s: %v", name, err)
					return
				}
			}
		}
		var writers sync.WaitGroup
		for n := byte(1); n <= 4; n++ {
			name := "member-" + string(rune('0'+n))
			writerCtx := sim.WithTask(ctx, name)
			store := storeOver(t, runtime, objects, name)
			writers.Go(func() {
				update(writerCtx, name, store, join(n))
				update(writerCtx, name, store, func(m Membership) (Membership, error) { return m.Serve(diskOf(n).ID, idOf(n)) })
				for weight := uint32(2); weight <= 4; weight++ {
					update(writerCtx, name, store, func(m Membership) (Membership, error) { return m.Weigh(diskOf(n).ID, weight) })
				}
				if n <= 2 {
					for round := range 6 {
						_ = round
						update(writerCtx, name, store, take(n))
					}
				}
				if n%2 == 0 {
					for _, change := range []func(Membership) (Membership, error){
						func(m Membership) (Membership, error) { return m.Drain(idOf(n)) },
						func(m Membership) (Membership, error) { return m.Let(diskOf(n).ID, idOf(n)) },
						letGo(shared.ID, idOf(n)),
						func(m Membership) (Membership, error) { return m.Remove(diskOf(n).ID) },
					} {
						update(writerCtx, name, store, change)
					}
				}
			})
		}
		// The world takes the store down and brings it back, and loses
		// replies and fails requests, at moments the scheduler chooses
		// against the writers.
		world := sim.WithTask(ctx, "world")
		views := make([]*View, 2)
		for at := range views {
			views[at] = NewView(world, ViewConfig{Store: storeOver(t, runtime, objects, "view"), Initial: Empty(),
				Interval: -1})
			defer views[at].Close()
		}
		held := make([]Membership, len(views))
		for step := range 40 {
			if err := runtime.Admit(world, "faults"); err != nil {
				t.Error(err)
				return
			}
			for at, view := range views {
				got, _ := view.Refresh(world)
				if got.Generation() < held[at].Generation() {
					t.Errorf("view %d went back from generation %d to %d", at, held[at].Generation(), got.Generation())
				}
				held[at] = got
			}
			id := "step/" + string(rune('0'+step%10)) + string(rune('0'+step/10))
			switch random.Intn(id, 6) {
			case 0:
				objects.FailNextAfterApply(sim.ObjectPut, 1)
			case 1:
				objects.FailNext(sim.ObjectPut, 1)
			case 2:
				objects.FailNext(sim.ObjectGet, 1)
			case 3:
				objects.Fail()
			default:
				objects.Recover()
			}
		}
		objects.Recover()
		writers.Wait()
		runtime.SetBuggify(false)
		// A member that drained still holds the shared disk if it took it
		// last; it is released, and the drained members leave.
		finisher := storeOver(t, runtime, objects, "finisher")
		finish := sim.WithTask(ctx, "finisher")
		for n := byte(2); n <= 4; n += 2 {
			update(finish, "finisher", finisher, letGo(shared.ID, idOf(n)))
			update(finish, "finisher", finisher, func(m Membership) (Membership, error) { return m.Leave(idOf(n)) })
		}
		final, err := finisher.Read(finish)
		if err != nil {
			t.Error(err)
			return
		}
		for n := byte(1); n <= 4; n++ {
			disk, listed := final.Disk(diskOf(n).ID)
			_, member := final.Member(idOf(n))
			switch {
			case n%2 == 0 && (listed || member):
				t.Errorf("member %d left, and the end holds it: %s", n, describe(final))
			case n%2 == 1 && (!listed || !member || disk.Weight != 4 || disk.State != Serving):
				t.Errorf("member %d stayed with its disk at weight 4, and the end holds %s", n, describe(final))
			}
		}
	}()
	if err := scheduler.Run(done); err != nil {
		t.Fatal(err)
	}
	history := applied.check(t)
	for _, ack := range acks {
		at := int(ack.m.Generation()) - 1
		if at < 0 || at >= len(history) || !history[at].Equal(ack.m) {
			t.Errorf("%s was told generation %d landed as %s, which the store does not hold", ack.writer,
				ack.m.Generation(), describe(ack.m))
		}
	}
	for at, m := range history {
		if m.Generation() != uint64(at+1) {
			t.Errorf("the store's write %d is generation %d", at+1, m.Generation())
		}
	}
	return runtime
}
