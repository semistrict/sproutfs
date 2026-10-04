package real_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/internal/real"
)

const s3TestBucket = "sproutfs-test"

// asS3 gives gofakes3 two behaviours of S3 it lacks. A DELETE carrying
// If-Match removes the object only while its ETag is that one, and fails with
// 412 otherwise; every request is served one at a time, so the check and the
// delete are one step. And a ranged GET carries no checksum of the whole
// object, which a client would check against the range and fail.
type asS3 struct {
	mu   sync.Mutex
	next http.Handler
}

// rangedReply drops the whole object's checksums from a ranged GET's reply.
type rangedReply struct {
	http.ResponseWriter
	written bool
}

func (r *rangedReply) WriteHeader(status int) {
	if !r.written {
		r.written = true
		for name := range r.Header() {
			if strings.HasPrefix(strings.ToLower(name), "x-amz-checksum") {
				r.Header().Del(name)
			}
		}
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *rangedReply) Write(data []byte) (int, error) {
	if !r.written {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(data)
}

func (c *asS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.Method == http.MethodGet && r.Header.Get("Range") != "" {
		w = &rangedReply{ResponseWriter: w}
		// A suffix longer than the object is the whole object over HTTP, and
		// gofakes3 refuses it.
		if suffix, found := strings.CutPrefix(r.Header.Get("Range"), "bytes=-"); found {
			head := httptest.NewRecorder()
			c.next.ServeHTTP(head, httptest.NewRequestWithContext(r.Context(), http.MethodHead, r.URL.String(), nil))
			length, lengthErr := strconv.ParseInt(head.Header().Get("Content-Length"), 10, 64)
			want, wantErr := strconv.ParseInt(suffix, 10, 64)
			if lengthErr == nil && wantErr == nil && length > 0 && want > length {
				r.Header.Set("Range", "bytes=0-"+strconv.FormatInt(length-1, 10))
			}
		}
	}
	if ifMatch := r.Header.Get("If-Match"); r.Method == http.MethodDelete && ifMatch != "" {
		head := httptest.NewRecorder()
		c.next.ServeHTTP(head, httptest.NewRequestWithContext(r.Context(), http.MethodHead, r.URL.String(), nil))
		if head.Code == http.StatusNotFound {
			http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", http.StatusNotFound)
			return
		}
		if head.Header().Get("ETag") != ifMatch {
			http.Error(w, "<Error><Code>PreconditionFailed</Code></Error>", http.StatusPreconditionFailed)
			return
		}
		r.Header.Del("If-Match")
	}
	c.next.ServeHTTP(w, r)
}

// newFakeS3 serves an empty bucket from an emulator and returns a store over
// it, opened the way a deployment opens one: through the ambient AWS
// configuration, here the environment alone, with the emulator's endpoint.
func newFakeS3(t *testing.T) platform.ObjectStore { return newFakeS3Behind(t, nil) }

// newFakeS3Behind is newFakeS3 with front, where it is not nil, in front of
// the emulator.
func newFakeS3Behind(t *testing.T, front func(http.Handler) http.Handler) platform.ObjectStore {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket(s3TestBucket); err != nil {
		t.Fatal(err)
	}
	var handler http.Handler = &asS3{next: gofakes3.New(backend).Server()}
	if front != nil {
		handler = front(handler)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	isolated := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(isolated, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(isolated, "credentials"))
	t.Setenv("AWS_ACCESS_KEY_ID", "sproutfs")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sproutfs")
	t.Setenv("AWS_REGION", "us-east-1")
	client, err := real.NewS3Client(t.Context(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := real.NewS3ObjectStore(client, s3TestBucket, "cluster")
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestS3ObjectStoreConformance(t *testing.T) {
	runObjectStoreConformance(t, newFakeS3)
}

// The same conformance against a real bucket, which is what says the adapter
// agrees with S3 rather than with an emulator. It runs when
// SPROUTFS_TEST_S3_BUCKET names a bucket the ambient AWS credentials may
// write. Each run writes under a prefix of its own and deletes what it wrote.
func TestS3ObjectStoreConformanceOnABucket(t *testing.T) {
	bucket := os.Getenv("SPROUTFS_TEST_S3_BUCKET")
	if bucket == "" {
		t.Skip("SPROUTFS_TEST_S3_BUCKET is unset")
	}
	client, err := real.NewS3Client(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	runObjectStoreConformance(t, func(t *testing.T) platform.ObjectStore {
		prefix := "sproutfs-conformance/" + filepath.Base(t.TempDir())
		store, err := real.NewS3ObjectStore(client, bucket, prefix)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// The test's own context is cancelled before its cleanups run.
			ctx := context.WithoutCancel(t.Context())
			root, err := platform.NewObjectPrefix("")
			if err != nil {
				t.Error(err)
				return
			}
			var keys []platform.ObjectKey
			if err := platform.ListAll(ctx, store, root, func(object platform.ObjectMetadata) error {
				keys = append(keys, object.Key)
				return nil
			}); err != nil {
				t.Error(err)
			}
			for _, key := range keys {
				if err := store.Delete(ctx, platform.DeleteRequest{Key: key}); err != nil {
					t.Error(err)
				}
			}
		})
		return store
	})
}
