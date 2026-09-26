package platform_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/semistrict/sproutfs/platform"
)

func TestObjectKeyValidation(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"logs/0001.segment", "metadata/log-1", "日本語"} {
		key, err := platform.NewObjectKey(value)
		if err != nil {
			t.Fatalf("NewObjectKey(%q): %v", value, err)
		}
		if key.String() != value {
			t.Fatalf("key.String() = %q, want %q", key.String(), value)
		}
	}
	for _, value := range []string{"", "bad\x00key", strings.Repeat("x", 1025)} {
		if _, err := platform.NewObjectKey(value); !errors.Is(err, platform.ErrInvalidObjectKey) {
			t.Fatalf("NewObjectKey(%q) error = %v, want ErrInvalidObjectKey", value, err)
		}
	}
}

func TestPutRequestRequiresReplayableSizedContent(t *testing.T) {
	t.Parallel()
	key, err := platform.NewObjectKey("segment")
	if err != nil {
		t.Fatal(err)
	}
	valid := platform.PutRequest{Key: key, Body: bytes.NewReader([]byte("data")), Size: 4}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	valid.Body = nil
	if err := valid.Validate(); !errors.Is(err, platform.ErrInvalidRange) {
		t.Fatalf("nil body error = %v, want ErrInvalidRange", err)
	}
}

func TestPutConditionsAreUnambiguous(t *testing.T) {
	t.Parallel()
	etag := platform.ETag("etag")
	conditions := platform.PutConditions{IfMatch: &etag, IfNoneMatch: true}
	if err := conditions.Validate(); !errors.Is(err, platform.ErrPrecondition) {
		t.Fatalf("Validate error = %v, want ErrPrecondition", err)
	}
}

// pagedStore lists what it holds a page of two keys at a time, as a provider
// that caps a listing's page does.
type pagedStore struct {
	platform.ObjectStore
	keys  []string
	pages int
}

func (s *pagedStore) List(_ context.Context, request platform.ListRequest) (platform.ListResult, error) {
	s.pages++
	var result platform.ListResult
	for _, value := range s.keys {
		if !strings.HasPrefix(value, request.Prefix.String()) || value <= request.ContinuationToken {
			continue
		}
		if len(result.Objects) == 2 {
			result.NextContinuationToken = result.Objects[1].Key.String()
			break
		}
		key, err := platform.NewObjectKey(value)
		if err != nil {
			return platform.ListResult{}, err
		}
		result.Objects = append(result.Objects, platform.ObjectMetadata{Key: key, Size: int64(len(value))})
	}
	return result, nil
}

// ListAll follows the continuation token to the end of a listing, visiting
// every object under the prefix once, in order, at a request per page.
func TestListAllVisitsEveryPage(t *testing.T) {
	t.Parallel()
	store := &pagedStore{keys: []string{"a/1", "a/2", "a/3", "a/4", "a/5", "b/1"}}
	prefix, err := platform.NewObjectPrefix("a/")
	if err != nil {
		t.Fatal(err)
	}
	var visited []string
	if err := platform.ListAll(t.Context(), store, prefix, func(object platform.ObjectMetadata) error {
		visited = append(visited, object.Key.String())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a/1", "a/2", "a/3", "a/4", "a/5"}; !slices.Equal(visited, want) {
		t.Fatalf("visited %v, want %v", visited, want)
	}
	if store.pages != 3 {
		t.Fatalf("the listing took %d requests, want a request per page of two", store.pages)
	}
	stop := errors.New("stop")
	store.pages = 0
	if err := platform.ListAll(t.Context(), store, prefix, func(platform.ObjectMetadata) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("a visit's error came back as %v", err)
	}
	if store.pages != 1 {
		t.Fatalf("a visit that failed went on for %d requests, want one", store.pages)
	}
}
