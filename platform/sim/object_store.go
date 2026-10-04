package sim

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/platform"
)

type ObjectOperation string

const (
	ObjectHead   ObjectOperation = "head"
	ObjectGet    ObjectOperation = "get"
	ObjectPut    ObjectOperation = "put"
	ObjectDelete ObjectOperation = "delete"
	ObjectList   ObjectOperation = "list"
)

type ObjectStoreConfig struct {
	HeadLatency    time.Duration
	GetLatency     time.Duration
	PutLatency     time.Duration
	DeleteLatency  time.Duration
	ListLatency    time.Duration
	BytesPerSecond int64
	MaxObjectSize  int64
	// Hold is how long a hung request, or a body stalled halfway, waits
	// before it goes on, unless its caller gives up on it first. One GET of
	// Cloud Storage once waited 52 minutes for its headers; the default is an
	// hour.
	Hold time.Duration
	// RequestChaos puts this store's requests under three Buggify sites: a
	// request held before its reply, a write applied and its reply then held,
	// and a body held halfway, each for Hold. They fire only while the
	// runtime's Buggify switch is on, and only on a store that asks for them,
	// because a caller that bounds none of its requests waits out every hold.
	RequestChaos bool
}

// The request chaos sites a store with RequestChaos consults.
const (
	SiteObjectHang           = "sim/object-store/hang"
	SiteObjectHangAfterApply = "sim/object-store/hang-after-apply"
	SiteObjectStallBody      = "sim/object-store/stall-body"
)

// The chance that each request chaos site fires on one request, once a seed
// has activated it.
const (
	hangProbability      = 0.02
	hangAfterProbability = 0.05
	stallProbability     = 0.05
)

func (c ObjectStoreConfig) withDefaults(defaults ObjectStoreConfig) ObjectStoreConfig {
	c.HeadLatency = cmp.Or(c.HeadLatency, defaults.HeadLatency)
	c.GetLatency = cmp.Or(c.GetLatency, defaults.GetLatency)
	c.PutLatency = cmp.Or(c.PutLatency, defaults.PutLatency)
	c.DeleteLatency = cmp.Or(c.DeleteLatency, defaults.DeleteLatency)
	c.ListLatency = cmp.Or(c.ListLatency, defaults.ListLatency)
	c.BytesPerSecond = cmp.Or(c.BytesPerSecond, defaults.BytesPerSecond)
	c.MaxObjectSize = cmp.Or(c.MaxObjectSize, defaults.MaxObjectSize)
	c.Hold = cmp.Or(c.Hold, defaults.Hold)
	c.RequestChaos = c.RequestChaos || defaults.RequestChaos
	return c
}

type storedObject struct {
	value    []byte
	etag     platform.ETag
	modified time.Time
	attrs    map[string]string
}

type ObjectStore struct {
	runtime *Runtime
	config  ObjectStoreConfig
	// name tells a second bucket's requests apart from the deployment's in
	// the trace and in what a scheduler is asked to admit: empty for the
	// runtime's own store, whose events carry the bare key.
	name string

	mu             sync.Mutex
	objects        map[string]storedObject
	sequence       map[string]uint64
	failNext       map[ObjectOperation]int
	failAfterApply map[ObjectOperation]int
	// hangNext, hangAfterApply and stallNext count the requests still to be
	// held before their reply, after their write is applied, and halfway
	// through their body; see HangNext, HangNextAfterApply and StallNextBody.
	hangNext       map[ObjectOperation]int
	hangAfterApply map[ObjectOperation]int
	stallNext      map[ObjectOperation]int
	failed         bool
	// observers see every change the store applies, in the order it applies
	// them; see Observe.
	observers []func(ObjectChange)
}

// ObjectChange is one change the store applied: an object written, or one
// deleted.
type ObjectChange struct {
	Key     string
	Value   []byte
	Deleted bool
}

// Observe has the store tell observe of every change it applies from now on,
// in the order it applies them, which is the order every reader sees. It is
// called with the store locked, so it must not call the store, and it must not
// keep Value past its return without copying it.
func (s *ObjectStore) Observe(observe func(ObjectChange)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observers = append(s.observers, observe)
}

// applied tells every observer of one change. Caller holds the lock.
func (s *ObjectStore) applied(change ObjectChange) {
	for _, observe := range s.observers {
		observe(change)
	}
}

func newObjectStore(runtime *Runtime, name string, config ObjectStoreConfig) *ObjectStore {
	return &ObjectStore{
		runtime:        runtime,
		config:         config,
		name:           name,
		objects:        make(map[string]storedObject),
		sequence:       make(map[string]uint64),
		failNext:       make(map[ObjectOperation]int),
		failAfterApply: make(map[ObjectOperation]int),
		hangNext:       make(map[ObjectOperation]int),
		hangAfterApply: make(map[ObjectOperation]int),
		stallNext:      make(map[ObjectOperation]int),
	}
}

func (s *ObjectStore) Head(ctx context.Context, key platform.ObjectKey) (platform.ObjectMetadata, error) {
	if key.IsZero() {
		return platform.ObjectMetadata{}, platform.ErrInvalidObjectKey
	}
	id, err := s.before(ctx, ObjectHead, key, s.config.HeadLatency)
	if err != nil {
		return platform.ObjectMetadata{}, err
	}
	s.mu.Lock()
	object, exists := s.objects[key.String()]
	s.mu.Unlock()
	if !exists {
		s.trace(ObjectHead, key, "not_found", 0, id)
		return platform.ObjectMetadata{}, platform.ErrNotFound
	}
	metadata := metadataFor(key, object)
	s.trace(ObjectHead, key, "ok", 0, id)
	return metadata, nil
}

func (s *ObjectStore) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	if request.Key.IsZero() {
		return platform.GetResult{}, platform.ErrInvalidObjectKey
	}
	if request.Range != nil {
		if err := request.Range.Validate(); err != nil {
			return platform.GetResult{}, err
		}
	}
	id, err := s.before(ctx, ObjectGet, request.Key, s.config.GetLatency)
	if err != nil {
		return platform.GetResult{}, err
	}
	s.mu.Lock()
	object, exists := s.objects[request.Key.String()]
	s.mu.Unlock()
	if !exists {
		s.trace(ObjectGet, request.Key, "not_found", 0, id)
		return platform.GetResult{}, platform.ErrNotFound
	}
	value := object.value
	switch {
	case request.Range == nil:
	case request.Range.Suffix > 0:
		// A suffix longer than the object is the whole object: the caller is
		// reading a bounded tail without knowing how long the object is.
		value = value[max(int64(len(value))-request.Range.Suffix, 0):]
	case request.Range.Offset >= int64(len(value)):
		s.trace(ObjectGet, request.Key, "invalid_range", 0, id)
		return platform.GetResult{}, platform.ErrInvalidRange
	default:
		end := min(request.Range.Offset+request.Range.Length, int64(len(value)))
		value = value[request.Range.Offset:end]
	}
	s.trace(ObjectGet, request.Key, "ok", len(value), id)
	body := &objectReader{
		runtime:        s.runtime,
		id:             fmt.Sprintf("object/body/%q/%d", s.label(request.Key), id),
		ctx:            ctx,
		reader:         bytes.NewReader(value),
		bytesPerSecond: s.config.BytesPerSecond,
	}
	if len(value) >= 2 && s.stalls(ObjectGet) {
		body.stallAt = int64(len(value) / 2)
		body.stall = func(ctx context.Context) error { return s.hold(ctx, ObjectGet, request.Key, id, "body") }
	}
	return platform.GetResult{
		Metadata:      metadataFor(request.Key, object),
		ContentLength: int64(len(value)),
		Body:          body,
	}, nil
}

func (s *ObjectStore) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	if err := request.Validate(); err != nil {
		return platform.PutResult{}, err
	}
	if request.Size > s.config.MaxObjectSize || request.Size > int64(maxInt()) {
		return platform.PutResult{}, platform.ErrMessageTooLarge
	}
	id, err := s.before(ctx, ObjectPut, request.Key, operationLatency(s.config.PutLatency, int(request.Size), s.config.BytesPerSecond))
	if err != nil {
		return platform.PutResult{}, err
	}
	value := make([]byte, int(request.Size))
	// A stalled upload takes the first half of its body and then nothing
	// more for a while, as a store that stops reading its connection does.
	half := int64(0)
	if request.Size >= 2 && s.stalls(ObjectPut) {
		half = request.Size / 2
		if _, err := io.ReadFull(io.NewSectionReader(request.Body, 0, half), value[:half]); err != nil {
			s.trace(ObjectPut, request.Key, "read_error", 0, id)
			return platform.PutResult{}, fmt.Errorf("read upload body: %w", err)
		}
		if err := s.hold(ctx, ObjectPut, request.Key, id, "body"); err != nil {
			return platform.PutResult{}, err
		}
	}
	reader := io.NewSectionReader(request.Body, half, request.Size-half)
	if _, err := io.ReadFull(reader, value[half:]); err != nil {
		s.trace(ObjectPut, request.Key, "read_error", 0, id)
		return platform.PutResult{}, fmt.Errorf("read upload body: %w", err)
	}
	object := storedObject{
		value:    value,
		etag:     etagOf(value),
		modified: time.Now(),
		attrs:    platform.CloneAttributes(request.Attributes),
	}

	s.mu.Lock()
	current, exists := s.objects[request.Key.String()]
	if !conditionsMatch(request.Conditions, current.etag, exists) {
		s.mu.Unlock()
		s.trace(ObjectPut, request.Key, "precondition_failed", len(value), id)
		return platform.PutResult{}, platform.ErrPrecondition
	}
	s.objects[request.Key.String()] = object
	s.applied(ObjectChange{Key: request.Key.String(), Value: value})
	s.mu.Unlock()
	if err := s.holdAfterApply(ctx, ObjectPut, request.Key, id); err != nil {
		return platform.PutResult{}, err
	}
	if s.takeFailAfterApply(ObjectPut) {
		s.trace(ObjectPut, request.Key, "applied_injected_fault", len(value), id)
		return platform.PutResult{}, platform.ErrInjectedFault
	}
	s.trace(ObjectPut, request.Key, "ok", len(value), id)
	return platform.PutResult{Metadata: metadataFor(request.Key, object)}, nil
}

func (s *ObjectStore) Delete(ctx context.Context, request platform.DeleteRequest) error {
	if request.Key.IsZero() {
		return platform.ErrInvalidObjectKey
	}
	id, err := s.before(ctx, ObjectDelete, request.Key, s.config.DeleteLatency)
	if err != nil {
		return err
	}
	s.mu.Lock()
	current, exists := s.objects[request.Key.String()]
	if request.IfMatch != nil && (!exists || current.etag != *request.IfMatch) {
		s.mu.Unlock()
		s.trace(ObjectDelete, request.Key, "precondition_failed", 0, id)
		return platform.ErrPrecondition
	}
	if exists {
		delete(s.objects, request.Key.String())
		s.applied(ObjectChange{Key: request.Key.String(), Deleted: true})
	}
	s.mu.Unlock()
	if err := s.holdAfterApply(ctx, ObjectDelete, request.Key, id); err != nil {
		return err
	}
	if s.takeFailAfterApply(ObjectDelete) {
		s.trace(ObjectDelete, request.Key, "applied_injected_fault", 0, id)
		return platform.ErrInjectedFault
	}
	s.trace(ObjectDelete, request.Key, "ok", 0, id)
	return nil
}

func (s *ObjectStore) List(ctx context.Context, request platform.ListRequest) (platform.ListResult, error) {
	if err := request.Validate(); err != nil {
		return platform.ListResult{}, err
	}
	prefix := request.Prefix.String()
	id, err := s.before(ctx, ObjectList, objectKeyForTrace(prefix), s.config.ListLatency)
	if err != nil {
		return platform.ListResult{}, err
	}

	s.mu.Lock()
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
		key, keyErr := platform.NewObjectKey(value)
		if keyErr != nil {
			s.mu.Unlock()
			return platform.ListResult{}, keyErr
		}
		objects = append(objects, metadataFor(key, s.objects[value]))
	}
	s.mu.Unlock()

	result := platform.ListResult{Objects: objects}
	if limit < len(keys) {
		result.NextContinuationToken = keys[limit-1]
	}
	s.trace(ObjectList, objectKeyForTrace(prefix), "ok", len(objects), id)
	return result, nil
}

func (s *ObjectStore) FailNext(operation ObjectOperation, count int) {
	s.mu.Lock()
	s.failNext[operation] += max(count, 0)
	s.mu.Unlock()
}

// FailNextAfterApply injects an ambiguous result into the next count
// successful mutating operations. The state change is applied durably before
// ErrInjectedFault is returned, modeling a lost response from an object store.
func (s *ObjectStore) FailNextAfterApply(operation ObjectOperation, count int) {
	s.mu.Lock()
	s.failAfterApply[operation] += max(count, 0)
	s.mu.Unlock()
}

// HangNext holds the next count requests of operation before their reply,
// for the store's Hold, as a request whose reply does not come: a write held
// this way has not been applied when its caller gives up on it. A request
// that waits out the hold goes on as if nothing had happened.
func (s *ObjectStore) HangNext(operation ObjectOperation, count int) {
	s.mu.Lock()
	s.hangNext[operation] += max(count, 0)
	s.mu.Unlock()
}

// HangNextAfterApply applies the next count writes of operation, a put or a
// delete, and then holds their replies for the store's Hold: a caller that
// gives up on one has made its change all the same, as a reply lost on its
// way back leaves it.
func (s *ObjectStore) HangNextAfterApply(operation ObjectOperation, count int) {
	s.mu.Lock()
	s.hangAfterApply[operation] += max(count, 0)
	s.mu.Unlock()
}

// StallNextBody holds the next count bodies of operation, a get or a put,
// halfway through, for the store's Hold: a get's reader has had the first
// half of the bytes it asked for, and a put has taken the first half of
// what it writes. A body of fewer than two bytes has no halfway, and does
// not spend a count.
func (s *ObjectStore) StallNextBody(operation ObjectOperation, count int) {
	s.mu.Lock()
	s.stallNext[operation] += max(count, 0)
	s.mu.Unlock()
}

func (s *ObjectStore) Fail() {
	s.mu.Lock()
	s.failed = true
	s.mu.Unlock()
	s.runtime.trace.record(Event{Kind: "object_store", Resource: "object_store", Operation: "fail", Outcome: "ok"})
}

func (s *ObjectStore) Recover() {
	s.mu.Lock()
	s.failed = false
	s.mu.Unlock()
	s.runtime.trace.record(Event{Kind: "object_store", Resource: "object_store", Operation: "recover", Outcome: "ok"})
}

func (s *ObjectStore) before(ctx context.Context, operation ObjectOperation, key platform.ObjectKey, latency time.Duration) (uint64, error) {
	if err := s.runtime.Admit(ctx, fmt.Sprintf("object/%s/%q", operation, s.label(key))); err != nil {
		return 0, err
	}
	s.mu.Lock()
	sequenceKey := string(operation) + "/" + key.String()
	s.sequence[sequenceKey]++
	id := s.sequence[sequenceKey]
	failed := s.failed
	injected := s.failNext[operation] > 0
	if injected {
		s.failNext[operation]--
	}
	s.mu.Unlock()
	if failed {
		// A store that is down still takes the round trip to say so. An
		// answer of no time at all would reach its caller at the instant it
		// asked, racing whatever the caller's other goroutines do at that
		// instant, and the Go scheduler would decide which went first.
		if err := s.runtime.ioDelay(ctx, fmt.Sprintf("object/%s/%q/%d", operation, s.label(key), id), latency); err != nil {
			s.trace(operation, key, "canceled", 0, id)
			return 0, err
		}
		s.trace(operation, key, "unavailable", 0, id)
		return 0, platform.ErrUnavailable
	}
	if injected {
		s.trace(operation, key, "injected_fault", 0, id)
		return 0, platform.ErrInjectedFault
	}
	if err := s.runtime.ioDelay(ctx, fmt.Sprintf("object/%s/%q/%d", operation, s.label(key), id), latency); err != nil {
		s.trace(operation, key, "canceled", 0, id)
		return 0, err
	}
	if s.take(s.hangNext, operation) || s.chaos(SiteObjectHang, hangProbability) {
		if err := s.hold(ctx, operation, key, id, "reply"); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// holdAfterApply holds the reply to a write the store has applied, when a
// test or the seed says so.
func (s *ObjectStore) holdAfterApply(ctx context.Context, operation ObjectOperation, key platform.ObjectKey, id uint64) error {
	if !s.take(s.hangAfterApply, operation) && !s.chaos(SiteObjectHangAfterApply, hangAfterProbability) {
		return nil
	}
	return s.hold(ctx, operation, key, id, "applied")
}

// stalls reports whether this body of operation stalls halfway.
func (s *ObjectStore) stalls(operation ObjectOperation) bool {
	return s.take(s.stallNext, operation) || s.chaos(SiteObjectStallBody, stallProbability)
}

// hold waits out the store's Hold for one request. what names where it is
// held: before its reply, after its write was applied, or halfway through
// its body. A caller that gives up first gets its context's cause.
func (s *ObjectStore) hold(ctx context.Context, operation ObjectOperation, key platform.ObjectKey, id uint64, what string) error {
	s.trace(operation, key, "held_"+what, 0, id)
	hold := s.config.Hold
	if err := s.runtime.delay(ctx, fmt.Sprintf("object/%s/%q/%d/hold-%s", operation, s.label(key), id, what),
		hold, hold, hold); err != nil {
		s.trace(operation, key, "canceled_"+what, 0, id)
		return err
	}
	return nil
}

// take spends one of the requests a test asked to be held, if any is left.
func (s *ObjectStore) take(counts map[ObjectOperation]int, operation ObjectOperation) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if counts[operation] == 0 {
		return false
	}
	counts[operation]--
	return true
}

// chaos reports whether one request chaos site fires for this request.
func (s *ObjectStore) chaos(site string, p float64) bool {
	return s.config.RequestChaos && s.runtime.buggifyHere(site, p)
}

func (s *ObjectStore) takeFailAfterApply(operation ObjectOperation) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAfterApply[operation] == 0 {
		return false
	}
	s.failAfterApply[operation]--
	return true
}

func (s *ObjectStore) trace(operation ObjectOperation, key platform.ObjectKey, outcome string, bytes int, id uint64) {
	s.runtime.trace.record(Event{
		Kind:      "object_store",
		Resource:  s.label(key),
		Operation: string(operation),
		Outcome:   outcome,
		Bytes:     bytes,
		LocalID:   id,
	})
}

// label is how key appears in the trace and to a scheduler: the bare key in
// the runtime's own store, and the store's name before it in any other.
func (s *ObjectStore) label(key platform.ObjectKey) string {
	if s.name == "" {
		return key.String()
	}
	return s.name + ":" + key.String()
}

func conditionsMatch(conditions platform.PutConditions, etag platform.ETag, exists bool) bool {
	if conditions.IfNoneMatch {
		return !exists
	}
	if conditions.IfMatch != nil {
		return exists && etag == *conditions.IfMatch
	}
	return true
}

func metadataFor(key platform.ObjectKey, object storedObject) platform.ObjectMetadata {
	return platform.ObjectMetadata{
		Key:          key,
		Size:         int64(len(object.value)),
		LastModified: object.modified,
		ETag:         object.etag,
		Attributes:   platform.CloneAttributes(object.attrs),
	}
}

func objectKeyForTrace(prefix string) platform.ObjectKey {
	key, _ := platform.NewObjectKey(cmp.Or(prefix, "*"))
	return key
}

type objectReader struct {
	runtime        *Runtime
	id             string
	read           uint64
	ctx            context.Context
	reader         *bytes.Reader
	bytesPerSecond int64
	closed         bool
	// stallAt is how many bytes the body hands over before stall holds it,
	// once, and zero for a body that does not stall; delivered is how many
	// it has handed over.
	stallAt   int64
	stall     func(context.Context) error
	delivered int64
}

func (r *objectReader) Read(destination []byte) (int, error) {
	if r.closed {
		return 0, platform.ErrClosed
	}
	if r.stallAt > 0 {
		if r.delivered == r.stallAt {
			r.stallAt = 0
			if err := r.stall(r.ctx); err != nil {
				return 0, err
			}
		} else if left := r.stallAt - r.delivered; int64(len(destination)) > left {
			destination = destination[:left]
		}
	}
	n, err := r.reader.Read(destination)
	r.delivered += int64(n)
	if n > 0 {
		r.read++
		if sleepErr := r.runtime.ioDelay(r.ctx, fmt.Sprintf("%s/%d", r.id, r.read), operationLatency(0, n, r.bytesPerSecond)); sleepErr != nil {
			return 0, sleepErr
		}
	}
	return n, err
}

func (r *objectReader) Close() error {
	r.closed = true
	return nil
}

func maxInt() int { return int(^uint(0) >> 1) }

var _ platform.ObjectStore = (*ObjectStore)(nil)
