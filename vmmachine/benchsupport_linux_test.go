//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/platform"
)

// ---------------------------------------------------------------------------
// Object storage
// ---------------------------------------------------------------------------

// objectCounters is what one scenario cost the object store. Deltas of it are
// what the records report, so a scenario that is supposed to read nothing can
// be asserted on rather than described.
type objectCounters struct {
	Heads    uint64 `json:"heads"`
	Gets     uint64 `json:"gets"`
	Puts     uint64 `json:"puts"`
	Deletes  uint64 `json:"deletes"`
	Lists    uint64 `json:"lists"`
	GetBytes int64  `json:"get_bytes"`
	PutBytes int64  `json:"put_bytes"`
}

func (c objectCounters) sub(earlier objectCounters) objectCounters {
	return objectCounters{
		Heads: c.Heads - earlier.Heads, Gets: c.Gets - earlier.Gets, Puts: c.Puts - earlier.Puts,
		Deletes: c.Deletes - earlier.Deletes, Lists: c.Lists - earlier.Lists,
		GetBytes: c.GetBytes - earlier.GetBytes, PutBytes: c.PutBytes - earlier.PutBytes,
	}
}

// countingObjectStore counts every request and the bytes it moved. It owns no
// storage of its own: the backing is the simulated store, the on-disk store or
// a real S3 endpoint, whichever the run selected.
type countingObjectStore struct {
	inner                             platform.ObjectStore
	heads, gets, puts, deletes, lists atomic.Uint64
	getBytes, putBytes                atomic.Int64
}

func (s *countingObjectStore) counters() objectCounters {
	return objectCounters{Heads: s.heads.Load(), Gets: s.gets.Load(), Puts: s.puts.Load(),
		Deletes: s.deletes.Load(), Lists: s.lists.Load(),
		GetBytes: s.getBytes.Load(), PutBytes: s.putBytes.Load()}
}

func (s *countingObjectStore) Head(ctx context.Context, key platform.ObjectKey) (platform.ObjectMetadata, error) {
	s.heads.Add(1)
	return s.inner.Head(ctx, key)
}

func (s *countingObjectStore) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	s.gets.Add(1)
	result, err := s.inner.Get(ctx, request)
	if err == nil {
		s.getBytes.Add(result.ContentLength)
	}
	return result, err
}

func (s *countingObjectStore) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	s.puts.Add(1)
	result, err := s.inner.Put(ctx, request)
	if err == nil {
		s.putBytes.Add(request.Size)
	}
	return result, err
}

func (s *countingObjectStore) Delete(ctx context.Context, request platform.DeleteRequest) error {
	s.deletes.Add(1)
	return s.inner.Delete(ctx, request)
}

func (s *countingObjectStore) List(ctx context.Context, request platform.ListRequest) (platform.ListResult, error) {
	s.lists.Add(1)
	return s.inner.List(ctx, request)
}

// objectLatency is the cost model the simulated store applies, restated here so
// the on-disk store charges a scenario exactly the same latency and bandwidth.
type objectLatency struct {
	Head, Get, Put, Delete, List time.Duration
	BytesPerSecond               int64
}

func (l objectLatency) charge(ctx context.Context, base time.Duration, bytes int64) error {
	total := base
	if l.BytesPerSecond > 0 && bytes > 0 {
		total += time.Duration(float64(bytes) / float64(l.BytesPerSecond) * float64(time.Second))
	}
	if total <= 0 {
		return context.Cause(ctx)
	}
	timer := time.NewTimer(total)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

// diskObjectStore keeps object bodies in a directory and their metadata in
// memory, under the same latency model as the simulated store. A workload
// benchmark publishes gigabytes of checkpoints, and a host that also runs the
// guests cannot hold them in the test process's heap.
type diskObjectStore struct {
	dir     string
	latency objectLatency

	mu      sync.Mutex
	objects map[string]diskObject
	next    uint64
}

type diskObject struct {
	name     string
	size     int64
	etag     platform.ETag
	modified time.Time
	attrs    map[string]string
}

func newDiskObjectStore(dir string, latency objectLatency) (*diskObjectStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &diskObjectStore{dir: dir, latency: latency, objects: make(map[string]diskObject)}, nil
}

func (s *diskObjectStore) metadata(key platform.ObjectKey, object diskObject) platform.ObjectMetadata {
	return platform.ObjectMetadata{Key: key, Size: object.size, LastModified: object.modified,
		ETag: object.etag, Attributes: platform.CloneAttributes(object.attrs)}
}

func (s *diskObjectStore) Head(ctx context.Context, key platform.ObjectKey) (platform.ObjectMetadata, error) {
	if key.IsZero() {
		return platform.ObjectMetadata{}, platform.ErrInvalidObjectKey
	}
	if err := s.latency.charge(ctx, s.latency.Head, 0); err != nil {
		return platform.ObjectMetadata{}, err
	}
	s.mu.Lock()
	object, exists := s.objects[key.String()]
	s.mu.Unlock()
	if !exists {
		return platform.ObjectMetadata{}, platform.ErrNotFound
	}
	return s.metadata(key, object), nil
}

func (s *diskObjectStore) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	if request.Key.IsZero() {
		return platform.GetResult{}, platform.ErrInvalidObjectKey
	}
	if request.Range != nil {
		if err := request.Range.Validate(); err != nil {
			return platform.GetResult{}, err
		}
	}
	s.mu.Lock()
	object, exists := s.objects[request.Key.String()]
	// Pin this version before a concurrent Put or Delete can unlink it.
	// An open file remains readable after unlink; a remembered path does not.
	var file *os.File
	var err error
	if exists {
		file, err = os.Open(filepath.Join(s.dir, object.name))
	}
	s.mu.Unlock()
	if !exists {
		if err := s.latency.charge(ctx, s.latency.Get, 0); err != nil {
			return platform.GetResult{}, err
		}
		return platform.GetResult{}, platform.ErrNotFound
	}
	if err != nil {
		return platform.GetResult{}, err
	}
	defer file.Close()
	offset, length := int64(0), object.size
	switch {
	case request.Range == nil:
	case request.Range.Suffix > 0:
		// A suffix longer than the object is the whole object.
		offset = max(object.size-request.Range.Suffix, 0)
		length = object.size - offset
	case request.Range.Offset >= object.size:
		return platform.GetResult{}, platform.ErrInvalidRange
	default:
		offset = request.Range.Offset
		length = min(request.Range.Length, object.size-offset)
	}
	if err := s.latency.charge(ctx, s.latency.Get, length); err != nil {
		return platform.GetResult{}, err
	}
	data := make([]byte, length)
	_, err = io.ReadFull(io.NewSectionReader(file, offset, length), data)
	if err != nil {
		return platform.GetResult{}, err
	}
	return platform.GetResult{Metadata: s.metadata(request.Key, object), ContentLength: length,
		Body: io.NopCloser(bytes.NewReader(data))}, nil
}

func (s *diskObjectStore) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	if err := request.Validate(); err != nil {
		return platform.PutResult{}, err
	}
	if err := s.latency.charge(ctx, s.latency.Put, request.Size); err != nil {
		return platform.PutResult{}, err
	}
	data := make([]byte, request.Size)
	if _, err := io.ReadFull(io.NewSectionReader(request.Body, 0, request.Size), data); err != nil {
		return platform.PutResult{}, fmt.Errorf("read upload body: %w", err)
	}
	s.mu.Lock()
	current, exists := s.objects[request.Key.String()]
	if request.Conditions.IfNoneMatch && exists {
		s.mu.Unlock()
		return platform.PutResult{}, platform.ErrPrecondition
	}
	if request.Conditions.IfMatch != nil && (!exists || current.etag != *request.Conditions.IfMatch) {
		s.mu.Unlock()
		return platform.PutResult{}, platform.ErrPrecondition
	}
	s.next++
	name := strconv.FormatUint(s.next, 36)
	s.mu.Unlock()

	path := filepath.Join(s.dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return platform.PutResult{}, err
	}
	object := diskObject{name: name, size: request.Size, etag: etagOf(data), modified: time.Now(),
		attrs: platform.CloneAttributes(request.Attributes)}

	s.mu.Lock()
	current, exists = s.objects[request.Key.String()]
	if (request.Conditions.IfNoneMatch && exists) ||
		(request.Conditions.IfMatch != nil && (!exists || current.etag != *request.Conditions.IfMatch)) {
		s.mu.Unlock()
		_ = os.Remove(path)
		return platform.PutResult{}, platform.ErrPrecondition
	}
	s.objects[request.Key.String()] = object
	s.mu.Unlock()
	if exists {
		_ = os.Remove(filepath.Join(s.dir, current.name))
	}
	return platform.PutResult{Metadata: s.metadata(request.Key, object)}, nil
}

func (s *diskObjectStore) Delete(ctx context.Context, request platform.DeleteRequest) error {
	if request.Key.IsZero() {
		return platform.ErrInvalidObjectKey
	}
	if err := s.latency.charge(ctx, s.latency.Delete, 0); err != nil {
		return err
	}
	s.mu.Lock()
	current, exists := s.objects[request.Key.String()]
	if request.IfMatch != nil && (!exists || current.etag != *request.IfMatch) {
		s.mu.Unlock()
		return platform.ErrPrecondition
	}
	delete(s.objects, request.Key.String())
	s.mu.Unlock()
	if exists {
		_ = os.Remove(filepath.Join(s.dir, current.name))
	}
	return nil
}

func (s *diskObjectStore) List(ctx context.Context, request platform.ListRequest) (platform.ListResult, error) {
	if err := request.Validate(); err != nil {
		return platform.ListResult{}, err
	}
	if err := s.latency.charge(ctx, s.latency.List, 0); err != nil {
		return platform.ListResult{}, err
	}
	prefix := request.Prefix.String()
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.objects))
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) && key > request.ContinuationToken {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	limit := len(keys)
	if request.Limit > 0 && int(request.Limit) < limit {
		limit = int(request.Limit)
	}
	objects := make([]platform.ObjectMetadata, 0, limit)
	for _, value := range keys[:limit] {
		key, err := platform.NewObjectKey(value)
		if err != nil {
			return platform.ListResult{}, err
		}
		objects = append(objects, s.metadata(key, s.objects[value]))
	}
	result := platform.ListResult{Objects: objects}
	if limit < len(keys) {
		result.NextContinuationToken = keys[limit-1]
	}
	return result, nil
}

// etagOf is a content validator; the store's callers may only compare it.
func etagOf(value []byte) platform.ETag {
	sum := sha256.Sum256(value)
	return platform.ETag(`"` + hex.EncodeToString(sum[:]) + `"`)
}

// countMappings is how many mappings one process's address space holds. Both a
// mapping replacement and a range write-protect are kernel walks over the
// mappings their range covers, so this is the size of the thing those walks
// scale with. It reports zero rather than failing: it is evidence about a
// measurement, never part of one.
func countMappings(pid int) int {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		slog.Warn("mapping count unavailable", "pid", pid, "error", err)
		return 0
	}
	return bytes.Count(raw, []byte("\n"))
}

// ---------------------------------------------------------------------------
// Console
// ---------------------------------------------------------------------------

// consoleGuest is what both a managed machine and the plain-Firecracker
// baseline expose to a scenario: console output read by offset, and a console
// to write.
type consoleGuest interface {
	Console(offset int64, limit int) (data []byte, from, next int64)
	WriteConsole(ctx context.Context, data []byte) error
	Wait(ctx context.Context) error
}

// guestTiming is one `run` command's own accounting, taken from wait4's rusage
// inside the guest, so it excludes everything the host spent starting the VM.
type guestTiming struct {
	Exit   int    `json:"exit"`
	WallNS uint64 `json:"wall_ns"`
	UserNS uint64 `json:"user_ns"`
	SysNS  uint64 `json:"sys_ns"`
}

// console follows one guest's output incrementally. A build in a guest writes
// megabytes to its console, and twenty of them are followed at once, so the
// reader keeps its position and reads only what is new: re-reading each log in
// full would charge the measurement for the harness's own I/O.
type console struct {
	guest   consoleGuest
	offset  int64
	pending []byte
}

func newConsole(guest consoleGuest) *console { return &console{guest: guest} }

// skipExisting drops whatever the console already holds, which is what a
// restored guest needs: its VMM's own startup output is not this scenario's.
func (c *console) skipExisting() error {
	_, _, next := c.guest.Console(math.MaxInt64, 0)
	c.offset, c.pending = next, nil
	return nil
}

// pull reads everything the guest has written since the last read, with the
// VMM's own log taken back out of it.
func (c *console) pull() error {
	for {
		data, from, next := c.guest.Console(c.offset, 64<<10)
		if from > c.offset {
			// The guest's ring dropped output this reader had not read yet, so
			// what it holds cannot be continued.
			c.pending = nil
		}
		c.offset = next
		if len(data) == 0 {
			c.pending = withoutVMMLog(c.pending)
			return nil
		}
		c.pending = append(c.pending, data...)
	}
}

// vmmLogLine is one line the VMM writes about itself. The ring is one stream
// for two writers — the guest's serial console and the VMM's own log both go to
// the process's output — so a line of this kind lands wherever the VMM happened
// to write it, which is as often as not in the middle of a line the guest was
// still writing. A capture running while a guest answers is exactly when that
// happens, and it makes the answer unreadable to anything looking for a
// complete line of the guest's.
var vmmLogLine = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T[\d:.]+ \[[^\n\]]*:[^\n\]]*\][^\n]*\n`)

// withoutVMMLog takes those lines back out, which rejoins the guest's own line
// around whatever was written through it. A line the VMM has not finished
// writing has no newline yet and is left for the next read to complete.
func withoutVMMLog(pending []byte) []byte {
	if !bytes.Contains(pending, []byte("-instance:")) {
		return pending
	}
	return vmmLogLine.ReplaceAll(pending, nil)
}

func (c *console) close() {}

// wait blocks until a complete line whose fields begin with want appears, and
// returns that line, consuming everything up to and including it.
func (c *console) wait(ctx context.Context, want string) (string, error) {
	line, _, err := c.waitOutput(ctx, want)
	return line, err
}

// waitOutput is wait that also returns everything the guest printed before the
// matching line, which is a command's own output.
func (c *console) waitOutput(ctx context.Context, want string) (string, string, error) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	exited := make(chan error, 1)
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { exited <- c.guest.Wait(waitCtx) }()
	for {
		if err := c.pull(); err != nil {
			return "", "", err
		}
		if line, end, ok := findGuestStatus(string(c.pending), want); ok {
			output := string(c.pending[:end-len(line)-1])
			c.pending = c.pending[end:]
			return line, output, nil
		}
		if index := bytes.Index(c.pending, []byte("SPROUTFS_ERROR")); index >= 0 {
			return "", "", fmt.Errorf("guest failed: %s", c.pending[index:min(index+200, len(c.pending))])
		}
		select {
		case err := <-exited:
			return "", "", fmt.Errorf("VMM exited while waiting for %q: %w\n%s", want, err, c.tail())
		case <-ctx.Done():
			return "", "", fmt.Errorf("waiting for %q: %w\n%s", want, context.Cause(ctx), c.tail())
		case <-ticker.C:
		}
	}
}

func (c *console) tail() string {
	if len(c.pending) > 4096 {
		return string(c.pending[len(c.pending)-4096:])
	}
	return string(c.pending)
}

func (c *console) send(ctx context.Context, line string) error {
	return c.guest.WriteConsole(ctx, []byte(line+"\n"))
}

// run executes one shell command in the guest and reports what the guest itself
// measured. first, when non-nil, receives the moment any output of the command
// appeared, which is what a fork's time to first output means.
func (c *console) run(ctx context.Context, command string) (guestTiming, error) {
	if err := c.send(ctx, "run "+command); err != nil {
		return guestTiming{}, err
	}
	line, err := c.wait(ctx, "SPROUTFS_RUN")
	if err != nil {
		return guestTiming{}, err
	}
	return parseGuestTiming(line)
}

// runObserved is run, calling first once the command has written anything at
// all, which is when a fork has shown it is alive rather than when it is done.
func (c *console) runObserved(ctx context.Context, command string, first func()) (guestTiming, error) {
	if err := c.send(ctx, "run "+command); err != nil {
		return guestTiming{}, err
	}
	for {
		grown, err := c.grown()
		if err != nil {
			return guestTiming{}, err
		}
		if grown > 0 {
			break
		}
		select {
		case <-ctx.Done():
			return guestTiming{}, context.Cause(ctx)
		case <-time.After(5 * time.Millisecond):
		}
	}
	first()
	line, err := c.wait(ctx, "SPROUTFS_RUN")
	if err != nil {
		return guestTiming{}, err
	}
	return parseGuestTiming(line)
}

// capture runs one shell command in the guest and returns what it printed.
func (c *console) capture(ctx context.Context, command string) (string, error) {
	if err := c.send(ctx, "run "+command); err != nil {
		return "", err
	}
	_, output, err := c.waitOutput(ctx, "SPROUTFS_RUN")
	return output, err
}

// benchGuestVCPUs is SPROUTFS_BENCH_VCPUS when it names a positive count, and
// benchVCPUs otherwise, for both guests. A diagnostic run boots with one vCPU to
// take contention between vCPUs out of what it measures.
func benchGuestVCPUs() int {
	if value, err := strconv.Atoi(os.Getenv("SPROUTFS_BENCH_VCPUS")); err == nil && value > 0 {
		return value
	}
	return benchVCPUs
}

// benchWriteAheadPages is the PMEM pager's write-ahead run:
// SPROUTFS_BENCH_WRITE_AHEAD_PAGES when it names a positive number of that
// pager's pages, which is how a run measures another write-ahead bound, and
// zero, one page, otherwise. The RAM pager keeps one page whatever this says —
// at 4 KiB the unit of ownership is the whole point, and a run that made a
// store's neighbours privately dirty before the guest had used them would give
// back exactly the sharing the small page buys.
func benchWriteAheadPages() int {
	if value, err := strconv.Atoi(os.Getenv("SPROUTFS_BENCH_WRITE_AHEAD_PAGES")); err == nil && value > 0 {
		return value
	}
	return 0
}

// parseGuestReady takes the two monotonic readings the guest's readiness line
// carries: the one at which its init started, which is the kernel's whole boot
// on the guest's own clock, and the one at which it became ready.
func parseGuestReady(line string) (entry, ready uint64, err error) {
	for _, field := range strings.Fields(line) {
		name, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch name {
		case "entry_ns":
			entry, err = strconv.ParseUint(value, 10, 64)
		case "ready_ns":
			ready, err = strconv.ParseUint(value, 10, 64)
		default:
			continue
		}
		if err != nil {
			return 0, 0, fmt.Errorf("unparsable readiness field %q: %w", field, err)
		}
	}
	if entry == 0 || ready < entry {
		return 0, 0, fmt.Errorf("readiness line carries no usable monotonic readings: %q", line)
	}
	return entry, ready, nil
}

func parseGuestTiming(line string) (guestTiming, error) {
	var timing guestTiming
	count, err := fmt.Sscanf(strings.TrimSpace(line), "SPROUTFS_RUN exit=%d wall_ns=%d user_ns=%d sys_ns=%d",
		&timing.Exit, &timing.WallNS, &timing.UserNS, &timing.SysNS)
	if err != nil || count != 4 {
		return guestTiming{}, fmt.Errorf("unparsable run status %q: %w", line, err)
	}
	return timing, nil
}

// findGuestStatus is containsGuestStatus with the position of the match, so a
// caller can advance past exactly the line it consumed.
func findGuestStatus(output, want string) (string, int, bool) {
	wanted := strings.Fields(want)
	consumed := 0
	for {
		line, rest, complete := strings.Cut(output, "\n")
		if !complete {
			return "", 0, false
		}
		consumed += len(line) + 1
		fields := strings.Fields(line)
		if len(fields) >= len(wanted) && slices.Equal(fields[:len(wanted)], wanted) {
			return line, consumed, true
		}
		output = rest
	}
}

// grown reports how much output the guest has produced that the reader has not
// consumed, which is how a scenario observes first output without taking it.
func (c *console) grown() (int, error) {
	if err := c.pull(); err != nil {
		return 0, err
	}
	return len(c.pending), nil
}

// discard drops everything unconsumed, so the next output is the next command's.
func (c *console) discard() error {
	if err := c.pull(); err != nil {
		return err
	}
	c.pending = nil
	return nil
}

// ---------------------------------------------------------------------------
// Plain Firecracker baseline
// ---------------------------------------------------------------------------

// plainConfig is a VM with no managed memory: anonymous guest RAM and a
// virtio-block root, which is what Firecracker does without this integration.
type plainConfig struct {
	Binary, Seccomp, Kernel, BootArgs string
	RootPath                          string
	Directory                         string
	MemoryMiB, VCPUs                  int
	// HugePages is Firecracker's huge_pages for guest memory: empty or "None"
	// for ordinary 4 KiB pages, "Transparent" to advise the host's THP, "2M" for
	// the HugeTLB pool. It is what separates the size of the guest's
	// translations from everything a pager does.
	HugePages string
	// SnapshotPath and MemoryPath restore an ordinary memory-file snapshot
	// instead of booting.
	SnapshotPath, MemoryPath string
	// CloneRoot gives a restored VM a root of its own. A snapshot names its
	// drive by the path it had, so several VMs restored from one would write
	// the same file; a clone is loaded paused, given this copy, and resumed.
	CloneRoot string
}

type plainVM struct {
	dir     string
	cmd     *exec.Cmd
	client  *http.Client
	stdin   *os.File
	log     *os.File
	done    chan struct{}
	exitErr error
	closed  bool
}

func startPlainVM(ctx context.Context, c plainConfig) (*plainVM, error) {
	dir, err := os.MkdirTemp(c.Directory, "sproutfs-plain-")
	if err != nil {
		return nil, err
	}
	p := &plainVM{dir: dir, done: make(chan struct{})}
	started := false
	defer func() {
		if !started {
			_ = p.Close()
		}
	}()
	api := filepath.Join(dir, "api.sock")
	// The compiled policy is the musl one, and this build is gnu: an ordinary
	// memory-file snapshot dies on a syscall it does not permit. The baseline
	// therefore runs without a filter, which can only make it faster than the
	// managed side it is compared against.
	args := []string{"--api-sock", api, "--no-seccomp"}
	if c.Seccomp != "" {
		args = []string{"--api-sock", api, "--seccomp-filter", c.Seccomp}
	}
	if c.SnapshotPath == "" {
		config := map[string]any{
			"machine-config": plainMachineConfig(c),
			"boot-source":    map[string]any{"kernel_image_path": c.Kernel, "boot_args": c.BootArgs},
			"drives": []any{map[string]any{"drive_id": "rootfs", "path_on_host": c.RootPath,
				"is_root_device": true, "is_read_only": false}},
		}
		raw, err := json.Marshal(config)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			return nil, err
		}
		args = append(args, "--config-file", path)
	}
	p.log, err = os.OpenFile(filepath.Join(dir, "console.log"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	input, output, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	p.stdin = output
	p.cmd = exec.Command(c.Binary, args...)
	p.cmd.Stdin = input
	p.cmd.Stdout = p.log
	p.cmd.Stderr = p.log
	if err := p.cmd.Start(); err != nil {
		_ = input.Close()
		return nil, err
	}
	_ = input.Close()
	go func() {
		p.exitErr = p.cmd.Wait()
		close(p.done)
	}()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", api)
	}, MaxConnsPerHost: 1}
	p.client = &http.Client{Transport: transport, Timeout: 5 * time.Minute}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(api); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		case <-p.done:
			return nil, fmt.Errorf("plain VMM exited during startup: %w\n%s", p.exitErr, p.consoleTail())
		case <-ticker.C:
		}
	}
	if c.SnapshotPath != "" {
		if err := p.request(ctx, http.MethodPut, "/snapshot/load", map[string]any{
			"snapshot_path": c.SnapshotPath,
			"mem_backend":   map[string]any{"backend_type": "File", "backend_path": c.MemoryPath},
			"resume_vm":     c.CloneRoot == "",
		}); err != nil {
			return nil, err
		}
		if c.CloneRoot != "" {
			if err := p.request(ctx, http.MethodPatch, "/drives/rootfs", map[string]any{
				"drive_id": "rootfs", "path_on_host": c.CloneRoot}); err != nil {
				return nil, err
			}
			if err := p.request(ctx, http.MethodPatch, "/vm", map[string]any{"state": "Resumed"}); err != nil {
				return nil, err
			}
		}
	}
	started = true
	return p, nil
}

func (p *plainVM) request(ctx context.Context, method, path string, value any) error {
	var body io.Reader
	if value != nil {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	if response.StatusCode >= 300 {
		return fmt.Errorf("plain VMM %s %s: %s: %s", method, path, response.Status, raw)
	}
	return nil
}

// memoryRollup reads what the kernel accounts to this VMM's address space, in
// bytes, from smaps_rollup: what it holds resident, its proportional share of
// that once pages other processes map are divided among them, and what is its
// alone. Guest memory is the whole of a VMM's footprint but for a few MiB, so
// this is what one plain clone costs the host.
func (p *plainVM) memoryRollup() (map[string]int64, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", p.cmd.Process.Pid))
	if err != nil {
		return nil, err
	}
	rollup := map[string]int64{}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[2] != "kB" {
			continue
		}
		kib, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return nil, err
		}
		switch name := strings.TrimSuffix(fields[0], ":"); name {
		case "Rss", "Pss", "Shared_Clean", "Shared_Dirty", "Private_Clean", "Private_Dirty":
			rollup[strings.ToLower(name)+"_bytes"] = kib << 10
		}
	}
	return rollup, nil
}

// ConsolePath is the baseline's own console file. The plain VMM writes its
// whole output there, so nothing is ever dropped and a read is a plain ReadAt.
func (p *plainVM) ConsolePath() string { return filepath.Join(p.dir, "console.log") }

func (p *plainVM) Console(offset int64, limit int) ([]byte, int64, int64) {
	file, err := os.Open(p.ConsolePath())
	if err != nil {
		return nil, offset, offset
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, offset, offset
	}
	// An offset past the end is how a reader asks where the end is, which is
	// what skipping a restored guest's existing output does: it reads from there,
	// so the answer is the end and not the offset it asked with.
	offset = min(offset, info.Size())
	length := min(info.Size()-offset, int64(limit))
	data := make([]byte, length)
	if length != 0 {
		if _, err := file.ReadAt(data, offset); err != nil && err != io.EOF {
			return nil, offset, offset
		}
	}
	return data, offset, offset + length
}

func (p *plainVM) WriteConsole(ctx context.Context, data []byte) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	_, err := p.stdin.Write(data)
	return err
}

func (p *plainVM) Wait(ctx context.Context) error {
	select {
	case <-p.done:
		return fmt.Errorf("plain VMM exited: %w", p.exitErr)
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// Snapshot pauses the VM and writes an ordinary memory-file snapshot, which is
// the baseline restore path.
func (p *plainVM) Snapshot(ctx context.Context, statePath, memoryPath string) error {
	if err := p.request(ctx, http.MethodPatch, "/vm", map[string]any{"state": "Paused"}); err != nil {
		return err
	}
	return p.request(ctx, http.MethodPut, "/snapshot/create", map[string]any{
		"snapshot_type": "Full", "snapshot_path": statePath, "mem_file_path": memoryPath})
}

func (p *plainVM) consoleTail() string {
	raw, err := os.ReadFile(p.ConsolePath())
	if err != nil {
		return ""
	}
	if len(raw) > 4096 {
		raw = raw[len(raw)-4096:]
	}
	return string(raw)
}

func (p *plainVM) Close() error {
	if p.closed {
		return nil
	}
	p.closed = true
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		<-p.done
	}
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	if p.log != nil {
		_ = p.log.Close()
	}
	return os.RemoveAll(p.dir)
}

// droppingGuest is a console whose oldest output falls away, which is what the
// managed machine's 1 MiB ring does to a reader that has fallen behind.
type droppingGuest struct {
	data  []byte
	start int64
}

func (g *droppingGuest) Console(offset int64, limit int) ([]byte, int64, int64) {
	end := g.start + int64(len(g.data))
	from := min(max(offset, g.start), end)
	length := min(end-from, int64(limit))
	return g.data[from-g.start : from-g.start+length], from, from + length
}

func (g *droppingGuest) WriteConsole(context.Context, []byte) error { return nil }
func (g *droppingGuest) Wait(ctx context.Context) error             { <-ctx.Done(); return context.Cause(ctx) }

func TestConsoleReaderRestartsWhereDroppedOutputEnds(t *testing.T) {
	guest := &droppingGuest{data: []byte("old output with an incomplete line")}
	reader := newConsole(guest)
	if err := reader.pull(); err != nil {
		t.Fatal(err)
	}
	if string(reader.pending) != "old output with an incomplete line" {
		t.Fatalf("first read = %q", reader.pending)
	}
	// Everything the reader has seen falls out of the ring behind it.
	guest.start, guest.data = 64, []byte("new\n")
	if err := reader.pull(); err != nil {
		t.Fatal(err)
	}
	if string(reader.pending) != "new\n" || reader.offset != 68 {
		t.Fatalf("reader kept an obsolete position: %d %q", reader.offset, reader.pending)
	}
}

func TestConsoleReaderSkipsWhatAGuestAlreadyPrinted(t *testing.T) {
	guest := &droppingGuest{data: []byte("startup output\n")}
	reader := newConsole(guest)
	if err := reader.skipExisting(); err != nil {
		t.Fatal(err)
	}
	guest.data = append(guest.data, "scenario\n"...)
	if err := reader.pull(); err != nil {
		t.Fatal(err)
	}
	if string(reader.pending) != "scenario\n" {
		t.Fatalf("reader read %q; want only what was printed after skipExisting", reader.pending)
	}
}

// TestConsoleReaderRejoinsALineTheVMMWroteThrough. The ring is one stream for
// two writers, and a capture running while a guest answers puts a line of the
// VMM's own log through the middle of the guest's. This is the exact shape a
// fan-out run left behind: the answer split mid-word, and neither half a line
// anything looking for a status could match.
func TestConsoleReaderRejoinsALineTheVMMWroteThrough(t *testing.T) {
	guest := &droppingGuest{data: []byte("SPROUTFS_PRESSURE_OK byt" +
		"2026-09-17T00:15:29.339676311 [anonymous-instance:fc_api] The API server received a Patch request.\n" +
		"es=67108864\n")}
	reader := newConsole(guest)
	if err := reader.pull(); err != nil {
		t.Fatal(err)
	}
	if string(reader.pending) != "SPROUTFS_PRESSURE_OK bytes=67108864\n" {
		t.Fatalf("the reader kept %q, want the guest's own line rejoined", reader.pending)
	}
	if _, _, ok := findGuestStatus(string(reader.pending), "SPROUTFS_PRESSURE_OK bytes=67108864"); !ok {
		t.Fatal("the rejoined line is still not a status this reader can match")
	}
}

// TestConsoleReaderWaitsForALineTheVMMHasNotFinished: a log line with no
// newline yet is the one that cannot be taken out, because what follows it may
// still be part of it. It is left for the read that completes it, and the
// guest's own complete lines around it are readable meanwhile.
func TestConsoleReaderWaitsForALineTheVMMHasNotFinished(t *testing.T) {
	guest := &droppingGuest{data: []byte(
		"SPROUTFS_VALUE ram=1 disk=2\n2026-09-17T00:15:29.3 [anonymous-instance:main] half")}
	reader := newConsole(guest)
	if err := reader.pull(); err != nil {
		t.Fatal(err)
	}
	if line, _, ok := findGuestStatus(string(reader.pending), "SPROUTFS_VALUE ram=1 disk=2"); !ok ||
		line != "SPROUTFS_VALUE ram=1 disk=2" {
		t.Fatalf("a complete guest line before an unfinished VMM one was lost: %q", reader.pending)
	}
	guest.data = append(guest.data, " of a line\nSPROUTFS_SYNC\n"...)
	if err := reader.pull(); err != nil {
		t.Fatal(err)
	}
	if string(reader.pending) != "SPROUTFS_VALUE ram=1 disk=2\nSPROUTFS_SYNC\n" {
		t.Fatalf("the completed VMM line was not taken out: %q", reader.pending)
	}
}

// plainMachineConfig is a plain VM's machine-config.
func plainMachineConfig(c plainConfig) map[string]any {
	config := map[string]any{"mem_size_mib": c.MemoryMiB, "vcpu_count": c.VCPUs}
	if c.HugePages != "" {
		config["huge_pages"] = c.HugePages
	}
	return config
}
