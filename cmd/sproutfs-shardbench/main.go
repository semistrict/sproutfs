// Command sproutfs-shardbench times a shard of the cluster's cache moving
// between hosts on Compute Engine (docs/hosting.md, "Shards on network
// disks"), with the real host, membership and attach API.
//
//	sproutfs-shardbench member -advertise 10.0.0.2:7500 -machine vm-a -bucket b -prefix p
//	    run one host that serves shards: the real host with no VMM, its
//	    cache on the shards the membership assigns it, which it opens at
//	    /dev/disk/by-id/google-<name>, behind a control port that reports
//	    it as a member, fills its shards by publishing a VM, and ends the
//	    process at once, as a machine that dies does.
//	sproutfs-shardbench control -members http://10.0.0.2:7600,... -volumes projects/p/zones/z/disks/d \
//	    -bucket b -prefix p -out results.json
//	    be the controller: take a pass of the membership and the attach API
//	    every interval, fill the shard through the member that serves it,
//	    then, round after round, take that member away, by draining it or by
//	    ending its process, and time each step until the shard serves again
//	    on another member.
//
// scripts/bench-shards-gce.sh runs both on disposable hosts.
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
		fmt.Fprintln(os.Stderr, "usage: sproutfs-shardbench member|control [flags]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "member":
		err = runMember(ctx, os.Args[2:])
	case "control":
		err = runControl(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		slog.Error("sproutfs-shardbench: failed", "error", err)
		os.Exit(1)
	}
}
