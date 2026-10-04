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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
)

// The sources a case reads from.
const (
	sourceCluster = "cluster"
	sourceStore   = "store"
	// sourceHot is the hot tier in front of the store, on a node given one.
	sourceHot = "hot"
	// sourceClusterLost is the cluster with one node's peer server closed part
	// way through the read and started again after it.
	sourceClusterLost = "cluster-lost"
)

// caseSpec is one way of reading a guest back: the guest's page size, and how
// the read walks its memory. Its text is page size, pattern, unit and
// concurrency, as in 2MiB/chain/page/1.
type caseSpec struct {
	pageSize    uint64
	pattern     string
	unit        string
	concurrency int
}

func (c caseSpec) String() string {
	return fmt.Sprintf("%s/%s/%s/%d", pageSizeName(c.pageSize), c.pattern, c.unit, c.concurrency)
}

func parseCase(text string) (caseSpec, error) {
	fields := strings.Split(text, "/")
	if len(fields) != 4 {
		return caseSpec{}, fmt.Errorf("a case %q: want page size/pattern/unit/concurrency", text)
	}
	spec := caseSpec{pattern: fields[1], unit: fields[2]}
	switch fields[0] {
	case "2MiB":
		spec.pageSize = checkpoint.PageSize2MiB
	case "4KiB":
		spec.pageSize = checkpoint.PageSize4KiB
	default:
		return caseSpec{}, fmt.Errorf("a case %q: its page size is 2MiB or 4KiB", text)
	}
	var err error
	if spec.concurrency, err = strconv.Atoi(fields[3]); err != nil {
		return caseSpec{}, fmt.Errorf("a case %q: %w", text, err)
	}
	if err := (access{Pattern: spec.pattern, Unit: spec.unit, Concurrency: spec.concurrency, Reads: 1}).check(); err != nil {
		return caseSpec{}, fmt.Errorf("a case %q: %w", text, err)
	}
	return spec, nil
}

func parseCases(text string) ([]caseSpec, error) {
	var specs []caseSpec
	for field := range strings.SplitSeq(text, ",") {
		if field == "" {
			continue
		}
		spec, err := parseCase(field)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// defaultCases is what a run reads: a chain of single pages and one of fault
// runs, independent pages at one, four and sixteen at a time, and the whole
// guest in order, sixteen at a time, by page at 2 MiB and by fault run at
// 4 KiB, where a page at a time would take a request per 4 KiB.
const defaultCases = "2MiB/chain/page/1,2MiB/chain/run/1,2MiB/random/page/1,2MiB/random/page/4," +
	"2MiB/random/page/16,2MiB/sequential/page/16," +
	"4KiB/chain/page/1,4KiB/chain/run/1,4KiB/random/page/1,4KiB/random/page/4,4KiB/random/page/16," +
	"4KiB/sequential/run/16"

// defaultProfiled is the cases whose reader a run profiles: the guest read in
// order, and a chain, at each page size.
const defaultProfiled = "2MiB/sequential/page/16,2MiB/chain/page/1,4KiB/sequential/run/16,4KiB/chain/page/1"

// driveConfig is one run.
type driveConfig struct {
	// pages is the pages of the guest of each page size a case reads.
	pages map[uint64]uint64
	code  string
	// rounds is how many times every case is read from every source, each
	// round in an order of its own.
	rounds  int
	cases   []caseSpec
	sources []string
	// profiled is the cases read once more from each source after the rounds,
	// with the reader's CPU profiled, which no table counts.
	profiled []caseSpec
	// reads is how many pages a chain or a random read of pages reads, and
	// runReads how many fault runs one of runs reads, at most every one.
	reads, runReads int
	// lost is the node lost loseAfter into a read of the lost source, and
	// cleared how long the reader is given after it to clear its mark.
	lost      int
	loseAfter time.Duration
	cleared   time.Duration
	// seed orders each round's cases and seeds each case's walk.
	seed uint64
	// calibrate is how long the reader times each step of a read.
	calibrate time.Duration
}

// caseResult is one read of a guest.
type caseResult struct {
	Round int `json:"round"`
	// Profiled says this read was profiled, after the rounds.
	Profiled    bool    `json:"profiled,omitempty"`
	Case        string  `json:"case"`
	PageSize    string  `json:"page_size"`
	Pattern     string  `json:"pattern"`
	Unit        string  `json:"unit"`
	Concurrency int     `json:"concurrency"`
	Source      string  `json:"source"`
	Reads       int     `json:"reads"`
	Seconds     float64 `json:"seconds"`
	OpenSeconds float64 `json:"open_seconds"`
	// Latency is the percentiles of the reads, in milliseconds, Histogram
	// how many took each power of two of milliseconds from a sixty-fourth of
	// one, and Latencies each read's, in nanoseconds.
	Latency   map[string]float64 `json:"latency_ms"`
	Histogram []int              `json:"histogram"`
	Latencies []int64            `json:"latencies_ns"`
	Wrong     int                `json:"wrong"`
	Failed    int                `json:"failed"`
	// MemoryHits is the reads the reader's memory tier served.
	MemoryHits uint64 `json:"memory_hits"`
	// Served is the stripe bytes each node served during the read, CPU
	// each node's CPU seconds, and Busy the reads each answered BUSY.
	Served []int64   `json:"served_bytes"`
	CPU    []float64 `json:"cpu_seconds"`
	Busy   []int64   `json:"busy"`
	// Read is what the reader's reads of the cluster did, and StoreGets and
	// StoreBytes its requests of the store.
	Read       checkpoint.ReadStats `json:"read"`
	StoreGets  int64                `json:"store_gets"`
	StoreBytes int64                `json:"store_bytes"`
	// Profile names the file holding the reader's CPU profile.
	Profile string `json:"profile,omitempty"`
	// Pager is what the reader's pager did, for a unit read through one.
	Pager *pagerStats `json:"pager,omitempty"`
}

// driveResult is a whole run.
type driveResult struct {
	Code        string                  `json:"code"`
	Guests      []guestRequest          `json:"guests"`
	Publish     map[string]publishReply `json:"publish"`
	Fills       []statsReply            `json:"after_publish"`
	Calibration calibration             `json:"calibration"`
	Cases       []caseResult            `json:"cases"`
}

func runDrive(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("drive", flag.ContinueOnError)
	nodesFlag := flags.String("nodes", "", "control addresses of the nodes, the publisher first and the reader second")
	pages := flags.Uint64("pages", 4096, "2 MiB pages of the guest whose pages are 2 MiB")
	smallPages := flags.Uint64("small-pages", 1<<20, "4 KiB pages of the guest whose pages are 4 KiB")
	rounds := flags.Int("rounds", 3, "rounds of every case")
	cases := flags.String("cases", defaultCases, "cases each round reads from each source")
	sources := flags.String("sources", sourceCluster+","+sourceStore, "what each case reads from: "+
		sourceCluster+", "+sourceStore+", "+sourceHot+" or "+sourceClusterLost)
	profiled := flags.String("profile", defaultProfiled, "cases read once more from each source with the reader profiled")
	reads := flags.Int("reads", 2000, "pages a chain or a random read of pages reads")
	runReads := flags.Int("run-reads", 400, "fault runs a chain or a random read of runs reads, at most every one")
	code := flags.String("code", "4+2", "the cluster's code")
	lost := flags.Int("lost", 3, "the node lost part way through a read of "+sourceClusterLost)
	loseAfter := flags.Duration("lose-after", 2*time.Second, "how far into the read the node is lost")
	seed := flags.Uint64("seed", 1, "orders each round's cases and seeds each case's walk")
	calibrateFor := flags.Duration("calibrate", 500*time.Millisecond, "how long the reader times each step of a read")
	out := flags.String("out", "results.json", "where the results go")
	profiles := flags.String("profiles", "profiles", "the directory the CPU profiles go in")
	if err := flags.Parse(args); err != nil {
		return err
	}
	config := driveConfig{pages: map[uint64]uint64{checkpoint.PageSize2MiB: *pages, checkpoint.PageSize4KiB: *smallPages},
		code: *code, rounds: *rounds, reads: *reads, runReads: *runReads, lost: *lost, loseAfter: *loseAfter,
		cleared: 20 * time.Second, seed: *seed, calibrate: *calibrateFor}
	var err error
	if config.cases, err = parseCases(*cases); err != nil {
		return err
	}
	if config.profiled, err = parseCases(*profiled); err != nil {
		return err
	}
	for source := range strings.SplitSeq(*sources, ",") {
		if source != sourceCluster && source != sourceStore && source != sourceHot && source != sourceClusterLost {
			return fmt.Errorf("a source %q: want %s, %s, %s or %s", source, sourceCluster, sourceStore, sourceHot,
				sourceClusterLost)
		}
		config.sources = append(config.sources, source)
	}
	client := &http.Client{Timeout: time.Hour}
	var nodes []controller
	for address := range strings.SplitSeq(*nodesFlag, ",") {
		nodes = append(nodes, remote{client: client, address: address})
	}
	result, profileFiles, err := drive(ctx, nodes, config)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*profiles, 0o755); err != nil {
		return err
	}
	for name, profile := range profileFiles {
		if err := os.WriteFile(filepath.Join(*profiles, name), profile, 0o644); err != nil {
			return err
		}
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(*out, encoded, 0o644)
}

// drive publishes a guest of each page size the cases read from the first
// node, so its windows fill the cluster, and reads it back on the second,
// round after round. It returns what each read did, and each CPU profile by
// the name its case gives it.
func drive(ctx context.Context, nodes []controller, config driveConfig) (driveResult, map[string][]byte, error) {
	if len(nodes) < 3 || config.lost < 2 || config.lost >= len(nodes) {
		return driveResult{}, nil, errors.New(
			"drive needs at least three nodes, and a lost node other than the publisher and the reader")
	}
	if err := followAll(ctx, nodes, config.code); err != nil {
		return driveResult{}, nil, err
	}
	result := driveResult{Code: config.code, Publish: make(map[string]publishReply)}
	guests := make(map[uint64]guestRequest)
	sequences := make(map[uint64]uint64)
	for _, spec := range slices.Concat(config.cases, config.profiled) {
		if _, found := guests[spec.pageSize]; found {
			continue
		}
		g := guestRequest{VM: "guest-" + strings.ToLower(pageSizeName(spec.pageSize)), PageSize: spec.pageSize,
			Pages: config.pages[spec.pageSize]}
		slog.InfoContext(ctx, "drive: publishing", "vm", g.VM, "pages", g.Pages)
		published, err := nodes[0].publish(ctx, g)
		if err != nil {
			return driveResult{}, nil, err
		}
		slog.InfoContext(ctx, "drive: published", "vm", g.VM, "seconds", published.Seconds,
			"settled", published.Settled, "fill", fmt.Sprintf("%+v", published.Fill))
		// A stripe the fills dropped is a window the cluster does not hold,
		// whose reads would be reads of the store.
		if published.Fill.Dropped != ([len(published.Fill.Dropped)]uint64{}) {
			return driveResult{}, nil, fmt.Errorf("publishing %s dropped stripes, by reason %v: the cluster does not "+
				"hold the whole guest", g.VM, published.Fill.Dropped)
		}
		guests[spec.pageSize], sequences[spec.pageSize] = g, published.Sequence
		result.Guests = append(result.Guests, g)
		result.Publish[g.VM] = published
	}
	var err error
	if result.Fills, err = statsOf(ctx, nodes); err != nil {
		return driveResult{}, nil, err
	}
	if result.Calibration, err = nodes[1].calibrate(ctx, calibrateRequest{Seconds: config.calibrate.Seconds()}); err != nil {
		return driveResult{}, nil, err
	}
	slog.InfoContext(ctx, "drive: calibrated", "cpu", result.Calibration.CPU, "sha", result.Calibration.SHA,
		"steps", fmt.Sprintf("%v", result.Calibration.Steps))
	type planned struct {
		spec   caseSpec
		source string
	}
	var plan []planned
	for _, spec := range config.cases {
		for _, source := range config.sources {
			plan = append(plan, planned{spec: spec, source: source})
		}
	}
	random := rand.New(rand.NewPCG(config.seed, 0))
	profiles := make(map[string][]byte)
	one := func(round int, p planned, profile bool) error {
		g := guests[p.spec.pageSize]
		// A read through a pager may read a fault run, as one of runs does,
		// so it reads as many; both ways of faulting read the same number of
		// hops, so the hops that land in runs already read are alike.
		reads := config.reads
		if p.spec.unit == unitRun || pagerUnit(p.spec.unit) {
			reads = config.runReads
		}
		units := g.Pages / (faultRunBytes / g.PageSize)
		if p.spec.unit != unitRun {
			units = g.Pages
		}
		a := access{Pattern: p.spec.pattern, Unit: p.spec.unit, Concurrency: p.spec.concurrency,
			Reads: min(reads, int(units)), Seed: random.Uint64()}
		if a.Pattern == patternSequential {
			a.Reads = int(units)
		}
		got, err := readCase(ctx, nodes, config, readRequest{Guest: g, Sequence: sequences[p.spec.pageSize],
			Source: nodeSource(p.source), Access: a, Profile: profile}, p.source == sourceClusterLost)
		if err != nil {
			return fmt.Errorf("round %d, %s from %s: %w", round, p.spec, p.source, err)
		}
		got.Round, got.Profiled, got.Case, got.Source = round, profile, p.spec.String(), p.source
		if profile {
			got.Profile = strings.ReplaceAll(p.spec.String(), "/", "-") + "-" + p.source + ".pprof"
			profiles[got.Profile] = got.profile
		}
		result.Cases = append(result.Cases, got.caseResult)
		slog.InfoContext(ctx, "drive: read", "round", round, "case", got.Case, "source", p.source,
			"seconds", got.Seconds, "p50", got.Latency["p50"], "p99", got.Latency["p99"], "max", got.Latency["max"],
			"memory_hits", got.MemoryHits, "store_gets", got.StoreGets, "wrong", got.Wrong, "failed", got.Failed)
		return nil
	}
	for round := range config.rounds {
		order := slices.Clone(plan)
		random.Shuffle(len(order), func(a, b int) { order[a], order[b] = order[b], order[a] })
		for _, p := range order {
			if err := one(round, p, false); err != nil {
				return driveResult{}, nil, err
			}
		}
	}
	for _, spec := range config.profiled {
		for _, source := range config.sources {
			if source == sourceClusterLost {
				continue
			}
			if err := one(config.rounds, planned{spec: spec, source: source}, true); err != nil {
				return driveResult{}, nil, err
			}
		}
	}
	return result, profiles, nil
}

// nodeSource is what the reader reads from for source: the lost case reads
// the cluster.
func nodeSource(source string) string {
	if source == sourceClusterLost {
		return sourceCluster
	}
	return source
}

// followAll has every node follow the list of all their caches under code.
func followAll(ctx context.Context, nodes []controller, code string) error {
	var caches []identityReply
	for _, n := range nodes {
		identity, err := n.identity(ctx, struct{}{})
		if err != nil {
			return err
		}
		caches = append(caches, identity)
	}
	for _, n := range nodes {
		if _, err := n.follow(ctx, listRequest{Code: code, Caches: caches}); err != nil {
			return err
		}
	}
	return nil
}

// readResult is one case's result and the CPU profile it took.
type readResult struct {
	caseResult
	profile []byte
}

// readCase reads one case: every node's memory tiers and the kernel's page
// cache dropped, then the reader's reads, with the lost node's peer server
// closed part way through when lose says and started again after.
func readCase(ctx context.Context, nodes []controller, config driveConfig, request readRequest,
	lose bool) (readResult, error) {
	for _, n := range nodes {
		if _, err := n.drop(ctx, struct{}{}); err != nil {
			return readResult{}, err
		}
	}
	before, err := statsOf(ctx, nodes)
	if err != nil {
		return readResult{}, err
	}
	lost := make(chan error, 1)
	if lose {
		go func() {
			timer := time.NewTimer(config.loseAfter)
			defer timer.Stop()
			select {
			case <-timer.C:
				_, err := nodes[config.lost].lose(ctx, struct{}{})
				lost <- err
			case <-ctx.Done():
				lost <- context.Cause(ctx)
			}
		}()
	} else {
		lost <- nil
	}
	got, err := nodes[1].read(ctx, request)
	if err := errors.Join(err, <-lost); err != nil {
		return readResult{}, err
	}
	if lose {
		if _, err := nodes[config.lost].back(ctx, struct{}{}); err != nil {
			return readResult{}, err
		}
	}
	after, err := statsOf(ctx, nodes)
	if err != nil {
		return readResult{}, err
	}
	one := readResult{caseResult: caseResult{PageSize: pageSizeName(request.Guest.PageSize),
		Pattern: request.Access.Pattern, Unit: request.Access.Unit, Concurrency: request.Access.Concurrency,
		Reads: len(got.Latencies), Seconds: got.Seconds, OpenSeconds: got.OpenSeconds, Latencies: got.Latencies,
		Wrong: got.Wrong, Failed: got.Failed, MemoryHits: got.MemoryHits, Pager: got.Pager}, profile: got.Profile}
	one.Latency, one.Histogram = shape(got.Latencies)
	for at := range nodes {
		one.Served = append(one.Served, after[at].StripeBytes-before[at].StripeBytes)
		one.CPU = append(one.CPU, after[at].CPUSeconds-before[at].CPUSeconds)
		one.Busy = append(one.Busy, after[at].StripesBusy-before[at].StripesBusy)
	}
	one.Read = readDelta(before[1].Read, after[1].Read)
	one.StoreGets = after[1].Store.Get.Calls - before[1].Store.Get.Calls
	one.StoreBytes = after[1].Store.Get.Bytes - before[1].Store.Get.Bytes
	if lose {
		// Long enough for the reader's probe to clear its mark of the node
		// that came back.
		timer := time.NewTimer(config.cleared)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return readResult{}, context.Cause(ctx)
		}
	}
	return one, nil
}

// histogramFloor is the top of a histogram's first band, in milliseconds.
const histogramFloor = 1.0 / 64

// shape is the percentiles of latencies in milliseconds, and how many fell in
// each power of two of milliseconds from a sixty-fourth of one.
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
	histogram := make([]int, 20)
	for _, latency := range sorted {
		bucket := 0
		for limit := histogramFloor; bucket < len(histogram)-1 && float64(latency)/1e6 >= limit; limit *= 2 {
			bucket++
		}
		histogram[bucket]++
	}
	return percentiles, histogram
}

// readDelta is what the reads of the cluster did between two readings.
func readDelta(before, after checkpoint.ReadStats) checkpoint.ReadStats {
	delta := after
	delta.Hits -= before.Hits
	delta.OwnHits -= before.OwnHits
	delta.EarlierHits -= before.EarlierHits
	delta.Misses -= before.Misses
	delta.Requests -= before.Requests
	delta.Replaced -= before.Replaced
	delta.SecondRequests -= before.SecondRequests
	delta.Refused -= before.Refused
	delta.StoreHedges -= before.StoreHedges
	delta.StoreHedgesWon -= before.StoreHedgesWon
	delta.StoreHedgesRefused -= before.StoreHedgesRefused
	delta.WrongStripes -= before.WrongStripes
	delta.DropsSent -= before.DropsSent
	delta.Repairs -= before.Repairs
	delta.Timeouts -= before.Timeouts
	delta.MarkedDown -= before.MarkedDown
	delta.Capped -= before.Capped
	delta.Cleared -= before.Cleared
	delta.HeadChecks -= before.HeadChecks
	delta.HeadMissing -= before.HeadMissing
	return delta
}

// statsOf reads every node's stats.
func statsOf(ctx context.Context, nodes []controller) ([]statsReply, error) {
	stats := make([]statsReply, len(nodes))
	for at, n := range nodes {
		var err error
		if stats[at], err = n.stats(ctx, struct{}{}); err != nil {
			return nil, err
		}
	}
	return stats, nil
}
