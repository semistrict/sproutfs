package platform

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"time"

	"github.com/semistrict/sproutfs/internal/ctxsync"
)

// Clock is the passage of time as a process sees it. It is a port beside Disk,
// Network and ObjectStore for the same reason they are: a deployment reads the
// wall clock, and a simulation must be able to hand a host its own, so that a
// deadline measured in checkpoint intervals is reached by a decision rather
// than by waiting for it.
//
// A nil Clock anywhere in a configuration means the wall clock. Production code
// therefore never names a clock it does not need, and a test that wants to
// retire a hold without waiting for it names one.
//
// Implementations are safe for concurrent use.
type Clock interface {
	Now() time.Time
	// Since is Now().Sub(t), which is the only thing a duration measurement
	// ever wants and the one call site a stray time.Since would hide in.
	Since(t time.Time) time.Duration
	// Sleep waits for d, returning nil once it elapses and the cancellation
	// cause of ctx if the context ends first. A non-positive d returns
	// immediately, or the cause if ctx is already done.
	Sleep(ctx context.Context, d time.Duration) error
	// AfterFunc runs f on its own goroutine once d has passed. The returned
	// stopper reports whether it prevented the call.
	AfterFunc(d time.Duration, f func()) Stopper
	NewTimer(d time.Duration) Timer
	NewTicker(d time.Duration) Ticker
}

// Stopper disarms a pending timer and reports whether it got there first.
type Stopper interface{ Stop() bool }

// Timer delivers one tick on C after the duration it was created with. It is
// the stdlib's timer as an interface, so a select on a deadline reads the same
// however the clock is implemented.
type Timer interface {
	Stopper
	C() <-chan time.Time
	// Reset rearms a timer for d, reporting whether it was still pending. A
	// caller must have stopped and drained a timer it reuses, exactly as the
	// stdlib requires.
	Reset(d time.Duration) bool
}

// Ticker delivers a tick on C every period until it is stopped.
type Ticker interface {
	C() <-chan time.Time
	Stop()
	Reset(d time.Duration)
}

// Entropy is the unpredictable-bytes seam: the writer nonce that tells a lost
// conditional write's author from a competing one, and the jitter that keeps a
// host's VMs from checkpointing in lockstep. It sits beside Clock because it
// has the same shape of problem — production draws from the operating system,
// and a simulation must be able to reproduce the draw.
//
// Nothing here fails. A process that cannot read entropy cannot be trusted to
// go on choosing nonces, and crypto/rand already ends one that tries.
type Entropy interface {
	// Fill writes unpredictable bytes over the whole of b.
	Fill(b []byte)
	// Uint64 returns one unpredictable value.
	Uint64() uint64
}

// WallClock is the operating system's clock, which is what every process that
// is not a simulation runs on. It is here rather than in adapters because it
// binds to nothing outside the standard library, and because it is the
// documented meaning of a nil Clock: a package that fills its own default must
// be able to name it without reaching for the adapter set a command chooses.
func WallClock() Clock { return wallClock{} }

// SystemEntropy is the operating system's entropy, drawn through crypto/rand.
func SystemEntropy() Entropy { return systemEntropy{} }

type wallClock struct{}

func (wallClock) Now() time.Time                  { return time.Now() }
func (wallClock) Since(t time.Time) time.Duration { return time.Since(t) }
func (wallClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return context.Cause(ctx)
	}
	return ctxsync.Sleep(ctx, d)
}

func (wallClock) AfterFunc(d time.Duration, f func()) Stopper {
	return stdTimer{time.AfterFunc(d, f)}
}

func (wallClock) NewTimer(d time.Duration) Timer   { return stdTimer{time.NewTimer(d)} }
func (wallClock) NewTicker(d time.Duration) Ticker { return stdTicker{time.NewTicker(d)} }

type stdTimer struct{ timer *time.Timer }

func (t stdTimer) C() <-chan time.Time        { return t.timer.C }
func (t stdTimer) Stop() bool                 { return t.timer.Stop() }
func (t stdTimer) Reset(d time.Duration) bool { return t.timer.Reset(d) }

type stdTicker struct{ ticker *time.Ticker }

func (t stdTicker) C() <-chan time.Time   { return t.ticker.C }
func (t stdTicker) Stop()                 { t.ticker.Stop() }
func (t stdTicker) Reset(d time.Duration) { t.ticker.Reset(d) }

type systemEntropy struct{}

// Fill reads from crypto/rand, which does not return an error in this Go
// version: it ends the process rather than hand back bytes it cannot vouch for.
func (systemEntropy) Fill(b []byte) {
	if _, err := cryptorand.Read(b); err != nil {
		panic("platform: the operating system's entropy is unreadable: " + err.Error())
	}
}

func (systemEntropy) Uint64() uint64 {
	var buffer [8]byte
	systemEntropy{}.Fill(buffer[:])
	return binary.LittleEndian.Uint64(buffer[:])
}

// ClockOr returns clock, or the wall clock when it is nil. Every configuration
// that takes an optional Clock fills its own default through here, so "nil
// means the wall clock" is written once.
func ClockOr(clock Clock) Clock {
	if clock == nil {
		return WallClock()
	}
	return clock
}

// EntropyOr returns entropy, or the operating system's when it is nil.
func EntropyOr(entropy Entropy) Entropy {
	if entropy == nil {
		return SystemEntropy()
	}
	return entropy
}
