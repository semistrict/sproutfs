package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
)

// caseResult is one restore of one round.
type caseResult struct {
	Round   int     `json:"round"`
	Case    string  `json:"case"`
	Seconds float64 `json:"seconds"`
	// Latency is the percentiles of the pages' reads, in milliseconds, and
	// Histogram how many took each power of two of milliseconds, from
	// under a quarter of one.
	Latency   map[string]float64 `json:"latency_ms"`
	Histogram []int              `json:"histogram"`
	Wrong     int                `json:"wrong"`
	Failed    int                `json:"failed"`
	// Served is the stripe bytes each node served during the restore, CPU
	// each node's CPU seconds, and Busy the reads each answered BUSY.
	Served []int64   `json:"served_bytes"`
	CPU    []float64 `json:"cpu_seconds"`
	Busy   []int64   `json:"busy"`
	// Read is what the reader's reads of the cluster did, and StoreGets and
	// StoreBytes its requests of the store.
	Read       checkpoint.ReadStats `json:"read"`
	StoreGets  int64                `json:"store_gets"`
	StoreBytes int64                `json:"store_bytes"`
}

// driveResult is a whole run.
type driveResult struct {
	Pages       uint64       `json:"pages"`
	Concurrency int          `json:"concurrency"`
	Code        string       `json:"code"`
	Publish     publishReply `json:"publish"`
	Fills       []statsReply `json:"after_publish"`
	Cases       []caseResult `json:"cases"`
}

func runDrive(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("drive", flag.ContinueOnError)
	nodesFlag := flags.String("nodes", "", "control addresses of the nodes, the publisher first and the reader second")
	pages := flags.Uint64("pages", 4096, "2 MiB pages of the guest's memory")
	rounds := flags.Int("rounds", 3, "rounds of every case")
	concurrency := flags.Int("concurrency", 16, "pages a restore reads at a time")
	code := flags.String("code", "4+2", "the cluster's code")
	lost := flags.Int("lost", 3, "the node lost part way through the lost case")
	loseAfter := flags.Duration("lose-after", 2*time.Second, "how far into the restore the node is lost")
	seed := flags.Uint64("seed", 1, "orders each round's cases")
	out := flags.String("out", "results.json", "where the results go")
	if err := flags.Parse(args); err != nil {
		return err
	}
	nodes := strings.Split(*nodesFlag, ",")
	if len(nodes) < 3 || *lost < 2 || *lost >= len(nodes) {
		return errors.New("drive needs at least three nodes, and a lost node other than the publisher and the reader")
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
	result := driveResult{Pages: *pages, Concurrency: *concurrency, Code: *code}
	vm := "guest"
	slog.InfoContext(ctx, "drive: publishing", "pages", *pages)
	if err := call(ctx, client, nodes[0], "POST", "/publish", publishRequest{VM: vm, Pages: *pages}, &result.Publish); err != nil {
		return err
	}
	slog.InfoContext(ctx, "drive: published", "seconds", result.Publish.Seconds, "settled", result.Publish.Settled,
		"fill", fmt.Sprintf("%+v", result.Publish.Fill))
	stats, err := statsOf(ctx, client, nodes)
	if err != nil {
		return err
	}
	result.Fills = stats
	cases := []string{"cluster", "store", "cluster-lost"}
	random := rand.New(rand.NewPCG(*seed, 0))
	for round := range *rounds {
		order := slices.Clone(cases)
		random.Shuffle(len(order), func(a, b int) { order[a], order[b] = order[b], order[a] })
		for _, name := range order {
			one, err := restore(ctx, client, nodes, name, round, vm, result.Publish.Sequence, *pages, *concurrency,
				*lost, *loseAfter)
			if err != nil {
				return err
			}
			result.Cases = append(result.Cases, one)
			slog.InfoContext(ctx, "drive: restored", "round", round, "case", name, "seconds", one.Seconds,
				"p50", one.Latency["p50"], "p99", one.Latency["p99"], "p999", one.Latency["p99.9"],
				"max", one.Latency["max"], "served", one.Served, "wrong", one.Wrong, "failed", one.Failed)
			if name == "cluster-lost" {
				// Long enough for the reader's probe to clear its mark of the
				// node that came back.
				time.Sleep(20 * time.Second)
			}
		}
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(*out, encoded, 0o644)
}

// restore runs one case: the kernel's page cache dropped on every node, then
// every page read back on the reader, with the lost node's peer server closed
// part way through in the lost case and started again after.
func restore(ctx context.Context, client *http.Client, nodes []string, name string, round int, vm string,
	sequence, pages uint64, concurrency, lost int, loseAfter time.Duration) (caseResult, error) {
	for _, address := range nodes {
		if err := call(ctx, client, address, "POST", "/drop", nil, nil); err != nil {
			return caseResult{}, err
		}
	}
	before, err := statsOf(ctx, client, nodes)
	if err != nil {
		return caseResult{}, err
	}
	lose := make(chan error, 1)
	if name == "cluster-lost" {
		go func() {
			time.Sleep(loseAfter)
			lose <- call(ctx, client, nodes[lost], "POST", "/lose", nil, nil)
		}()
	} else {
		lose <- nil
	}
	var restored restoreReply
	if err := call(ctx, client, nodes[1], "POST", "/restore", restoreRequest{VM: vm, Sequence: sequence, Pages: pages,
		Store: name == "store", Concurrency: concurrency}, &restored); err != nil {
		return caseResult{}, err
	}
	if err := <-lose; err != nil {
		return caseResult{}, err
	}
	if name == "cluster-lost" {
		if err := call(ctx, client, nodes[lost], "POST", "/back", nil, nil); err != nil {
			return caseResult{}, err
		}
	}
	after, err := statsOf(ctx, client, nodes)
	if err != nil {
		return caseResult{}, err
	}
	one := caseResult{Round: round, Case: name, Seconds: restored.Seconds, Wrong: restored.Wrong, Failed: restored.Failed}
	one.Latency, one.Histogram = shape(restored.Latencies)
	for at := range nodes {
		one.Served = append(one.Served, after[at].StripeBytes-before[at].StripeBytes)
		one.CPU = append(one.CPU, after[at].CPUSeconds-before[at].CPUSeconds)
		one.Busy = append(one.Busy, after[at].StripesBusy-before[at].StripesBusy)
	}
	one.Read = readDelta(before[1].Read, after[1].Read)
	one.StoreGets = after[1].Store.Get.Calls - before[1].Store.Get.Calls
	one.StoreBytes = after[1].Store.Get.Bytes - before[1].Store.Get.Bytes
	return one, nil
}

// shape is the percentiles of latencies in milliseconds, and how many fell in
// each power of two of milliseconds from a quarter of one.
func shape(latencies []int64) (map[string]float64, []int) {
	sorted := slices.Clone(latencies)
	slices.Sort(sorted)
	at := func(q float64) float64 {
		if len(sorted) == 0 {
			return 0
		}
		index := min(int(q*float64(len(sorted))), len(sorted)-1)
		return float64(sorted[index]) / 1e6
	}
	total := int64(0)
	for _, latency := range sorted {
		total += latency
	}
	percentiles := map[string]float64{"p50": at(0.5), "p90": at(0.9), "p99": at(0.99), "p99.9": at(0.999),
		"max": at(1), "mean": float64(total) / float64(max(len(sorted), 1)) / 1e6}
	histogram := make([]int, 16)
	for _, latency := range sorted {
		bucket := 0
		for limit := 0.25; bucket < len(histogram)-1 && float64(latency)/1e6 >= limit; limit *= 2 {
			bucket++
		}
		histogram[bucket]++
	}
	return percentiles, histogram
}

// readDelta is what the reads of the cluster did between two readings.
func readDelta(before, after checkpoint.ReadStats) checkpoint.ReadStats {
	return checkpoint.ReadStats{Hits: after.Hits - before.Hits, OwnHits: after.OwnHits - before.OwnHits,
		Misses: after.Misses - before.Misses, Requests: after.Requests - before.Requests,
		Replaced: after.Replaced - before.Replaced, SecondRequests: after.SecondRequests - before.SecondRequests,
		Refused: after.Refused - before.Refused, StoreHedges: after.StoreHedges - before.StoreHedges,
		StoreHedgesWon: after.StoreHedgesWon - before.StoreHedgesWon,
		StoreHedgesRefused: after.StoreHedgesRefused - before.StoreHedgesRefused,
		WrongStripes: after.WrongStripes - before.WrongStripes, DropsSent: after.DropsSent - before.DropsSent,
		Repairs: after.Repairs - before.Repairs, Timeouts: after.Timeouts - before.Timeouts,
		MarkedDown: after.MarkedDown - before.MarkedDown, Capped: after.Capped - before.Capped,
		Cleared: after.Cleared - before.Cleared, Down: after.Down, Delay: after.Delay, Bound: after.Bound}
}

// statsOf reads every node's stats.
func statsOf(ctx context.Context, client *http.Client, nodes []string) ([]statsReply, error) {
	stats := make([]statsReply, len(nodes))
	for at, address := range nodes {
		if err := call(ctx, client, address, "GET", "/stats", nil, &stats[at]); err != nil {
			return nil, err
		}
	}
	return stats, nil
}

// call makes one request of a node's control API.
func call(ctx context.Context, client *http.Client, address, method, path string, body, into any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://"+address+path, reader)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		text, _ := io.ReadAll(response.Body)
		return fmt.Errorf("%s %s on %s: %s: %s", method, path, address, response.Status, strings.TrimSpace(string(text)))
	}
	if into == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(into)
}
