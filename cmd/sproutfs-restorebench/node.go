package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/adapters"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/resource"
)

// volume is the one volume a published guest has: its memory, in 2 MiB
// pages.
const volume = "ram0"

// node is one host of the cluster: its cache, the stores that read through
// it and around it, its peer server and its table of peers.
type node struct {
	address platform.Address
	listen  platform.Address
	network platform.Network
	objects *platform.MeteredObjectStore
	table   *peer.Table
	cache   *checkpoint.Cache
	// clustered reads through the cache, its disk and the cluster; direct
	// reads through a cache that keeps nothing on disk, so every page it
	// misses in memory is read from the store.
	clustered, direct *checkpoint.Store
	serveRate         int64

	mu     sync.Mutex
	server *peer.Server
	list   atomic.Pointer[rank.List]
	// served is what the servers this node has run served before the one
	// running now.
	served peer.ServerStats
}

func runNode(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("node", flag.ContinueOnError)
	listen := flags.String("listen", ":7500", "address the peer server listens on")
	advertise := flags.String("advertise", "", "address peers reach this node's peer server at")
	controlAddress := flags.String("control", ":7600", "address the control API listens on")
	dir := flags.String("dir", "/mnt/ssd", "directory of the cache's file")
	cacheBytes := flags.Int64("cache-bytes", 40<<30, "bytes of disk the cache may hold")
	bucket := flags.String("bucket", "", "Cloud Storage bucket")
	prefix := flags.String("prefix", "", "prefix of this run's objects in the bucket")
	serveRate := flags.Int64("serve-bytes-per-second", 500<<20, "the peer server's serving bandwidth for stripes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *advertise == "" || *bucket == "" || *prefix == "" {
		return errors.New("node needs -advertise, -bucket and -prefix")
	}
	store, closer, err := adapters.NewGCS(ctx, "", *bucket, *prefix)
	if err != nil {
		return err
	}
	defer closer.Close()
	metered, err := platform.NewMeteredObjectStore(store, nil)
	if err != nil {
		return err
	}
	disk, err := adapters.NewDisk(*dir)
	if err != nil {
		return err
	}
	file, err := disk.Open(ctx, "cache", platform.OpenOptions{Create: true})
	if err != nil {
		return err
	}
	defer file.Close()
	n := &node{address: platform.Address(*advertise), listen: platform.Address(*listen),
		network: adapters.NewNetwork(), objects: metered, serveRate: *serveRate}
	n.table, err = peer.NewTable(ctx, peer.TableConfig{Dial: func(ctx context.Context, to platform.Address) (platform.Conn, error) {
		return n.network.Dial(ctx, "", to)
	}})
	if err != nil {
		return err
	}
	defer n.table.Close()
	// The memory tier holds a few pages, so a restore reads each page from
	// where it is kept rather than from what an earlier round left in memory.
	memory, err := resource.New(64 << 20)
	if err != nil {
		return err
	}
	// Every window is in the share, and the fills of a publication have room
	// to put all of it on its ranks: the cluster is measured holding the whole
	// guest.
	n.cache, err = checkpoint.NewCache(ctx, memory, checkpoint.CacheConfig{Disk: file, DiskBytes: *cacheBytes,
		Deployment: checkpoint.CacheDeployment{Store: "gcs", Bucket: *bucket, Prefix: *prefix}, ClusterPercent: 100,
		Peers: n.table, FillQueueBytes: 4 << 30, FillBytesPerSecond: 4 << 30})
	if err != nil {
		return err
	}
	defer n.cache.Close()
	alone := rank.Alone(rank.Cache{Identity: n.cache.Identity(), Weight: 1, Address: n.address})
	n.list.Store(&alone)
	n.cache.FollowCaches(func() rank.List { return *n.list.Load() })
	n.clustered, err = checkpoint.NewStore(checkpoint.Config{ObjectStore: metered, Cache: n.cache})
	if err != nil {
		return err
	}
	directMemory, err := resource.New(64 << 20)
	if err != nil {
		return err
	}
	directCache, err := checkpoint.NewCache(ctx, directMemory, checkpoint.CacheConfig{})
	if err != nil {
		return err
	}
	defer directCache.Close()
	n.direct, err = checkpoint.NewStore(checkpoint.Config{ObjectStore: metered, Cache: directCache})
	if err != nil {
		return err
	}
	if err := n.serve(ctx); err != nil {
		return err
	}
	defer n.stopServing()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /identity", n.handleIdentity)
	mux.HandleFunc("POST /list", n.handleList)
	mux.HandleFunc("POST /publish", n.handlePublish)
	mux.HandleFunc("POST /restore", n.handleRestore)
	mux.HandleFunc("POST /lose", n.handleLose)
	mux.HandleFunc("POST /back", n.handleBack)
	mux.HandleFunc("POST /drop", n.handleDrop)
	mux.HandleFunc("GET /stats", n.handleStats)
	server := &http.Server{Addr: *controlAddress, Handler: mux, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	slog.InfoContext(ctx, "node: serving", "cache", n.cache.Identity(), "peer", n.address, "control", *controlAddress)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// serve starts the peer server.
func (n *node) serve(ctx context.Context) error {
	server, err := peer.NewServer(ctx, peer.ServerConfig{Network: n.network, Address: n.listen,
		PageSize: checkpoint.PageSize2MiB, Cache: n.cache, StripeBytesPerSecond: n.serveRate})
	if err != nil {
		return err
	}
	n.mu.Lock()
	n.server = server
	n.mu.Unlock()
	return nil
}

// stopServing closes the peer server, as a host that is lost stops answering,
// and keeps what it served.
func (n *node) stopServing() {
	n.mu.Lock()
	server := n.server
	n.server = nil
	n.mu.Unlock()
	if server == nil {
		return
	}
	_ = server.Close()
	stats := server.Stats()
	n.mu.Lock()
	n.served.StripeReads += stats.StripeReads
	n.served.Stripes += stats.Stripes
	n.served.StripeBytes += stats.StripeBytes
	n.served.StripesBusy += stats.StripesBusy
	n.mu.Unlock()
}

// identityReply is a node's cache as the list names it.
type identityReply struct {
	Identity string `json:"identity"`
	Address  string `json:"address"`
}

func (n *node) handleIdentity(w http.ResponseWriter, _ *http.Request) {
	identity := n.cache.Identity()
	reply(w, identityReply{Identity: hex.EncodeToString(identity[:]), Address: string(n.address)})
}

// listRequest is the list of caches every node follows.
type listRequest struct {
	Code   string          `json:"code"`
	Caches []identityReply `json:"caches"`
}

func (n *node) handleList(w http.ResponseWriter, r *http.Request) {
	var request listRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		fail(w, err)
		return
	}
	code, err := rank.ParseCode(request.Code)
	if err != nil {
		fail(w, err)
		return
	}
	var caches []rank.Cache
	for _, cache := range request.Caches {
		identity, err := rank.ParseIdentity(cache.Identity)
		if err != nil {
			fail(w, err)
			return
		}
		caches = append(caches, rank.Cache{Identity: identity, Weight: 1, Address: platform.Address(cache.Address)})
	}
	list, err := rank.NewList(code, caches)
	if err != nil {
		fail(w, err)
		return
	}
	n.list.Store(&list)
	reply(w, struct{}{})
}

// publishRequest publishes a guest of pages pages of noise under vm.
type publishRequest struct {
	VM    string `json:"vm"`
	Pages uint64 `json:"pages"`
}

// publishReply is what the publication took and what its fills did.
type publishReply struct {
	Sequence uint64               `json:"sequence"`
	Seconds  float64              `json:"seconds"`
	Settled  float64              `json:"settled_seconds"`
	Fill     checkpoint.FillStats `json:"fill"`
}

func (n *node) handlePublish(w http.ResponseWriter, r *http.Request) {
	var request publishRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		fail(w, err)
		return
	}
	ctx := r.Context()
	began := time.Now()
	root, err := n.clustered.Root(ctx, control.Ref{VM: request.VM, Sequence: 1},
		map[string]checkpoint.VolumeSpec{volume: {Size: request.Pages * checkpoint.PageSize2MiB,
			PageSize: checkpoint.PageSize2MiB}})
	if err != nil {
		fail(w, err)
		return
	}
	publication := n.clustered.Begin(root, control.Ref{VM: request.VM, Sequence: 2})
	for page := range request.Pages {
		publication.Dirty(volume, page)
	}
	index, err := publication.Commit(ctx, noise{vm: request.VM})
	if err != nil {
		fail(w, err)
		return
	}
	committed := time.Since(began)
	if err := n.cache.SettleFills(ctx); err != nil {
		fail(w, err)
		return
	}
	reply(w, publishReply{Sequence: index.Ref().Sequence, Seconds: committed.Seconds(),
		Settled: time.Since(began).Seconds(), Fill: n.cache.Stats().Fill})
}

// noise is a guest's memory: every page bytes no encoder shrinks, drawn from
// the VM's name and the page.
type noise struct{ vm string }

func (s noise) ReadPage(_ context.Context, _ string, page uint64, dst []byte) error {
	state := page*0x9e3779b97f4a7c15 ^ 0xd1b54a32d192ed03
	for _, b := range []byte(s.vm) {
		state = state*31 + uint64(b)
	}
	for at := 0; at+8 <= len(dst); at += 8 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		binary.LittleEndian.PutUint64(dst[at:], state)
	}
	return nil
}

// restoreRequest reads every page of a published guest back, concurrency at a
// time, from the cluster or straight from the store.
type restoreRequest struct {
	VM          string `json:"vm"`
	Sequence    uint64 `json:"sequence"`
	Pages       uint64 `json:"pages"`
	Store       bool   `json:"store"`
	Concurrency int    `json:"concurrency"`
}

// restoreReply is how long the restore took, and each page's read.
type restoreReply struct {
	Seconds   float64 `json:"seconds"`
	Latencies []int64 `json:"latencies_ns"`
	Wrong     int     `json:"wrong"`
	Failed    int     `json:"failed"`
}

func (n *node) handleRestore(w http.ResponseWriter, r *http.Request) {
	var request restoreRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		fail(w, err)
		return
	}
	ctx := r.Context()
	store := n.clustered
	if request.Store {
		store = n.direct
	}
	began := time.Now()
	index, err := store.Open(ctx, control.Ref{VM: request.VM, Sequence: request.Sequence})
	if err != nil {
		fail(w, err)
		return
	}
	latencies := make([]int64, request.Pages)
	var next atomic.Uint64
	var wrong, failed atomic.Int64
	var workers sync.WaitGroup
	for range max(request.Concurrency, 1) {
		workers.Go(func() {
			got := make([]byte, checkpoint.PageSize2MiB)
			want := make([]byte, checkpoint.PageSize2MiB)
			source := noise{vm: request.VM}
			for {
				page := next.Add(1) - 1
				if page >= request.Pages {
					return
				}
				start := time.Now()
				err := store.Read(ctx, index, volume, page*checkpoint.PageSize2MiB, got)
				latencies[page] = int64(time.Since(start))
				if err != nil {
					slog.WarnContext(ctx, "node: a page did not read", "page", page, "error", err)
					failed.Add(1)
					continue
				}
				_ = source.ReadPage(ctx, volume, page, want)
				if string(got) != string(want) {
					wrong.Add(1)
				}
			}
		})
	}
	workers.Wait()
	reply(w, restoreReply{Seconds: time.Since(began).Seconds(), Latencies: latencies, Wrong: int(wrong.Load()),
		Failed: int(failed.Load())})
}

func (n *node) handleLose(w http.ResponseWriter, _ *http.Request) {
	n.stopServing()
	reply(w, struct{}{})
}

func (n *node) handleBack(w http.ResponseWriter, r *http.Request) {
	if err := n.serve(context.WithoutCancel(r.Context())); err != nil {
		fail(w, err)
		return
	}
	reply(w, struct{}{})
}

// handleDrop drops the kernel's page cache, so the next restore reads the
// cache's file from the disk rather than from memory.
func (n *node) handleDrop(w http.ResponseWriter, _ *http.Request) {
	syscall.Sync()
	if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3\n"), 0); err != nil {
		fail(w, err)
		return
	}
	reply(w, struct{}{})
}

// statsReply is what a node has done since it started.
type statsReply struct {
	CPUSeconds  float64                `json:"cpu_seconds"`
	StripeReads int64                  `json:"stripe_reads"`
	Stripes     int64                  `json:"stripes"`
	StripeBytes int64                  `json:"stripe_bytes"`
	StripesBusy int64                  `json:"stripes_busy"`
	Read        checkpoint.ReadStats   `json:"read"`
	Fill        checkpoint.FillStats   `json:"fill"`
	Disk        checkpoint.DiskStats   `json:"disk"`
	Store       platform.ObjectTraffic `json:"store"`
}

func (n *node) handleStats(w http.ResponseWriter, _ *http.Request) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		fail(w, err)
		return
	}
	n.mu.Lock()
	served := n.served
	if n.server != nil {
		stats := n.server.Stats()
		served.StripeReads += stats.StripeReads
		served.Stripes += stats.Stripes
		served.StripeBytes += stats.StripeBytes
		served.StripesBusy += stats.StripesBusy
	}
	n.mu.Unlock()
	stats := n.cache.Stats()
	reply(w, statsReply{CPUSeconds: time.Duration(usage.Utime.Nano() + usage.Stime.Nano()).Seconds(),
		StripeReads: served.StripeReads, Stripes: served.Stripes, StripeBytes: served.StripeBytes,
		StripesBusy: served.StripesBusy, Read: stats.Read, Fill: stats.Fill, Disk: stats.Disk,
		Store: n.objects.Traffic()})
}

func reply(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Warn("node: a reply did not go", "error", err)
	}
}

func fail(w http.ResponseWriter, err error) {
	http.Error(w, fmt.Sprintf("%v", err), http.StatusInternalServerError)
}
