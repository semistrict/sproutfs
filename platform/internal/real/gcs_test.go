package real_test

import (
	"io"
	"strconv"
	"testing"

	"github.com/fsouza/fake-gcs-server/fakestorage"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/internal/real"
)

const gcsTestBucket = "sproutfs-demo"

func TestGCSObjectStoreConformance(t *testing.T) {
	t.Parallel()
	runObjectStoreConformance(t, func(t *testing.T) platform.ObjectStore {
		_, store := newFakeGCS(t, fakestorage.Options{NoListener: true})
		return store
	})
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
