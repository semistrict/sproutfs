package real_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/fsouza/fake-gcs-server/fakestorage"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/internal/real"
)

const gcsTestBucket = "sproutfs-demo"

func TestGCSObjectStoreConformance(t *testing.T) {
	t.Parallel()
	runObjectStoreConformance(t, newConditionalFakeGCS)
}

// generationDeletes gives the GCS emulator the conditional delete GCS has and
// it does not: a DELETE carrying ifGenerationMatch removes the object only
// while its generation is that one, and fails with 412 otherwise. Every
// request is served one at a time, so the check and the delete are one step.
type generationDeletes struct {
	mu   sync.Mutex
	next http.Handler
}

func (g *generationDeletes) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if want := r.URL.Query().Get("ifGenerationMatch"); r.Method == http.MethodDelete && want != "" {
		metadata := httptest.NewRecorder()
		g.next.ServeHTTP(metadata, httptest.NewRequestWithContext(r.Context(), http.MethodGet, r.URL.Path, nil))
		if metadata.Code == http.StatusNotFound {
			http.Error(w, `{"error":{"code":404,"message":"No such object"}}`, http.StatusNotFound)
			return
		}
		var object struct {
			Generation string `json:"generation"`
		}
		if err := json.Unmarshal(metadata.Body.Bytes(), &object); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if object.Generation != want {
			http.Error(w, `{"error":{"code":412,"message":"conditionNotMet"}}`, http.StatusPreconditionFailed)
			return
		}
	}
	g.next.ServeHTTP(w, r)
}

// newConditionalFakeGCS serves an empty bucket from the emulator, with the
// conditional delete above, through the client a deployment opens for an
// emulator endpoint.
func newConditionalFakeGCS(t *testing.T) platform.ObjectStore {
	return newConditionalFakeGCSBehind(t, nil)
}

// newConditionalFakeGCSBehind is newConditionalFakeGCS with front, where it
// is not nil, in front of the emulator.
func newConditionalFakeGCSBehind(t *testing.T, front func(http.Handler) http.Handler) platform.ObjectStore {
	t.Helper()
	server, err := fakestorage.NewServerWithOptions(fakestorage.Options{NoListener: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Stop)
	server.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: gcsTestBucket})
	var handler http.Handler = &generationDeletes{next: server.HTTPHandler()}
	if front != nil {
		handler = front(handler)
	}
	listener := httptest.NewServer(handler)
	t.Cleanup(listener.Close)
	client, err := real.NewGCSClient(t.Context(), listener.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	store, err := real.NewGCSObjectStore(client, gcsTestBucket, "cluster")
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestGCSObjectStoreUsesTheGenerationAsETag(t *testing.T) {
	t.Parallel()
	server, store := newFakeGCS(t, fakestorage.Options{NoListener: true})
	key := objectKey(t, "control")
	written, err := store.Put(t.Context(), putRequest(key, "one", platform.PutConditions{IfNoneMatch: true}))
	if err != nil {
		t.Fatal(err)
	}
	object, err := server.GetObject(gcsTestBucket, "cluster/control")
	if err != nil {
		t.Fatal(err)
	}
	if written.Metadata.ETag != platform.ETag(strconv.FormatInt(object.Generation, 10)) {
		t.Fatalf("ETag = %q, want generation %d", written.Metadata.ETag, object.Generation)
	}
}

// The demo points the host at an emulator through the endpoint option rather
// than through process environment, so the option itself is exercised here.
func TestGCSClientTargetsAnEmulatorEndpoint(t *testing.T) {
	t.Parallel()
	server, err := fakestorage.NewServerWithOptions(fakestorage.Options{Scheme: "http"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Stop)
	server.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: gcsTestBucket})
	client, err := real.NewGCSClient(t.Context(), server.URL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	store, err := real.NewGCSObjectStore(client, gcsTestBucket, "cluster")
	if err != nil {
		t.Fatal(err)
	}
	key := objectKey(t, "control")
	if _, err := store.Put(t.Context(), putRequest(key, "one", platform.PutConditions{IfNoneMatch: true})); err != nil {
		t.Fatal(err)
	}
	result, err := store.Get(t.Context(), platform.GetRequest{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Body.Close()
	body, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "one" {
		t.Fatalf("body = %q, want \"one\"", string(body))
	}
}

func newFakeGCS(t *testing.T, options fakestorage.Options) (*fakestorage.Server, *real.GCSObjectStore) {
	t.Helper()
	server, err := fakestorage.NewServerWithOptions(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Stop)
	server.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: gcsTestBucket})
	client := server.Client()
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	store, err := real.NewGCSObjectStore(client, gcsTestBucket, "cluster")
	if err != nil {
		t.Fatal(err)
	}
	return server, store
}
