package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// witness flush measures what a guest's flush costs: each of --threads
// writes --block bytes at a time into a file of its own and flushes it with
// fdatasync, as a database appending to its log does, for --seconds. With
// --no-sync it writes and never flushes, which is the write rate the flushes
// are measured against. It prints one JSON object: the writes, their bytes,
// the rate, and the latency of each write with its flush.
//
// A file is written whole before the clock starts, so every timed write lands
// on a block the filesystem has already allocated and initialized: the flush
// then carries the block and no metadata of the guest's filesystem. Each
// write stamps the block with a count, so no two writes of one block hold the
// same bytes.

// flushOptions is one flush command line, parsed.
type flushOptions struct {
	dir            string
	threads, block int
	fileBytes      int64
	duration       time.Duration
	sync           bool
}

// flushReport is what one run of the command measured.
type flushReport struct {
	Threads      int     `json:"threads"`
	Block        int     `json:"block"`
	Sync         bool    `json:"sync"`
	Seconds      float64 `json:"seconds"`
	Writes       int     `json:"writes"`
	Bytes        int64   `json:"bytes"`
	MiBPerSecond float64 `json:"mib_per_second"`
	// The latency of one write and its flush, or of the write alone, in
	// microseconds.
	P50   float64 `json:"p50_us"`
	P99   float64 `json:"p99_us"`
	P999  float64 `json:"p999_us"`
	Max   float64 `json:"max_us"`
	Mean  float64 `json:"mean_us"`
	Began string  `json:"began"`
	Ended string  `json:"ended"`
}

func parseFlush(args []string) (flushOptions, error) {
	set := flag.NewFlagSet("flush", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	parsed := flushOptions{}
	set.StringVar(&parsed.dir, "dir", "", "the directory the files are written in")
	set.IntVar(&parsed.threads, "threads", 1, "how many threads write, each into a file of its own")
	set.IntVar(&parsed.block, "block", 4096, "the bytes of one write")
	file := set.String("file", "4M", "the size of each thread's file")
	set.DurationVar(&parsed.duration, "seconds", 10*time.Second, "how long the writes run")
	noSync := set.Bool("no-sync", false, "write and never flush")
	if err := set.Parse(args); err != nil {
		return flushOptions{}, err
	}
	if set.NArg() != 0 {
		return flushOptions{}, fmt.Errorf("flush takes flags only, not %q", set.Args())
	}
	if parsed.dir == "" {
		return flushOptions{}, errors.New("flush needs --dir, the directory it writes its files in")
	}
	size, err := parseSize(*file)
	if err != nil {
		return flushOptions{}, fmt.Errorf("--file: %w", err)
	}
	parsed.fileBytes, parsed.sync = size, !*noSync
	switch {
	case parsed.threads < 1:
		return flushOptions{}, errors.New("--threads is at least 1")
	case parsed.block < 8 || parsed.block%8 != 0:
		return flushOptions{}, errors.New("--block is a multiple of 8 bytes")
	case parsed.fileBytes < int64(parsed.block) || parsed.fileBytes%int64(parsed.block) != 0:
		return flushOptions{}, errors.New("--file is a whole number of blocks")
	case parsed.duration <= 0:
		return flushOptions{}, errors.New("--seconds is a positive duration, such as 10s")
	}
	return parsed, nil
}

// flush runs the command and prints its report.
func flush(args []string) error {
	parsed, err := parseFlush(args)
	if err != nil {
		return fmt.Errorf("%w\n\n%s", err, usage)
	}
	report, err := measureFlushes(parsed)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}

// measureFlushes writes every thread's file whole, then runs the threads
// together for the duration.
func measureFlushes(parsed flushOptions) (flushReport, error) {
	if err := os.MkdirAll(parsed.dir, 0o755); err != nil {
		return flushReport{}, err
	}
	files := make([]*os.File, parsed.threads)
	defer func() {
		for _, file := range files {
			if file != nil {
				file.Close()
			}
		}
	}()
	for index := range files {
		path := filepath.Join(parsed.dir, fmt.Sprintf("flush-%d", index))
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return flushReport{}, err
		}
		files[index] = file
		if err := prefill(file, parsed.fileBytes); err != nil {
			return flushReport{}, fmt.Errorf("writing %s whole: %w", path, err)
		}
	}
	latencies := make([][]time.Duration, parsed.threads)
	failures := make([]error, parsed.threads)
	start := make(chan struct{})
	var group sync.WaitGroup
	for index := range files {
		group.Go(func() {
			<-start
			latencies[index], failures[index] = writeFlushing(files[index], uint64(index), parsed)
		})
	}
	began := time.Now()
	close(start)
	group.Wait()
	ended := time.Now()
	if err := errors.Join(failures...); err != nil {
		return flushReport{}, err
	}
	all := slices.Concat(latencies...)
	slices.Sort(all)
	report := flushReport{Threads: parsed.threads, Block: parsed.block, Sync: parsed.sync,
		Seconds: ended.Sub(began).Seconds(), Writes: len(all), Bytes: int64(len(all)) * int64(parsed.block),
		Began: began.UTC().Format(time.RFC3339Nano), Ended: ended.UTC().Format(time.RFC3339Nano)}
	report.MiBPerSecond = float64(report.Bytes) / float64(1<<20) / report.Seconds
	if len(all) > 0 {
		micro := func(d time.Duration) float64 { return float64(d) / float64(time.Microsecond) }
		report.P50, report.P99 = micro(quantile(all, 0.50)), micro(quantile(all, 0.99))
		report.P999, report.Max = micro(quantile(all, 0.999)), micro(all[len(all)-1])
		var total time.Duration
		for _, d := range all {
			total += d
		}
		report.Mean = micro(total / time.Duration(len(all)))
	}
	return report, nil
}

// quantile is the q quantile of sorted, the nearest rank at or above it.
func quantile(sorted []time.Duration, q float64) time.Duration {
	rank := int(q*float64(len(sorted)) + 0.5)
	return sorted[min(max(rank, 1), len(sorted))-1]
}

// prefill writes the file whole, with bytes that are not zero, and flushes it.
func prefill(file *os.File, size int64) error {
	chunk := make([]byte, 1<<20)
	for index := range chunk {
		chunk[index] = byte(index*7 + 1)
	}
	for at := int64(0); at < size; at += int64(len(chunk)) {
		n := min(int64(len(chunk)), size-at)
		if _, err := file.WriteAt(chunk[:n], at); err != nil {
			return err
		}
	}
	return file.Sync()
}

// writeFlushing writes one thread's blocks in turn around its file, each
// stamped with the thread and a count, and flushes each where the options
// say, until the duration is up. It reports what each write took.
func writeFlushing(file *os.File, thread uint64, parsed flushOptions) ([]time.Duration, error) {
	block := make([]byte, parsed.block)
	for index := range block {
		block[index] = byte(index*13 + 5)
	}
	blocks := parsed.fileBytes / int64(parsed.block)
	latencies := make([]time.Duration, 0, 1<<14)
	deadline := time.Now().Add(parsed.duration)
	for count := uint64(0); ; count++ {
		began := time.Now()
		if !began.Before(deadline) {
			return latencies, nil
		}
		binary.LittleEndian.PutUint64(block, thread<<48|count)
		if _, err := file.WriteAt(block, int64(count%uint64(blocks))*int64(parsed.block)); err != nil {
			return latencies, err
		}
		if parsed.sync {
			if err := fdatasync(file); err != nil {
				return latencies, fmt.Errorf("flushing %s: %w", file.Name(), err)
			}
		}
		latencies = append(latencies, time.Since(began))
	}
}
