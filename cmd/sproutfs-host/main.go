// Command sproutfs-host runs VMs. One process assembles that role over one
// object store, one authenticated network and one set of budgets: the GCS
// store the deployment's control records and checkpoints live in, the pager
// with its HugeTLB arena and spill file, the VMM scratch, and a Firecracker
// supervisor behind the HTTP API this file serves.
//
// It owns no durable local state. A VM's authority is the epoch in its control
// record and its data is the checkpoint that record selects, so a host that is
// lost costs its VMs only the writes since their last interval checkpoint.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/internal/jsonhttp"
	"github.com/semistrict/sproutfs/platform/adapters"
)

// shutdownTimeout bounds each half of the orderly close: stopping the API, and
// then the supervisor's own close, in which every VM publishes a final
// checkpoint. They run one after the other, so the shutdown behind the signal
// is at most twice this — sixty seconds — and the deployment's termination
// grace period is that plus the drainTimeout the preStop hook spends before it:
// thirty-one minutes and one, which is the 1920 seconds of
// `terminationGracePeriodSeconds` the manifest sets.
const shutdownTimeout = 30 * time.Second

// version is what this binary says it is, stamped at link time with the build's
// `git describe`. A demo cluster is rolled by replacing one image, so the only
// way to tell which build a pod is running is to ask it.
var version = "dev"

// drainTimeout is how long the preStop command waits for the drain it asked
// for. The drain bounds itself at thirty minutes, so this is only the backstop
// for an answer that never comes back over the loopback at all — a minute past
// the server's bound, and no more: a client that waited much longer than the
// drain the server gives up on spends the shutdown's own share of the grace
// waiting for nothing, and the pod is killed with the VMs that did not move
// still holding pages no checkpoint has.
const drainTimeout = 31 * time.Minute

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	work := run
	if len(os.Args) > 1 && os.Args[1] == "drain" {
		work = drain
	}
	if err := work(); err != nil {
		slog.Error("sproutfs-host: exiting", "error", err)
		os.Exit(1)
	}
}

// drain asks the host serving in this container to migrate every VM away, and
// returns when it has. It is the deployment's preStop hook: the drain is a POST
// carrying the deployment's token, and a preStop httpGet hook can be neither,
// so the hook runs this command instead. The environment is the pod's own, so
// the port and the token are the ones the server was started with.
func drain() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	config, err := loadConfig(environment)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	client := hostapi.NewClient(fmt.Sprintf("http://127.0.0.1:%d", config.APIPort),
		&http.Client{Timeout: drainTimeout}, config.APIToken)
	result, err := client.Drain(ctx)
	if err != nil {
		return fmt.Errorf("draining this host: %w", err)
	}
	slog.Info("sproutfs-host: drained", "host", result.Host, "moved", result.Moved,
		"remaining", result.Remaining, "seconds", float64(result.Seconds))
	return nil
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	config, err := loadConfig(environment)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	slog.Info("sproutfs-host: starting", "version", version, "host", config.PodName, "bucket", config.Bucket,
		"prefix", config.Prefix, "arena_bytes", config.ArenaBytes, "checkpoint_interval", config.CheckpointInterval.String())

	// Which adapter stands behind each of the host's ports is this command's
	// decision and nothing else's: the host is given the object store, the
	// network and the node disk it runs over, never the names of any of them.
	objects, client, err := adapters.NewGCS(ctx, config.Endpoint, config.Bucket, config.Prefix)
	if err != nil {
		return fmt.Errorf("gcs object store: %w", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			slog.Error("sproutfs-host: closing the object store client failed", "error", err)
		}
	}()
	disk, err := adapters.NewDisk(config.ScratchDir)
	if err != nil {
		return fmt.Errorf("scratch directory %s: %w", config.ScratchDir, err)
	}
	supervisor := config.SupervisorConfig
	supervisor.ObjectStore, supervisor.Network = objects, adapters.NewNetwork()
	supervisor.Disk, supervisor.Disks = disk, adapters.NewDisk
	supervisor.Starter = &config.Firecracker

	svc, err := host.Start(ctx, supervisor)
	if err != nil {
		return err
	}
	// The supervisor owns the VMM processes and the pager: nothing else may
	// close them, and they close in that order.
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if err := svc.Close(closeCtx); err != nil {
			slog.Error("sproutfs-host: shutdown failed", "error", err)
		}
	}()

	if config.APIToken == "" {
		slog.Warn("sproutfs-host: serving without authentication", "set", jsonhttp.TokenEnv)
	}
	server := &http.Server{
		Addr:    net.JoinHostPort("", fmt.Sprint(config.APIPort)),
		Handler: newServer(svc, config.APIToken),
		// A drain, a migration and a boot all take as long as they take; the
		// read side is what a hung client is bounded by.
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	failed := make(chan error, 1)
	go func() {
		slog.Info("sproutfs-host: serving", "address", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- err
			return
		}
		failed <- nil
	}()
	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}
	slog.Info("sproutfs-host: signalled, stopping the API")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}
