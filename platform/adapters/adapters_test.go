package adapters_test

import (
	"testing"
	"time"

	"github.com/semistrict/sproutfs/platform/adapters"
	"github.com/semistrict/sproutfs/platform/bounded"
)

func environment(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// A deployment names its provider once, and each provider reads its own
// emulator endpoint: a GCS emulator's address is never handed to S3.
func TestObjectStoreFromEnvironment(t *testing.T) {
	for _, test := range []struct {
		name   string
		values map[string]string
		want   adapters.ObjectStoreConfig
	}{
		{name: "gcs by default",
			values: map[string]string{"SPROUTFS_BUCKET": "b", "SPROUTFS_PREFIX": "p/",
				"SPROUTFS_GCS_ENDPOINT": "localhost:4443", "SPROUTFS_S3_ENDPOINT": "http://localhost:9000"},
			want: adapters.ObjectStoreConfig{Provider: "gcs", Bucket: "b", Prefix: "p/", Endpoint: "localhost:4443"}},
		{name: "s3",
			values: map[string]string{"SPROUTFS_OBJECT_STORE": "s3", "SPROUTFS_BUCKET": "b",
				"SPROUTFS_GCS_ENDPOINT": "localhost:4443", "SPROUTFS_S3_ENDPOINT": "http://localhost:9000"},
			want: adapters.ObjectStoreConfig{Provider: "s3", Bucket: "b", Endpoint: "http://localhost:9000"}},
		{name: "bounds",
			values: map[string]string{"SPROUTFS_BUCKET": "b", "SPROUTFS_STORE_FIRST_BYTE_TIMEOUT": "4s",
				"SPROUTFS_STORE_STALL_TIMEOUT": " 250ms "},
			want: adapters.ObjectStoreConfig{Provider: "gcs", Bucket: "b",
				Bounds: bounded.Bounds{FirstByte: 4 * time.Second, Stall: 250 * time.Millisecond}}},
	} {
		got, err := adapters.ObjectStoreFromEnvironment(environment(test.values))
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if got != test.want {
			t.Fatalf("%s: got %+v, want %+v", test.name, got, test.want)
		}
	}
}

func TestObjectStoreFromEnvironmentRefusesWhatItCannotOpen(t *testing.T) {
	_, err := adapters.ObjectStoreFromEnvironment(environment(map[string]string{"SPROUTFS_OBJECT_STORE": "azure"}))
	if err == nil || err.Error() != "SPROUTFS_OBJECT_STORE is \"azure\", want gcs or s3\nSPROUTFS_BUCKET is required" {
		t.Fatalf("got %v", err)
	}
}
