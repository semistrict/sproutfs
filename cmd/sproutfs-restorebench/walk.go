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

// A walk is a chain of single pages (see access.go) read on the second node
// from the regional bucket, from a hot tier and from the cluster, and the hot
// tier's fills on a cold run. The hot tier's copy of each guest is published
// under a name of its own with the same bytes, so each round reads the same
// chain of pages from every source.

// walkSource is one place a walk reads from: the name results give it, and
// the source the reader reads.
type walkSource struct{ label, source string }

var (
	walkRegional = walkSource{"regional", sourceStore}
	walkHot      = walkSource{"hot", sourceHot}
	walkCluster  = walkSource{"cluster", sourceCluster}
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
	HotStats []statsReply   `json:"after"`
}

// walkConfig is one walk run: the guests' pages, the reads of each walk, the
// rounds, the cluster's code and the seed that orders each round and chooses
// where its chains start.
type walkConfig struct {
	pages, pages4K uint64
	reads, rounds  int
	code           string
	seed           uint64
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
	seed := flags.Uint64("seed", 1, "orders each round's cases and chooses where each round's chains start")
	out := flags.String("out", "walk.json", "where the results go")
	if err := flags.Parse(args); err != nil {
		return err
	}
	client := &http.Client{Timeout: time.Hour}
	var nodes []controller
	for address := range strings.SplitSeq(*nodesFlag, ",") {
		nodes = append(nodes, remote{client: client, address: address})
	}
	result, err := walkRun(ctx, nodes, walkConfig{pages: *pages, pages4K: *pages4K, reads: *reads, rounds: *rounds,
		code: *code, seed: *seed})
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(*out, encoded, 0o644)
}

// walkRun publishes each guest from the first node, into the cluster and,
// under another name, into the hot tier, then walks them on the second.
func walkRun(ctx context.Context, nodes []controller, config walkConfig) (walkResult, error) {
	if len(nodes) < 2 {
		return walkResult{}, errors.New("walk needs at least two nodes")
	}
	if err := followAll(ctx, nodes, config.code); err != nil {
		return walkResult{}, err
	}
	result := walkResult{Pages: config.pages, Pages4K: config.pages4K, Reads: config.reads, Code: config.code}
	guests := []guestRequest{{VM: "guest", Noise: "guest", PageSize: checkpoint.PageSize2MiB, Pages: config.pages},
		{VM: "guest-hot", Noise: "guest", PageSize: checkpoint.PageSize2MiB, Pages: config.pages, Hot: true}}
	if config.pages4K > 0 {
		guests = append(guests,
			guestRequest{VM: "small", Noise: "small", PageSize: checkpoint.PageSize4KiB, Pages: config.pages4K},
			guestRequest{VM: "small-hot", Noise: "small", PageSize: checkpoint.PageSize4KiB, Pages: config.pages4K,
				Hot: true})
	}
	sequences := make(map[string]uint64)
	for _, guest := range guests {
		slog.InfoContext(ctx, "walk: publishing", "vm", guest.VM, "pages", guest.Pages, "page_bytes", guest.PageSize)
		published, err := nodes[0].publish(ctx, guest)
		if err != nil {
			return walkResult{}, err
		}
		slog.InfoContext(ctx, "walk: published", "vm", guest.VM, "seconds", published.Seconds,
			"settled", published.Settled)
		result.Publish = append(result.Publish, published)
		sequences[guest.VM] = published.Sequence
	}
	random := rand.New(rand.NewPCG(config.seed, 0))
	reader := nodes[1]
	one := func(round int, source walkSource, guest guestRequest, seed uint64) (walkCase, error) {
		return walkOne(ctx, reader, round, source, guest, sequences[guest.VM], seed, config.reads)
	}
	// A cold hot tier: the guests whose publications filled the cluster are
	// in the hot tier not at all, so the first walks through it miss and fill
	// it behind them. Each round starts somewhere else, and the fills of each
	// settle before the next.
	for round := range config.rounds {
		for _, guest := range guests {
			if guest.Hot {
				continue
			}
			walked, err := one(round, walkHot, guest, random.Uint64())
			if err != nil {
				return walkResult{}, err
			}
			result.Cold = append(result.Cold, walked)
		}
	}
	// Each round reads the same chain of pages from every source, in an order
	// of its own: the hot tier's copy of each guest has the same bytes under
	// another name, so the same seed starts the same chain through it.
	for round := range config.rounds {
		seed := random.Uint64()
		type planned struct {
			source walkSource
			guest  guestRequest
		}
		var walks []planned
		for _, guest := range guests {
			if guest.Hot {
				walks = append(walks, planned{walkHot, guest})
				continue
			}
			walks = append(walks, planned{walkRegional, guest}, planned{walkCluster, guest})
		}
		random.Shuffle(len(walks), func(a, b int) { walks[a], walks[b] = walks[b], walks[a] })
		for _, planned := range walks {
			walked, err := one(round, planned.source, planned.guest, seed)
			if err != nil {
				return walkResult{}, err
			}
			result.Cases = append(result.Cases, walked)
		}
	}
	var err error
	result.HotStats, err = statsOf(ctx, nodes)
	return result, err
}

// walkOne runs one chain of reads on the reader, with every memory tier and
// the kernel's page cache dropped first and the hot tier's fills settled
// after, and what it cost.
func walkOne(ctx context.Context, reader controller, round int, source walkSource, guest guestRequest,
	sequence, seed uint64, reads int) (walkCase, error) {
	if _, err := reader.drop(ctx, struct{}{}); err != nil {
		return walkCase{}, err
	}
	before, err := reader.stats(ctx, struct{}{})
	if err != nil {
		return walkCase{}, err
	}
	walked, err := reader.read(ctx, readRequest{Guest: guest, Sequence: sequence, Source: source.source,
		Access: access{Pattern: patternChain, Unit: unitPage, Concurrency: 1, Reads: reads, Seed: seed}})
	if err != nil {
		return walkCase{}, err
	}
	if _, err := reader.settle(ctx, struct{}{}); err != nil {
		return walkCase{}, err
	}
	after, err := reader.stats(ctx, struct{}{})
	if err != nil {
		return walkCase{}, err
	}
	one := walkCase{Round: round, Source: source.label, PageBytes: guest.PageSize, Reads: reads,
		Seconds: walked.Seconds, HopsPerSecond: float64(reads) / walked.Seconds, Wrong: walked.Wrong,
		StoreGets:  after.Store.Get.Calls - before.Store.Get.Calls,
		StoreBytes: after.Store.Get.Bytes - before.Store.Get.Bytes,
		HotGets:    after.HotStore.Get.Calls - before.HotStore.Get.Calls,
		HotPuts:    after.HotStore.Put.Calls - before.HotStore.Put.Calls,
		Hot:        hotDelta(before.Hot, after.Hot), Read: readDelta(before.Read, after.Read)}
	one.Latency, one.Histogram = shape(walked.Latencies)
	slog.InfoContext(ctx, "walk: walked", "round", round, "source", source.label, "vm", guest.VM,
		"hops_per_second", one.HopsPerSecond, "p50", one.Latency["p50"], "p90", one.Latency["p90"],
		"p99", one.Latency["p99"], "max", one.Latency["max"], "wrong", one.Wrong,
		"hot", fmt.Sprintf("%+v", one.Hot))
	if walked.Wrong != 0 {
		return one, fmt.Errorf("%s from %s read %d pages wrong", guest.VM, source.label, walked.Wrong)
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
