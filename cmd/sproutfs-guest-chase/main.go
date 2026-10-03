// Command sproutfs-guest-chase loads a Valkey server in a guest with data whose
// reads depend on each other, and later walks it one request at a time, timing
// each request. It is what measures a restored guest whose working set is a
// heap of linked objects: every step of the walk asks for memory the step before
// it named, so a page the guest does not have yet stops the walk for exactly as
// long as the host takes to bring it in.
//
//	chase load --keys N --members M --value B --seed S
//	    write N string keys forming one cycle in a random order, each value
//	    naming the next key, and a sorted set of M members whose scores are a
//	    second random order; then print what the server holds.
//	chase walk --keys N --members M --value B --seed S --steps K --scan C
//	    follow the chain for K steps, each GET naming the next from the bytes
//	    the one before returned, then read the sorted set in rank order, C
//	    members a request; check every byte, and print every request's time.
//
// The data is a pure function of the shape and the seed (see dataset.go), so
// the walk knows what each key should hold without anything carried across a
// suspend. It speaks to the server over a unix socket, because the guest has
// no network.
//
// It is a static binary of the standard library alone, built and installed
// into the valkey guest image the way the guest agent is.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// version is what this binary says it is, stamped at link time with the
// build's `git describe`, as every other command in this repository is.
var version = "dev"

const usage = `sproutfs-guest-chase loads a Valkey server with dependent data and walks it.

  chase load --keys N --members M --value B --seed S [--socket PATH]
  chase walk --keys N --members M --value B --seed S --steps K --scan C
             [--chunk 100] [--budget 5m] [--socket PATH]
  chase version`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err := runOn("unix", os.Args[1], os.Args[2:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "chase %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

// shape is the flags a load and a walk share: what the data is.
type shape struct {
	socket  string
	keys    uint64
	members uint64
	value   int
	seed    uint64
}

func (s *shape) register(flags *flag.FlagSet) {
	flags.StringVar(&s.socket, "socket", "/tmp/valkey.sock", "the server's unix socket")
	flags.Uint64Var(&s.keys, "keys", 1000, "string keys in the chain")
	flags.Uint64Var(&s.members, "members", 0, "members of the sorted set")
	flags.IntVar(&s.value, "value", 100, "bytes of each string value")
	flags.Uint64Var(&s.seed, "seed", 1, "what the orders and the padding are drawn from")
}

// runOn runs one command against a server reached over network, which is
// unix but for the tests, whose fake listens on loopback.
func runOn(network, command string, args []string, out io.Writer) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	var data shape
	switch command {
	case "version":
		_, err := fmt.Fprintln(out, version)
		return err
	case "load":
		data.register(flags)
		if err := flags.Parse(args); err != nil {
			return err
		}
		d, err := newDataset(data.keys, data.members, data.value, data.seed)
		if err != nil {
			return err
		}
		c, err := dial(network, data.socket)
		if err != nil {
			return err
		}
		defer c.Close()
		return load(c, d, out)
	case "walk":
		data.register(flags)
		var settings walkSettings
		flags.Uint64Var(&settings.steps, "steps", 10000, "GETs along the chain")
		flags.Uint64Var(&settings.scan, "scan", 0, "members of the sorted set to read in rank order")
		flags.Uint64Var(&settings.chunk, "chunk", 100, "members each ZRANGE asks for")
		flags.DurationVar(&settings.budget, "budget", 5*time.Minute,
			"how long each phase may run before it stops and reports how far it got")
		if err := flags.Parse(args); err != nil {
			return err
		}
		d, err := newDataset(data.keys, data.members, data.value, data.seed)
		if err != nil {
			return err
		}
		if settings.scan > data.members {
			return fmt.Errorf("a scan of %d members of a set of %d", settings.scan, data.members)
		}
		if settings.chunk == 0 {
			return fmt.Errorf("a scan needs at least one member a request")
		}
		began := time.Now()
		c, err := dial(network, data.socket)
		if err != nil {
			return err
		}
		defer c.Close()
		report, err := walk(c, d, settings)
		report.ConnectMicros = report.ConnectAt.Sub(began).Microseconds()
		if encoded := json.NewEncoder(out).Encode(report); encoded != nil {
			return errors.Join(err, encoded)
		}
		return err
	}
	return fmt.Errorf("no command %q\n\n%s", command, usage)
}

// loadBatch is how many commands a load sends before it reads their replies.
const loadBatch = 1000

// membersPerZadd is how many members one ZADD carries.
const membersPerZadd = 500

func load(c *client, d dataset, out io.Writer) error {
	began := time.Now()
	if _, err := c.do(words("PING")...); err != nil {
		return err
	}
	pending := 0
	settle := func() error {
		if err := c.flush(); err != nil {
			return err
		}
		// A write the server refused answers with an error, which receive
		// returns; SET's OK and ZADD's count say nothing more.
		for ; pending > 0; pending-- {
			if _, err := c.receive(); err != nil {
				return err
			}
		}
		return nil
	}
	for key := range d.keys {
		if err := c.send([]byte("SET"), keyName(key), d.value(key)); err != nil {
			return err
		}
		if pending++; pending == loadBatch {
			if err := settle(); err != nil {
				return err
			}
		}
	}
	if err := settle(); err != nil {
		return err
	}
	stringsTook := time.Since(began)
	for first := uint64(0); first < d.members; first += membersPerZadd {
		args := [][]byte{[]byte("ZADD"), []byte(zset)}
		for member := first; member < min(first+membersPerZadd, d.members); member++ {
			args = append(args, strconv.AppendUint(nil, d.score(member), 10), memberName(member))
		}
		if err := c.send(args...); err != nil {
			return err
		}
		if pending++; pending == loadBatch/10 {
			if err := settle(); err != nil {
				return err
			}
		}
	}
	if err := settle(); err != nil {
		return err
	}
	if d.members > 0 {
		got, err := c.do(words("ZCARD", zset)...)
		if err != nil {
			return err
		}
		if uint64(got.Integer) != d.members {
			return fmt.Errorf("the sorted set holds %d members, not the %d written", got.Integer, d.members)
		}
	}
	size, err := c.do(words("DBSIZE")...)
	if err != nil {
		return err
	}
	if want := d.keys + min(d.members, 1); uint64(size.Integer) != want {
		return fmt.Errorf("the server holds %d keys, not the %d written", size.Integer, want)
	}
	info, err := c.do(words("INFO", "memory")...)
	if err != nil {
		return err
	}
	memory := map[string]string{}
	for line := range strings.Lines(string(info.Bulk)) {
		if field, value, ok := strings.Cut(strings.TrimSpace(line), ":"); ok &&
			(field == "used_memory" || field == "used_memory_rss" || field == "mem_allocator") {
			memory[field] = value
		}
	}
	return json.NewEncoder(out).Encode(map[string]any{
		"keys": d.keys, "members": d.members, "value_bytes": d.valueBytes, "seed": d.seed,
		"strings_seconds": stringsTook.Seconds(), "seconds": time.Since(began).Seconds(), "memory": memory,
	})
}
