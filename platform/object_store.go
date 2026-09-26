package platform

import (
	"context"
	"io"
	"maps"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// ObjectKey is a validated, provider-independent object identifier. It is a
// value object rather than a path: slashes have no filesystem semantics.
type ObjectKey struct {
	value string
}

func NewObjectKey(value string) (ObjectKey, error) {
	if value == "" || len(value) > 1024 || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return ObjectKey{}, ErrInvalidObjectKey
	}
	return ObjectKey{value: value}, nil
}

func (k ObjectKey) String() string { return k.value }
func (k ObjectKey) IsZero() bool   { return k.value == "" }

// ObjectPrefix is a validated prefix for paginated object listing. Unlike an
// ObjectKey, the empty prefix is valid and denotes the entire namespace.
type ObjectPrefix struct {
	value string
}

func NewObjectPrefix(value string) (ObjectPrefix, error) {
	if len(value) > 1024 || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return ObjectPrefix{}, ErrInvalidObjectKey
	}
	return ObjectPrefix{value: value}, nil
}

func (p ObjectPrefix) String() string { return p.value }

// ETag is an opaque validator supplied by the object store. Callers may
// compare or pass it back in a condition, but must not infer its algorithm.
type ETag string

type ObjectMetadata struct {
	Key          ObjectKey
	Size         int64
	LastModified time.Time
	ETag         ETag
	// Attributes are immutable provider metadata associated with an object.
	// Adapters must return an independent map that callers may safely mutate.
	Attributes map[string]string
}

// CloneAttributes returns an independent copy of attributes, or nil when empty.
// Adapters use it to satisfy the ownership rule above.
func CloneAttributes(attributes map[string]string) map[string]string {
	if len(attributes) == 0 {
		return nil
	}
	return maps.Clone(attributes)
}

// ByteRange requests Length bytes beginning at Offset, or, in its suffix form,
// the last Suffix bytes of the object and nothing about where they begin. The
// forms are exclusive: Length is positive in the first, and Offset and Length
// are zero in the second. A suffix longer than the object returns the whole
// object, as an HTTP suffix range does, which is what lets a reader fetch a
// bounded tail without first asking how long the object is. A nil range in
// GetRequest means the complete object.
type ByteRange struct {
	Offset int64
	Length int64
	Suffix int64
}

func (r ByteRange) Validate() error {
	if r.Suffix != 0 {
		if r.Suffix < 0 || r.Offset != 0 || r.Length != 0 {
			return ErrInvalidRange
		}
		return nil
	}
	if r.Offset < 0 || r.Length <= 0 || r.Length > math.MaxInt64-r.Offset {
		return ErrInvalidRange
	}
	return nil
}

type PutConditions struct {
	// IfMatch requires the current ETag to match.
	IfMatch *ETag
	// IfNoneMatch requires the object not to exist.
	IfNoneMatch bool
}

func (c PutConditions) Validate() error {
	if c.IfMatch != nil && c.IfNoneMatch {
		return ErrPrecondition
	}
	return nil
}

type GetRequest struct {
	Key   ObjectKey
	Range *ByteRange
}

type GetResult struct {
	Metadata      ObjectMetadata
	ContentLength int64
	Body          io.ReadCloser
}

// PutRequest uses ReaderAt plus an exact size so adapters can retry without
// buffering the entire object. The caller must keep Body valid until Put
// returns.
type PutRequest struct {
	Key         ObjectKey
	Body        io.ReaderAt
	Size        int64
	ContentType string
	Attributes  map[string]string
	Conditions  PutConditions
}

func (r PutRequest) Validate() error {
	if r.Key.IsZero() {
		return ErrInvalidObjectKey
	}
	if r.Body == nil || r.Size < 0 {
		return ErrInvalidRange
	}
	return r.Conditions.Validate()
}

type PutResult struct {
	Metadata ObjectMetadata
}

type DeleteRequest struct {
	Key     ObjectKey
	IfMatch *ETag
}

// ListRequest describes one page of a lexicographically ordered listing.
// ContinuationToken is opaque and may only be reused with the same store and
// prefix. Limit zero selects the adapter's default; positive values are capped
// by the backing provider.
type ListRequest struct {
	Prefix            ObjectPrefix
	ContinuationToken string
	Limit             int32
}

func (r ListRequest) Validate() error {
	if r.Limit < 0 {
		return ErrInvalidRange
	}
	return nil
}

type ListResult struct {
	Objects               []ObjectMetadata
	NextContinuationToken string
}

// ObjectStore is the provider-independent seam for immutable data objects and
// strongly conditional metadata publication used by platform consumers.
type ObjectStore interface {
	Head(context.Context, ObjectKey) (ObjectMetadata, error)
	Get(context.Context, GetRequest) (GetResult, error)
	Put(context.Context, PutRequest) (PutResult, error)
	Delete(context.Context, DeleteRequest) error
	List(context.Context, ListRequest) (ListResult, error)
}

// ListAll visits every object under prefix, in the order the store lists them,
// one page of the listing at a time: a request per page, and nothing else. It
// stops at the first error a page or a visit returns.
func ListAll(ctx context.Context, store ObjectStore, prefix ObjectPrefix, visit func(ObjectMetadata) error) error {
	token := ""
	for {
		page, err := store.List(ctx, ListRequest{Prefix: prefix, ContinuationToken: token})
		if err != nil {
			return err
		}
		for _, object := range page.Objects {
			if err := visit(object); err != nil {
				return err
			}
		}
		if page.NextContinuationToken == "" {
			return nil
		}
		token = page.NextContinuationToken
	}
}

// ReadObjectBody consumes and closes a whole-object Get result. It first
// certifies the response describes the object that was asked for — the echoed
// key, a non-empty validator, a size agreeing with the content length, and a
// length inside [minimum, maximum] — and only then reads the body, over-reading
// by one byte so a body longer than its declared length is detected. Both
// rejections return corrupt so each caller keeps its own sentinel; transport
// errors from the body pass through unchanged.
func ReadObjectBody(result GetResult, key ObjectKey, minimum, maximum int64, corrupt error) ([]byte, error) {
	defer result.Body.Close()
	if result.Metadata.Key != key || result.Metadata.ETag == "" || result.Metadata.Size != result.ContentLength ||
		result.ContentLength < minimum || result.ContentLength > maximum {
		return nil, corrupt
	}
	data, err := io.ReadAll(io.LimitReader(result.Body, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != result.ContentLength {
		return nil, corrupt
	}
	return data, nil
}

// ReadObject fetches key in full and certifies it with ReadObjectBody, also
// returning the metadata callers need for a later compare-and-set. Get errors
// pass through unchanged so callers can still distinguish ErrNotFound.
func ReadObject(ctx context.Context, store ObjectStore, key ObjectKey, minimum, maximum int64, corrupt error) ([]byte, ObjectMetadata, error) {
	result, err := store.Get(ctx, GetRequest{Key: key})
	if err != nil {
		return nil, ObjectMetadata{}, err
	}
	data, err := ReadObjectBody(result, key, minimum, maximum, corrupt)
	if err != nil {
		return nil, ObjectMetadata{}, err
	}
	return data, result.Metadata, nil
}
