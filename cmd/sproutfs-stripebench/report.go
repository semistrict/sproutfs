package main

import (
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
// condition and one code.
type caseResult struct {
	Name      string    `json:"name"`
	Load      string    `json:"load"`
	Medium    string    `json:"medium"`
	Condition string    `json:"condition"`
	Code      string    `json:"code"`
	Started   time.Time `json:"started"`
	// Elapsed runs from the first read due to the last read done.
	Elapsed time.Duration `json:"elapsed_ns"`

	Issued   uint64 `json:"issued"`
	Hits     uint64 `json:"hits"`
	Misses   uint64 `json:"misses"`
	TimedOut uint64 `json:"timed_out"`
	// Decoded counts the hits that had to rebuild a data stripe from parity.
	Decoded uint64 `json:"decoded"`

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
}

func (r *caseResult) summarize() {
	r.P50 = r.Latency.quantile(0.5)
	r.P90 = r.Latency.quantile(0.9)
	r.P99 = r.Latency.quantile(0.99)
	r.P999 = r.Latency.quantile(0.999)
	r.Max = r.Latency.max
	r.Shape = r.Latency.shape()
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
	r.Latency.add(o.Latency)
	r.Servers = serverStats{
		CPU:     max(r.Servers.CPU, o.Servers.CPU),
		Sent:    max(r.Servers.Sent, o.Servers.Sent),
		Replies: max(r.Servers.Replies, o.Servers.Replies),
		Reads:   max(r.Servers.Reads, o.Servers.Reads),
	}
	r.ClientCPU += o.ClientCPU
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
	Drained     int           `json:"drained"`
	Slow        int           `json:"slow"`
	Cases       []caseResult  `json:"cases"`
}

func (r *record) merge(o record) error {
	if !slices.Equal(r.Servers, o.Servers) || r.Objects != o.Objects || r.ObjectBytes != o.ObjectBytes ||
		r.Seed != o.Seed {
		return errors.New("the records read different object sets")
	}
	r.Clients = append(r.Clients, o.Clients...)
	for _, c := range o.Cases {
		i := slices.IndexFunc(r.Cases, func(m caseResult) bool { return m.Name == c.Name })
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

// table writes the record as text: one row per case, then each case's shape.
func (r *record) table(w io.Writer) error {
	fmt.Fprintf(w, "%d client(s), %d servers, %d objects of %d bytes, %.0f reads/s per client, slow server +%v\n",
		len(r.Clients), len(r.Servers), r.Objects, r.ObjectBytes, r.Rate, r.SlowDelay)
	fmt.Fprintf(w, "Latency in ms, of hits only. Server CPU in cores, summed over servers. Sent is server bytes per read.\n\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "case\treads\tmiss\ttimeout\tdecoded\tp50\tp90\tp99\tp99.9\tmax\tserver CPU\tsent KB\t")
	for _, c := range r.Cases {
		cores := 0.0
		if c.Elapsed > 0 {
			cores = c.Servers.CPU.Seconds() / c.Elapsed.Seconds()
		}
		sent := 0.0
		if c.Issued > 0 {
			sent = float64(c.Servers.Sent) / float64(c.Issued) / 1000
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%.2f\t%.0f\t\n",
			c.Name, c.Issued, share(c.Misses, c.Issued), share(c.TimedOut, c.Issued), share(c.Decoded, c.Issued),
			ms(c.P50), ms(c.P90), ms(c.P99), ms(c.P999), ms(c.Max), cores, sent)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w, "\nShape: the share of hits in each doubling of latency.")
	for _, c := range r.Cases {
		var parts []string
		for _, b := range c.Shape {
			parts = append(parts, fmt.Sprintf("%s %s", short(b.From), share(b.Count, c.Hits)))
		}
		fmt.Fprintf(w, "%s: %s\n", c.Name, strings.Join(parts, " | "))
	}
	return nil
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
