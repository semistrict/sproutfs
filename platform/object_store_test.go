package platform_test

import (
	"bytes"
	"errors"
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
