package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

// caseResult is what one client saw in one case: one load, one medium, one
// condition, one code and one read mode.
type caseResult struct {
	Name      string `json:"name"`
	Load      string `json:"load"`
	Medium    string `json:"medium"`
	Condition string `json:"condition"`
	Code      string `json:"code"`
	Read      string `json:"read"`
	// Repeat is which run of the case this is, from 0.
	Repeat  int       `json:"repeat"`
	Started time.Time `json:"started"`
	// Elapsed runs from the first read due to the last read done.
	Elapsed time.Duration `json:"elapsed_ns"`

	Issued   uint64 `json:"issued"`
	Hits     uint64 `json:"hits"`
	Misses   uint64 `json:"misses"`
	TimedOut uint64 `json:"timed_out"`
	// Decoded counts the hits that had to rebuild a data stripe from parity.
	Decoded uint64 `json:"decoded"`
	// Requests counts the stripe requests the reads sent.
	Requests uint64 `json:"requests"`
	// Second counts the reads that asked the rest of the holders after the
	// delay, and Refused those that would have but found the budget empty.
	Second  uint64 `json:"second_requests"`
	Refused uint64 `json:"refused"`
	// SecondShare is Second over Issued, and SentPerRead the bytes the
	// servers sent per read.
	SecondShare float64 `json:"second_share"`
	SentPerRead float64 `json:"sent_bytes_per_read"`

	// Latency is the latency of the hits only.
	Latency histogram     `json:"latency"`
	P50     time.Duration `json:"p50_ns"`
	P90     time.Duration `json:"p90_ns"`
	P99     time.Duration `json:"p99_ns"`
	P999    time.Duration `json:"p999_ns"`
	Max     time.Duration `json:"max_ns"`
	Shape   []shapeBucket `json:"shape"`

	// Servers is what every server did over the case, summed. When clients
	// ran together, each saw the same servers over the same window, so a
	// merged record keeps the largest any client saw.
	Servers   serverStats   `json:"servers"`
	ClientCPU time.Duration `json:"client_cpu_ns"`

	// Where the time went. Client is the client process's Go runtime, and
	// Host the client's host, server and all.
	Client clientStats `json:"client"`
	Host   hostStats   `json:"host"`
	// Asked is the stripe requests sent to each server.
	Asked []uint64 `json:"asked"`
	// ServerQueued, ServerRead and ServerWaited are the servers' time on every
	// reply to a stripe request, as the replies say, those that came after
	// their read had what it needed included.
	ServerQueued histogram `json:"server_queued"`
	ServerRead   histogram `json:"server_read"`
	ServerWaited histogram `json:"server_waited"`
	// Network and Decode are each hit's: the rest of the round trip of the
	// stripe that completed it, and rebuilding the object.
	Network histogram `json:"network"`
	Decode  histogram `json:"decode"`
	// Tail is where the time of the slowest hits went.
	Tail tailStats `json:"tail"`
}

func (r *caseResult) summarize() {
	r.P50 = r.Latency.quantile(0.5)
	r.P90 = r.Latency.quantile(0.9)
	r.P99 = r.Latency.quantile(0.99)
	r.P999 = r.Latency.quantile(0.999)
	r.Max = r.Latency.max
	r.Shape = r.Latency.shape()
	r.SecondShare, r.SentPerRead = 0, 0
	if r.Issued > 0 {
		r.SecondShare = float64(r.Second) / float64(r.Issued)
		r.SentPerRead = float64(r.Servers.Sent) / float64(r.Issued)
	}
}

// add merges another client's record of the same case.
func (r *caseResult) add(o caseResult) {
	if o.Started.Before(r.Started) {
		r.Started = o.Started
	}
	r.Elapsed = max(r.Elapsed, o.Elapsed)
	r.Issued += o.Issued
	r.Hits += o.Hits
	r.Misses += o.Misses
	r.TimedOut += o.TimedOut
	r.Decoded += o.Decoded
	r.Requests += o.Requests
	r.Second += o.Second
	r.Refused += o.Refused
	r.Latency.add(o.Latency)
	r.Servers = serverStats{
		CPU:     max(r.Servers.CPU, o.Servers.CPU),
		Sent:    max(r.Servers.Sent, o.Servers.Sent),
		Replies: max(r.Servers.Replies, o.Servers.Replies),
		Reads:   max(r.Servers.Reads, o.Servers.Reads),
	}
	r.ClientCPU += o.ClientCPU
	r.Client.add(o.Client)
	r.Host.add(o.Host)
	r.Asked = addCounts(r.Asked, o.Asked)
	r.ServerQueued.add(o.ServerQueued)
	r.ServerRead.add(o.ServerRead)
	r.ServerWaited.add(o.ServerWaited)
	r.Network.add(o.Network)
	r.Decode.add(o.Decode)
	r.Tail.add(o.Tail)
	r.summarize()
}

// record is one client's run, or the merge of clients that ran together.
type record struct {
	Clients     []string      `json:"clients"`
	Servers     []string      `json:"servers"`
	Objects     int           `json:"objects"`
	ObjectBytes int           `json:"object_bytes"`
	Seed        uint64        `json:"seed"`
	Rate        float64       `json:"rate_per_client"`
	Concurrency int           `json:"concurrency"`
	Timeout     time.Duration `json:"timeout_ns"`
	SlowDelay   time.Duration `json:"slow_delay_ns"`
	HedgeMin    time.Duration `json:"hedge_min_ns"`
	Drained     int           `json:"drained"`
	Slow        int           `json:"slow"`
	// Repeats is how many times each case ran, each round in an order drawn
	// from OrderSeed.
	Repeats   int    `json:"repeats"`
	OrderSeed uint64 `json:"order_seed"`
	// TailFrom is the latency from which a hit counts in its case's tail.
	TailFrom time.Duration `json:"tail_from_ns"`
	// Cases is every case of every round, in the order they ran.
	Cases []caseResult `json:"cases"`
}

func (r *record) merge(o record) error {
	if !slices.Equal(r.Servers, o.Servers) || r.Objects != o.Objects || r.ObjectBytes != o.ObjectBytes ||
		r.Seed != o.Seed {
		return errors.New("the records read different object sets")
	}
	if r.HedgeMin != o.HedgeMin {
		return errors.New("the records hedged with different floors")
	}
	if r.Repeats != o.Repeats || r.OrderSeed != o.OrderSeed || r.TailFrom != o.TailFrom {
		return errors.New("the records ran different rounds or broke down different tails")
	}
	r.Clients = append(r.Clients, o.Clients...)
	for _, c := range o.Cases {
		i := slices.IndexFunc(r.Cases, func(m caseResult) bool { return m.Name == c.Name && m.Repeat == c.Repeat })
		if i < 0 {
			r.Cases = append(r.Cases, c)
			continue
		}
		r.Cases[i].add(c)
	}
	return nil
}

func (r *record) write(prefix string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(prefix+".json", append(b, '\n'), 0o644); err != nil {
		return err
	}
	var text strings.Builder
	if err := r.table(&text); err != nil {
		return err
	}
	if err := os.WriteFile(prefix+".txt", []byte(text.String()), 0o644); err != nil {
		return err
	}
	_, err = os.Stdout.WriteString(text.String())
	return err
}

func readRecord(path string) (record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return record{}, err
	}
	var r record
	if err := json.Unmarshal(b, &r); err != nil {
		return record{}, fmt.Errorf("%s: %w", path, err)
	}
	for i := range r.Cases {
		r.Cases[i].summarize()
	}
	return r, nil
}

// label names a case's row: its name, and its round when there are several.
func (r *record) label(c caseResult) string {
	if r.Repeats > 1 {
		return fmt.Sprintf("%s #%d", c.Name, c.Repeat+1)
	}
	return c.Name
}

// table writes the record as text: one row per case, where each case's time
// went, where its tail's time went, how its p99 spread over the rounds, and
// each case's shape.
func (r *record) table(w io.Writer) error {
	fmt.Fprintf(w, "%d client(s), %d servers, %d objects of %d bytes, %.0f reads/s per client, slow server +%v\n",
		len(r.Clients), len(r.Servers), r.Objects, r.ObjectBytes, r.Rate, r.SlowDelay)
	fmt.Fprintf(w, "Latency in ms, of hits only. Asks is stripe requests per read. 2nd is the reads that asked the rest\n"+
		"of the holders after the delay, refused those the budget stopped (hedged reads only; delay at least %v).\n"+
		"Server CPU in cores, summed over servers. Sent is server bytes per read.\n\n", r.HedgeMin)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "case\treads\tmiss\ttimeout\tdecoded\tasks\t2nd\trefused\tp50\tp90\tp99\tp99.9\tmax\tserver CPU\tsent KB\t")
	for _, c := range r.Cases {
		asks := 0.0
		if c.Issued > 0 {
			asks = float64(c.Requests) / float64(c.Issued)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%.2f\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%.2f\t%.0f\t\n",
			r.label(c), c.Issued, share(c.Misses, c.Issued), share(c.TimedOut, c.Issued), share(c.Decoded, c.Issued),
			asks, share(c.Second, c.Issued), share(c.Refused, c.Issued),
			ms(c.P50), ms(c.P90), ms(c.P99), ms(c.P999), ms(c.Max), cores(c.Servers.CPU, c.Elapsed), c.SentPerRead/1000)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if err := r.timeTable(w); err != nil {
		return err
	}
	if err := r.tailTable(w); err != nil {
		return err
	}
	if r.Repeats > 1 {
		if err := r.spreadTable(w); err != nil {
			return err
		}
	}
	fmt.Fprintln(w, "\nShape: the share of hits in each doubling of latency.")
	for _, c := range r.Cases {
		var parts []string
		for _, b := range c.Shape {
			parts = append(parts, fmt.Sprintf("%s %s", short(b.From), share(b.Count, c.Hits)))
		}
		fmt.Fprintf(w, "%s: %s\n", r.label(c), strings.Join(parts, " | "))
	}
	return nil
}

// timeTable writes where each case's time went: in the client process, at
// the servers, on the network and on the client's host.
func (r *record) timeTable(w io.Writer) error {
	fmt.Fprintf(w, "\nWhere the time went, in ms. Client CPU in cores, summed over clients; GC is the client's\n"+
		"collector: cycles, the time the world stopped and its longest 1%%. Sched p99 is how long a ready\n"+
		"goroutine waited for a thread. Decode is rebuilding each hit. Queue, read and wait are the servers'\n"+
		"time on every stripe reply: before serving, reading the store, and behind the replies ahead of it on\n"+
		"the connection. Network is the rest of the round trip of the stripe that completed each hit. Host\n"+
		"busy, softirq and steal are cores per host; retrans is TCP segments sent again, of those sent.\n\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "case\tclient CPU\tGCs\tGC pause\tpause p99\tsched p99\tdecode p50\tdecode p99\t"+
		"queue p99\tread p99\twait p99\tnetwork p50\tnetwork p99\thost busy\tsoftirq\tsteal\tretrans\tRTOs\t")
	for _, c := range r.Cases {
		host := []string{"-", "-", "-", "-", "-"}
		if h := c.Host; h.Hosts > 0 {
			per := c.Elapsed * time.Duration(h.Hosts)
			host = []string{
				fmt.Sprintf("%.2f", cores(h.User+h.System+h.IRQ+h.SoftIRQ, per)),
				fmt.Sprintf("%.2f", cores(h.SoftIRQ, per)),
				fmt.Sprintf("%.2f", cores(h.Steal, per)),
				share(h.TCPRetrans, h.TCPOut),
				fmt.Sprint(h.TCPTimeouts),
			}
		}
		fmt.Fprintf(tw, "%s\t%.2f\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t\n",
			r.label(c), cores(c.ClientCPU, c.Elapsed), c.Client.GCCycles, ms(c.Client.GCPause),
			ms(c.Client.GCPauses.quantile(0.99)), ms(c.Client.Sched.quantile(0.99)),
			ms(c.Decode.quantile(0.5)), ms(c.Decode.quantile(0.99)),
			ms(c.ServerQueued.quantile(0.99)), ms(c.ServerRead.quantile(0.99)), ms(c.ServerWaited.quantile(0.99)),
			ms(c.Network.quantile(0.5)), ms(c.Network.quantile(0.99)), strings.Join(host, "\t"))
	}
	return tw.Flush()
}

// tailTable writes where the time of each case's slowest hits went.
func (r *record) tailTable(w io.Writer) error {
	fmt.Fprintf(w, "\nThe tail: the hits that took %v or longer, and the mean of each part of their time in ms:\n"+
		"from due to the first request (issue), to the request of the stripe that completed the read (hedge),\n"+
		"that stripe's time at the server (queue, read, wait) and on the network, from the connection to the\n"+
		"read (deliver), and rebuilding (decode). By server is the share of them each server's stripe completed.\n\n",
		r.TailFrom)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "case\ttail\t%s\tby server\t\n", strings.Join(pathParts, "\t"))
	for _, c := range r.Cases {
		t := c.Tail
		means := make([]string, len(pathParts))
		for i, d := range t.Sum.parts() {
			means[i] = "-"
			if t.Reads > 0 {
				means[i] = ms(d / time.Duration(t.Reads))
			}
		}
		by := make([]string, len(t.ByServer))
		for i, n := range t.ByServer {
			by[i] = fmt.Sprintf("%.0f", 100*float64(n)/float64(t.Reads))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t\n", r.label(c), share(t.Reads, c.Hits), strings.Join(means, "\t"),
			strings.Join(by, " "))
	}
	return tw.Flush()
}

// spread is one case over its rounds.
type spread struct {
	name string
	p99  []time.Duration // by round
	all  histogram       // every round's latencies
}

// spreads groups the record's cases by name over their rounds, in the order
// of the conditions, then the codes, then the read modes.
func (r *record) spreads() []spread {
	cases := slices.Clone(r.Cases)
	slices.SortStableFunc(cases, func(a, b caseResult) int {
		return cmp.Or(
			cmp.Compare(slices.IndexFunc(conditions, func(c condition) bool { return c.name == a.Condition }),
				slices.IndexFunc(conditions, func(c condition) bool { return c.name == b.Condition })),
			cmp.Compare(a.Code, b.Code),
			cmp.Compare(slices.Index(readModeNames, a.Read), slices.Index(readModeNames, b.Read)),
			cmp.Compare(a.Name, b.Name),
			cmp.Compare(a.Repeat, b.Repeat))
	})
	var out []spread
	for _, c := range cases {
		if len(out) == 0 || out[len(out)-1].name != c.Name {
			out = append(out, spread{name: c.Name})
		}
		s := &out[len(out)-1]
		s.p99 = append(s.p99, c.P99)
		s.all.add(c.Latency)
	}
	return out
}

// spreadTable writes how each case's p99 spread over the rounds.
func (r *record) spreadTable(w io.Writer) error {
	fmt.Fprintf(w, "\nThe p99 of each round in ms, its smallest and largest, and the p99 and p99.9 of every round's\n"+
		"hits together.\n\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "case\tp99 by round\tleast\tmost\tmost/least\tp99 of all\tp99.9 of all\t")
	for _, s := range r.spreads() {
		rounds := make([]string, len(s.p99))
		for i, d := range s.p99 {
			rounds[i] = fmt.Sprintf("%.1f", float64(d)/float64(time.Millisecond))
		}
		least, most := slices.Min(s.p99), slices.Max(s.p99)
		ratio := "-"
		if least > 0 {
			ratio = fmt.Sprintf("%.1f", float64(most)/float64(least))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t\n", s.name, strings.Join(rounds, " "), ms(least), ms(most), ratio,
			ms(s.all.quantile(0.99)), ms(s.all.quantile(0.999)))
	}
	return tw.Flush()
}

// cores is CPU time over a span, in cores.
func cores(cpu, over time.Duration) float64 {
	if over <= 0 {
		return 0
	}
	return cpu.Seconds() / over.Seconds()
}

func ms(d time.Duration) string { return fmt.Sprintf("%.3f", float64(d)/float64(time.Millisecond)) }

// short is a bucket's bound to three figures or so: 131µs, 1.05ms, 16.8ms.
func short(d time.Duration) string {
	switch {
	case d < time.Microsecond:
		return fmt.Sprintf("%dns", d)
	case d < time.Millisecond:
		return fmt.Sprintf("%.0fµs", float64(d)/float64(time.Microsecond))
	case d < time.Second:
		return fmt.Sprintf("%.3gms", float64(d)/float64(time.Millisecond))
	default:
		return fmt.Sprintf("%.3gs", d.Seconds())
	}
}

func share(n, of uint64) string {
	if of == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(n)/float64(of))
}

// runReport merges the records of clients that ran the same cases together.
func runReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	out := fs.String("out", "", "where to write the merged record: PREFIX.json and PREFIX.txt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" || fs.NArg() == 0 {
		return errors.New("usage: report -out PREFIX RECORD.json...")
	}
	merged, err := readRecord(fs.Arg(0))
	if err != nil {
		return err
	}
	for _, path := range fs.Args()[1:] {
		r, err := readRecord(path)
		if err != nil {
			return err
		}
		if err := merged.merge(r); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return merged.write(*out)
}
