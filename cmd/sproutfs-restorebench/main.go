// Command sproutfs-restorebench measures a guest's memory read back on
// another host from the cluster's disk cache and from the object store
// (plans/disk-cache-2026-10-02.md, "How it is proved", measurement 3).
//
//	sproutfs-restorebench node -index 0 -advertise 10.0.0.2:7500 -bucket b -prefix p
//	    run one host of the cluster: a page cache on a local disk, the peer
//	    server that answers its peers from it, a table of peers, and the
//	    object store, behind a control port.
//	sproutfs-restorebench drive -nodes 10.0.0.2:7600,... -out results.json
//	    publish a guest's memory from the first node, so its windows fill the
//	    cluster, and read it all back on the second, round after round: from
//	    the cluster, from the store, and from the cluster with a third node
//	    lost part way through.
//
// A node runs the real checkpoint store, page cache, peer server and table of
// peers a host runs, over TCP and the real bucket. No VMM runs: a restore is
// every page of the guest's memory read through the store, a few at a time,
// as a pager's faults read them. scripts/bench-restore-gce.sh runs it on six
// GCE hosts.
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
		fmt.Fprintln(os.Stderr, "usage: sproutfs-restorebench node|drive [flags]")
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
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q: want node or drive\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		slog.Error("sproutfs-restorebench: exiting", "mode", os.Args[1], "error", err)
		os.Exit(1)
	}
}
