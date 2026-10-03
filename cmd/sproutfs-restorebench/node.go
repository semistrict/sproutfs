package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/pprof"
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

// volume is the one volume a published guest has: its memory.
const volume = "ram0"

// nodeConfig is what one host of the cluster runs over.
type nodeConfig struct {
	// address is where peers reach this node's peer server, listen where it
	// listens, and dialFrom the address it dials them from, empty over TCP.
	address  platform.Address
	listen   platform.Address
	dialFrom platform.Address
	network  platform.Network
	objects  platform.ObjectStore
	// file is the cache's file on the node's disk, of which the cache may
	// hold cacheBytes.
	file       platform.File
	cacheBytes int64
	deployment checkpoint.CacheDeployment
	// memoryBytes is each cache's memory tier, and fillQueueBytes what the
	// cache holds of the fills it has not sent yet.
	memoryBytes    int64
	fillQueueBytes int64
	// serveRate is the peer server's serving bandwidth for stripes.
	serveRate int64
	// dropPageCache drops the kernel's page cache, so the next read of the
	// cache's file reads the disk.
	dropPageCache func() error
}

// node is one host of the cluster: its cache, the stores that read through
// it and around it, its peer server and its table of peers.
type node struct {
	config  nodeConfig
	objects *platform.MeteredObjectStore
	table   *peer.Table
	cache   *checkpoint.Cache
	// clustered reads through the cache, its disk and the cluster; direct
	// reads through a cache that keeps nothing on disk, so every page it
	// misses in memory is read from the store.
	clustered, direct *checkpoint.Store
	directCache       *checkpoint.Cache

	mu     sync.Mutex
	server *peer.Server
	list   atomic.Pointer[rank.List]
	// served is what the servers this node has run served before the one
	// running now.
	served peer.ServerStats
	// guests is the memory of each guest this node has published or read.
	guests map[string]*guest
}

func runNode(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("node", flag.ContinueOnError)
	listen := flags.String("listen", ":7500", "address the peer server listens on")
	advertise := flags.String("advertise", "", "address peers reach this node's peer server at")
	controlAddress := flags.String("control", ":7600", "address the control API listens on")
	dir := flags.String("dir", "/mnt/ssd", "directory of the cache's file")
	cacheBytes := flags.Int64("cache-bytes", 40<<30, "bytes of disk the cache may hold")
	memoryBytes := flags.Int64("memory-bytes", 1<<30, "bytes of memory each cache's memory tier may hold")
	fillQueueBytes := flags.Int64("fill-queue-bytes", 4<<30, "bytes of fills the cache holds before it drops them")
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
	disk, err := adapters.NewDisk(*dir)
	if err != nil {
		return err
	}
	file, err := disk.Open(ctx, "cache", platform.OpenOptions{Create: true})
	if err != nil {
		return err
	}
	defer file.Close()
	n, err := newNode(ctx, nodeConfig{address: platform.Address(*advertise), listen: platform.Address(*listen),
		network: adapters.NewNetwork(),
		objects: store, file: file, cacheBytes: *cacheBytes,
		deployment:  checkpoint.CacheDeployment{Store: "gcs", Bucket: *bucket, Prefix: *prefix},
		memoryBytes: *memoryBytes, fillQueueBytes: *fillQueueBytes, serveRate: *serveRate,
		dropPageCache: dropPageCache})
	if err != nil {
		return err
	}
	defer n.close()
	server := &http.Server{Addr: *controlAddress, Handler: n.handler(), ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	slog.InfoContext(ctx, "node: serving", "cache", n.cache.Identity(), "peer", *advertise, "control", *controlAddress)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// newNode opens a node's table of peers, its two caches and stores, and its
// peer server. It follows a list of itself alone until it is given the
// cluster's.
func newNode(ctx context.Context, config nodeConfig) (*node, error) {
	metered, err := platform.NewMeteredObjectStore(config.objects, nil)
	if err != nil {
		return nil, err
	}
	n := &node{config: config, objects: metered, guests: make(map[string]*guest)}
	opened := false
	defer func() {
		if !opened {
			n.close()
		}
	}()
	n.table, err = peer.NewTable(ctx, peer.TableConfig{Dial: func(ctx context.Context, to platform.Address) (platform.Conn, error) {
		return config.network.Dial(ctx, config.dialFrom, to)
	}})
	if err != nil {
		return nil, err
	}
	// Every case clears the memory tier before it reads, and no case reads a
	// page twice, so the memory tier serves no page a case reads. It is large
	// enough to keep the page tables a restore loads once.
	memory, err := resource.New(config.memoryBytes)
	if err != nil {
		return nil, err
	}
	// Every window is in the share, and the fills of a publication have room
	// to put all of it on its ranks: the cluster is measured holding the whole
	// guest.
	n.cache, err = checkpoint.NewCache(ctx, memory, checkpoint.CacheConfig{Disk: config.file,
		DiskBytes: config.cacheBytes, Deployment: config.deployment, ClusterPercent: 100, Peers: n.table,
		FillQueueBytes: config.fillQueueBytes, FillBytesPerSecond: 4 << 30})
	if err != nil {
		return nil, err
	}
	alone := rank.Alone(rank.Cache{Identity: n.cache.Identity(), Weight: 1, Address: config.address})
	n.list.Store(&alone)
	n.cache.FollowCaches(func() rank.List { return *n.list.Load() })
	n.clustered, err = checkpoint.NewStore(checkpoint.Config{ObjectStore: metered, Cache: n.cache})
	if err != nil {
		return nil, err
	}
	directMemory, err := resource.New(config.memoryBytes)
	if err != nil {
		return nil, err
	}
	n.directCache, err = checkpoint.NewCache(ctx, directMemory, checkpoint.CacheConfig{})
	if err != nil {
		return nil, err
	}
	n.direct, err = checkpoint.NewStore(checkpoint.Config{ObjectStore: metered, Cache: n.directCache})
	if err != nil {
		return nil, err
	}
	if err := n.serve(ctx); err != nil {
		return nil, err
	}
	opened = true
	return n, nil
}

// close closes what newNode opened, the peer server first.
func (n *node) close() {
	n.stopServing()
	if n.directCache != nil {
		n.directCache.Close()
	}
	if n.cache != nil {
		n.cache.Close()
	}
	if n.table != nil {
		_ = n.table.Close()
	}
}

// serve starts the peer server.
func (n *node) serve(ctx context.Context) error {
	server, err := peer.NewServer(ctx, peer.ServerConfig{Network: n.config.network, Address: n.config.listen,
		PageSize: checkpoint.PageSize2MiB, Cache: n.cache, StripeBytesPerSecond: n.config.serveRate})
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

func (n *node) identity(context.Context, struct{}) (identityReply, error) {
	identity := n.cache.Identity()
	return identityReply{Identity: hex.EncodeToString(identity[:]), Address: string(n.config.address)}, nil
}

// listRequest is the list of caches every node follows.
type listRequest struct {
	Code   string          `json:"code"`
	Caches []identityReply `json:"caches"`
}

func (n *node) follow(_ context.Context, request listRequest) (struct{}, error) {
	code, err := rank.ParseCode(request.Code)
	if err != nil {
		return struct{}{}, err
	}
	var caches []rank.Cache
	for _, cache := range request.Caches {
		identity, err := rank.ParseIdentity(cache.Identity)
		if err != nil {
			return struct{}{}, err
		}
		caches = append(caches, rank.Cache{Identity: identity, Weight: 1, Address: platform.Address(cache.Address)})
	}
	list, err := rank.NewList(code, caches)
	if err != nil {
		return struct{}{}, err
	}
	n.list.Store(&list)
	return struct{}{}, nil
}

// guestRequest names a guest: its VM, and its memory's pages.
type guestRequest struct {
	VM       string `json:"vm"`
	PageSize uint64 `json:"page_size"`
	Pages    uint64 `json:"pages"`
}

// guestOf is the memory of the guest request names, made once.
func (n *node) guestOf(request guestRequest) (*guest, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if g := n.guests[request.VM]; g != nil {
		if g.pageSize != request.PageSize || g.pages != request.Pages {
			return nil, fmt.Errorf("guest %s has %d pages of %d bytes, not %d of %d", request.VM, g.pages, g.pageSize,
				request.Pages, request.PageSize)
		}
		return g, nil
	}
	g, err := newGuest(request.VM, request.PageSize, request.Pages)
	if err != nil {
		return nil, err
	}
	n.guests[request.VM] = g
	return g, nil
}

// publishReply is what the publication took and what its fills did.
type publishReply struct {
	Sequence uint64               `json:"sequence"`
	Seconds  float64              `json:"seconds"`
	Settled  float64              `json:"settled_seconds"`
	Fill     checkpoint.FillStats `json:"fill"`
}

// publish publishes the guest's memory, every page of it, and waits for its
// fills to settle.
func (n *node) publish(ctx context.Context, request guestRequest) (publishReply, error) {
	g, err := n.guestOf(request)
	if err != nil {
		return publishReply{}, err
	}
	began := time.Now()
	root, err := n.clustered.Root(ctx, control.Ref{VM: request.VM, Sequence: 1},
		map[string]checkpoint.VolumeSpec{volume: {Size: g.pages * g.pageSize, PageSize: g.pageSize}})
	if err != nil {
		return publishReply{}, err
	}
	publication := n.clustered.Begin(root, control.Ref{VM: request.VM, Sequence: 2})
	for page := range g.pages {
		publication.Dirty(volume, page)
	}
	index, err := publication.Commit(ctx, g)
	if err != nil {
		return publishReply{}, err
	}
	committed := time.Since(began)
	if err := n.cache.SettleFills(ctx); err != nil {
		return publishReply{}, err
	}
	return publishReply{Sequence: index.Ref().Sequence, Seconds: committed.Seconds(),
		Settled: time.Since(began).Seconds(), Fill: n.cache.Stats().Fill}, nil
}

// readRequest reads a published guest's memory back as its access says, from
// the cluster or straight from the store.
type readRequest struct {
	Guest    guestRequest `json:"guest"`
	Sequence uint64       `json:"sequence"`
	Store    bool         `json:"store"`
	Access   access       `json:"access"`
	// Profile asks for the CPU profile of the reads.
	Profile bool `json:"profile"`
}

// readReply is what the reads did, how long opening the checkpoint took
// before them, the reads its memory tier served, and the CPU profile of the
// reads when one was asked for.
type readReply struct {
	walked
	OpenSeconds float64 `json:"open_seconds"`
	MemoryHits  uint64  `json:"memory_hits"`
	Profile     []byte  `json:"profile,omitempty"`
}

func (n *node) read(ctx context.Context, request readRequest) (readReply, error) {
	g, err := n.guestOf(request.Guest)
	if err != nil {
		return readReply{}, err
	}
	// What the reads are checked against is made before they begin.
	_ = g.sum(0)
	store, cache := n.clustered, n.cache
	if request.Store {
		store, cache = n.direct, n.directCache
	}
	began := time.Now()
	index, err := store.Open(ctx, control.Ref{VM: request.Guest.VM, Sequence: request.Sequence})
	if err != nil {
		return readReply{}, err
	}
	out := readReply{OpenSeconds: time.Since(began).Seconds()}
	hits := cache.Stats().Hits
	out.Profile, err = profiled(request.Profile, func() error {
		var err error
		out.walked, err = walk(ctx, g, request.Access, func(ctx context.Context, offset uint64, dst []byte) error {
			return store.Read(ctx, index, volume, offset, dst)
		})
		return err
	})
	if err != nil {
		return readReply{}, err
	}
	out.MemoryHits = cache.Stats().Hits - hits
	return out, nil
}

// profiled runs work, and when enabled is the process's CPU profile while it
// ran.
func profiled(enabled bool, work func() error) ([]byte, error) {
	if !enabled {
		return nil, work()
	}
	var profile bytes.Buffer
	if err := pprof.StartCPUProfile(&profile); err != nil {
		return nil, err
	}
	err := work()
	pprof.StopCPUProfile()
	return profile.Bytes(), err
}

func (n *node) lose(context.Context, struct{}) (struct{}, error) {
	n.stopServing()
	return struct{}{}, nil
}

func (n *node) back(ctx context.Context, _ struct{}) (struct{}, error) {
	return struct{}{}, n.serve(context.WithoutCancel(ctx))
}

// drop empties both caches' memory tiers and drops the kernel's page cache,
// so the next read reads every page from where it is kept.
func (n *node) drop(context.Context, struct{}) (struct{}, error) {
	n.cache.Clear()
	n.directCache.Clear()
	return struct{}{}, n.config.dropPageCache()
}

// dropPageCache drops the kernel's page cache.
func dropPageCache() error {
	syscall.Sync()
	return os.WriteFile("/proc/sys/vm/drop_caches", []byte("3\n"), 0)
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

func (n *node) stats(context.Context, struct{}) (statsReply, error) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return statsReply{}, err
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
	return statsReply{CPUSeconds: time.Duration(usage.Utime.Nano() + usage.Stime.Nano()).Seconds(),
		StripeReads: served.StripeReads, Stripes: served.Stripes, StripeBytes: served.StripeBytes,
		StripesBusy: served.StripesBusy, Read: stats.Read, Fill: stats.Fill, Disk: stats.Disk,
		Store: n.objects.Traffic()}, nil
}
