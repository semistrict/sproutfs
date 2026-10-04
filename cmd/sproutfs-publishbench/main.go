// Command sproutfs-publishbench measures one publication of a large guest's
// memory to the object store: what a `stop --suspend` of a guest with a
// multi-GiB heap spends uploading it (docs/measurements/gce-publication-throughput-2026-10-04.md).
//
//	sproutfs-publishbench -bucket b -prefix p -gib 4 -rounds 3 -out results.json
//
// It holds the guest's memory in its own memory, as a pager's arena does, and
// publishes every page of it through the real checkpoint store and the real
// Cloud Storage adapter, sized as a host on this machine sizes them (or as the
// flags say). Each round times the commit, the CPU the process used, every
// PUT and the uploads in flight, and the time the publication spent reading
// pages, and can write a CPU profile. Every object a round writes is deleted
// before the next round.
//
// scripts/bench-publish-gce.sh builds it against this release and the release
// before and runs both on one GCE host.
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
	"runtime"
	"runtime/pprof"
	"syscall"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/adapters"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.Error("sproutfs-publishbench: exiting", "error", err)
		os.Exit(1)
	}
}

// benchConfig is one run: where it publishes, what, how often, and how the
// store is sized.
type benchConfig struct {
	bucket, prefix, build, out, profile string
	gib, rounds, firstRound             int
	encoders, uploads, builders         int
}

func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("sproutfs-publishbench", flag.ContinueOnError)
	var config benchConfig
	flags.StringVar(&config.bucket, "bucket", "", "Cloud Storage bucket")
	flags.StringVar(&config.prefix, "prefix", "", "prefix of this run's objects in the bucket")
	flags.StringVar(&config.build, "build", "", "name of the build being measured, recorded with each round")
	flags.StringVar(&config.out, "out", "", "file each round's record is appended to, as one JSON line")
	flags.StringVar(&config.profile, "cpuprofile", "", "directory a CPU profile of each round is written to, none when empty")
	flags.IntVar(&config.gib, "gib", 4, "GiB of guest memory published")
	flags.IntVar(&config.rounds, "rounds", 1, "publications, one after another")
	flags.IntVar(&config.firstRound, "first-round", 0, "number the first round is recorded under")
	flags.IntVar(&config.encoders, "encoders", 0, "encoders in the store's codec pool, the host's sizing when zero")
	flags.IntVar(&config.uploads, "uploads", 0, "upload slots, the host's sizing when zero")
	flags.IntVar(&config.builders, "builders", 0, "part builders, the host's sizing when zero")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if config.bucket == "" || config.prefix == "" || config.build == "" || config.out == "" {
		return errors.New("needs -bucket, -prefix, -build and -out")
	}
	if config.gib < 1 || config.rounds < 1 || config.firstRound < 0 {
		return errors.New("-gib and -rounds are at least 1, and -first-round at least 0")
	}
	sizing := hostSizing(runtime.NumCPU())
	config.encoders = cmpOr(config.encoders, sizing.encoders)
	config.uploads = cmpOr(config.uploads, sizing.uploads)
	config.builders = cmpOr(config.builders, sizing.builders)

	objects, closer, err := adapters.NewObjectStore(ctx, adapters.ObjectStoreConfig{Bucket: config.bucket, Prefix: config.prefix})
	if err != nil {
		return err
	}
	defer closer.Close()
	g := newGuest(uint64(config.gib) << 30 / checkpoint.PageSize2MiB)
	slog.InfoContext(ctx, "publishbench: guest ready", "pages", g.pages(), "encoders", config.encoders,
		"uploads", config.uploads, "builders", config.builders)
	for round := config.firstRound; round < config.firstRound+config.rounds; round++ {
		record, err := publish(ctx, config, objects, g, round)
		if cleanup := deleteAll(context.WithoutCancel(ctx), objects); cleanup != nil {
			err = errors.Join(err, cleanup)
		}
		if err != nil {
			return fmt.Errorf("round %d: %w", round, err)
		}
		if err := appendRecord(config.out, record); err != nil {
			return err
		}
		slog.InfoContext(ctx, "publishbench: round", "build", config.build, "round", round,
			"seconds", record.Seconds, "uploaded_mb_per_s", record.UploadedMBPerSecond, "cpus", record.CPUsUsed)
	}
	return nil
}

// sizing is how a store's publication is sized.
type sizing struct{ encoders, uploads, builders int }

// hostSizing is what host/host.go gives a store on a machine of cpus
// processors: encodeWorkers, uploadSlots and partBuilders.
func hostSizing(cpus int) sizing {
	clamp := func(per, low, high int) int { return min(max(cpus/per, low), high) }
	return sizing{encoders: clamp(2, 2, 16), uploads: clamp(2, 8, 64), builders: clamp(4, 2, 8)}
}

func cmpOr(value, otherwise int) int {
	if value != 0 {
		return value
	}
	return otherwise
}

// record is one round's measurement.
type record struct {
	Build    string `json:"build"`
	Round    int    `json:"round"`
	CPUs     int    `json:"machine_cpus"`
	Encoders int    `json:"encoders"`
	Uploads  int    `json:"uploads"`
	Builders int    `json:"builders"`

	Pages         uint64 `json:"pages"`
	RawBytes      uint64 `json:"raw_bytes"`
	UploadedBytes int64  `json:"uploaded_bytes"`
	// Seconds is the commit, from the first page read to the index object's
	// PUT.
	Seconds             float64 `json:"seconds"`
	UploadedMBPerSecond float64 `json:"uploaded_mb_per_s"`
	RawMBPerSecond      float64 `json:"raw_mb_per_s"`
	// CPUSeconds is the process's user and system time over the commit, and
	// CPUsUsed that over the commit's seconds.
	CPUSeconds float64 `json:"cpu_seconds"`
	CPUsUsed   float64 `json:"cpus"`
	// ReadSeconds is the time the publication spent in the source's ReadPage.
	ReadSeconds float64 `json:"read_seconds"`
	puts
}

func publish(ctx context.Context, config benchConfig, objects platform.ObjectStore, g *guest, round int) (record, error) {
	meter := &putMeter{ObjectStore: objects}
	codecs, err := blob.NewCodecs(config.encoders, 1)
	if err != nil {
		return record{}, err
	}
	store, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: meter, Codecs: codecs,
		Concurrency: config.uploads, MaxBuilders: config.builders})
	if err != nil {
		return record{}, err
	}
	vm := fmt.Sprintf("publishbench-%d", round)
	root, err := store.Root(ctx, control.Ref{VM: vm, Sequence: 1},
		map[string]checkpoint.VolumeSpec{volume: {Size: g.pages() * checkpoint.PageSize2MiB,
			PageSize: checkpoint.PageSize2MiB}})
	if err != nil {
		return record{}, err
	}
	publication := store.Begin(root, control.Ref{VM: vm, Sequence: 2})
	for page := range g.pages() {
		publication.Dirty(volume, page)
	}
	source := &timedSource{guest: g}
	stopProfile, err := startProfile(config.profile, config.build, round)
	if err != nil {
		return record{}, err
	}
	meter.reset()
	cpuBefore := cpuSeconds()
	started := time.Now()
	_, err = publication.Commit(ctx, source)
	seconds := time.Since(started).Seconds()
	cpu := cpuSeconds() - cpuBefore
	if stopErr := stopProfile(); stopErr != nil {
		err = errors.Join(err, stopErr)
	}
	if err != nil {
		return record{}, err
	}
	measured := meter.measured(seconds)
	raw := g.pages() * checkpoint.PageSize2MiB
	return record{Build: config.build, Round: round, CPUs: runtime.NumCPU(),
		Encoders: config.encoders, Uploads: config.uploads, Builders: config.builders,
		Pages: g.pages(), RawBytes: raw, UploadedBytes: measured.Bytes, Seconds: seconds,
		UploadedMBPerSecond: float64(measured.Bytes) / seconds / 1e6,
		RawMBPerSecond:      float64(raw) / seconds / 1e6,
		CPUSeconds:          cpu, CPUsUsed: cpu / seconds,
		ReadSeconds: source.seconds(), puts: measured}, nil
}

// startProfile starts a CPU profile of one round, into dir, and returns what
// stops it. No directory profiles nothing.
func startProfile(dir, build string, round int) (func() error, error) {
	if dir == "" {
		return func() error { return nil }, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	file, err := os.Create(filepath.Join(dir, fmt.Sprintf("cpu-%s-%d.pprof", build, round)))
	if err != nil {
		return nil, err
	}
	if err := pprof.StartCPUProfile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() error {
		pprof.StopCPUProfile()
		return file.Close()
	}, nil
}

// cpuSeconds is the user and system time this process has used.
func cpuSeconds() float64 {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	seconds := func(t syscall.Timeval) float64 { return float64(t.Sec) + float64(t.Usec)/1e6 }
	return seconds(usage.Utime) + seconds(usage.Stime)
}

// deleteAll deletes every object under the run's prefix.
func deleteAll(ctx context.Context, objects platform.ObjectStore) error {
	return platform.ListAll(ctx, objects, platform.ObjectPrefix{}, func(object platform.ObjectMetadata) error {
		return objects.Delete(ctx, platform.DeleteRequest{Key: object.Key})
	})
}

func appendRecord(path string, r record) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
