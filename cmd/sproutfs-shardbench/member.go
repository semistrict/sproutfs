package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/adapters"
	"github.com/semistrict/sproutfs/resource"
	"github.com/semistrict/sproutfs/volume"
)

// fillPage is the page of the VM a fill publishes: the default for a disk.
const fillPage = 2 << 20

// member is one host that serves shards, and its control port.
type member struct {
	host  *host.Host
	fills int
}

func runMember(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("member", flag.ContinueOnError)
	advertise := flags.String("advertise", "", "address the peer server listens and is reached at")
	controlAddress := flags.String("control", ":7600", "address the control API listens on")
	machine := flags.String("machine", "", "the instance this host runs on, which its shards are attached to")
	devices := flags.String("devices", "", "the directory the instance's disks are named in, /dev/disk/by-id when empty")
	bucket := flags.String("bucket", "", "Cloud Storage bucket")
	prefix := flags.String("prefix", "", "prefix of this run's objects in the bucket")
	memoryBytes := flags.Int64("memory-bytes", 2<<30, "bytes of memory the host's page cache may hold")
	interval := flags.Duration("interval", time.Second, "how often the host reads the membership and looks at its shards")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *advertise == "" || *machine == "" || *bucket == "" || *prefix == "" {
		return errors.New("member needs -advertise, -machine, -bucket and -prefix")
	}
	store, closer, err := adapters.NewObjectStore(ctx, adapters.ObjectStoreConfig{Bucket: *bucket, Prefix: *prefix})
	if err != nil {
		return err
	}
	defer closer.Close()
	resources, err := resource.New(*memoryBytes)
	if err != nil {
		return err
	}
	started, err := host.StartHost(ctx, host.Config{Network: adapters.NewNetwork(), Resources: resources,
		ObjectStore: store, CacheBytes: *memoryBytes,
		// A fill publishes faster than a disk takes it, so its queue holds
		// what one publication between two settles writes.
		Cache: checkpoint.CacheConfig{ClusterPercent: 100, FillQueueBytes: 1 << 30,
			Deployment: checkpoint.CacheDeployment{Store: "gcs", Bucket: *bucket, Prefix: *prefix}},
		Shards:             host.ShardsConfig{Devices: adapters.NewGCEDevices(*devices), Machine: *machine, Interval: *interval},
		Migration:          host.MigrationConfig{Address: platform.Address(*advertise)},
		MembershipInterval: *interval, CheckpointInterval: -1, EpochInterval: -1})
	if err != nil {
		return err
	}
	defer func() { _ = started.Close(context.WithoutCancel(ctx)) }()
	m := &member{host: started}
	server := &http.Server{Addr: *controlAddress, Handler: m.handler(), ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	self, _ := started.Member()
	slog.InfoContext(ctx, "member: serving", "member", self.ID.String(), "machine", *machine, "peer", *advertise,
		"control", *controlAddress)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (m *member) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /member", func(w http.ResponseWriter, _ *http.Request) {
		self, _ := m.host.Member()
		reply(w, hostapi.MemberOf(self, m.host.Membership()))
	})
	mux.HandleFunc("GET /shards", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, m.host.Status().Cache.Shards)
	})
	mux.HandleFunc("POST /fill", func(w http.ResponseWriter, r *http.Request) {
		bytes, err := strconv.ParseInt(r.URL.Query().Get("bytes"), 10, 64)
		if err != nil || bytes < fillPage {
			http.Error(w, "fill needs bytes, at least one page", http.StatusBadRequest)
			return
		}
		took, err := m.fill(r.Context(), bytes)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		reply(w, map[string]any{"bytes": bytes, "seconds": took.Seconds(), "shards": m.host.Status().Cache.Shards,
			"fill": m.host.Status().Cache.Fill})
	})
	// The machine dies: the process ends now, with nothing closed.
	mux.HandleFunc("POST /exit", func(http.ResponseWriter, *http.Request) {
		slog.Warn("member: ending at once, as a machine that dies does")
		os.Exit(3)
	})
	return mux
}

// fill publishes a VM of bytes of noise, which fills the cluster's cache,
// and returns once every fill is written.
func (m *member) fill(ctx context.Context, bytes int64) (time.Duration, error) {
	began := time.Now()
	m.fills++
	id := fmt.Sprintf("shardbench-fill-%d-%d", os.Getpid(), m.fills)
	size := uint64(bytes) / fillPage * fillPage
	vm, err := m.host.Volumes().Create(ctx, id, []volume.VolumeSpec{{Name: "disk", Size: size, PageSize: fillPage}})
	if err != nil {
		return 0, err
	}
	defer func() { _ = vm.Close(context.WithoutCancel(ctx)) }()
	noise := rand.New(rand.NewChaCha8([32]byte{byte(m.fills)}))
	page := make([]byte, fillPage)
	disk := vm.Volume("disk")
	for offset := uint64(0); offset < size; offset += fillPage {
		for at := 0; at < len(page); at += 8 {
			value := noise.Uint64()
			for b := range 8 {
				page[at+b] = byte(value >> (8 * b))
			}
		}
		if err := disk.Write(ctx, offset, page); err != nil {
			return 0, err
		}
		// Publish every 256 MiB, so the overlay holds no more than that.
		if (offset/fillPage+1)%128 == 0 {
			if err := vm.Checkpoint(ctx); err != nil {
				return 0, err
			}
			if err := m.host.SettleFills(ctx); err != nil {
				return 0, err
			}
		}
	}
	if err := vm.Checkpoint(ctx); err != nil {
		return 0, err
	}
	if err := m.host.SettleFills(ctx); err != nil {
		return 0, err
	}
	return time.Since(began), nil
}

func reply(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Warn("member: writing a reply failed", "error", err)
	}
}
