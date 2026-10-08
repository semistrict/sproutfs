package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
	"unsafe"
)

// witness reads measures what a guest's reads of one file cost while it
// writes others: it writes --file of --size whole, then reads it at random,
// --block at a time, one read every --interval, while one writer writes
// --write-bytes of fresh files into --dir, at --write-rate bytes a second where
// that is given; and goes on reading for --idle once the writes are done. It prints one JSON object: the latency of the reads
// during the writes and after them, and what the writes did.
//
// On a DAX root a read is a copy out of the host's page, so what it costs is
// whether the host still has that page: the reads say whether a host that
// evicts under the writes keeps the pages the guest keeps reading. Every byte
// a read returns is checked against the file's pattern, so a fast read of the
// wrong bytes is a failure and not a good number.

// readsOptions is one reads command line, parsed.
type readsOptions struct {
	file, dir                   string
	size, writeBytes, writeRate int64
	block                       int
	interval, idle              time.Duration
	direct                      bool
}

// readsPhase is the reads of one phase, in microseconds.
type readsPhase struct {
	Reads int     `json:"reads"`
	P50   float64 `json:"p50_us"`
	P90   float64 `json:"p90_us"`
	P99   float64 `json:"p99_us"`
	Max   float64 `json:"max_us"`
}

// readsReport is what one run of the command measured.
type readsReport struct {
	Block        int        `json:"block"`
	FileBytes    int64      `json:"file_bytes"`
	WrittenBytes int64      `json:"written_bytes"`
	WriteSeconds float64    `json:"write_seconds"`
	Writing      readsPhase `json:"writing"`
	Idle         readsPhase `json:"idle"`
}

func parseReads(args []string) (readsOptions, error) {
	set := flag.NewFlagSet("reads", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	parsed := readsOptions{}
	set.StringVar(&parsed.file, "file", "", "the file that is read")
	set.StringVar(&parsed.dir, "dir", "", "the directory the writes go into")
	size := set.String("size", "64M", "the size of the file that is read")
	written := set.String("write-bytes", "1G", "how much the writer writes")
	rate := set.String("write-rate", "", "the bytes the writer writes a second, as fast as it can where unset")
	set.IntVar(&parsed.block, "block", 4096, "the bytes of one read")
	set.DurationVar(&parsed.interval, "interval", 2*time.Millisecond, "the time between two reads")
	set.DurationVar(&parsed.idle, "idle", 5*time.Second, "how long the reads go on once the writes are done")
	buffered := set.Bool("buffered", false, "read without O_DIRECT")
	if err := set.Parse(args); err != nil {
		return readsOptions{}, err
	}
	if set.NArg() != 0 {
		return readsOptions{}, fmt.Errorf("reads takes flags only, not %q", set.Args())
	}
	if parsed.file == "" || parsed.dir == "" {
		return readsOptions{}, errors.New("reads needs --file, the file it reads, and --dir, where it writes")
	}
	var err error
	if parsed.size, err = parseSize(*size); err != nil {
		return readsOptions{}, fmt.Errorf("--size: %w", err)
	}
	if parsed.writeBytes, err = parseSize(*written); err != nil {
		return readsOptions{}, fmt.Errorf("--write-bytes: %w", err)
	}
	if *rate != "" {
		if parsed.writeRate, err = parseSize(*rate); err != nil {
			return readsOptions{}, fmt.Errorf("--write-rate: %w", err)
		}
	}
	parsed.direct = !*buffered
	switch {
	case parsed.block < 512 || parsed.block%512 != 0:
		return readsOptions{}, errors.New("--block is a multiple of 512 bytes")
	case parsed.size < int64(parsed.block) || parsed.size%int64(parsed.block) != 0:
		return readsOptions{}, errors.New("--size is a whole number of blocks")
	case parsed.writeBytes < 0:
		return readsOptions{}, errors.New("--write-bytes is not negative")
	case parsed.interval < 0 || parsed.idle < 0:
		return readsOptions{}, errors.New("--interval and --idle are not negative")
	}
	return parsed, nil
}

// reads runs the command and prints its report.
func reads(args []string) error {
	parsed, err := parseReads(args)
	if err != nil {
		return fmt.Errorf("%w\n\n%s", err, usage)
	}
	report, err := measureReads(parsed)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}

// readPattern is the word at offset of the read file: its offset, so every
// block names where it is.
func readPattern(offset int64) uint64 { return uint64(offset) ^ 0x5eed_0000_0000_0000 }

// measureReads writes the read file, then reads it while the writer writes,
// and after.
func measureReads(parsed readsOptions) (readsReport, error) {
	if err := writeReadFile(parsed.file, parsed.size); err != nil {
		return readsReport{}, fmt.Errorf("writing %s: %w", parsed.file, err)
	}
	if err := os.MkdirAll(parsed.dir, 0o755); err != nil {
		return readsReport{}, err
	}
	flags := os.O_RDONLY
	if parsed.direct {
		flags |= oDirect
	}
	file, err := os.OpenFile(parsed.file, flags, 0)
	if err != nil {
		return readsReport{}, err
	}
	defer file.Close()
	report := readsReport{Block: parsed.block, FileBytes: parsed.size}
	writing := make(chan struct{})
	var written int64
	var writeErr error
	var group sync.WaitGroup
	began := time.Now()
	group.Go(func() {
		defer close(writing)
		written, writeErr = writeFresh(parsed.dir, parsed.writeBytes, parsed.writeRate)
	})
	during, readErr := readUntil(file, parsed, writing)
	report.WriteSeconds = time.Since(began).Seconds()
	group.Wait()
	if err := errors.Join(writeErr, readErr); err != nil {
		return readsReport{}, err
	}
	idleDone := make(chan struct{})
	time.AfterFunc(parsed.idle, func() { close(idleDone) })
	after, err := readUntil(file, parsed, idleDone)
	if err != nil {
		return readsReport{}, err
	}
	report.WrittenBytes = written
	report.Writing, report.Idle = phase(during), phase(after)
	return report, nil
}

// writeReadFile writes the file whole with its pattern and flushes it.
func writeReadFile(path string, size int64) error {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	chunk := make([]byte, 1<<20)
	for at := int64(0); at < size; at += int64(len(chunk)) {
		n := min(int64(len(chunk)), size-at)
		for word := int64(0); word < n; word += 8 {
			binary.LittleEndian.PutUint64(chunk[word:], readPattern(at+word))
		}
		if _, err := file.WriteAt(chunk[:n], at); err != nil {
			return err
		}
	}
	return file.Sync()
}

// writeFresh writes total bytes into new files of 64 MiB in dir, bytes no
// two files share, and flushes each. A rate other than zero paces the writes
// to that many bytes a second, as a build or an install writes over minutes.
func writeFresh(dir string, total, rate int64) (int64, error) {
	const fileBytes = 64 << 20
	chunk := make([]byte, 1<<20)
	var written int64
	began := time.Now()
	for index := 0; written < total; index++ {
		file, err := os.Create(filepath.Join(dir, fmt.Sprintf("fresh-%d", index)))
		if err != nil {
			return written, err
		}
		for at := int64(0); at < fileBytes && written < total; at += int64(len(chunk)) {
			for word := 0; word < len(chunk); word += 8 {
				binary.LittleEndian.PutUint64(chunk[word:], uint64(written+int64(word))*0x9e37_79b9_7f4a_7c15)
			}
			n := min(int64(len(chunk)), total-written)
			if _, err := file.Write(chunk[:n]); err != nil {
				file.Close()
				return written, err
			}
			written += n
			if rate > 0 {
				time.Sleep(time.Until(began.Add(time.Duration(float64(written) / float64(rate) * float64(time.Second)))))
			}
		}
		if err := errors.Join(file.Sync(), file.Close()); err != nil {
			return written, err
		}
	}
	return written, nil
}

// readUntil reads blocks of the file at random, one every interval, checking
// each, until done is closed, and reports what each read took.
func readUntil(file *os.File, parsed readsOptions, done <-chan struct{}) ([]time.Duration, error) {
	block := alignedBlock(parsed.block)
	blocks := parsed.size / int64(parsed.block)
	random := rand.New(rand.NewPCG(uint64(parsed.size), uint64(parsed.block)))
	var latencies []time.Duration
	for {
		select {
		case <-done:
			return latencies, nil
		default:
		}
		offset := random.Int64N(blocks) * int64(parsed.block)
		started := time.Now()
		if _, err := file.ReadAt(block, offset); err != nil {
			return latencies, fmt.Errorf("reading %d bytes at %d: %w", len(block), offset, err)
		}
		latencies = append(latencies, time.Since(started))
		for word := 0; word < len(block); word += 8 {
			if got, want := binary.LittleEndian.Uint64(block[word:]), readPattern(offset+int64(word)); got != want {
				return latencies, fmt.Errorf("the word at %d reads %#x, want %#x", offset+int64(word), got, want)
			}
		}
		time.Sleep(parsed.interval)
	}
}

// alignedBlock is a buffer of n bytes aligned for O_DIRECT, whose reads need
// a buffer aligned to the device's block.
func alignedBlock(n int) []byte {
	const align = 4096
	buffer := make([]byte, n+align)
	skip := (align - int(uintptrOf(buffer)%align)) % align
	return buffer[skip : skip+n]
}

// uintptrOf is the address of a buffer's first byte.
func uintptrOf(buffer []byte) uintptr { return uintptr(unsafe.Pointer(unsafe.SliceData(buffer))) }

// phase is the latencies of one phase, sorted and summarized.
func phase(latencies []time.Duration) readsPhase {
	if len(latencies) == 0 {
		return readsPhase{}
	}
	sorted := slices.Clone(latencies)
	slices.Sort(sorted)
	micro := func(d time.Duration) float64 { return float64(d) / float64(time.Microsecond) }
	return readsPhase{Reads: len(sorted), P50: micro(quantile(sorted, 0.50)), P90: micro(quantile(sorted, 0.90)),
		P99: micro(quantile(sorted, 0.99)), Max: micro(sorted[len(sorted)-1])}
}
