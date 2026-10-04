package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"slices"
	"strings"
	"time"
)

// hostLine is one of the lines a host logs for a start, as its JSON handler
// writes it: "host: a VM runs", "host: a VM forked" or "host: a VM's first
// faults".
type hostLine struct {
	Time      time.Time     `json:"time"`
	Msg       string        `json:"msg"`
	VM        string        `json:"vm"`
	How       string        `json:"how"`
	Children  []string      `json:"children"`
	TotalMS   float64       `json:"total_ms"`
	RunningMS float64       `json:"running_ms"`
	StoreMS   float64       `json:"store_ms"`
	Steps     []loggedStep  `json:"steps"`
	Store     []loggedStore `json:"store"`
	Attach    []struct {
		Region         string  `json:"region"`
		MS             float64 `json:"ms"`
		PopulateMS     float64 `json:"populate_ms"`
		PopulatedPages float64 `json:"populated_pages"`
	} `json:"attach"`
	Faults []struct {
		Region         string  `json:"region"`
		Before         float64 `json:"before"`
		BeforeWaitedMS float64 `json:"before_waited_ms"`
		After          float64 `json:"after"`
		AfterWaitedMS  float64 `json:"after_waited_ms"`
		FirstMS        float64 `json:"first_ms"`
		FirstWaitedMS  float64 `json:"first_waited_ms"`
	} `json:"faults"`
	UnpublishedPages float64 `json:"unpublished_pages"`
}

type loggedStep struct {
	Name   string  `json:"name"`
	AtMS   float64 `json:"at_ms"`
	TookMS float64 `json:"took_ms"`
}

type loggedStore struct {
	Kind  string  `json:"kind"`
	Calls int     `json:"calls"`
	MS    float64 `json:"ms"`
}

const (
	lineRuns   = "host: a VM runs"
	lineForked = "host: a VM forked"
	lineFaults = "host: a VM's first faults"
)

// hostLines reads the lines of starts out of the hosts' logs: as kubectl logs
// prints them, or as the node keeps them, where each line is prefixed with its
// time and stream. Anything else in them, a line of another kind or one that
// is not JSON, is skipped.
func hostLines(r io.Reader) ([]hostLine, error) {
	var lines []hostLine
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		text := scanner.Bytes()
		at := bytes.IndexByte(text, '{')
		if at < 0 {
			continue
		}
		text = bytes.TrimSpace(text[at:])
		var line hostLine
		if err := json.Unmarshal(text, &line); err != nil {
			continue
		}
		switch line.Msg {
		case lineRuns, lineForked, lineFaults:
			lines = append(lines, line)
		}
	}
	return lines, scanner.Err()
}

// start is one sample with the hosts' lines for it.
type start struct {
	sample sample
	// runs is the line of the host the VM started on, faults its first
	// faults, and forked the parent's host's line of a fork.
	runs, faults, forked *hostLine
}

// startHow is how the host names the request that started a case's VM.
func startHow(name string) string {
	switch name {
	case caseCold:
		return "create"
	case caseRestore:
		return "open"
	default:
		return "receive"
	}
}

// join matches each started sample with its lines. A VM restored round after
// round has one line a round, so the lines of one VM are matched in the order
// the hosts logged them.
func join(samples []sample, lines []hostLine) []start {
	type key struct{ vm, how, msg string }
	byKey := map[key][]*hostLine{}
	forked := map[string]*hostLine{}
	for i := range lines {
		line := &lines[i]
		if line.Msg == lineForked {
			for _, child := range line.Children {
				forked[child] = line
			}
			continue
		}
		k := key{line.VM, line.How, line.Msg}
		byKey[k] = append(byKey[k], line)
	}
	for _, matched := range byKey {
		slices.SortStableFunc(matched, func(a, b *hostLine) int { return a.Time.Compare(b.Time) })
	}
	used := map[key]int{}
	var starts []start
	for _, s := range samples {
		joined := start{sample: s, forked: forked[s.VM]}
		if s.Started {
			how := startHow(s.Case)
			for _, msg := range []string{lineRuns, lineFaults} {
				k := key{s.VM, how, msg}
				if at := used[k]; at < len(byKey[k]) {
					if msg == lineRuns {
						joined.runs = byKey[k][at]
					} else {
						joined.faults = byKey[k][at]
					}
					used[k] = at + 1
				}
			}
		}
		starts = append(starts, joined)
	}
	return starts
}

// group is the row a start is reported in: its case, and for a restore whether
// it was on the host that suspended the VM.
func group(s sample) string {
	if s.Case != caseRestore {
		return s.Case
	}
	if s.Host == s.From {
		return "restore, same host"
	}
	return "restore, other host"
}

// percentile is the nearest-rank percentile of values, which it sorts.
func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	slices.Sort(values)
	rank := int(math.Ceil(p*float64(len(values)))) - 1
	return values[max(rank, 0)]
}

// series collects values by name, remembering the order names first came.
type series struct {
	names  []string
	values map[string][]float64
}

func (s *series) add(name string, value float64) {
	if s.values == nil {
		s.values = map[string][]float64{}
	}
	if _, seen := s.values[name]; !seen {
		s.names = append(s.names, name)
	}
	s.values[name] = append(s.values[name], value)
}

// containers are steps that hold others: their time is their parts'.
var containers = map[string]bool{"vmm start": true, "seal": true}

// toRunning is the time from a start's first request to its guest running: the
// request's send, half of what its round trip took beyond the host's own time,
// and the host's time to running.
func toRunning(st start) float64 {
	s := st.sample
	return s.RequestAtMS + (s.RequestMS-st.runs.TotalMS)/2 + st.runs.RunningMS
}

// steps is a start's time to running split into its parts, each named as the
// report names it. The parts of a fork on the parent's host come first.
func steps(st start, into *series) {
	s := st.sample
	if st.forked != nil {
		for _, step := range st.forked.Steps {
			into.add("parent: "+step.Name, step.TookMS)
		}
		into.add("parent: fork round trip beyond the host", s.ForkMS-st.forked.TotalMS)
		into.add("control plane: fork answer to receive request", s.RequestAtMS-s.ForkMS)
	}
	into.add("request: one way", (s.RequestMS-st.runs.TotalMS)/2)
	for _, step := range st.runs.Steps {
		if step.AtMS < st.runs.RunningMS {
			into.add(step.Name, step.TookMS)
		}
	}
}

func fmtMS(value float64) string {
	if math.IsNaN(value) {
		return "–"
	}
	switch {
	case value >= 100:
		return fmt.Sprintf("%.0f", value)
	case value >= 10:
		return fmt.Sprintf("%.1f", value)
	default:
		return fmt.Sprintf("%.2f", value)
	}
}

// summarize writes the report's tables for starts, and for the store's own
// times when there are any.
func summarize(w io.Writer, starts []start, store *storeTimes) error {
	var groups []string
	byGroup := map[string][]start{}
	failed := map[string]int{}
	for _, st := range starts {
		name := group(st.sample)
		if _, seen := byGroup[name]; !seen {
			groups = append(groups, name)
			byGroup[name] = nil
		}
		if st.sample.Error != "" || st.runs == nil {
			failed[name]++
			continue
		}
		byGroup[name] = append(byGroup[name], st)
	}
	var out strings.Builder
	out.WriteString("## From the request to the guest\n\n")
	out.WriteString("| Case | starts | failed | to running p50 | p90 | p99 | max | agent answers p50 | p99 | store p50 | p99 |\n")
	out.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, name := range groups {
		var running, agent, storeMS []float64
		for _, st := range byGroup[name] {
			running = append(running, toRunning(st))
			agent = append(agent, st.sample.AgentMS)
			storeMS = append(storeMS, st.runs.StoreMS)
		}
		fmt.Fprintf(&out, "| %s | %d | %d | %s | %s | %s | %s | %s | %s | %s | %s |\n", name, len(byGroup[name]),
			failed[name], fmtMS(percentile(running, 0.5)), fmtMS(percentile(running, 0.9)),
			fmtMS(percentile(running, 0.99)), fmtMS(percentile(running, 1)), fmtMS(percentile(agent, 0.5)),
			fmtMS(percentile(agent, 0.99)), fmtMS(percentile(storeMS, 0.5)), fmtMS(percentile(storeMS, 0.99)))
	}
	for _, name := range groups {
		if len(byGroup[name]) == 0 {
			continue
		}
		var split, stores, attach, faults series
		for _, st := range byGroup[name] {
			steps(st, &split)
			for _, kind := range st.runs.Store {
				stores.add(kind.Kind, kind.MS)
				stores.add(kind.Kind+" calls", float64(kind.Calls))
			}
			if st.forked != nil {
				for _, kind := range st.forked.Store {
					stores.add("parent: "+kind.Kind, kind.MS)
					stores.add("parent: "+kind.Kind+" calls", float64(kind.Calls))
				}
			}
			for _, region := range st.runs.Attach {
				attach.add(region.Region+" attach", region.MS)
				attach.add(region.Region+" populate", region.PopulateMS)
				attach.add(region.Region+" populated pages", region.PopulatedPages)
			}
			if st.faults != nil {
				for _, region := range st.faults.Faults {
					faults.add(region.Region+" faults before running", region.Before)
					faults.add(region.Region+" faults in the first second", region.After)
					faults.add(region.Region+" ms waited in them", region.AfterWaitedMS)
					if region.FirstMS != 0 {
						faults.add(region.Region+" first fault, ms from running", region.FirstMS-st.faults.RunningMS)
						faults.add(region.Region+" first fault, ms waited", region.FirstWaitedMS)
					}
				}
			}
		}
		fmt.Fprintf(&out, "\n## %s\n\n| Step | p50 ms | p99 ms |\n| --- | --- | --- |\n", name)
		largest, largestP50 := "", -1.0
		for _, step := range split.names {
			p50, p99 := percentile(split.values[step], 0.5), percentile(split.values[step], 0.99)
			fmt.Fprintf(&out, "| %s | %s | %s |\n", step, fmtMS(p50), fmtMS(p99))
			if !containers[strings.TrimPrefix(step, "parent: ")] && p50 > largestP50 {
				largest, largestP50 = step, p50
			}
		}
		fmt.Fprintf(&out, "\nThe largest step at the median: %s, %s ms.\n", largest, fmtMS(largestP50))
		for _, table := range []struct {
			title string
			of    *series
		}{{"Store calls before the guest runs, per start", &stores}, {"Attach", &attach},
			{"Faults", &faults}} {
			if len(table.of.names) == 0 {
				continue
			}
			fmt.Fprintf(&out, "\n%s:\n\n| | p50 | p99 |\n| --- | --- | --- |\n", table.title)
			for _, row := range table.of.names {
				fmt.Fprintf(&out, "| %s | %s | %s |\n", row, fmtMS(percentile(table.of.values[row], 0.5)),
					fmtMS(percentile(table.of.values[row], 0.99)))
			}
		}
	}
	if store != nil {
		out.WriteString("\n## The store alone\n\n| Call | count | p50 ms | p90 | p99 | max |\n| --- | --- | --- | --- | --- | --- |\n")
		for _, call := range []struct {
			name  string
			times []float64
		}{{"create if absent", store.Create}, {"read", store.Get}, {"compare-and-set", store.Update},
			{"delete", store.Delete}} {
			times := slices.Clone(call.times)
			fmt.Fprintf(&out, "| %s | %d | %s | %s | %s | %s |\n", call.name, len(times),
				fmtMS(percentile(times, 0.5)), fmtMS(percentile(times, 0.9)), fmtMS(percentile(times, 0.99)),
				fmtMS(percentile(times, 1)))
		}
	}
	_, err := io.WriteString(w, out.String())
	return err
}

func readSamples(r io.Reader) ([]sample, error) {
	var samples []sample
	decoder := json.NewDecoder(r)
	for {
		var s sample
		err := decoder.Decode(&s)
		if errors.Is(err, io.EOF) {
			return samples, nil
		}
		if err != nil {
			return nil, err
		}
		samples = append(samples, s)
	}
}

func runSummary(args []string, w io.Writer) error {
	flags := flag.NewFlagSet("summary", flag.ContinueOnError)
	samplesPath := flags.String("samples", "samples.jsonl", "the driver's samples")
	logsPaths := flags.String("logs", "pods.log", "the hosts' logs, separated by commas")
	storePath := flags.String("store", "", "the store bench's times, if any")
	if err := flags.Parse(args); err != nil {
		return err
	}
	samplesFile, err := os.Open(*samplesPath)
	if err != nil {
		return err
	}
	defer samplesFile.Close()
	samples, err := readSamples(samplesFile)
	if err != nil {
		return fmt.Errorf("reading %s: %w", *samplesPath, err)
	}
	var lines []hostLine
	for _, path := range strings.Split(*logsPaths, ",") {
		logsFile, err := os.Open(path)
		if err != nil {
			return err
		}
		read, err := hostLines(logsFile)
		if closeErr := logsFile.Close(); closeErr != nil {
			slog.Error("sproutfs-startbench: closing a log", "path", path, "error", closeErr)
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		lines = append(lines, read...)
	}
	var store *storeTimes
	if *storePath != "" {
		raw, err := os.ReadFile(*storePath)
		if err != nil {
			return err
		}
		store = &storeTimes{}
		if err := json.Unmarshal(raw, store); err != nil {
			return fmt.Errorf("reading %s: %w", *storePath, err)
		}
	}
	return summarize(w, join(samples, lines), store)
}
