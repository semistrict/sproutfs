package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/platform/adapters"
	"github.com/semistrict/sproutfs/platform/bounded"
)

// A hot tier is named by a URL alone: its provider by the scheme, its bucket,
// its prefix, and an endpoint where an emulator or an S3-compatible server
// stands in. Unset, there is none. A URL naming anything else is refused.
func TestConfigReadsTheHotTier(t *testing.T) {
	for _, test := range []struct {
		value string
		want  *adapters.ObjectStoreConfig
	}{
		{"", nil},
		{"gs://hot-us-east4-a/sproutfs", &adapters.ObjectStoreConfig{Provider: "gcs", Bucket: "hot-us-east4-a",
			Prefix: "sproutfs"}},
		{"gs://hot", &adapters.ObjectStoreConfig{Provider: "gcs", Bucket: "hot"}},
		{"s3://hot/deployment/a?endpoint=http://127.0.0.1:9000", &adapters.ObjectStoreConfig{Provider: "s3",
			Bucket: "hot", Prefix: "deployment/a", Endpoint: "http://127.0.0.1:9000"}},
		// An S3 Express One Zone directory bucket in the hosts' zone.
		{"s3://hot--use1-az4--x-s3/sproutfs", &adapters.ObjectStoreConfig{Provider: "s3",
			Bucket: "hot--use1-az4--x-s3", Prefix: "sproutfs"}},
	} {
		values := minimal()
		if test.value != "" {
			values["SPROUTFS_HOT_TIER"] = test.value
		}
		config, err := loadConfig(environ(values))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(config.HotTier, test.want) {
			t.Fatalf("SPROUTFS_HOT_TIER %q configured %+v, want %+v", test.value, config.HotTier, test.want)
		}
	}
	for _, value := range []string{"https://hot/p", "gs:///p", "gs://hot/p?region=us", "s3://user@hot/p",
		"gs://hot:443/p", "hot"} {
		values := minimal()
		values["SPROUTFS_HOT_TIER"] = value
		want := `SPROUTFS_HOT_TIER: object store URL "` + value + `"`
		if _, err := loadConfig(environ(values)); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("a hot tier of %q configured a host: %v", value, err)
		}
	}
}

// A hot tier's requests wait within the bounds the deployment's own bucket
// does, and a bound that is not a positive duration is refused by name.
func TestConfigBoundsTheHotTierAsTheDeploymentsBucket(t *testing.T) {
	values := minimal()
	values["SPROUTFS_HOT_TIER"] = "gs://hot/sproutfs"
	values["SPROUTFS_STORE_FIRST_BYTE_TIMEOUT"] = "3s"
	values["SPROUTFS_STORE_STALL_TIMEOUT"] = "1500ms"
	config, err := loadConfig(environ(values))
	if err != nil {
		t.Fatal(err)
	}
	want := bounded.Bounds{FirstByte: 3 * time.Second, Stall: 1500 * time.Millisecond}
	if config.Store.Bounds != want || config.HotTier.Bounds != want {
		t.Fatalf("bounds %+v and hot tier bounds %+v, want %+v for both", config.Store.Bounds,
			config.HotTier.Bounds, want)
	}
	for _, value := range []string{"0s", "-1s", "ten seconds"} {
		values := minimal()
		values["SPROUTFS_STORE_STALL_TIMEOUT"] = value
		want := `SPROUTFS_STORE_STALL_TIMEOUT is "` + value + `", want a positive duration such as 10s`
		if _, err := loadConfig(environ(values)); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("a stall bound of %q configured a host: %v", value, err)
		}
	}
}

// The hot tier and the cluster cache are alternatives. A host given a hot
// tier and a cluster share above zero refuses to start, and names the two
// settings to choose between. A share of zero beside a hot tier is no cluster
// cache, and starts.
func TestConfigRefusesAHotTierBesideTheClusterCache(t *testing.T) {
	values := minimal()
	values["SPROUTFS_HOT_TIER"] = "gs://hot/sproutfs"
	values["SPROUTFS_CACHE_CLUSTER_PERCENT"] = "25"
	want := "SPROUTFS_HOT_TIER and SPROUTFS_CACHE_CLUSTER_PERCENT=25 are both set: the hot tier and the " +
		"cluster cache are alternatives; unset one of them"
	if _, err := loadConfig(environ(values)); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("a hot tier beside a cluster share of 25%% configured a host: %v", err)
	}
	values["SPROUTFS_CACHE_CLUSTER_PERCENT"] = "0"
	config, err := loadConfig(environ(values))
	if err != nil {
		t.Fatal(err)
	}
	hot := &adapters.ObjectStoreConfig{Provider: "gcs", Bucket: "hot", Prefix: "sproutfs"}
	if !reflect.DeepEqual(config.HotTier, hot) || config.CacheClusterPercent != 0 {
		t.Fatalf("a hot tier beside a cluster share of 0 configured %+v and %d%%", config.HotTier,
			config.CacheClusterPercent)
	}
}
