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
}

func (c ObjectStoreConfig) withDefaults(defaults ObjectStoreConfig) ObjectStoreConfig {
	c.HeadLatency = cmp.Or(c.HeadLatency, defaults.HeadLatency)
	c.GetLatency = cmp.Or(c.GetLatency, defaults.GetLatency)
	c.PutLatency = cmp.Or(c.PutLatency, defaults.PutLatency)
	c.DeleteLatency = cmp.Or(c.DeleteLatency, defaults.DeleteLatency)
	c.ListLatency = cmp.Or(c.ListLatency, defaults.ListLatency)
	c.BytesPerSecond = cmp.Or(c.BytesPerSecond, defaults.BytesPerSecond)
	c.MaxObjectSize = cmp.Or(c.MaxObjectSize, defaults.MaxObjectSize)
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

	mu             sync.Mutex
	objects        map[string]storedObject
	sequence       map[string]uint64
	failNext       map[ObjectOperation]int
	failAfterApply map[ObjectOperation]int
	failed         bool
}

func newObjectStore(runtime *Runtime, config ObjectStoreConfig) *ObjectStore {
	return &ObjectStore{
		runtime:        runtime,
		config:         config,
		objects:        make(map[string]storedObject),
		sequence:       make(map[string]uint64),
		failNext:       make(map[ObjectOperation]int),
		failAfterApply: make(map[ObjectOperation]int),
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
	return platform.GetResult{
		Metadata:      metadataFor(request.Key, object),
		ContentLength: int64(len(value)),
		Body: &objectReader{
			runtime:        s.runtime,
			id:             fmt.Sprintf("object/body/%q/%d", request.Key.String(), id),
			ctx:            ctx,
			reader:         bytes.NewReader(value),
			bytesPerSecond: s.config.BytesPerSecond,
		},
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
	reader := io.NewSectionReader(request.Body, 0, request.Size)
	if _, err := io.ReadFull(reader, value); err != nil {
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
	s.mu.Unlock()
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
	delete(s.objects, request.Key.String())
	s.mu.Unlock()
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
	if err := s.runtime.Admit(ctx, fmt.Sprintf("object/%s/%q", operation, key.String())); err != nil {
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
		s.trace(operation, key, "unavailable", 0, id)
		return 0, platform.ErrUnavailable
	}
	if injected {
		s.trace(operation, key, "injected_fault", 0, id)
		return 0, platform.ErrInjectedFault
	}
	if err := s.runtime.ioDelay(ctx, fmt.Sprintf("object/%s/%q/%d", operation, key.String(), id), latency); err != nil {
		s.trace(operation, key, "canceled", 0, id)
		return 0, err
	}
	return id, nil
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
		Resource:  key.String(),
		Operation: string(operation),
		Outcome:   outcome,
		Bytes:     bytes,
		LocalID:   id,
	})
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
}

func (r *objectReader) Read(destination []byte) (int, error) {
	if r.closed {
		return 0, platform.ErrClosed
	}
	n, err := r.reader.Read(destination)
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
