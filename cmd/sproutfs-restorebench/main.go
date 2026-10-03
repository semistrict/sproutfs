// Command sproutfs-restorebench measures a guest's memory read back on
// another host from the cluster's disk cache and from the object store
// (plans/disk-cache-2026-10-02.md, "How it is proved", measurement 3).
//
//	sproutfs-restorebench node -advertise 10.0.0.2:7500 -bucket b -prefix p
//	    run one host of the cluster: a page cache on a local disk, the peer
//	    server that answers its peers from it, a table of peers, and the
//	    object store, behind a control port.
//	sproutfs-restorebench drive -nodes 10.0.0.2:7600,... -out results.json
//	    publish a guest of 2 MiB pages and one of 4 KiB pages from the first
//	    node, so their windows fill the cluster, and read them back on the
//	    second, round after round, from the cluster and from the store: in
//	    order, at random, and as a chain whose every read is named by the
//	    bytes of the one before, a page or a fault run at a time.
//	sproutfs-restorebench walk -nodes 10.0.0.2:7600,... -out walk.json
//	    publish a guest of 2 MiB pages and one of 4 KiB pages, each once
//	    filling the cluster and once writing the hot tier, and on the second
//	    node read a chain of single pages: from the regional bucket, from the
//	    hot tier and from the cluster, and from a cold hot tier, which those
//	    reads fill.
//	sproutfs-restorebench calibrate -for 1s
//	    time each step of reading a page on this host's processor, and print
//	    it as JSON.
//
// A node started with -hot-bucket also reads through a hot tier in that
// bucket, under the run's prefix.
//
// A node runs the real checkpoint store, page cache, peer server and table of
// peers a host runs, over TCP and the real bucket. No VMM runs: a restore is
// the guest's memory read through the store as a pager's faults read it. The
// reader times each step of a read on its processor, and profiles its CPU on
// the cases asked for. scripts/bench-restore-gce.sh runs drive on six GCE
// hosts, and scripts/bench-hot-tier-gce.sh runs walk.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sproutfs-restorebench node|drive|walk|calibrate [flags]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "node":
		err = runNode(ctx, os.Args[2:])
	case "drive":
		err = runDrive(ctx, os.Args[2:])
	case "walk":
		err = runWalk(ctx, os.Args[2:])
	case "calibrate":
		err = runCalibrate(ctx, os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q: want node, drive, walk or calibrate\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		slog.Error("sproutfs-restorebench: exiting", "mode", os.Args[1], "error", err)
		os.Exit(1)
	}
}
