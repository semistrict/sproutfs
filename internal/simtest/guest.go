package simtest

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/testbacking"
	"github.com/semistrict/sproutfs/internal/testpager"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/volume"
)

// A volume is a backing a fault can ask for the window it needs, with the pages
// its memory region already holds left out. It is asserted here because this is where
// the two packages meet: a volume that stopped being one would cost every fault
// a request per stretch of its window and nothing would fail.
var _ vmmemory.SparseLoader = (*volume.Volume)(nil)

// stateBytes is the simulated VMM state a migration carries: the guest's
// current value and how many stores it has made. A destination that restored it
// continues exactly where the source stopped, and one that lost it says so.
const stateBytes = 9

// guest is the simulated VMM process of one VM: one memory region per volume, a guest
// that stores into them through their mappings, and the migration Runtime the
// coordinator drives.
//
// It has no life of its own: a store happens when the driver asks for one, so
// what the model holds is exact at every moment the driver looks at it. A
// guest that stored in a loop of its own would make every assertion a race
// against that loop rather than against the fault under test.
type guest struct {
	instance string
	// id names this VMM process apart from every other one of the same VM: the
	// host it runs on and that host's incarnation, which is what makes a
	// restarted host's guest a different logical caller from the dead one's.
	id  string
	ctx context.Context
	// admit is what every concurrent memory region seal passes through, and reverse
	// the order they are created in. Both are the recorded scenario's; a
	// campaign that chooses no completion order has neither.
	admit   func(ctx context.Context, id string) error
	reverse bool
	// captures counts the pauses this process has taken, which names one
	// capture's seals apart from the next one's.
	captures int

	names         []string
	pages         map[string]int
	memoryRegions map[string]*vmmemory.MemoryRegion
	mappings      map[string]*testpager.Mapping
	// pageBytes is the page each of this guest's volumes is mapped in, which is
	// the page of the pager holding that memory region. A VM's memory and its disks are
	// two pagers of two pages, so every byte offset here is per volume.
	pageBytes map[string]int
	// ephemeral names the volumes that are ephemeral disks: mapped on the
	// ephemeral pager, and held by no checkpoint.
	ephemeral map[string]bool
	// backings is what each memory region attached through, which is where a
	// peer backing whose two answers disagreed says so.
	backings map[string]*testbacking.Admitting

	mu sync.Mutex
	// model is the byte the guest last stored into every page of every volume,
	// which is what any later read of that page must return.
	model  map[string][]byte
	writes int64
	value  byte
	// refuseStop is the fault that fails a migration's pause after the guest
	// has already stopped: the release must unseal the memory regions and leave the
	// guest running again.
	refuseStop error
	// stopped reports vCPUs that are not running. A guest stores nothing while
	// it is stopped, which is what makes a capture or an abandoned migration
	// that never resumed it visible: the next store fails rather than quietly
	// succeeding against pages nothing is driving. sealing is closed when the
	// pause of a checkpoint of the disks ends, and nil while none stands: a
	// host's loop takes one whenever it has to, and a store meanwhile waits
	// for the vCPUs to run again, as a guest's does.
	stopped bool
	sealing chan struct{}
	closed  bool
	// exited closes when the VMM process ends without its host asking it to,
	// and exit is why: a command whose answer never came, after which
	// vmmachine kills the process rather than guess at its state, or a crash.
	// Wait returns it, every command after it fails, and the guest stores
	// nothing more.
	exited chan struct{}
	exit   error
	// exiting is what runs when the process ends, with why: the flushes
	// it never answered.
	exiting []func(error)
	// flushes is what durable flush promises this VM, which every store is
	// recorded into: nil in a world without it (journals.go).
	flushes *flushes
}

// newGuest attaches one memory region per volume of vm through backing, which is the
// volume itself on a source and a peer-backed volume on a destination. Every
// memory region enters the simulator through a stable identity of its own — the VM,
// the host running it and that host's incarnation — so two concurrent
// identical reads are ordered by their logical caller rather than by
// completion.
func (w *World) newGuest(h *hostState, p *pager, vm *volume.VM, backings map[string]vmmemory.Backing, state []byte) (*guest, error) {
	ctx := w.ctx
	g := &guest{instance: vm.ID(), ctx: ctx, pages: map[string]int{}, exited: make(chan struct{}),
		memoryRegions: map[string]*vmmemory.MemoryRegion{}, mappings: map[string]*testpager.Mapping{},
		pageBytes: map[string]int{}, ephemeral: map[string]bool{}, backings: map[string]*testbacking.Admitting{},
		model: map[string][]byte{}, admit: w.config.Admit, reverse: w.config.ReverseMemoryRegions,
		id: fmt.Sprintf("%s/%s/%d/%d", vm.ID(), h.name, h.incarnation, w.nextGuest())}
	for _, v := range vm.Volumes() {
		name := v.Name()
		var backing vmmemory.Backing = v
		if supplied, ok := backings[name]; ok {
			backing = supplied
		}
		// The simulated machine binds the same shapes the real one does: one
		// RAM volume, and every other volume a PMEM disk. Each attaches to the
		// pager of its own kind, over that pager's own arena, and an ephemeral
		// disk to the ephemeral pager.
		kind := vmmemory.Pmem
		if name == MemoryVolume {
			kind = vmmemory.Ram
		}
		pager := p.pagers.Of(kind, v.Ephemeral())
		tenant := control.TenantOf(vm.ID())
		mp := testpager.NewMapping(p.arenaOf(pager), tenant)
		admitted, admitting := testbacking.New(backing, p.runtime, g.id+"/"+name)
		g.backings[name] = admitting
		memoryRegion, err := pager.Attach(ctx, vmmemory.MemoryRegionBacking{Kind: kind, Backing: admitted, Tenant: tenant}, mp)
		if err != nil {
			return nil, err
		}
		g.names = append(g.names, name)
		g.memoryRegions[name] = memoryRegion
		g.mappings[name] = mp
		g.pageBytes[name] = int(pager.PageSize())
		g.ephemeral[name] = v.Ephemeral()
		g.pages[name] = int(v.Size() / pager.PageSize())
		g.model[name] = make([]byte, v.Size())
	}
	switch len(state) {
	case 0:
	case stateBytes:
		// The VMM state carries the guest's own counters, so a destination that
		// restored it continues exactly where the source stopped.
		g.value = state[0]
		g.writes = int64(binary.LittleEndian.Uint64(state[1:]))
	default:
		return nil, fmt.Errorf("simtest: restored %d state bytes, want %d", len(state), stateBytes)
	}
	return g, nil
}

// MemoryRegions reports this process's memory by volume name, which is what a
// migration hands over and a capture seals.
func (g *guest) MemoryRegions() map[string]*vmmemory.MemoryRegion {
	result := make(map[string]*vmmemory.MemoryRegion, len(g.memoryRegions))
	for name, memoryRegion := range g.memoryRegions {
		result[name] = memoryRegion
	}
	return result
}

// Prepare is a capture's pause: the guest stops storing, its state is captured,
// and every memory region seals the pages the checkpoint will publish.
func (g *guest) Prepare(ctx context.Context) ([]byte, map[string]volume.DirtySource, error) {
	state, err := g.pause(ctx, "prepare")
	if err != nil {
		return nil, nil, err
	}
	// The VMM seals as part of the snapshot request, which it may refuse
	// having sealed some of its memory regions, or never answer.
	if err := g.ends(ctx, "prepare"); err != nil {
		return nil, nil, err
	}
	if err := g.refuses(ctx, "prepare"); err != nil {
		return nil, nil, g.refuseSealed(ctx, err)
	}
	g.captures++
	names := slices.Clone(g.names)
	if g.reverse {
		slices.Reverse(names)
	}
	if g.admit == nil {
		sources := make(map[string]volume.DirtySource, len(g.memoryRegions))
		for _, name := range names {
			if err := g.memoryRegions[name].Seal(ctx); err != nil {
				return nil, nil, err
			}
			// A VMM seals every memory region it maps. An ephemeral disk's
			// seal takes nothing, and no checkpoint is given its pages.
			if !g.ephemeral[name] {
				sources[name] = sealed(g.memoryRegions[name].Checkpoint())
			}
		}
		return state, sources, nil
	}
	// Every memory region seals concurrently, each admitted as a caller of its own, so
	// the order they seal in is the scheduler's rather than the order the
	// goroutines happened to be created in.
	sources := map[string]volume.DirtySource{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var result error
	for _, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := g.admit(ctx, fmt.Sprintf("capture/%s/%d/%s", g.id, g.captures, name))
			if err == nil {
				err = g.memoryRegions[name].Seal(ctx)
			}
			mu.Lock()
			defer mu.Unlock()
			if err == nil && !g.ephemeral[name] {
				sources[name] = sealed(g.memoryRegions[name].Checkpoint())
			}
			result = errors.Join(result, err)
		}()
	}
	wg.Wait()
	if result != nil {
		return nil, nil, result
	}
	return state, sources, nil
}

// Stop is a migration's pause: the guest stops storing and its state is
// captured. Nothing is sealed and nothing is uploaded — the pages this process
// keeps are what the destination fetches.
func (g *guest) Stop(ctx context.Context) ([]byte, error) {
	state, err := g.pause(ctx, "stop")
	if err != nil {
		return nil, err
	}
	if err := g.ends(ctx, "stop"); err != nil {
		return nil, err
	}
	if err := g.refuses(ctx, "stop"); err != nil {
		return nil, g.refuseSealed(ctx, err)
	}
	return state, nil
}

// pause stops the vCPUs and captures the state, which is the first half of a
// capture and of a migration's stop. A VMM may refuse the pause, and then
// nothing happened; or never answer it, and then it is killed. A refused stop
// the RefusedStop fault stages fails after the pause began.
func (g *guest) pause(ctx context.Context, command string) ([]byte, error) {
	if err := g.alive(command); err != nil {
		return nil, err
	}
	if err := g.ends(ctx, "pause"); err != nil {
		return nil, err
	}
	if err := g.refuses(ctx, "pause"); err != nil {
		return nil, err
	}
	g.mu.Lock()
	refused := g.refuseStop
	state := make([]byte, stateBytes)
	state[0] = g.value
	binary.LittleEndian.PutUint64(state[1:], uint64(g.writes))
	g.stopped = true
	g.mu.Unlock()
	if refused != nil {
		return nil, g.refuseSealed(ctx, refused)
	}
	return state, nil
}

// refuseSealed is a refusal that came after the pause began: it seals one
// memory region on its way out, so the release has to unseal this process
// and leave both the memory and the disk writable before the capture or the
// migration can be tried again.
func (g *guest) refuseSealed(ctx context.Context, refusal error) error {
	if err := g.memoryRegions[g.names[0]].Seal(ctx); err != nil {
		return errors.Join(refusal, err)
	}
	return refusal
}

// setRefuseStop makes this guest's next migration pause fail after it has
// already stopped, or stops it doing so.
func (g *guest) setRefuseStop(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refuseStop = err
}

// SealDisks is a disk checkpoint's pause: the guest stops storing and the
// memory regions of its disks seal, while its RAM and its state are left alone.
func (g *guest) SealDisks(ctx context.Context) (map[string]volume.DirtySource, error) {
	if err := g.alive("seal-disks"); err != nil {
		return nil, err
	}
	if err := g.ends(ctx, "seal-disks"); err != nil {
		return nil, err
	}
	// The pause is the one request this command makes of the VMM, and a
	// refused one changed nothing.
	if err := g.refuses(ctx, "seal-disks"); err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.stopped = true
	if g.sealing == nil {
		g.sealing = make(chan struct{})
	}
	g.mu.Unlock()
	sources := map[string]volume.DirtySource{}
	for _, name := range g.names {
		if !g.memoryRegions[name].OnInterval() {
			continue
		}
		if err := g.memoryRegions[name].Seal(ctx); err != nil {
			return nil, err
		}
		sources[name] = sealed(g.memoryRegions[name].Checkpoint())
	}
	return sources, nil
}

// Resume restarts the vCPUs. This guest only stores when the driver asks it to,
// so what a resume means here is that stores are accepted again — which is
// exactly what an abandoned capture or migration owes the guest it stopped, and
// exactly what a run that never checked would not notice was missing.
func (g *guest) Resume(ctx context.Context) error {
	if err := g.alive("resume"); err != nil {
		return err
	}
	// vmmachine kills a VMM whose resume failed however it failed, because a
	// guest that may or may not be running is one nothing can reason about.
	if err := g.ends(ctx, "resume"); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopped = false
	if g.sealing != nil {
		close(g.sealing)
		g.sealing = nil
	}
	return nil
}

// runs returns once the vCPUs run, and fails while they are stopped by
// anything but a checkpoint of the disks, whose pause it waits out.
func (g *guest) runs(ctx context.Context) error {
	for {
		g.mu.Lock()
		stopped, sealing, exit := g.stopped, g.sealing, g.exit
		g.mu.Unlock()
		switch {
		case exit != nil:
			return exit
		case !stopped:
			return nil
		case sealing == nil:
			return fmt.Errorf("%s: the guest's vCPUs are stopped", g.instance)
		}
		select {
		case <-sealing:
		case <-g.exited:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

// Release unseals every memory region and resumes a still-paused process, which is
// what an abandoned capture or migration owes the guest it stopped.
func (g *guest) Release(ctx context.Context) error {
	if err := g.alive("release"); err != nil {
		return err
	}
	// As for a resume, a release that fails kills the process.
	if err := g.ends(ctx, "release"); err != nil {
		return err
	}
	for _, name := range g.names {
		if err := g.memoryRegions[name].Unseal(ctx); err != nil {
			return err
		}
	}
	return g.Resume(ctx)
}

// Wait reports the end of the VMM process: one killed because a command's
// answer never came, or one that crashed.
func (g *guest) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-g.exited:
		return g.exitCause()
	}
}

// Close ends the process and detaches its memory regions. As vmmachine's, it
// may fail to give something of the process back, and the process is gone
// all the same.
func (g *guest) Close() error {
	err := g.detach(context.Background())
	if err == nil && sim.Buggify(g.ctx, "simtest/vmm-close-fails", vmmFaultChance) {
		err = fmt.Errorf("%s: %w: a staging file of the VMM could not be removed", g.instance, ErrInjected)
	}
	return err
}

// The simulated VMM's faults. A real one answers each command over its API
// socket (vmmachine.Process.request). It may refuse one, and then it runs and
// did nothing the refusal does not say it did. Or the answer may never come,
// and then vmmachine kills the process rather than guess at its state: Wait
// returns, and the host gives the VM up. Each command has a site of each, and
// a running guest may crash.
const (
	vmmFaultChance = 0.1
	vmmCrashChance = 0.02
)

// errRefused is what a VMM that refused a command answers.
var errRefused = errors.New("simtest: the VMM refused the command")

// refuses reports the command a VMM refused, or nil.
func (g *guest) refuses(ctx context.Context, command string) error {
	if !sim.Buggify(ctx, "simtest/vmm-refuses/"+command, vmmFaultChance) {
		return nil
	}
	return fmt.Errorf("%s: %w: %s", g.instance, errRefused, command)
}

// ends is a command whose answer never came: the process is killed, and the
// command fails with why.
func (g *guest) ends(ctx context.Context, command string) error {
	if !sim.Buggify(ctx, "simtest/vmm-ends/"+command, vmmFaultChance) {
		return nil
	}
	return g.exitWith(fmt.Errorf("%s: %w: the VMM never answered %s, so it was killed", g.instance,
		ErrInjected, command))
}

// crashes ends a running process on its own, which is the end a host learns
// of only through Wait. It is drawn before each of the campaign's stores.
func (g *guest) crashes(ctx context.Context) error {
	if !sim.Buggify(ctx, "simtest/vmm-crashes", vmmCrashChance) {
		return nil
	}
	return g.exitWith(fmt.Errorf("%s: %w: the VMM crashed", g.instance, ErrInjected))
}

// exitWith ends the process with cause, once, and reports why it ended.
func (g *guest) exitWith(cause error) error {
	g.mu.Lock()
	if g.exit != nil {
		defer g.mu.Unlock()
		return g.exit
	}
	g.exit = fmt.Errorf("%w: %w", errExited, cause)
	g.stopped = true
	close(g.exited)
	exiting := g.exiting
	g.exiting = nil
	g.mu.Unlock()
	for _, fn := range exiting {
		fn(g.exit)
	}
	return g.exit
}

// atExit runs fn with why the process ended when it ends, or at once if it
// has.
func (g *guest) atExit(fn func(error)) {
	g.mu.Lock()
	exit := g.exit
	if exit == nil {
		g.exiting = append(g.exiting, fn)
	}
	g.mu.Unlock()
	if exit != nil {
		fn(exit)
	}
}

// errExited is every command of a process that has ended, and every store
// into it.
var errExited = errors.New("simtest: the VMM process has ended")

// exitCause is why the process ended, nil while it runs.
func (g *guest) exitCause() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.exit
}

// alive fails a command to a process that has ended, as a request to a
// socket nothing listens on does.
func (g *guest) alive(command string) error {
	if err := g.exitCause(); err != nil {
		return fmt.Errorf("%s: %w", command, err)
	}
	return nil
}

// hasExited reports a process that ended without its host asking it to.
func (g *guest) hasExited() bool { return g.exitCause() != nil }

// isClosed reports a VMM process that has been ended, by its host or by
// whatever gave the VM up.
func (g *guest) isClosed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed
}

// detach gives this process's pages back. It is what closing a VMM does, and
// what the loss of a host does to every VMM it was running.
func (g *guest) detach(ctx context.Context) error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	g.mu.Unlock()
	var errs []error
	for _, name := range g.names {
		// The memory region goes first: the pager maps and protects through this
		// mapping, so emptying it before the detach leaves a capture in flight
		// driving a map that is being cleared underneath it.
		errs = append(errs, g.memoryRegions[name].Detach(ctx))
		g.mappings[name].Forget()
	}
	return errors.Join(errs...)
}

// store writes one page the way a guest does: it stores through the mapping
// when the mapping is writable, and takes a write fault when a seal took that
// access away. The model is updated under the mapping lock the store took, so a
// checkpoint can never contain bytes the model does not.
func (g *guest) store(name string, page uint64) error { return g.storeIn(g.ctx, name, page) }

// storeIn is store under a caller's own context, which is what a scenario about
// a store the pager holds back needs: the guest's context outlives the fault,
// so a test with nothing else to cancel would wait for a checkpoint that is
// never coming.
func (g *guest) storeIn(ctx context.Context, name string, page uint64) error {
	g.mu.Lock()
	g.value++
	if g.value == 0 {
		g.value = 1
	}
	value := g.value
	g.mu.Unlock()
	return g.storeValueIn(ctx, name, page, value)
}

// storeValue stores one named byte, which is what a campaign reading a VM back
// as one whole generation writes into every page of it.
func (g *guest) storeValue(name string, page uint64, value byte) error {
	return g.storeValueIn(g.ctx, name, page, value)
}

func (g *guest) storeValueIn(ctx context.Context, name string, page uint64, value byte) error {
	if err := g.runs(ctx); err != nil {
		return fmt.Errorf("%w, so it stores nothing", err)
	}
	mp := g.mappings[name]
	for range 8 {
		if g.storeModel(name, mp, page, value) {
			return nil
		}
		if err := g.memoryRegions[name].Fault(ctx, page, true); err != nil {
			return fmt.Errorf("%s store fault on %s page %d: %w", g.instance, name, page, err)
		}
	}
	return fmt.Errorf("%s store on %s page %d never resolved", g.instance, name, page)
}

// takeWritable takes one page writable and stores nothing into it, which is the
// fault a guest cannot avoid making: KVM finishes a cold read from a worker
// that always asks for the page writable, and an architecture can report a
// guest kernel's cache maintenance as a write. The model records nothing,
// because nothing was written, so whatever the page reads afterwards — the
// copy, or the page it was copied from once the settle gives it back — must
// still be the bytes the guest last wrote.
func (g *guest) takeWritable(ctx context.Context, name string, page uint64) error {
	if err := g.runs(ctx); err != nil {
		return fmt.Errorf("%w, so it faults on nothing", err)
	}
	if err := g.memoryRegions[name].Fault(ctx, page, true); err != nil {
		return fmt.Errorf("%s write fault on %s page %d: %w", g.instance, name, page, err)
	}
	return nil
}

// giveBack is the give-back of a VM's cold copies, which a session runs soon
// after each is made; a campaign has no sessions, so it draws this beside its
// stores. It pauses nothing, so it runs while checkpoints, forks and
// migrations of this VM are in flight.
//
// Its invariant is checked at once: every page the guest maps reads what the
// guest last wrote. A copy given back maps the page it was copied from, which
// is only right where the guest never changed it, and a wrong one would
// otherwise surface only at the next read of that page.
func (g *guest) giveBack(ctx context.Context) error {
	for _, name := range g.names {
		memoryRegion := g.memoryRegions[name]
		if _, err := memoryRegion.GiveBackColdCopies(ctx); err != nil {
			return fmt.Errorf("%s cold-copy give-back on %s: %w", g.instance, name, err)
		}
		if err := g.mapsWhatItWrote(name); err != nil {
			return err
		}
	}
	return nil
}

// mapsWhatItWrote requires every page of one volume the guest maps from the
// arena to hold the bytes the guest last wrote there.
func (g *guest) mapsWhatItWrote(name string) error {
	mp := g.mappings[name]
	g.mu.Lock()
	defer g.mu.Unlock()
	size := g.pageBytes[name]
	for _, page := range slices.Sorted(maps.Keys(mp.Mapped())) {
		got, mapped := mp.Read(page)
		if !mapped || got == nil {
			continue
		}
		want := g.model[name][int(page)*size : (int(page)+1)*size]
		if !bytes.Equal(got, want) {
			return fmt.Errorf("%s %s page %d maps %d after a give-back, want the %d the guest wrote",
				g.instance, name, page, got[0], want[0])
		}
	}
	return nil
}

func (g *guest) storeModel(name string, mp *testpager.Mapping, page uint64, value byte) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !mp.Store(page, value) {
		return false
	}
	size := g.pageBytes[name]
	for i := range size {
		g.model[name][int(page)*size+i] = value
	}
	g.writes++
	g.flushes.stored(name, page, size, value)
	return true
}

// stored is how many stores this guest has made, which is the counter its VMM
// state carries: a destination or a takeover that restored it continues exactly
// where the guest before it stopped, and one that lost it says so.
func (g *guest) stored() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.writes
}

// storeAll writes one whole generation into every page of every volume, which
// is what a campaign that reads a recovered VM back as one generation needs in
// it.
func (g *guest) storeAll(value byte) error {
	for _, name := range g.names {
		for page := range uint64(g.pages[name]) {
			if err := g.storeValue(name, page, value); err != nil {
				return err
			}
		}
	}
	return nil
}

// read faults one page in for reading and returns its bytes, which is what this
// guest sees.
func (g *guest) read(ctx context.Context, name string, page uint64) ([]byte, error) {
	mp := g.mappings[name]
	data, ok := mp.Read(page)
	if !ok {
		if err := g.memoryRegions[name].Fault(ctx, page, false); err != nil {
			return nil, err
		}
		if data, ok = mp.Read(page); !ok {
			return nil, fmt.Errorf("%s page %d unmapped after a fault", name, page)
		}
	}
	if data == nil {
		return make([]byte, g.pageBytes[name]), nil
	}
	return data, nil
}

// readAll reads every page of every memory region through this guest's own fault path
// and reports what it read, which pages could not be read at all, and the
// failures that made them unreadable. It is what a VM that came back somewhere
// else is compared against: the pager reconstructing a page and the volume
// holding it are the same claim from here.
func (g *guest) readAll(ctx context.Context) (map[string][]byte, map[string][]bool, error) {
	read := map[string][]byte{}
	missing := map[string][]bool{}
	var errs []error
	for _, name := range g.names {
		size := g.pageBytes[name]
		read[name] = make([]byte, g.pages[name]*size)
		missing[name] = make([]bool, g.pages[name])
		for page := range uint64(g.pages[name]) {
			got, err := g.read(ctx, name, page)
			if err != nil {
				missing[name][page] = true
				errs = append(errs, fmt.Errorf("%s %s page %d: %w: %w",
					g.instance, name, page, errUnreadable, err))
				continue
			}
			copy(read[name][int(page)*size:], got)
		}
	}
	return read, missing, errors.Join(errs...)
}

// snapshot is the bytes this guest believes it has, which is what a destination
// or a restart must read back.
func (g *guest) snapshot() map[string][]byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	result := make(map[string][]byte, len(g.model))
	for name, data := range g.model {
		result[name] = bytes.Clone(data)
	}
	return result
}

// checkpointed is what a checkpoint of this guest holds, and what a fork of it
// starts from: the bytes it believes it has, with every ephemeral disk zeroed,
// because no checkpoint and no fork point holds a page of one.
func (g *guest) checkpointed() map[string][]byte {
	model := g.snapshot()
	for name := range model {
		if g.ephemeral[name] {
			clear(model[name])
		}
	}
	return model
}

// adopt takes bytes a migration, a fork or a restart handed this guest as its
// own model: memory it did not write itself.
func (g *guest) adopt(model map[string][]byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for name, data := range model {
		g.model[name] = bytes.Clone(data)
	}
}

// errUnreadable is a page whose bytes could not be fetched at all: the only
// copy of them is on a peer a fault has taken away, so there is nothing to
// compare. It is not a byte read wrong, which is why it is told apart from one.
var errUnreadable = errors.New("the page could not be read")

// verify reads every page of every memory region and requires it to equal model. It is
// the campaign's first invariant: a guest never reads bytes it did not write.
//
// A page that reads the wrong bytes ends the verification, because there is
// nothing further to learn from a guest whose memory is already wrong. A page
// that cannot be read at all does not: every other page is still checked, and
// the caller decides whether a fault was on that excuses it.
//
// A backing that told this guest's pager two different things about where a
// page's bytes are ends it too, whatever the pages read: no fault excuses it,
// and the page it was about may not be one this run reads again.
func (g *guest) verify(ctx context.Context, model map[string][]byte) error {
	for _, name := range g.names {
		if err := g.backings[name].Disagreement(); err != nil {
			return fmt.Errorf("%s: its backing's two answers disagree: %w", g.instance, err)
		}
	}
	var unreadable error
	for _, name := range g.names {
		want, found := model[name]
		if !found {
			return fmt.Errorf("%s: the model has no volume %s", g.instance, name)
		}
		for page := range uint64(g.pages[name]) {
			got, err := g.read(ctx, name, page)
			if err != nil {
				unreadable = fmt.Errorf("%s %s page %d: %w: %w", g.instance, name, page, errUnreadable, err)
				continue
			}
			size := g.pageBytes[name]
			if !bytes.Equal(got, want[int(page)*size:(int(page)+1)*size]) {
				return fmt.Errorf("%s %s page %d reads %d, want %d",
					g.instance, name, page, got[0], want[int(page)*size])
			}
		}
	}
	return unreadable
}

// pager is one host's pagers and the arena and spill file each of them owns:
// RAM's, PMEM's and the ephemeral disks'. They share nothing: a RAM page and a
// PMEM page are different numbers of bytes, so a slot of one arena could not
// hold a page of the other, and an ephemeral disk's pages are budgeted apart.
type pager struct {
	pagers  vmmemory.Pagers
	arenas  map[*vmmemory.Host]*testpager.Arena
	runtime *sim.Runtime
}

// arenaOf is the shared page store the memory regions of one pager map through.
func (p *pager) arenaOf(pager *vmmemory.Host) *testpager.Arena { return p.arenas[pager] }

// sealedPages is a memory region's sealed checkpoint as the simulated VMM
// hands it to a capture: the pager's own, with the errors the real one returns
// returned at random. Each fails before it does anything, as the pager's do
// when its region can no longer be held live or a read of its spill fails.
type sealedPages struct{ volume.DirtySource }

// sealed wraps one memory region's checkpoint.
func sealed(checkpoint *vmmemory.MemoryRegionCheckpoint) volume.DirtySource {
	return sealedPages{checkpoint}
}

// sealedFaultChance is how often an activated site of a sealed checkpoint
// fails one call, and sealedReadFaultChance one read of a page: a checkpoint
// reads every page it seals, and one that failed most of its reads would
// leave a seed nothing but failed checkpoints.
const (
	sealedFaultChance     = 0.1
	sealedReadFaultChance = 0.02
)

// ReadDirty fails as a read of a page the pager spilled fails.
func (s sealedPages) ReadDirty(ctx context.Context, page uint64, dst []byte) error {
	if sim.Buggify(ctx, "simtest/dirty-source/read-fails", sealedReadFaultChance) {
		return fmt.Errorf("%w: reading sealed page %d from the spill file", ErrInjected, page)
	}
	return s.DirtySource.ReadDirty(ctx, page, dst)
}

// Settle fails as one whose region is no longer serving does.
func (s sealedPages) Settle(ctx context.Context) (int, error) {
	if sim.Buggify(ctx, "simtest/dirty-source/settle-fails", sealedFaultChance) {
		return 0, fmt.Errorf("%w: settling a seal", ErrInjected)
	}
	return s.DirtySource.Settle(ctx)
}

// Share fails before it lends anything.
func (s sealedPages) Share(ctx context.Context, ref control.Ref, volume string) error {
	if sim.Buggify(ctx, "simtest/dirty-source/share-fails", sealedFaultChance) {
		return fmt.Errorf("%w: sharing %s of %s", ErrInjected, volume, ref)
	}
	return s.DirtySource.Share(ctx, ref, volume)
}

// Retire fails leaving the seal in place, as one whose look-up of its pages'
// identities failed does: the call may be repeated.
func (s sealedPages) Retire(ctx context.Context, published bool) error {
	if sim.Buggify(ctx, "simtest/dirty-source/retire-fails", sealedFaultChance) {
		return fmt.Errorf("%w: retiring a seal, published=%t", ErrInjected, published)
	}
	return s.DirtySource.Retire(ctx, published)
}
