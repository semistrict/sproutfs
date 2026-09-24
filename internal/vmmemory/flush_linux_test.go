//go:build linux && (amd64 || arm64)

package vmmemory_test

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/vmwire"
)

// flushAnswer is how long a flush request or its answer may take to cross the
// session. It is a bound on a socket read and a goroutine handoff, far longer
// than either takes, so a failure is a frame that never arrives.
const flushAnswer = 10 * time.Second

// heldFlush is one flush request as the host's callback was handed it.
type heldFlush struct {
	region *vmmemory.Region
	done   func(error)
}

// holdFlushes installs a callback that answers nothing itself and hands every
// flush to the test, in the order the pager delivered them.
func holdFlushes(s pipeSession) <-chan heldFlush {
	held := make(chan heldFlush, 16)
	s.h.SetFlushed(func(r *vmmemory.Region, done func(error)) { held <- heldFlush{r, done} })
	return held
}

func sendFlush(t *testing.T, s pipeSession, id uint64) {
	t.Helper()
	if err := vmwire.Write(s.client, vmwire.Frame{Kind: vmwire.Flush, ID: id}); err != nil {
		t.Fatal(err)
	}
}

func nextHeld(t *testing.T, held <-chan heldFlush) heldFlush {
	t.Helper()
	select {
	case h := <-held:
		return h
	case <-time.After(flushAnswer):
		t.Fatalf("a flush did not reach the host's callback within %s", flushAnswer)
		return heldFlush{}
	}
}

func nextResult(t *testing.T, s pipeSession) vmwire.Frame {
	t.Helper()
	select {
	case f := <-s.results:
		return f
	case <-time.After(flushAnswer):
		t.Fatalf("the pager sent no answer within %s", flushAnswer)
		return vmwire.Frame{}
	}
}

func result(id uint64, errno syscall.Errno) vmwire.Frame {
	return vmwire.Frame{Kind: vmwire.Result, ID: id, Flags: uint64(errno)}
}

// A guest's flush waits for the host. The pager hands the request, with the
// region it was sent on, to the callback the host installed, and answers it
// when the host calls done and not before: a second flush answered first is
// the first answer the client sees.
func TestAFlushIsAnsweredWhenTheHostCallsDone(t *testing.T) {
	s := pipeConnection(t, vmmemory.Pmem, newKernelBacking(1, hugePageSize))
	held := holdFlushes(s)
	sendFlush(t, s, 1)
	first := nextHeld(t, held)
	if first.region != s.c.Region().Memory {
		t.Fatalf("the flush reached the host as region %p, want the session's %p", first.region, s.c.Region().Memory)
	}
	sendFlush(t, s, 2)
	second := nextHeld(t, held)
	second.done(nil)
	if got := nextResult(t, s); got != result(2, 0) {
		t.Fatalf("the first answer the client saw was %+v, want flush 2's while flush 1 is held", got)
	}
	first.done(nil)
	if got := nextResult(t, s); got != result(1, 0) {
		t.Fatalf("flush 1 was answered with %+v", got)
	}
	stats, err := s.h.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Flushes != 2 {
		t.Fatalf("the pager counted %d flushes, want 2", stats.Flushes)
	}
}

// Every request is answered exactly once, with what the host said about it,
// in whatever order the host says it. A done called twice sends nothing more:
// the answer to a later flush is the next thing the client reads.
func TestEveryFlushIsAnsweredOnce(t *testing.T) {
	s := pipeConnection(t, vmmemory.Pmem, newKernelBacking(1, hugePageSize))
	held := holdFlushes(s)
	var dones []func(error)
	for id := uint64(1); id <= 3; id++ {
		sendFlush(t, s, id)
		dones = append(dones, nextHeld(t, held).done)
	}
	dones[2](errors.New("the checkpoint failed"))
	dones[0](nil)
	dones[0](nil)
	dones[1](nil)
	sendFlush(t, s, 4)
	nextHeld(t, held).done(nil)
	for _, want := range []vmwire.Frame{result(3, syscall.EIO), result(1, 0), result(2, 0), result(4, 0)} {
		if got := nextResult(t, s); got != want {
			t.Fatalf("the client read %+v, want %+v", got, want)
		}
	}
}

// A host that installs nothing makes no guest wait.
func TestWithNoCallbackAFlushIsAnsweredAtOnce(t *testing.T) {
	s := pipeConnection(t, vmmemory.Pmem, newKernelBacking(1, hugePageSize))
	sendFlush(t, s, 1)
	if got := nextResult(t, s); got != result(1, 0) {
		t.Fatalf("a flush with no callback installed was answered with %+v", got)
	}
}

// The callback is the host's code, and the reader must never wait on it: a
// checkpoint the host takes for a flush needs this session's reader for its
// seal. Flushes that arrive while a callback is busy are read and counted, and
// handed over in order once it returns.
func TestABusyFlushCallbackDoesNotHoldUpTheReader(t *testing.T) {
	s := pipeConnection(t, vmmemory.Pmem, newKernelBacking(1, hugePageSize))
	held := make(chan heldFlush)
	release := make(chan struct{})
	first := true
	s.h.SetFlushed(func(r *vmmemory.Region, done func(error)) {
		held <- heldFlush{r, done}
		if first {
			first = false
			<-release
		}
	})
	sendFlush(t, s, 1)
	one := nextHeld(t, held) // and now the callback is blocked
	sendFlush(t, s, 2)
	sendFlush(t, s, 3)
	deadline := time.Now().Add(flushAnswer)
	for {
		stats, err := s.h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if stats.Flushes == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the reader counted %d flushes while the callback was busy, want 3", stats.Flushes)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	two, three := nextHeld(t, held), nextHeld(t, held)
	for _, h := range []heldFlush{three, two, one} {
		h.done(nil)
	}
	for _, want := range []vmwire.Frame{result(3, 0), result(2, 0), result(1, 0)} {
		if got := nextResult(t, s); got != want {
			t.Fatalf("the client read %+v, want %+v", got, want)
		}
	}
}

// A host may never answer a flush — it drops those of a VM that leaves it — so
// a session ends without waiting for one, and a done called after it has ended
// returns without sending anything.
func TestAnUnansweredFlushDoesNotHoldASessionOpen(t *testing.T) {
	s := pipeConnection(t, vmmemory.Pmem, newKernelBacking(1, hugePageSize))
	held := holdFlushes(s)
	sendFlush(t, s, 1)
	h := nextHeld(t, held)
	cause := errors.New("the VM left this host")
	s.cancel(cause)
	ctx, cancel := context.WithTimeout(t.Context(), flushAnswer)
	defer cancel()
	if err := s.c.Wait(ctx); !errors.Is(err, cause) {
		t.Fatalf("the session ended with %v, want %v", err, cause)
	}
	if err := s.c.Close(ctx); err != nil {
		t.Fatalf("closing a session with a flush unanswered: %v", err)
	}
	h.done(nil)
}

// A flush is a disk's, and it is a request like a seal: a flush on a RAM
// session, one without a fresh ID, or one that states anything else, is a
// client this pager does not understand, and the session ends as it does on
// any frame it cannot read.
func TestAMalformedFlushEndsTheSession(t *testing.T) {
	for _, c := range []struct {
		name   string
		kind   vmmemory.RegionKind
		frames []vmwire.Frame
		want   string
	}{
		{"a flush of RAM", vmmemory.Ram, []vmwire.Frame{{Kind: vmwire.Flush, ID: 1}}, "flush of a ram region"},
		{"a flush without an ID", vmmemory.Pmem, []vmwire.Frame{{Kind: vmwire.Flush}}, "invalid flush request"},
		{"a flush whose ID is not fresh", vmmemory.Pmem,
			[]vmwire.Frame{{Kind: vmwire.Flush, ID: 2}, {Kind: vmwire.Flush, ID: 2}}, "invalid flush request"},
		{"a flush of a range", vmmemory.Pmem,
			[]vmwire.Frame{{Kind: vmwire.Flush, ID: 1, Length: hugePageSize}}, "invalid flush request"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := pipeConnection(t, c.kind, newKernelBacking(1, hugePageSize))
			for _, f := range c.frames {
				if err := vmwire.Write(s.client, f); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), flushAnswer)
			defer cancel()
			err := s.c.Wait(ctx)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s ended the session with %v, want it to name %q", c.name, err, c.want)
			}
		})
	}
}
