package sim

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	simv1 "github.com/semistrict/sproutfs/internal/platform/internal/gen/sproutfs/sim/v1"
	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/proto"
)

type scheduleTrace struct {
	mu     sync.Mutex
	seed   uint64
	start  time.Time
	events []*simv1.TraceEvent
}

func scheduleOutcome(err error) string {
	if err != nil {
		return err.Error()
	}
	return "ok"
}

func (trace *scheduleTrace) record(kind simv1.EventKind, id, outcome string, payload []byte) {
	trace.append(simv1.TraceEvent_builder{
		Kind: kind.Enum(), OperationId: proto.String(id), Outcome: proto.String(outcome), Payload: bytes.Clone(payload),
	}.Build())
}

func (trace *scheduleTrace) window(id string, earliest, latest time.Duration) {
	trace.append(simv1.TraceEvent_builder{
		Kind: simv1.EventKind_EVENT_KIND_SUBMITTED.Enum(), OperationId: proto.String(id),
		EarliestNs: proto.Int64(int64(earliest)), LatestNs: proto.Int64(int64(latest)),
	}.Build())
}

func (trace *scheduleTrace) append(event *simv1.TraceEvent) {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	event.SetSeed(trace.seed)
	event.SetSequence(uint64(len(trace.events) + 1))
	event.SetElapsedNs(int64(time.Since(trace.start)))
	trace.events = append(trace.events, event)
}

// Recording contains independent, length-delimited protobuf streams. Full keeps
// observed submission order. Execution removes only submission/arrival records
// and renumbers, preserving all other ordering and data. Adapters preserves the
// runtime's complete existing trace without filtering or sorting.
// Byte slices belong to the caller and can be compared directly across runs.
type Recording struct {
	Full, Execution, Adapters            []byte
	fullText, executionText, adapterText string
}

// Recording snapshots the scheduler and optional adapter trace after Run.
// It serializes in memory; WriteFiles belongs outside the synctest bubble.
func (s *Scheduler) Recording(adapters *Trace) (Recording, error) {
	s.trace.mu.Lock()
	full := append([]*simv1.TraceEvent(nil), s.trace.events...)
	s.trace.mu.Unlock()
	var execution []*simv1.TraceEvent
	for _, event := range full {
		if event.GetKind() == simv1.EventKind_EVENT_KIND_SUBMITTED || event.GetKind() == simv1.EventKind_EVENT_KIND_READY {
			continue
		}
		copy := proto.Clone(event).(*simv1.TraceEvent)
		copy.SetSequence(uint64(len(execution) + 1))
		execution = append(execution, copy)
	}
	var adapterEvents []*simv1.TraceEvent
	if adapters != nil {
		for _, event := range adapters.Events() {
			adapterEvents = append(adapterEvents, simv1.TraceEvent_builder{
				Seed: proto.Uint64(s.seed), Sequence: proto.Uint64(event.Sequence),
				ElapsedNs: proto.Int64(int64(event.At.Sub(s.trace.start))), Kind: simv1.EventKind_EVENT_KIND_ADAPTER.Enum(),
				OperationId: proto.String(fmt.Sprintf("%s/%s/%s/%d", event.Kind, event.Resource, event.Operation, event.LocalID)),
				Outcome:     proto.String(event.Outcome),
				Adapter: simv1.AdapterEvent_builder{
					Kind: proto.String(event.Kind), Resource: proto.String(event.Resource), Operation: proto.String(event.Operation),
					ByteCount: proto.Int64(int64(event.Bytes)), LocalId: proto.Uint64(event.LocalID),
				}.Build(),
			}.Build())
		}
	}
	var result Recording
	var err error
	if result.Full, result.fullText, err = encodeScheduleEvents(full); err != nil {
		return Recording{}, err
	}
	if result.Execution, result.executionText, err = encodeScheduleEvents(execution); err != nil {
		return Recording{}, err
	}
	if result.Adapters, result.adapterText, err = encodeScheduleEvents(adapterEvents); err != nil {
		return Recording{}, err
	}
	return result, nil
}

func encodeScheduleEvents(events []*simv1.TraceEvent) ([]byte, string, error) {
	var data bytes.Buffer
	var text strings.Builder
	for _, event := range events {
		if _, err := (protodelim.MarshalOptions{MarshalOptions: proto.MarshalOptions{Deterministic: true}}).MarshalTo(&data, event); err != nil {
			return nil, "", err
		}
		fmt.Fprintf(&text, "%02d %dns %s %s outcome=%q payload=%q window=[%d,%d]ns\n",
			event.GetSequence(), event.GetElapsedNs(), event.GetKind(), event.GetOperationId(),
			event.GetOutcome(), event.GetPayload(), event.GetEarliestNs(), event.GetLatestNs())
		if adapter := event.GetAdapter(); adapter != nil {
			fmt.Fprintf(&text, "  adapter=%s resource=%s operation=%s bytes=%d local_id=%d\n", adapter.GetKind(), adapter.GetResource(), adapter.GetOperation(), adapter.GetByteCount(), adapter.GetLocalId())
		}
	}
	return data.Bytes(), text.String(), nil
}

// WriteFiles writes all three protobuf streams and their readable companions.
// Call outside virtual time, using a fresh directory for each process run.
func (r Recording) WriteFiles(dir, prefix string) error {
	if prefix == "" || filepath.Base(prefix) != prefix || prefix == "." || prefix == ".." {
		return fmt.Errorf("invalid recording prefix")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for _, file := range []struct {
		suffix string
		data   []byte
	}{
		{".pb", r.Full}, {".execution.pb", r.Execution}, {".adapter.pb", r.Adapters},
		{".txt", []byte(r.fullText)}, {".execution.txt", []byte(r.executionText)}, {".adapter.txt", []byte(r.adapterText)},
	} {
		if err := os.WriteFile(filepath.Join(dir, prefix+file.suffix), file.data, 0600); err != nil {
			return err
		}
	}
	return nil
}

// WriteText writes one of the complete human-readable streams for diagnostics.
func (r Recording) WriteText(w io.Writer) error {
	_, err := fmt.Fprintf(w, "Execution:\n%s\nAdapters:\n%s", r.executionText, r.adapterText)
	return err
}
