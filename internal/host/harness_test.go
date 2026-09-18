package host_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/host"
	"github.com/semistrict/sproutfs/internal/knobs"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/adapters"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/volume"
)

// hostKnobs is what these tests are written around: the deployment's tunables
// with the thirty-two page arena the host suite's pagers are sized to, and the
// write bound the guests store within.
func hostKnobs() knobs.Knobs {
	k := knobs.Defaults()
	k.ResidentPages, k.LogicalPages, k.DirtyPages = 32, 64, 32
	k.ReadAheadPages, k.WriteAheadPages = 1, 1
	return k
}

type hostHarness struct {
	runtime *sim.Runtime
	prefix  platform.ObjectPrefix
	// pages is where each host serves migration pages, which is the only
	// address a host publishes and the only one another host ever dials.
	pages   []platform.Address
	configs []host.Config
	hosts   []*host.Host
	// processes is the simulated process each host runs inside, and disks the
	// one disk each host index keeps across every incarnation of itself. A host
	// that is lost and started again is what internal/simtest's campaigns do;
	// here they are what makes each host's spill file and each host's goroutines
	// its own.
	processes []*sim.Process
	disks     []*sim.Disk
	// setup builds what one incarnation of a host owns besides the Host itself —
	// its pager, its arena, the spill file on that host's disk — and returns
	// what closes them.
	setup        []func(ctx context.Context, incarnation int) (func(), error)
	incarnations []int
}

// hostNetwork names the host a dial comes from. Real TCP takes an ephemeral
// source port and a host passes no source address at all; the simulator models
// a link between two named endpoints, so a test supplies the name its host
// would have on the wire.
type hostNetwork struct {
	*sim.Network
	local platform.Address
}

func (n *hostNetwork) Dial(ctx context.Context, _, to platform.Address) (platform.Conn, error) {
	return n.Network.Dial(ctx, n.local, to)
}

// gatedStore refuses every object-store operation while blocked names a reason
// this caller cannot reach the store. The store is not carried by the simulated
// network, so this is how it is taken away from one host: a link named as an
// endpoint and separated, or a process that is gone.
type gatedStore struct {
	platform.ObjectStore
	blocked func() error
}

func (s *gatedStore) Head(ctx context.Context, key platform.ObjectKey) (platform.ObjectMetadata, error) {
	if err := s.blocked(); err != nil {
		return platform.ObjectMetadata{}, err
	}
	return s.ObjectStore.Head(ctx, key)
}

func (s *gatedStore) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	if err := s.blocked(); err != nil {
		return platform.GetResult{}, err
	}
	return s.ObjectStore.Get(ctx, request)
}

func (s *gatedStore) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	if err := s.blocked(); err != nil {
		return platform.PutResult{}, err
	}
	return s.ObjectStore.Put(ctx, request)
}

func (s *gatedStore) Delete(ctx context.Context, request platform.DeleteRequest) error {
	if err := s.blocked(); err != nil {
		return err
	}
	return s.ObjectStore.Delete(ctx, request)
}

func (s *gatedStore) List(ctx context.Context, request platform.ListRequest) (platform.ListResult, error) {
	if err := s.blocked(); err != nil {
		return platform.ListResult{}, err
	}
	return s.ObjectStore.List(ctx, request)
}

// rootVolume is the single volume every host test's VM owns.
var rootVolume = []volume.VolumeSpec{{Name: "root", Size: 8192}}

func newHostHarness(t *testing.T) *hostHarness { return newSizedHostHarness(t, 2) }

// newSizedHostHarness admits count hosts and prepares their configurations
// without starting them, so a test can adjust them first.
func newSizedHostHarness(t *testing.T, count int) *hostHarness {
	t.Helper()
	return newSizedHostHarnessOn(t, count, sim.ObjectStoreConfig{GetLatency: time.Nanosecond,
		PutLatency: time.Nanosecond, ListLatency: time.Nanosecond, BytesPerSecond: 1 << 60})
}

// newSizedHostHarnessOn is newSizedHostHarness over a named object store, which
// is how a test that needs a publication to still be in flight gets one.
func newSizedHostHarnessOn(t *testing.T, count int, store sim.ObjectStoreConfig) *hostHarness {
	t.Helper()
	runtime := sim.New(sim.Config{ObjectStore: store})
	prefix, err := platform.NewObjectPrefix("host-cluster/")
	if err != nil {
		t.Fatal(err)
	}
	h := &hostHarness{runtime: runtime, prefix: prefix}
	t.Cleanup(func() {
		runtime.ObjectStore().Recover()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, started := range h.hosts {
			if started == nil {
				continue
			}
			if err := started.Close(ctx); err != nil {
				t.Error(err)
			}
		}
		// Whatever the test did to these hosts, what the store holds once they
		// are closed must still be a deployment. The allowances are what no
		// writer ever returns for and only a collector reconciles: the
		// checkpoint a later open found selected, the parts of a publication
		// that never reached its index, a checkpoint whose pin was released
		// after the sweep that would have taken it or whose sweep the store
		// refused, and the pin a fork handed to another host wrote for a child
		// these tests never stand up.
		if err := volume.CheckDeployment(ctx, runtime.ObjectStore(), prefix,
			volume.AllowSupersededEpoch, volume.AllowUnpublishedIndex,
			volume.AllowUnreferencedCheckpoint, volume.AllowUnrecordedVM); err != nil {
			t.Error(err)
		}
	})
	for range count {
		address := freeAddress(t)
		h.admit(host.Config{
			// Plain TCP over the loopback: these hosts reach each other the
			// way a deployment's do.
			Network:     adapters.NewNetwork(),
			Resources:   testresource.New(),
			ObjectStore: runtime.ObjectStore(), ObjectPrefix: prefix,
			Cache:     checkpoint.CacheConfig{},
			Migration: host.MigrationConfig{Address: address},
			// These tests drive every checkpoint themselves, so the interval loop is
			// off: a checkpoint arriving on its own would race their assertions about
			// exactly what is durable and when.
			CheckpointInterval: -1,
		}, address)
	}
	return h
}

// admit prepares one more host: the process it runs inside, the disk that
// process keeps across every restart of itself, and its own view of the
// deployment's object store, which is what a kill takes away.
// hostID names one host: its process, its disk, and its own endpoint on the
// simulated network.
func (h *hostHarness) hostID(n int) string { return fmt.Sprintf("host-%d", n) }

func (h *hostHarness) admit(config host.Config, address platform.Address) {
	n := len(h.configs)
	h.disks = append(h.disks, h.runtime.NewDisk(h.hostID(n),
		// A host's disk comes back with its unsynced modifications resolved
		// rather than restored, so a start that read across a crash would read
		// bytes nobody wrote.
		sim.DiskConfig{PowerLossFaults: true}))
	h.processes = append(h.processes, h.runtime.NewProcess(sim.ProcessConfig{
		ID: h.hostID(n), Disk: h.disks[n]}))
	h.pages = append(h.pages, address)
	h.configs = append(h.configs, config)
	h.hosts = append(h.hosts, nil)
	h.setup = append(h.setup, nil)
	h.incarnations = append(h.incarnations, 0)
}

func (h *hostHarness) start(t *testing.T) {
	t.Helper()
	for n := range h.configs {
		h.launch(t, n)
	}
}

// launch starts host n inside its own simulated process. The process's context
// is the host's, so a kill cancels every goroutine that host owns; whatever
// else the incarnation owns is built and closed in there too, so a killed
// host's pages go with it.
func (h *hostHarness) launch(t *testing.T, n int) {
	t.Helper()
	parent := t.Context()
	ready := make(chan error, 1)
	err := h.processes[n].Start(parent, func(ctx context.Context) {
		h.incarnations[n]++
		var release func()
		if h.setup[n] != nil {
			built, err := h.setup[n](ctx, h.incarnations[n])
			if err != nil {
				ready <- err
				return
			}
			release = built
		}
		started, err := host.StartHost(ctx, h.configs[n])
		h.hosts[n] = started
		ready <- err
		if err == nil {
			<-ctx.Done()
			_ = started.Close(context.Background())
		}
		if release != nil {
			release()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
}

// stop ends one host the orderly way: it closes, publishing a final checkpoint
// of everything it still holds, and only then does its process end. It is what
// a drained host does, and the thing a kill is defined against.
func (h *hostHarness) stop(t *testing.T, n int) {
	t.Helper()
	if err := h.hosts[n].Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := h.processes[n].Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.hosts[n] = nil
}

// simPager builds one incarnation's pager over that host's own disk. The spill
// file is the only local state a host keeps, and it is scratch by construction:
// the pager truncates it at every start, so a restart reads none of what its
// own crash left in it.
func (h *hostHarness) simPager(ctx context.Context, n int, config vmmemory.Config) (*vmmemory.Host, *pageArena, func(), error) {
	spill, err := h.disks[n].Open(ctx, "spill", platform.OpenOptions{Create: true})
	if err != nil {
		return nil, nil, nil, err
	}
	arena := &pageArena{slots: make([][]byte, config.ResidentPages)}
	pager, err := vmmemory.New(ctx, h.configs[n].Resources, config, arena, spill)
	if err != nil {
		return nil, nil, nil, errors.Join(err, spill.Close())
	}
	return pager, arena, func() {
		_ = pager.Close(context.Background())
		_ = spill.Close()
	}, nil
}

func freeAddress(t *testing.T) platform.Address {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return platform.Address(listener.Addr().String())
}

// assertPageServerReleased requires a closed host to have given its page
// server's port back, which is what lets another process take the address.
func assertPageServerReleased(t *testing.T, address platform.Address) {
	t.Helper()
	if address == "" {
		return
	}
	listener, err := net.Listen("tcp", string(address))
	if err != nil {
		t.Fatalf("host retained the page server listener at %s: %v", address, err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, ctx context.Context, vm *volume.VM, offset uint64, length int) string {
	t.Helper()
	buffer := make([]byte, length)
	if err := vm.Volume("root").Read(ctx, offset, buffer); err != nil {
		t.Fatal(err)
	}
	return string(buffer)
}

func write(t *testing.T, ctx context.Context, vm *volume.VM, offset uint64, data string) {
	t.Helper()
	if err := vm.Volume("root").Write(ctx, offset, []byte(data)); err != nil {
		t.Fatal(err)
	}
}

// putWatcher reports when a host next writes to its object store, which is what
// a test needs to act while a publication is still in flight rather than a
// measured distance into one.
type putWatcher struct {
	platform.ObjectStore
	mu      sync.Mutex
	waiting chan struct{}
}

// next returns a channel closed by the first write after this call.
func (w *putWatcher) next() <-chan struct{} {
	waiting := make(chan struct{})
	w.mu.Lock()
	w.waiting = waiting
	w.mu.Unlock()
	return waiting
}

func (w *putWatcher) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	w.mu.Lock()
	waiting := w.waiting
	w.waiting = nil
	w.mu.Unlock()
	if waiting != nil {
		close(waiting)
	}
	return w.ObjectStore.Put(ctx, request)
}
