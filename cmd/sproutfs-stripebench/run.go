package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func runClient(args []string) error {
	fs := flag.NewFlagSet("client", flag.ContinueOnError)
	servers := fs.String("servers", "", "every server's address, comma-separated; a server's place is its index")
	objects := fs.Int("objects", 4096, "how many objects there are")
	objectBytes := fs.Int("object-bytes", 350_000, "each object's size")
	seed := fs.Uint64("seed", 1, "the seed the objects are derived from")
	codes := fs.String("codes", "1+0,4+1,4+2", "the codes to read under")
	conds := fs.String("conditions", "healthy,slow,drained,drained-slow,drained-stall",
		"the conditions to read under: healthy, slow, stall, drained, drained-slow, drained-stall")
	load := fs.String("load", "idle", "a label for the load this run puts on the servers")
	disk := fs.Bool("disk", false, "have servers drop each stripe from the page cache once read, so every read reads the disk")
	rate := fs.Float64("rate", 500, "reads per second, arriving at random; 0 reads in a closed loop")
	concurrency := fs.Int("concurrency", 2048, "the most reads in flight")
	duration := fs.Duration("duration", 20*time.Second, "how long each case reads")
	gap := fs.Duration("gap", 2*time.Second, "the pause between cases, for the last reads to finish")
	warmup := fs.Duration("warmup", 5*time.Second, "reads before the first case, not recorded")
	startAt := fs.Int64("start-at", 0, "when the first read is due, in Unix milliseconds, so clients run together; 0 is now")
	timeout := fs.Duration("timeout", time.Second, "how long a read waits for k stripes")
	slowDelay := fs.Duration("slow-delay", 20*time.Millisecond, "how late the slow server answers")
	drained := fs.Int("drained", -1, "the server a drained condition removes; -1 is the last")
	slow := fs.Int("slow", -1, "the server a slow or stall condition slows; -1 is the one before the last")
	dialWait := fs.Duration("dial-wait", 3*time.Minute, "how long to wait for the servers to be up")
	name := fs.String("name", "", "this client's name in the record; empty is the host name")
	out := fs.String("out", "", "where to write the record: PREFIX.json and PREFIX.txt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	addrs := splitList(*servers)
	parsed, err := parseCodes(*codes)
	if err != nil {
		return err
	}
	set := objectSet{servers: len(addrs), objects: *objects, objectBytes: *objectBytes, seed: *seed, codes: parsed}
	if err := set.validate(); err != nil {
		return err
	}
	conditions, err := parseConditions(*conds)
	if err != nil {
		return err
	}
	if *drained < 0 {
		*drained = len(addrs) - 1
	}
	if *slow < 0 {
		*slow = len(addrs) - 2
	}
	switch {
	case *out == "":
		return errors.New("set -out")
	case *drained >= len(addrs) || *slow < 0 || *slow >= len(addrs) || *drained == *slow:
		return fmt.Errorf("the drained server %d and the slow server %d must be two of the %d servers", *drained, *slow, len(addrs))
	case *rate < 0 || *concurrency < 1:
		return errors.New("want a rate of 0 or more and a concurrency of 1 or more")
	}
	if *name == "" {
		if *name, err = os.Hostname(); err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	started := time.Now()
	want, err := newExpected(set)
	if err != nil {
		return err
	}
	slog.Info("hashed the objects", "objects", set.objects, "took", time.Since(started).Round(time.Millisecond))
	c, err := connect(ctx, set, addrs, *dialWait)
	if err != nil {
		return err
	}
	defer c.close()
	c.timeout = *timeout

	s := schedule{
		load: *load, disk: *disk, rate: *rate, concurrency: *concurrency, duration: *duration,
		drained: *drained, slow: *slow, slowDelay: *slowDelay, seed: *seed ^ uint64(time.Now().UnixNano()),
	}
	base := time.Now().Add(time.Second)
	if *startAt != 0 {
		base = time.UnixMilli(*startAt)
		if time.Until(base) < 0 {
			return fmt.Errorf("the start, %v, has passed", base)
		}
	}
	if *warmup > 0 {
		w := s
		w.duration = *warmup
		if _, err := c.runCase(ctx, w, conditions[0], len(c.layouts)-1, base, want); err != nil {
			return fmt.Errorf("warm up: %w", err)
		}
	}
	rec := record{
		Clients: []string{*name}, Servers: addrs, Objects: set.objects, ObjectBytes: set.objectBytes,
		Seed: set.seed, Rate: *rate, Concurrency: *concurrency, Timeout: *timeout, SlowDelay: *slowDelay,
		Drained: *drained, Slow: *slow,
	}
	i := 0
	for _, cond := range conditions {
		for ci := range c.layouts {
			start := base.Add(*warmup + time.Duration(i)*(*duration+*gap))
			i++
			if late := time.Since(start); late > 0 {
				slog.Warn("a case starts late, out of step with any other client", "late", late)
				start = time.Now()
			}
			res, err := c.runCase(ctx, s, cond, ci, start, want)
			if err != nil {
				return fmt.Errorf("case %s: %w", res.Name, err)
			}
			rec.Cases = append(rec.Cases, res)
		}
	}
	// The servers keep the last case's modes. Every case, and the warm-up,
	// sets them for itself, so the next run need not care; and a client that
	// finished first must not change them under another still reading.
	return rec.write(*out)
}

// connect dials every server, retrying until wait has passed, and checks each
// holds the set at the index the client gives it.
func connect(ctx context.Context, set objectSet, addrs []string, wait time.Duration) (*client, error) {
	layouts, err := set.layouts()
	if err != nil {
		return nil, err
	}
	c := &client{set: set, layouts: layouts, bufs: &buffers{}}
	deadline := time.Now().Add(wait)
	var d net.Dialer
	for i, addr := range addrs {
		var conn net.Conn
		for {
			conn, err = d.DialContext(ctx, "tcp", addr)
			if err == nil || time.Now().After(deadline) || ctx.Err() != nil {
				break
			}
			slog.Info("waiting for a server", "server", addr, "error", err)
			time.Sleep(time.Second)
		}
		if err != nil {
			c.close()
			return nil, fmt.Errorf("dial server %d at %s: %w", i, addr, err)
		}
		c.peers = append(c.peers, newPeer(i, conn, c.bufs))
	}
	if err := c.hello(); err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

func (c *client) hello() error {
	for i, p := range c.peers {
		if _, err := p.call(request{op: opHello, object: uint32(i), arg: c.set.fingerprint()}, controlTimeout); err != nil {
			return err
		}
	}
	return nil
}

func (c *client) close() {
	for _, p := range c.peers {
		if err := p.close(); err != nil && !closedConn(err) {
			slog.Warn("close a server connection", "server", p.index, "error", err)
		}
	}
}
