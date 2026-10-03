// Command sproutfs-stripebench measures whether reading k of k+m stripes from
// several servers beats reading one whole object from one server. It is the
// first measurement of the disk cache plan (plans/disk-cache-2026-10-02.md):
// one 350 KB read from one host against 4+1 and 4+2 reads of 90 KB stripes, at
// the median and the tail, with the hosts idle, serving at full rate, with one
// host drained, and with one host drained and another slow.
//
//	sproutfs-stripebench server -index 0 -servers 6 -file /mnt/stripes/store
//	    build this server's share of the objects in the file, then serve it
//	    over TCP. A server holds, for each code, the stripes rendezvous hashing
//	    ranks it for, and for each object the stripes it holds lie together.
//	sproutfs-stripebench client -servers a:7400,b:7400,... -out results/idle
//	    read objects at a fixed rate, every code under every condition, and
//	    write a JSON record and a text table of each case's latency.
//	sproutfs-stripebench report -out results/full results/full-*.json
//	    merge the records of clients that ran the same cases at once.
//
// A whole object is the code 1+0: one stripe, on the server ranked first.
// Servers and clients derive the same objects from the same seed, so nothing
// but the server list is shared.
package main

import (
	"fmt"
	"log/slog"
	"os"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sproutfs-stripebench server|client|report [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "server":
		err = runServer(os.Args[2:])
	case "client":
		err = runClient(os.Args[2:])
	case "report":
		err = runReport(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q: want server, client or report\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		slog.Error("sproutfs-stripebench: exiting", "mode", os.Args[1], "error", err)
		os.Exit(1)
	}
}
