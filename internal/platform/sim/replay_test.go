package sim_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
)

type replayResult struct {
	choice  uint64
	durable string
	header  string
	payload string
	events  []replayEvent
}

type replayEvent struct {
	after     time.Duration
	kind      string
	resource  string
	operation string
	outcome   string
	bytes     int
	localID   uint64
}

func TestSeededScenarioReplaysExactly(t *testing.T) {
	first := runReplayScenario(t, 872341)
	second := runReplayScenario(t, 872341)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same seed produced different scenarios:\nfirst:  %#v\nsecond: %#v", first, second)
	}

	different := runReplayScenario(t, 872342)
	if reflect.DeepEqual(first, different) {
		t.Fatal("different seeds produced identical scenario results")
	}
}

func runReplayScenario(t *testing.T, seed uint64) replayResult {
	t.Helper()
	var result replayResult
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{
			Seed: seed,
			Network: sim.NetworkConfig{
				Latency: 2 * time.Second,
				Jitter:  500 * time.Millisecond,
			},
		})
		random := runtime.Random("replay")
		result.choice = random.Uint64("payload")

		disk := runtime.NewDisk("machine-1", sim.DiskConfig{})
		durable := []byte(fmt.Sprintf("durable-%016x", result.choice))
		openSyncedThenDirty(t, disk, "state", durable, []byte("volatile"))
		if err := disk.PowerLoss(t.Context()); err != nil {
			t.Fatal(err)
		}
		file, err := disk.Open(t.Context(), "state", platform.OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		read := make([]byte, len(durable))
		if _, err := file.ReadAt(t.Context(), read, 0); err != nil {
			t.Fatal(err)
		}
		result.durable = string(read)
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}

		key, err := platform.NewObjectKey("replay/object")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.ObjectStore().Put(t.Context(), platform.PutRequest{
			Key: key, Body: bytes.NewReader(durable), Size: int64(len(durable)),
		}); err != nil {
			t.Fatal(err)
		}

		listener, client, server := connectedPair(t, runtime)
		defer listener.Close()
		defer client.Close()
		defer server.Close()
		extraDelay := random.Duration("network-delay", time.Second)
		runtime.Network().DelayNext("client", "server", extraDelay)
		runtime.Network().CorruptNext("client", "server", 1)
		sendStart := time.Now()
		if err := client.Send(t.Context(), platform.Frame{
			Header: []byte("control"), Payload: bytes.NewReader(durable), PayloadSize: int64(len(durable)),
		}); err != nil {
			t.Fatal(err)
		}
		receiveCtx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		frame, err := server.Receive(receiveCtx)
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(sendStart); elapsed < extraDelay {
			t.Fatalf("Send elapsed = %v, want at least injected delay %v", elapsed, extraDelay)
		}
		assertTraceEvent(t, runtime.Trace().Events(), "network", "send", "corrupted")
		body, readErr := io.ReadAll(frame.Payload)
		closeErr := frame.Payload.Close()
		if readErr != nil || closeErr != nil {
			t.Fatal(readErr, closeErr)
		}
		result.header = string(frame.Header)
		result.payload = string(body)
		result.events = normalizeReplayEvents(runtime.Trace().Events())
	})
	return result
}

func normalizeReplayEvents(events []sim.Event) []replayEvent {
	if len(events) == 0 {
		return nil
	}
	start := events[0].At
	result := make([]replayEvent, len(events))
	for index, event := range events {
		result[index] = replayEvent{
			after:     event.At.Sub(start),
			kind:      event.Kind,
			resource:  event.Resource,
			operation: event.Operation,
			outcome:   event.Outcome,
			bytes:     event.Bytes,
			localID:   event.LocalID,
		}
	}
	return result
}
