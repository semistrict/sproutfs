// Command sproutfs-peerbench measures a guest fault's latency while a post-copy
// stream fills the link between two hosts, and stripe reads beside it, over the
// peer server (docs/migration.md#the-peer-server).
//
//	sproutfs-peerbench server -listen :7500
//	    serve one VM of several memory regions of 2 MiB pages of noise, and a
//	    disk cache that answers every stripe read from a file, so a stripe goes
//	    out with sendfile.
//	sproutfs-peerbench client -server host:7500 -case stream -out results.json
//	    run one case and append its record: a fault on one page of each region
//	    every few milliseconds, with the stream, stripe reads, both or neither
//	    beside it.
//
// scripts/bench-peer-gce.sh builds the same workload against the release
// before, whose page server had no classes and no background budget, and runs
// both on two GCE hosts.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/adapters"
	"github.com/semistrict/sproutfs/rank"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sproutfs-peerbench server|client [flags]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "server":
		err = runServer(ctx, os.Args[2:])
	case "client":
		err = runClient(ctx, os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q: want server or client\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		slog.Error("sproutfs-peerbench: exiting", "mode", os.Args[1], "error", err)
		os.Exit(1)
	}
}

const (
	pageBytes   = peer.MaxPageSize
	stripeBytes = 90 << 10
	// stripeFile is the file stripes are read from: 256 MiB of noise, more
	// than the stripes a case reads, so a read is a range of a file and not
	// of one buffer.
	stripeFileBytes = 256 << 20
)

var cacheIdentity = rank.Identity{0x70, 0x65, 0x65, 0x72}

// benchMembership is the membership the server and its client both hold: the
// server, a member of its own, serving its one disk.
var benchMembership = func() membership.Membership {
	m, err := membership.New(1, rank.CodeFor(1),
		[]membership.Member{{ID: cacheIdentity, Address: "server", State: membership.Active}},
		[]membership.Disk{{ID: cacheIdentity, Volume: "peerbench-stripes", Weight: 1, Member: cacheIdentity,
			State: membership.Serving, Assigned: 1}})
	if err != nil {
		panic(err)
	}
	return m
}()

func runServer(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	listen := flags.String("listen", ":7500", "address to serve on")
	regions := flags.Int("regions", 4, "memory regions of the VM served")
	pages := flags.Uint64("pages", 1024, "2 MiB pages in each region")
	dir := flags.String("dir", os.TempDir(), "directory for the stripe file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cache, err := openCache(ctx, *dir)
	if err != nil {
		return err
	}
	pool := noisePages(16, pageBytes)
	server, err := peer.NewServer(ctx, peer.ServerConfig{Network: adapters.NewNetwork(),
		Address: platform.Address(*listen), PageSize: pageBytes, Cache: cache,
		Membership: membership.NewFixed(benchMembership), Member: cacheIdentity})
	if err != nil {
		return err
	}
	served := make(map[string]peer.Pages, *regions)
	for region := range *regions {
		served[regionName(region)] = benchPages{pool: pool, count: *pages}
	}
	server.Serve("vm", served)
	slog.Info("sproutfs-peerbench: serving", "address", *listen, "regions", *regions, "pages", *pages)
	<-ctx.Done()
	stats := server.Stats()
	slog.Info("sproutfs-peerbench: done", "requests", stats.Requests, "served", stats.Served, "refused", stats.Refused)
	return server.Close()
}

func regionName(region int) string { return fmt.Sprintf("region%d", region) }

// benchPages is a region whose every page is held and no checkpoint's, so a
// fault on it asks again while the source is busy, as one on a page only the
// source holds does.
type benchPages struct {
	pool  [][]byte
	count uint64
}

func (p benchPages) ReadResident(_ context.Context, page uint64, dst []byte) (bool, bool, error) {
	if page >= p.count {
		return false, false, peer.ErrPastEnd
	}
	copy(dst, p.pool[page%uint64(len(p.pool))])
	return true, true, nil
}

func (p benchPages) Resident() ([]uint64, error) { return p.Unpublished() }

func (p benchPages) Unpublished() ([]uint64, error) {
	pages := make([]uint64, p.count)
	for i := range pages {
		pages[i] = uint64(i)
	}
	return pages, nil
}

func (p benchPages) PageSize() uint64 { return pageBytes }

// benchCache answers every stripe read with one stripe from its file, at an
// offset the window and page choose.
type benchCache struct{ file platform.File }

func openCache(ctx context.Context, dir string) (*benchCache, error) {
	path := filepath.Join(dir, "peerbench-stripes")
	noise := noisePages(stripeFileBytes/pageBytes, pageBytes)
	var data []byte
	for _, page := range noise {
		data = append(data, page...)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	disk, err := adapters.NewDisk(dir)
	if err != nil {
		return nil, err
	}
	file, err := disk.Open(ctx, "peerbench-stripes", platform.OpenOptions{})
	if err != nil {
		return nil, err
	}
	return &benchCache{file: file}, nil
}

func (c *benchCache) Identity() rank.Identity { return cacheIdentity }

func (c *benchCache) ReadStripes(_ context.Context, _ membership.Membership, read peer.StripeRead) (peer.Stripes, error) {
	if read.MaxBytes < stripeBytes || len(read.Pages) != 1 {
		return peer.Stripes{}, fmt.Errorf("a read of %d bytes of %d pages", read.MaxBytes, len(read.Pages))
	}
	slot := (read.Window.Number*512 + uint64(read.Pages[0])) % (stripeFileBytes / stripeBytes)
	return peer.Stripes{Items: []peer.StripeItem{{Page: read.Pages[0], Index: 0, Length: 4 * stripeBytes,
		Size: stripeBytes}}, Payload: platform.FileRange{File: c.file, Offset: int64(slot * stripeBytes)},
		Size: stripeBytes}, nil
}

func (c *benchCache) Keep(context.Context, membership.Membership, peer.Keep) error {
	return peer.ErrDropped
}

func (c *benchCache) Drop(context.Context, peer.Drop) error { return nil }

func (c *benchCache) Presence(_ context.Context, presence peer.Presence) ([][]uint32, error) {
	return make([][]uint32, len(presence.Windows)), nil
}

func runClient(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("client", flag.ContinueOnError)
	server := flags.String("server", "", "the server's address")
	name := flags.String("case", "stream", "idle, stream, stripes or stream-stripes")
	regions := flags.Int("regions", 4, "memory regions to fault and stream")
	pages := flags.Uint64("pages", 1024, "2 MiB pages in each region")
	duration := flags.Duration("duration", 30*time.Second, "how long faults are timed")
	faultEvery := flags.Duration("fault-every", 20*time.Millisecond, "time between two faults of one region")
	stripeRate := flags.Int("stripes", 500, "stripe reads a second, in the cases that read them")
	out := flags.String("out", "", "file to append the case's record to")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *server == "" || *out == "" {
		return errors.New("-server and -out are required")
	}
	config := benchConfig{Case: *name, Regions: *regions, Pages: *pages, PageBytes: pageBytes, Duration: *duration,
		FaultEvery: *faultEvery}
	switch *name {
	case "idle":
	case "stream":
		config.Stream = true
	case "stripes":
		config.StripesPerSecond = *stripeRate
	case "stream-stripes":
		config.Stream, config.StripesPerSecond = true, *stripeRate
	default:
		return fmt.Errorf("unknown case %q", *name)
	}
	network := adapters.NewNetwork()
	table, err := peer.NewTable(ctx, peer.TableConfig{Dial: func(ctx context.Context, to platform.Address) (platform.Conn, error) {
		return network.Dial(ctx, "", to)
	}})
	if err != nil {
		return err
	}
	defer table.Close()
	client := tableClient{source: table.Peer(platform.Address(*server))}
	result, err := runCase(ctx, client, client, config)
	if err != nil {
		return err
	}
	return appendRecord(*out, result)
}

// tableClient is this release's destination: one table of peers, a guest
// fault in the fault class, the stream in bulk reads at the background budget.
type tableClient struct{ source *peer.Peer }

// streamWorkers is a received memory region's stream concurrency.
const streamWorkers = 4

func (c tableClient) streamConcurrency() int { return streamWorkers }

func (c tableClient) fault(ctx context.Context, region int, page uint64) (bool, error) {
	return c.ask(ctx, region, page)
}

func (c tableClient) stream(ctx context.Context, region int, page uint64) (bool, error) {
	return c.ask(peer.WithPriority(peer.WithStream(ctx), peer.Resident), region, page)
}

func (c tableClient) ask(ctx context.Context, region int, page uint64) (bool, error) {
	answer, err := c.source.Pages(ctx, peer.PageRequest{VM: "vm", Volume: regionName(region), First: page, Count: 1,
		PageSize: pageBytes})
	if err != nil {
		return false, err
	}
	if answer.Busy != nil {
		return true, nil
	}
	if len(answer.Payload) != pageBytes {
		return false, fmt.Errorf("page %d of %s came back as %d bytes", page, regionName(region), len(answer.Payload))
	}
	return false, nil
}

func (c tableClient) readStripe(ctx context.Context, n uint64) error {
	route, _ := benchMembership.Route(cacheIdentity)
	reply, err := c.source.ReadStripes(ctx, route, peer.StripeRead{
		Window: rank.Window{Volume: "ram0", Number: n / 512}, Pages: []uint32{uint32(n % 512)},
		Code: rank.Code{K: 4, M: 2}, MaxBytes: 128 << 10})
	if err != nil {
		return err
	}
	defer reply.Release()
	if len(reply.Payload) != stripeBytes {
		return fmt.Errorf("stripe %d came back as %d bytes", n, len(reply.Payload))
	}
	return nil
}

func appendRecord(path string, result caseResult) error {
	line, err := json.Marshal(result)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	fmt.Println(string(line))
	return file.Close()
}
