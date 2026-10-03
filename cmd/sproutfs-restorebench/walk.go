package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
)

// walkCase is one walk of one round: dependent single reads of one guest
// from one source.
type walkCase struct {
	Round     int     `json:"round"`
	Source    string  `json:"source"`
	PageBytes uint64  `json:"page_bytes"`
	Reads     int     `json:"reads"`
	Seconds   float64 `json:"seconds"`
	// HopsPerSecond is the reads the walk made in a second: each waited for
	// the one before it.
	HopsPerSecond float64            `json:"hops_per_second"`
	Latency       map[string]float64 `json:"latency_ms"`
	Histogram     []int              `json:"histogram"`
	Wrong         int                `json:"wrong"`
	// StoreGets and StoreBytes are the reader's requests of the regional
	// bucket, HotGets and HotPuts its requests of the hot bucket, and Hot what
	// its hot tier did, over the walk and the fills it settled.
	StoreGets  int64                   `json:"store_gets"`
	StoreBytes int64                   `json:"store_bytes"`
	HotGets    int64                   `json:"hot_gets"`
	HotPuts    int64                   `json:"hot_puts"`
	Hot        checkpoint.HotTierStats `json:"hot"`
	Read       checkpoint.ReadStats    `json:"read"`
}

// walkResult is a whole walk run.
type walkResult struct {
	Pages    uint64         `json:"pages"`
	Pages4K  uint64         `json:"pages_4k"`
	Reads    int            `json:"reads"`
	Code     string         `json:"code"`
	Publish  []publishReply `json:"publish"`
	Cold     []walkCase     `json:"cold"`
	Cases    []walkCase     `json:"cases"`
	Settled  []float64      `json:"cold_settled_seconds"`
	HotStats []statsReply   `json:"after"`
}

// guestOf is a guest the walk publishes: its name, the noise its pages are
// drawn from, its pages and their size, and whether its publication writes the
// hot tier rather than filling the cluster.
type guestOf struct {
	vm, noise string
	pages     uint64
	pageBytes uint64
	hot       bool
}

// runWalk measures dependent single reads from the regional bucket, from a
// warm hot tier and from the cluster, and the hot tier's fills on a cold run.
func runWalk(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("walk", flag.ContinueOnError)
	nodesFlag := flags.String("nodes", "", "control addresses of the nodes, the publisher first and the reader second")
	pages := flags.Uint64("pages", 1024, "2 MiB pages of the guest")
	pages4K := flags.Uint64("pages-4k", 131072, "4 KiB pages of the second guest, none when zero")
	reads := flags.Int("reads", 500, "reads of one walk")
	rounds := flags.Int("rounds", 3, "rounds of every case")
	code := flags.String("code", "4+2", "the cluster's code")
	seed := flags.Uint64("seed", 1, "orders each round's cases and chooses each round's first page")
	out := flags.String("out", "walk.json", "where the results go")
	if err := flags.Parse(args); err != nil {
		return err
	}
	nodes := strings.Split(*nodesFlag, ",")
	if len(nodes) < 2 {
		return errors.New("walk needs at least two nodes")
	}
	client := &http.Client{Timeout: time.Hour}
	var caches []identityReply
	for _, address := range nodes {
		var identity identityReply
		if err := call(ctx, client, address, "GET", "/identity", nil, &identity); err != nil {
			return err
		}
		caches = append(caches, identity)
	}
	for _, address := range nodes {
		if err := call(ctx, client, address, "POST", "/list", listRequest{Code: *code, Caches: caches}, nil); err != nil {
			return err
		}
	}
	result := walkResult{Pages: *pages, Pages4K: *pages4K, Reads: *reads, Code: *code}
	guests := []guestOf{{vm: "guest", noise: "guest", pages: *pages, pageBytes: checkpoint.PageSize2MiB},
		{vm: "guest-hot", noise: "guest", pages: *pages, pageBytes: checkpoint.PageSize2MiB, hot: true}}
	if *pages4K > 0 {
		guests = append(guests,
			guestOf{vm: "small", noise: "small", pages: *pages4K, pageBytes: checkpoint.PageSize4KiB},
			guestOf{vm: "small-hot", noise: "small", pages: *pages4K, pageBytes: checkpoint.PageSize4KiB, hot: true})
	}
	for _, guest := range guests {
		var published publishReply
		slog.InfoContext(ctx, "walk: publishing", "vm", guest.vm, "pages", guest.pages, "page_bytes", guest.pageBytes)
		if err := call(ctx, client, nodes[0], "POST", "/publish", publishRequest{VM: guest.vm, Noise: guest.noise,
			Pages: guest.pages, PageBytes: guest.pageBytes, Hot: guest.hot}, &published); err != nil {
			return err
		}
		slog.InfoContext(ctx, "walk: published", "vm", guest.vm, "seconds", published.Seconds,
			"settled", published.Settled)
		result.Publish = append(result.Publish, published)
	}
	random := rand.New(rand.NewPCG(*seed, 0))
	reader := nodes[1]
	// A cold hot tier: the guests whose publications filled the cluster are
	// in the hot tier not at all, so the first walks through it miss and fill
	// it behind them. Each round starts somewhere else, and the fills of each
	// settle before the next.
	for round := range *rounds {
		for _, guest := range guests {
			if guest.hot {
				continue
			}
			one, err := walkOne(ctx, client, reader, round, "hot", guest, random.Uint64(), *reads)
			if err != nil {
				return err
			}
			result.Cold = append(result.Cold, one)
		}
	}
	// Each round reads the same chain of pages from every source, in an
	// order of its own: the hot tier's copy of each guest has the same bytes
	// under another name, filled by its publication.
	for round := range *rounds {
		start := random.Uint64()
		type walk struct {
			source string
			guest  guestOf
		}
		var walks []walk
		for _, guest := range guests {
			switch {
			case guest.hot:
				walks = append(walks, walk{"hot", guest})
			default:
				walks = append(walks, walk{"regional", guest}, walk{"cluster", guest})
			}
		}
		random.Shuffle(len(walks), func(a, b int) { walks[a], walks[b] = walks[b], walks[a] })
		for _, one := range walks {
			measured, err := walkOne(ctx, client, reader, round, one.source, one.guest, start, *reads)
			if err != nil {
				return err
			}
			result.Cases = append(result.Cases, measured)
		}
	}
	stats, err := statsOf(ctx, client, nodes)
	if err != nil {
		return err
	}
	result.HotStats = stats
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(*out, encoded, 0o644)
}

// walkOne runs one walk on the reader and what it cost, with the hot tier's
// fills settled after it.
func walkOne(ctx context.Context, client *http.Client, reader string, round int, source string, guest guestOf,
	start uint64, reads int) (walkCase, error) {
	var before, after statsReply
	if err := call(ctx, client, reader, "GET", "/stats", nil, &before); err != nil {
		return walkCase{}, err
	}
	var walked walkReply
	if err := call(ctx, client, reader, "POST", "/walk", walkRequest{VM: guest.vm, Noise: guest.noise, Sequence: 2,
		Pages: guest.pages, PageBytes: guest.pageBytes, Reads: reads, Start: start, Source: source},
		&walked); err != nil {
		return walkCase{}, err
	}
	if err := call(ctx, client, reader, "POST", "/settle", nil, nil); err != nil {
		return walkCase{}, err
	}
	if err := call(ctx, client, reader, "GET", "/stats", nil, &after); err != nil {
		return walkCase{}, err
	}
	one := walkCase{Round: round, Source: source, PageBytes: guest.pageBytes, Reads: reads, Seconds: walked.Seconds,
		HopsPerSecond: float64(reads) / walked.Seconds, Wrong: walked.Wrong,
		StoreGets:  after.Store.Get.Calls - before.Store.Get.Calls,
		StoreBytes: after.Store.Get.Bytes - before.Store.Get.Bytes,
		HotGets:    after.HotStore.Get.Calls - before.HotStore.Get.Calls,
		HotPuts:    after.HotStore.Put.Calls - before.HotStore.Put.Calls,
		Hot:        hotDelta(before.Hot, after.Hot), Read: readDelta(before.Read, after.Read)}
	one.Latency, one.Histogram = shape(walked.Latencies)
	slog.InfoContext(ctx, "walk: walked", "round", round, "source", source, "vm", guest.vm,
		"hops_per_second", one.HopsPerSecond, "p50", one.Latency["p50"], "p90", one.Latency["p90"],
		"p99", one.Latency["p99"], "max", one.Latency["max"], "wrong", one.Wrong,
		"hot", fmt.Sprintf("%+v", one.Hot))
	if walked.Wrong != 0 {
		return one, fmt.Errorf("%s from %s read %d pages wrong", guest.vm, source, walked.Wrong)
	}
	return one, nil
}

// hotDelta is what a hot tier did between two readings.
func hotDelta(before, after checkpoint.HotTierStats) checkpoint.HotTierStats {
	delta := after
	delta.Hits -= before.Hits
	delta.Misses -= before.Misses
	for at := range delta.Failed {
		delta.Failed[at] -= before.Failed[at]
	}
	delta.Skipped -= before.Skipped
	delta.MarkedDown -= before.MarkedDown
	delta.FromReads -= before.FromReads
	delta.FromPublications -= before.FromPublications
	delta.Duplicates -= before.Duplicates
	delta.Sent -= before.Sent
	delta.SentBytes -= before.SentBytes
	delta.Present -= before.Present
	for at := range delta.Dropped {
		delta.Dropped[at] -= before.Dropped[at]
	}
	delta.HeadChecks -= before.HeadChecks
	delta.HeadMissing -= before.HeadMissing
	return delta
}
