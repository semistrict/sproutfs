// Package bounded bounds every request a process makes of an object store.
// A request may wait for the store's first byte for only so long, and a body
// may stall between two bytes for only so long. A request that waits longer
// is cancelled, and made again where making it again is safe.
//
// The bound is on waiting, not on the whole request. Objects range from a
// control record of a few hundred bytes to a part of 64 MiB, so no one total
// suits them all, while a store that has said nothing for ten seconds is a
// store that is not going to. Without a bound, one GET of Cloud Storage waited
// 52 minutes for its headers, and a guest fault, a pull or a publication
// waited with it.
//
// A request is made again when repeating it cannot change what the store
// holds in a way its caller does not already reckon with: a HEAD, a GET and a
// LIST, and a PUT under a condition. A conditional PUT whose first attempt
// landed and whose reply never came is refused on its second attempt, by the
// object the first one wrote. Every caller of a conditional write already
// settles a refusal against what the store holds, as it settles a lost reply,
// so the retry adds nothing it has not already met. An unconditional PUT and
// a DELETE are not made again: a second attempt could undo a change another
// writer made after the first, so their timeouts go back to the caller as
// ErrTimedOut, an ambiguous outcome the caller resolves as it resolves any
// other.
//
// A GET whose body stalls is asked again for the bytes it had not yet
// delivered, and only if the object is still the one the first reply
// described, so a caller reads one object's bytes and never two objects'.
package bounded

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// Bounds is how long a request may wait on its store.
type Bounds struct {
	// FirstByte is how long a request waits for the store's first byte: the
	// whole reply of a HEAD, a LIST or a DELETE, the headers of a GET, and
	// the reply of a PUT once its body has been handed over, or before the
	// store has taken any of it. Zero is DefaultFirstByte.
	FirstByte time.Duration
	// Stall is how long a body may go between two bytes: a GET's while its
	// reader waits for more, and a PUT's while the store takes it. Zero is
	// DefaultStall.
	Stall time.Duration
}

// The defaults are many times what either provider takes at the 99th
// percentile, which is a few hundred milliseconds for a 2 MiB GET, and longer
// than a few lost packets cost a TCP connection in retransmissions. So a
// healthy request does not reach them, a throttled one keeps the time its
// client library backs off for inside one attempt, and a stuck one is
// abandoned in seconds rather than an hour.
const (
	DefaultFirstByte = 10 * time.Second
	DefaultStall     = 10 * time.Second
)

// ErrInvalidBounds reports a bound that is negative.
var ErrInvalidBounds = errors.New("bounded: a bound is negative")

// Validate refuses a negative bound. Zero takes the default, so no
// configuration can leave a request unbounded.
func (b Bounds) Validate() error {
	if b.FirstByte < 0 || b.Stall < 0 {
		return fmt.Errorf("%w: first byte %s, stall %s", ErrInvalidBounds, b.FirstByte, b.Stall)
	}
	return nil
}

// WithDefaults is b with every zero bound replaced by its default.
func (b Bounds) WithDefaults() Bounds {
	if b.FirstByte == 0 {
		b.FirstByte = DefaultFirstByte
	}
	if b.Stall == 0 {
		b.Stall = DefaultStall
	}
	return b
}

var (
	// ErrTimedOut reports a request its store did not answer within its
	// bounds, and that was not made again: a write a second attempt could
	// not make safely. Whether the store applied it is unknown. It is a kind
	// of platform.ErrUnavailable.
	ErrTimedOut = fmt.Errorf("%w: an object store request outlived its bound", platform.ErrUnavailable)
	// ErrChanged reports a body that stalled, whose object was another one
	// when the rest of it was asked for. It is a kind of
	// platform.ErrUnavailable: reading the object again reads the new one
	// whole.
	ErrChanged = fmt.Errorf("%w: an object changed while its body stalled", platform.ErrUnavailable)
	// ErrNoStore reports a bounded store asked to wrap nothing.
	ErrNoStore = errors.New("bounded: no object store to bound")
)

// The probes a campaign of the bounds reaches.
const (
	// ProbeFirstByte is a request that waited past its first-byte bound, and
	// ProbeStall a body that stalled past its stall bound.
	ProbeFirstByte = "bounded/first-byte-timeout"
	ProbeStall     = "bounded/stall-timeout"
	// ProbeRetried is a request made again after a timeout, and
	// ProbeWriteRetried a conditional PUT among them, the one whose first
	// attempt may have landed.
	ProbeRetried      = "bounded/retried"
	ProbeWriteRetried = "bounded/write-retried"
	// ProbeResumed is a stalled body asked again for the rest of its bytes,
	// and ProbeChanged one whose object had changed by then.
	ProbeResumed = "bounded/resumed"
	ProbeChanged = "bounded/changed"
	// ProbeNotRepeated is a write that timed out and went back to its caller.
	ProbeNotRepeated = "bounded/not-repeated"
)

// Probes is every probe this package marks.
var Probes = []string{ProbeFirstByte, ProbeStall, ProbeRetried, ProbeWriteRetried, ProbeResumed,
	ProbeChanged, ProbeNotRepeated}

// The in-tree bugs of this package. BugUnbounded arms no bound at all, which
// is the store as it was before this package. BugResumeAnyObject resumes a
// stalled body without checking that its object is still the one it began
// as.
const (
	BugUnbounded       = "store-request-unbounded"
	BugResumeAnyObject = "store-resume-any-object"
)

// Recovery is what one operation's bounds did: the attempts cancelled at
// each bound, and the attempts made again after one of them. A body resumed
// after a stall is a GET made again.
type Recovery struct {
	FirstByteTimeouts int64 `json:"first_byte_timeouts"`
	StallTimeouts     int64 `json:"stall_timeouts"`
	Retries           int64 `json:"retries"`
}

// Recoveries is a store's Recovery by operation.
type Recoveries struct {
	Head, Get, Put, Delete, List Recovery
}

// Store bounds every request of the store beneath it. It is safe for
// concurrent use.
type Store struct {
	store  platform.ObjectStore
	bounds Bounds
	clock  platform.Clock
	counts [5]counts
}

var _ platform.ObjectStore = (*Store)(nil)

// counts is one operation's tallies.
type counts struct {
	firstByte, stall, retries atomic.Int64
}

// New bounds store's requests by bounds, measured on clock; a nil clock is
// the wall clock, which is the clock a store's own latency passes on.
func New(store platform.ObjectStore, bounds Bounds, clock platform.Clock) (*Store, error) {
	if store == nil {
		return nil, ErrNoStore
	}
	if err := bounds.Validate(); err != nil {
		return nil, err
	}
	return &Store{store: store, bounds: bounds.WithDefaults(), clock: platform.ClockOr(clock)}, nil
}

// Bounds is what this store bounds its requests by.
func (s *Store) Bounds() Bounds { return s.bounds }

// Recoveries reports what the bounds have done since the store was built.
func (s *Store) Recoveries() Recoveries {
	recovery := func(operation platform.ObjectOperation) Recovery {
		c := &s.counts[operation]
		return Recovery{FirstByteTimeouts: c.firstByte.Load(), StallTimeouts: c.stall.Load(),
			Retries: c.retries.Load()}
	}
	return Recoveries{Head: recovery(platform.HeadOperation), Get: recovery(platform.GetOperation),
		Put: recovery(platform.PutOperation), Delete: recovery(platform.DeleteOperation),
		List: recovery(platform.ListOperation)}
}

func (s *Store) Head(ctx context.Context, key platform.ObjectKey) (platform.ObjectMetadata, error) {
	var metadata platform.ObjectMetadata
	a, err := s.call(ctx, platform.HeadOperation, key.String(), true, func(a *attempt) (err error) {
		metadata, err = s.store.Head(a.ctx, key)
		return err
	})
	if err != nil {
		return platform.ObjectMetadata{}, err
	}
	a.end()
	return metadata, nil
}

func (s *Store) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	result, a, err := s.get(ctx, request)
	if err != nil {
		return platform.GetResult{}, err
	}
	b := &body{store: s, ctx: ctx, key: request.Key, etag: result.Metadata.ETag, size: result.Metadata.Size,
		start: start(request, result), length: result.ContentLength, current: result.Body, attempt: a}
	result.Body = b
	return result, nil
}

// get makes one GET and hands back its attempt, which lives on while its body
// is read.
func (s *Store) get(ctx context.Context, request platform.GetRequest) (platform.GetResult, *attempt, error) {
	var result platform.GetResult
	a, err := s.call(ctx, platform.GetOperation, request.Key.String(), true, func(a *attempt) (err error) {
		result, err = s.store.Get(a.ctx, request)
		return err
	})
	if err != nil {
		return platform.GetResult{}, nil, err
	}
	// Between the headers and the first read, nothing waits on the store.
	a.watch.idle()
	return result, a, nil
}

// start is where in its object a GET's body begins.
func start(request platform.GetRequest, result platform.GetResult) int64 {
	switch {
	case request.Range == nil:
		return 0
	case request.Range.Suffix > 0:
		return result.Metadata.Size - result.ContentLength
	default:
		return request.Range.Offset
	}
}

func (s *Store) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	if err := request.Validate(); err != nil {
		return platform.PutResult{}, err
	}
	conditional := request.Conditions.IfNoneMatch || request.Conditions.IfMatch != nil
	var result platform.PutResult
	a, err := s.call(ctx, platform.PutOperation, request.Key.String(), conditional, func(a *attempt) (err error) {
		attempted := request
		attempted.Body = &upload{ReaderAt: request.Body, size: request.Size, watch: a.watch}
		result, err = s.store.Put(a.ctx, attempted)
		return err
	})
	if err != nil {
		return platform.PutResult{}, err
	}
	a.end()
	return result, nil
}

func (s *Store) Delete(ctx context.Context, request platform.DeleteRequest) error {
	a, err := s.call(ctx, platform.DeleteOperation, request.Key.String(), false, func(a *attempt) error {
		return s.store.Delete(a.ctx, request)
	})
	if err != nil {
		return err
	}
	a.end()
	return nil
}

func (s *Store) List(ctx context.Context, request platform.ListRequest) (platform.ListResult, error) {
	var result platform.ListResult
	a, err := s.call(ctx, platform.ListOperation, request.Prefix.String(), true, func(a *attempt) (err error) {
		result, err = s.store.List(a.ctx, request)
		return err
	})
	if err != nil {
		return platform.ListResult{}, err
	}
	a.end()
	return result, nil
}

// call makes one request, and makes it again for as long as an attempt
// outlives its bound and repeat says the request is one to make again. It
// returns the attempt that answered, which its caller ends. Only the caller's
// own context ends the retries: a store that never answers is a store a
// fault, a pull or a publication cannot do without, and each attempt is
// counted and logged.
func (s *Store) call(ctx context.Context, operation platform.ObjectOperation, name string, repeat bool,
	do func(*attempt) error) (*attempt, error) {
	for attempts := 1; ; attempts++ {
		a := s.begin(ctx)
		err := do(a)
		if err == nil {
			return a, nil
		}
		a.end()
		which, expired := a.watch.expired()
		if !expired || ctx.Err() != nil {
			return nil, err
		}
		s.timedOut(ctx, operation, name, which, attempts, repeat)
		if !repeat {
			sim.Probe(ctx, ProbeNotRepeated)
			return nil, fmt.Errorf("%w: %s %s waited %s for %s", ErrTimedOut, operation, name, s.limit(which), which)
		}
		s.retried(ctx, operation)
	}
}

// begin starts one attempt, waiting for its first byte.
func (s *Store) begin(ctx context.Context) *attempt {
	attemptCtx, cancel := context.WithCancelCause(ctx)
	a := &attempt{ctx: attemptCtx, cancel: cancel,
		watch: &watch{clock: s.clock, bounds: s.bounds, cancel: cancel, off: sim.Bug(ctx, BugUnbounded)}}
	a.watch.wait(firstByte)
	return a
}

// timedOut counts and reports one attempt its bound cancelled.
func (s *Store) timedOut(ctx context.Context, operation platform.ObjectOperation, name string, which bound,
	attempts int, repeat bool) {
	c := &s.counts[operation]
	switch which {
	case firstByte:
		c.firstByte.Add(1)
		sim.Probe(ctx, ProbeFirstByte)
	case stall:
		c.stall.Add(1)
		sim.Probe(ctx, ProbeStall)
	}
	slog.WarnContext(ctx, "object store: a request outlived its bound", "operation", operation.String(),
		"object", name, "bound", which.String(), "limit", s.limit(which).String(), "attempt", attempts,
		"again", repeat)
}

// retried counts one request made again.
func (s *Store) retried(ctx context.Context, operation platform.ObjectOperation) {
	s.counts[operation].retries.Add(1)
	sim.Probe(ctx, ProbeRetried)
	if operation == platform.PutOperation {
		sim.Probe(ctx, ProbeWriteRetried)
	}
}

func (s *Store) limit(which bound) time.Duration {
	if which == stall {
		return s.bounds.Stall
	}
	return s.bounds.FirstByte
}

// attempt is one request made of the store, under a context of its own that
// its watch cancels.
type attempt struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	watch  *watch
}

// end releases the attempt: its watch stops, and anything still running
// under its context is told to give up.
func (a *attempt) end() {
	a.watch.stop()
	a.cancel(context.Canceled)
}

// upload is a PUT's body as its attempt hands it over, which is how the
// attempt knows the store is taking it: each read of it is progress, under
// the stall bound while there is more to hand over and under the first-byte
// bound once the store holds all of it.
type upload struct {
	io.ReaderAt
	size  int64
	watch *watch
}

func (u *upload) ReadAt(p []byte, offset int64) (int, error) {
	n, err := u.ReaderAt.ReadAt(p, offset)
	if n > 0 {
		under := stall
		if offset+int64(n) >= u.size {
			under = firstByte
		}
		u.watch.progress(under)
	}
	return n, err
}

// body is a GET's body. Each read waits under the stall bound, and a read
// past it asks the store for the rest of the bytes, from where the body
// stopped, of the same object.
type body struct {
	store *Store
	// ctx is the caller's: the one that ends the body's retries.
	ctx context.Context
	key platform.ObjectKey
	// etag and size are the object the first reply described, and start and
	// length where the body is in it and how long it is.
	etag          platform.ETag
	size          int64
	start, length int64
	// read is how many bytes of the body the caller has been handed.
	read int64
	// current is the reply the body reads from now and attempt the request
	// that made it, both nil once the body ended or failed; failed is the
	// error that ended it, nil for one that ended with its last byte.
	current io.ReadCloser
	attempt *attempt
	failed  error
	closed  bool
}

func (b *body) Read(p []byte) (int, error) {
	if b.closed {
		return 0, platform.ErrClosed
	}
	for b.current != nil {
		b.attempt.watch.wait(stall)
		n, err := b.current.Read(p)
		b.attempt.watch.idle()
		b.read += int64(n)
		if err == nil || err == io.EOF {
			return n, err
		}
		which, expired := b.attempt.watch.expired()
		if !expired || b.ctx.Err() != nil {
			return n, err
		}
		b.store.timedOut(b.ctx, platform.GetOperation, b.key.String(), which, 1, true)
		b.resume()
		if n > 0 {
			return n, nil
		}
	}
	if b.failed != nil {
		return 0, b.failed
	}
	return 0, io.EOF
}

// resume gives up on the stalled reply and asks for the bytes after the ones
// already handed over. A body with nothing left to hand over has ended, and
// one whose rest cannot be had has failed.
func (b *body) resume() {
	b.release()
	switch left := b.length - b.read; {
	case b.length < 0:
		b.failed = fmt.Errorf("%w: GET %s stalled, and its length is unknown", ErrTimedOut, b.key)
	case left == 0:
	default:
		b.store.retried(b.ctx, platform.GetOperation)
		result, a, err := b.store.get(b.ctx, platform.GetRequest{Key: b.key,
			Range: &platform.ByteRange{Offset: b.start + b.read, Length: left}})
		switch {
		case err != nil:
			b.failed = err
		case (result.Metadata.ETag != b.etag || result.Metadata.Size != b.size || result.ContentLength != left) &&
			!sim.Bug(b.ctx, BugResumeAnyObject):
			sim.Probe(b.ctx, ProbeChanged)
			b.failed = fmt.Errorf("%w: GET %s began as %s of %d bytes and resumed as %s of %d bytes",
				ErrChanged, b.key, b.etag, b.size, result.Metadata.ETag, result.Metadata.Size)
			b.current, b.attempt = result.Body, a
			b.release()
		default:
			sim.Probe(b.ctx, ProbeResumed)
			b.current, b.attempt = result.Body, a
		}
	}
}

// release closes the reply the body reads from and ends its request.
func (b *body) release() {
	if b.current == nil {
		return
	}
	if err := b.current.Close(); err != nil {
		slog.DebugContext(b.ctx, "object store: closing a reply failed", "object", b.key.String(), "error", err)
	}
	b.attempt.end()
	b.current, b.attempt = nil, nil
}

func (b *body) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	if b.current == nil {
		return nil
	}
	err := b.current.Close()
	b.attempt.end()
	b.current, b.attempt = nil, nil
	return err
}
