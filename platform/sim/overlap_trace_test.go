package sim_test

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	simv1 "github.com/semistrict/sproutfs/platform/internal/gen/sproutfs/sim/v1"
	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/proto"
)

// Observes every prototype action lifecycle and network operation result,
// including cleanup. This is not an internal network or Go scheduler trace.
// Buffer inside the virtual-time bubble; write files only after it exits.
type overlapPrototypeTrace struct {
	mu     sync.Mutex
	seed   uint64
	start  time.Time
	events []*simv1.TraceEvent
}

func traceOutcome(err error) string {
	if err != nil {
		return err.Error()
	}
	return "ok"
}

func (trace *overlapPrototypeTrace) record(kind simv1.EventKind, id, outcome string, payload []byte) {
	if trace == nil {
		return
	}
	trace.append(simv1.TraceEvent_builder{
		Kind: kind.Enum(), OperationId: proto.String(id), Outcome: proto.String(outcome), Payload: bytes.Clone(payload),
	}.Build())
}

func (trace *overlapPrototypeTrace) recordWindow(action overlapPrototypeAction) {
	if trace == nil {
		return
	}
	trace.append(simv1.TraceEvent_builder{
		Kind: simv1.EventKind_EVENT_KIND_SUBMITTED.Enum(), OperationId: proto.String(action.id),
		EarliestNs: proto.Int64(int64(action.earliest)), LatestNs: proto.Int64(int64(action.latest)),
	}.Build())
}

func (trace *overlapPrototypeTrace) append(event *simv1.TraceEvent) {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	event.SetSeed(trace.seed)
	event.SetSequence(uint64(len(trace.events) + 1))
	event.SetElapsedNs(int64(time.Since(trace.start)))
	trace.events = append(trace.events, event)
}

// Keep observed execution order. Only arrival/submission events are removed;
// sequence numbers are reassigned because those removed events occupied slots.
// The full file remains available so nondeterministic arrivals stay visible.
func overlapExecutionEvents(events []*simv1.TraceEvent) []*simv1.TraceEvent {
	var execution []*simv1.TraceEvent
	for _, event := range events {
		if event.GetKind() == simv1.EventKind_EVENT_KIND_SUBMITTED || event.GetKind() == simv1.EventKind_EVENT_KIND_READY {
			continue
		}
		copy := proto.Clone(event).(*simv1.TraceEvent)
		copy.SetSequence(uint64(len(execution) + 1))
		execution = append(execution, copy)
	}
	return execution
}

func writeOverlapTrace(t *testing.T, path string, events []*simv1.TraceEvent) []byte {
	t.Helper()
	var data bytes.Buffer
	for _, event := range events {
		if _, err := (protodelim.MarshalOptions{MarshalOptions: proto.MarshalOptions{Deterministic: true}}).MarshalTo(&data, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func readOverlapTrace(t *testing.T, data []byte) []*simv1.TraceEvent {
	t.Helper()
	reader := bufio.NewReader(bytes.NewReader(data))
	var events []*simv1.TraceEvent
	for {
		event := new(simv1.TraceEvent)
		err := protodelim.UnmarshalFrom(reader, event)
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
}

func writeOverlapTraceText(t *testing.T, path string, events []*simv1.TraceEvent) {
	t.Helper()
	var text strings.Builder
	for _, event := range events {
		fmt.Fprintf(&text, "%02d %dns %s %s outcome=%q payload=%q window=[%d,%d]ns\n",
			event.GetSequence(), event.GetElapsedNs(), event.GetKind(), event.GetOperationId(),
			event.GetOutcome(), event.GetPayload(), event.GetEarliestNs(), event.GetLatestNs())
		if adapter := event.GetAdapter(); adapter != nil {
			fmt.Fprintf(&text, "  adapter=%s resource=%s operation=%s bytes=%d local_id=%d\n",
				adapter.GetKind(), adapter.GetResource(), adapter.GetOperation(), adapter.GetByteCount(), adapter.GetLocalId())
		}
	}
	if err := os.WriteFile(path, []byte(text.String()), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDynamicOverlapTraceFiles(t *testing.T) {
	dir := os.Getenv("SPROUTFS_OVERLAP_TRACE_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for seed := uint64(1); seed <= overlapTraceSeeds(t); seed++ {
		t.Run(fmt.Sprintf("seed-%02d", seed), func(t *testing.T) {
			var execution, adapters [2][]byte
			for order := range 2 {
				scheduler, runtime := runDynamicWorkload(t, seed, order == 1)
				recording, err := scheduler.Recording(runtime.Trace())
				if err != nil {
					t.Fatal(err)
				}
				if err := recording.WriteFiles(dir, fmt.Sprintf("seed-%02d-order-%d", seed, order)); err != nil {
					t.Fatal(err)
				}
				execution[order], adapters[order] = recording.Execution, recording.Adapters

			}
			if !bytes.Equal(execution[0], execution[1]) {
				t.Fatalf("execution differs across creation order; traces in %s", dir)
			}
			if !bytes.Equal(adapters[0], adapters[1]) {
				t.Fatalf("actual adapter events differ across creation order; traces in %s", dir)
			}
		})
	}
	t.Logf("dynamic protobuf traces: %s", dir)
}

// Set SPROUTFS_OVERLAP_TRACE_DIR to retain the binary, length-delimited protobuf
// files. Run once per process (-count=1), choosing a fresh directory each time.
// *.pb is the complete seam trace; *.execution.pb excludes submission/arrival.
func TestOverlapPrototypeTraceFiles(t *testing.T) {
	dir := os.Getenv("SPROUTFS_OVERLAP_TRACE_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for seed := uint64(1); seed <= overlapTraceSeeds(t); seed++ {
		var execution [2][]byte
		for order := range 2 {
			trace := &overlapPrototypeTrace{seed: seed}
			runOverlapListenerRace(t, seed, order == 1, trace)
			name := fmt.Sprintf("seed-%02d-order-%d", seed, order)
			data := writeOverlapTrace(t, filepath.Join(dir, name+".pb"), trace.events)
			decoded := readOverlapTrace(t, data)
			if len(decoded) != len(trace.events) || len(decoded) == 0 {
				t.Fatal("trace file lost events")
			}
			for i, event := range decoded {
				if !proto.Equal(event, trace.events[i]) {
					t.Fatalf("trace file changed event %d", i+1)
				}
			}
			writeOverlapTraceText(t, filepath.Join(dir, name+".txt"), decoded)
			execution[order] = writeOverlapTrace(t, filepath.Join(dir, name+".execution.pb"), overlapExecutionEvents(decoded))
		}
		if !bytes.Equal(execution[0], execution[1]) {
			t.Fatalf("seed %d execution trace changed with reversed registration; inspect %s", seed, dir)
		}
	}
	t.Logf("protobuf traces: %s", dir)
}

func overlapTraceSeeds(t *testing.T) uint64 {
	t.Helper()
	value := os.Getenv("SPROUTFS_OVERLAP_TRACE_SEEDS")
	if value == "" {
		return 32
	}
	count, err := strconv.ParseUint(value, 10, 32)
	if err != nil || count == 0 {
		t.Fatal("SPROUTFS_OVERLAP_TRACE_SEEDS must be positive")
	}
	return count
}
