// Command sproutfs-startbench measures how long a VM takes to start: from the
// request to the guest running, and to the guest's agent answering, for a cold
// start, a restore, a fork on the same host and a fork to another host.
//
//	sproutfs-startbench drive -hosts http://10.0.0.2:8080,http://10.0.0.3:8080 \
//	    -token-file token -template alpine -count 300 -out samples.jsonl
//	    start VMs through the host API, one at a time, and write one line per
//	    start: the requests' round trips and when the agent first answered.
//	sproutfs-startbench store -bucket b -prefix p -count 300 -out store.json
//	    time the object store's calls on a start's path directly: a create of a
//	    small object if absent, a read of it, a compare-and-set of it and its
//	    delete.
//	sproutfs-startbench summary -samples samples.jsonl -logs pods.log -store store.json
//	    join each start with the hosts' log lines for it ("host: a VM runs",
//	    "host: a VM forked" and "host: a VM's first faults") and print the
//	    percentiles of every step as Markdown.
//
// The hosts time each start's steps themselves and log them, so the split
// outlives the processes. scripts/bench-start-gce.sh runs drive and store on a
// GCE node and summary on the results it copies back.
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
		fmt.Fprintln(os.Stderr, "usage: sproutfs-startbench drive|store|summary [flags]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "drive":
		err = runDrive(ctx, os.Args[2:])
	case "store":
		err = runStore(ctx, os.Args[2:])
	case "summary":
		err = runSummary(os.Args[2:], os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q: want drive, store or summary\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		slog.Error("sproutfs-startbench: exiting", "mode", os.Args[1], "error", err)
		os.Exit(1)
	}
}
