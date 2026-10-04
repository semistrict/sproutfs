package host

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
)

// tickingStore takes a fixed time on its clock for every call of each
// operation, and answers every call with success.
type tickingStore struct {
	clock *sim.Clock
	takes map[platform.ObjectOperation]time.Duration
}

func (s tickingStore) Head(context.Context, platform.ObjectKey) (platform.ObjectMetadata, error) {
	s.clock.Advance(s.takes[platform.HeadOperation])
	return platform.ObjectMetadata{}, nil
}

func (s tickingStore) Get(_ context.Context, request platform.GetRequest) (platform.GetResult, error) {
	s.clock.Advance(s.takes[platform.GetOperation])
	return platform.GetResult{Metadata: platform.ObjectMetadata{Key: request.Key},
		Body: io.NopCloser(bytes.NewReader(nil))}, nil
}

func (s tickingStore) Put(context.Context, platform.PutRequest) (platform.PutResult, error) {
	s.clock.Advance(s.takes[platform.PutOperation])
	return platform.PutResult{}, nil
}

func (s tickingStore) Delete(context.Context, platform.DeleteRequest) error { return nil }

func (s tickingStore) List(context.Context, platform.ListRequest) (platform.ListResult, error) {
	return platform.ListResult{}, nil
}

// loggedLine is one of the timeline's log lines as the JSON handler writes it.
type loggedLine struct {
	Msg        string         `json:"msg"`
	VM         string         `json:"vm"`
	How        string         `json:"how"`
	Resumed    bool           `json:"resumed"`
	TotalMS    float64        `json:"total_ms"`
	RunningMS  float64        `json:"running_ms"`
	StoreMS    float64        `json:"store_ms"`
	WindowMS   float64        `json:"window_ms"`
	Steps      []loggedStep   `json:"steps"`
	Store      []loggedStore  `json:"store"`
	StoreAfter []loggedStore  `json:"store_after"`
	Attach     []loggedAttach `json:"attach"`
	Faults     []loggedFaults `json:"faults"`
}

func readLines(t *testing.T, out *bytes.Buffer) []loggedLine {
	t.Helper()
	var lines []loggedLine
	scanner := bufio.NewScanner(bytes.NewReader(out.Bytes()))
	for scanner.Scan() {
		var line loggedLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("a log line is not JSON: %v: %s", err, scanner.Text())
		}
		lines = append(lines, line)
	}
	return lines
}

func objectKey(t *testing.T, value string) platform.ObjectKey {
	t.Helper()
	key, err := platform.NewObjectKey(value)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// A start's line carries each step from the request's arrival, the store's
// calls before the guest ran by kind, their union, and the calls after it
// apart.
func TestAStartsLineSplitsItsTimeAndItsStoreCalls(t *testing.T) {
	runtime := sim.New(sim.Config{Seed: 1})
	clock := runtime.NewClock("host")
	store, err := platform.NewMeteredObjectStore(tickingStore{clock: clock, takes: map[platform.ObjectOperation]time.Duration{
		platform.PutOperation: 20 * time.Millisecond, platform.GetOperation: 5 * time.Millisecond}}, clock)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	ctx, line := startTimeline(sim.WithRuntime(t.Context(), runtime), clock, "vm-a", "create")
	line.logger = slog.New(slog.NewJSONHandler(&out, nil))
	record := objectKey(t, "control/vm-a")

	ended := step(ctx, "fork")
	if _, err := store.Put(ctx, platform.PutRequest{Key: record, Body: bytes.NewReader(nil),
		Conditions: platform.PutConditions{IfNoneMatch: true}}); err != nil {
		t.Fatal(err)
	}
	ended()
	ended = step(ctx, "root")
	result, err := store.Get(ctx, platform.GetRequest{Key: objectKey(t, "vm/template-a/ckpt/3/index")})
	if err != nil {
		t.Fatal(err)
	}
	result.Body.Close()
	etag := platform.ETag("1")
	if _, err := store.Put(ctx, platform.PutRequest{Key: record, Body: bytes.NewReader(nil),
		Conditions: platform.PutConditions{IfMatch: &etag}}); err != nil {
		t.Fatal(err)
	}
	ended()
	line.add("vmm process", clock.Now(), 7*time.Millisecond)
	line.attached("ram0", 4*time.Millisecond, vmmemory.PopulateStats{Commands: 2, Runs: 2, Pages: 300,
		DurationNS: int64(3 * time.Millisecond)})
	clock.Advance(7 * time.Millisecond)
	ran(ctx, nil)
	// A read the running guest's fault makes is the store's, but not on the
	// way to the guest running.
	result, err = store.Get(ctx, platform.GetRequest{Key: objectKey(t, "vm/template-a/ckpt/3/part/0")})
	if err != nil {
		t.Fatal(err)
	}
	result.Body.Close()
	clock.Advance(2 * time.Millisecond)
	line.log(ctx, "host: a VM runs", "resumed", false)

	want := loggedLine{Msg: "host: a VM runs", VM: "vm-a", How: "create", TotalMS: 59, RunningMS: 52, StoreMS: 45,
		Steps: []loggedStep{{Name: "fork", AtMS: 0, TookMS: 20}, {Name: "root", AtMS: 20, TookMS: 25},
			{Name: "vmm process", AtMS: 45, TookMS: 7}},
		Store: []loggedStore{{Kind: "control record conditional put", Calls: 2, MS: 40, MaxMS: 20},
			{Kind: "index get", Calls: 1, MS: 5, MaxMS: 5}},
		StoreAfter: []loggedStore{{Kind: "part get", Calls: 1, MS: 5, MaxMS: 5}},
		Attach: []loggedAttach{{Region: "ram0", MS: 4, PopulateMS: 3, PopulatedPages: 300,
			PopulateCommands: 2}}}
	lines := readLines(t, &out)
	if len(lines) != 1 {
		t.Fatalf("the start logged %d lines, want 1: %s", len(lines), out.String())
	}
	got := lines[0]
	if got.Msg != want.Msg || got.VM != want.VM || got.How != want.How || got.TotalMS != want.TotalMS ||
		got.RunningMS != want.RunningMS || got.StoreMS != want.StoreMS ||
		!slices.Equal(got.Steps, want.Steps) || !slices.Equal(got.Store, want.Store) ||
		!slices.Equal(got.StoreAfter, want.StoreAfter) || !slices.Equal(got.Attach, want.Attach) {
		t.Fatalf("the start logged\n%+v\nwant\n%+v", got, want)
	}
}

// The first faults are logged a window after the guest runs, per memory
// region, with the faults before it ran apart from those after.
func TestAStartsFirstFaultsAreLoggedAWindowAfterItRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := sim.New(sim.Config{Seed: 1}).NewClock("host")
		var out bytes.Buffer
		ctx, line := startTimeline(t.Context(), clock, "vm-b", "receive")
		line.logger = slog.New(slog.NewJSONHandler(&out, nil))
		clock.Advance(30 * time.Millisecond)
		regions := map[string]*vmmemory.MemoryRegion{"root": {}, "ram0": {}}
		ran(ctx, regions)
		line.log(ctx, "host: a VM runs")
		clock.Advance(firstFaultsWindow - time.Millisecond)
		clock.Settle()
		synctest.Wait()
		if lines := readLines(t, &out); len(lines) != 1 {
			t.Fatalf("before its window ended the start logged %d lines, want 1", len(lines))
		}
		clock.Advance(time.Millisecond)
		clock.Settle()
		synctest.Wait()
		lines := readLines(t, &out)
		if len(lines) != 2 {
			t.Fatalf("the start logged %d lines, want 2: %s", len(lines), out.String())
		}
		got := lines[1]
		want := loggedLine{Msg: "host: a VM's first faults", VM: "vm-b", How: "receive", RunningMS: 30, WindowMS: 1000,
			Faults: []loggedFaults{{Region: "ram0"}, {Region: "root"}}}
		if got.Msg != want.Msg || got.VM != want.VM || got.How != want.How || got.RunningMS != want.RunningMS ||
			got.WindowMS != want.WindowMS || !slices.Equal(got.Faults, want.Faults) {
			t.Fatalf("the first faults line is\n%+v\nwant\n%+v", got, want)
		}
	})
}

// The store's share of a start is the time at least one call was in flight,
// so calls that overlap are not counted twice.
func TestTheStoresShareIsTheUnionOfItsCalls(t *testing.T) {
	start := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	call := func(at, took time.Duration) platform.ObjectCall {
		return platform.ObjectCall{Began: start.Add(at), Took: took}
	}
	calls := []platform.ObjectCall{call(20*time.Millisecond, 5*time.Millisecond),
		call(0, 10*time.Millisecond), call(5*time.Millisecond, 10*time.Millisecond),
		call(21*time.Millisecond, time.Millisecond)}
	if got := spanned(calls); got != 20*time.Millisecond {
		t.Fatalf("the calls spanned %v, want 20ms", got)
	}
	if got := spanned(nil); got != 0 {
		t.Fatalf("no calls spanned %v", got)
	}
}

// Without a timeline in its context a step, a run and a log record nothing.
func TestAStepWithoutATimelineRecordsNothing(t *testing.T) {
	ctx := t.Context()
	step(ctx, "anything")()
	ran(ctx, nil)
	if line := timelineOf(ctx); line != nil {
		t.Fatalf("a bare context carries a timeline: %+v", line)
	}
	timelineOf(ctx).log(ctx, "host: a VM runs")
}
