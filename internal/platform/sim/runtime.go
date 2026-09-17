// Package sim provides in-memory platform adapters intended to be constructed
// and used entirely inside a testing/synctest bubble.
package sim

import (
	"cmp"
	"context"
	"encoding/binary"
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	Seed        uint64
	Network     NetworkConfig
	ObjectStore ObjectStoreConfig
	// Wait optionally chooses when an I/O completion proceeds within a timing
	// window. It must be driven inside the same synctest bubble. IDs identify
	// adapter operations; callers sharing a resource still need stable request
	// admission to make its per-resource sequence reproducible.
	// Nil preserves the ordinary simulator's timing and concurrency.
	Wait func(context.Context, string, time.Duration, time.Duration) error
	// Buggify activates the per-site fault injection in production code. It is
	// off by default so recording and replay comparisons keep their bytes; a
	// campaign that wants the sites sets it here or calls Runtime.SetBuggify.
	Buggify bool
	// Now reads the simulated instant. Faults with a deadline, such as a
	// clogged link, compare against it. Nil takes the standard library, which
	// inside a testing/synctest bubble is that bubble's virtual clock; a
	// deployment's injected clock can be passed here instead.
	Now func() time.Time
}

func DefaultConfig() Config {
	return Config{
		Seed: 1,
		Network: NetworkConfig{
			Latency:        500 * time.Microsecond,
			Jitter:         100 * time.Microsecond,
			ConnectLatency: 500 * time.Microsecond,
			InboxSize:      256,
			MaxHeaderSize:  1 << 20,
			MaxPayloadSize: 1 << 30,
			BytesPerSecond: 10 << 30,
		},
		ObjectStore: ObjectStoreConfig{
			HeadLatency:    5 * time.Millisecond,
			GetLatency:     10 * time.Millisecond,
			PutLatency:     20 * time.Millisecond,
			DeleteLatency:  10 * time.Millisecond,
			ListLatency:    10 * time.Millisecond,
			BytesPerSecond: 500 << 20,
			MaxObjectSize:  5 << 30,
		},
	}
}

// Runtime owns a coherent simulated world. New must be called inside the
// synctest bubble that will use it; the adapters create bubbled channels.
type Runtime struct {
	seed    uint64
	trace   *Trace
	wait    func(context.Context, string, time.Duration, time.Duration) error
	now     func() time.Time
	network *Network
	objects *ObjectStore
	// buggify is the campaign switch every Buggify site consults first, and
	// bugs is the fixed set of in-tree guards SPROUTFS_SIM_BUG named for this
	// process. Neither is drawn from the seed.
	buggify atomic.Bool
	bugs    map[string]bool

	mu            sync.Mutex
	disks         map[string]*Disk
	taskSequences map[string]uint64
	// The fault-injection bookkeeping: which sites were activated and how often
	// each fired, how many times each seeded choice has been drawn, which
	// probes execution reached, and which bug guards answered yes.
	buggified   map[string]bool
	fired       map[string]uint64
	occurrences map[string]uint64
	probes      map[string]uint64
	notedBugs   map[string]bool
}

func New(config Config) *Runtime {
	defaults := DefaultConfig()
	config.Seed = cmp.Or(config.Seed, defaults.Seed)
	config.Network = config.Network.withDefaults(defaults.Network)
	config.ObjectStore = config.ObjectStore.withDefaults(defaults.ObjectStore)

	if config.Now == nil {
		config.Now = time.Now
	}
	r := &Runtime{
		seed:  config.Seed,
		trace: newTrace(),
		wait:  config.Wait,
		now:   config.Now,
		disks: make(map[string]*Disk),
		bugs:  enabledBugs(),
	}
	r.buggify.Store(config.Buggify)
	r.network = newNetwork(r, config.Network)
	r.objects = newObjectStore(r, config.ObjectStore)
	return r
}

func (r *Runtime) delay(ctx context.Context, id string, minimum, maximum, ordinary time.Duration) error {
	if r.wait != nil {
		return r.wait(ctx, id, minimum, maximum)
	}
	return sleep(ctx, ordinary)
}

// Fixed-latency adapters expose a +/-25% experimental timing window only when
// controlled. The ordinary simulator continues to use its configured latency.
func (r *Runtime) ioDelay(ctx context.Context, id string, ordinary time.Duration) error {
	ordinary = max(0, ordinary)
	return r.delay(ctx, id, ordinary-ordinary/4, ordinary+ordinary/4, ordinary)
}

// Now is the instant every deadline in the simulated world is compared against.
func (r *Runtime) Now() time.Time { return r.now() }

func (r *Runtime) Network() *Network         { return r.network }
func (r *Runtime) ObjectStore() *ObjectStore { return r.objects }
func (r *Runtime) Trace() *Trace             { return r.trace }

func (r *Runtime) NewDisk(id string, config DiskConfig) *Disk {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.disks[id]; exists {
		panic("sim: duplicate disk id: " + id)
	}
	disk := newDisk(r, id, config.withDefaults(DefaultDiskConfig()))
	r.disks[id] = disk
	return disk
}

// sample returns stable pseudo-random data for a semantic key. Unlike a shared
// PRNG stream, adding a random choice in one module cannot perturb another.
func (r *Runtime) sample(key string) uint64 {
	return keyedSample(r.seed, key)
}

func keyedSample(value uint64, key string) uint64 {
	h := fnv.New64a()
	var seed [8]byte
	binary.LittleEndian.PutUint64(seed[:], value)
	_, _ = h.Write(seed[:])
	_, _ = h.Write([]byte(key))
	return mix64(h.Sum64())
}

func mix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

func jitter(sample uint64, maximum time.Duration) time.Duration {
	if maximum <= 0 {
		return 0
	}
	span := uint64(maximum)*2 + 1
	return time.Duration(sample%span) - maximum
}
