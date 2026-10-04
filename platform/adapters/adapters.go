// Package adapters constructs the platform adapters that talk to real
// machines: the host filesystem, TCP, and the object stores a deployment runs
// on. The adapters themselves live under this package's parent's internal
// directory, so nothing outside platform can name one, and every constructor
// here returns a port. Choosing an implementation is therefore something a
// process does in one place, and no package can come to depend on a concrete
// adapter's own surface.
//
// The constructors are here rather than in platform because the adapters
// import platform for the ports they implement, which platform cannot import
// back. The simulated adapters are a package of their own for a different
// reason: tests drive the real implementations over them rather than choosing
// between them at startup.
package adapters

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/bounded"
	"github.com/semistrict/sproutfs/platform/internal/real"
)

// NewDisk opens the host filesystem rooted at root. The root must exist.
func NewDisk(root string) (platform.Disk, error) { return real.NewDisk(root) }

// NewDeviceWrites reads the bytes written by the block device that holds dir.
// Only Linux has it, and only for a directory on a block device.
func NewDeviceWrites(ctx context.Context, dir string) (platform.DeviceWrites, error) {
	return real.NewDeviceWrites(ctx, dir)
}

// NewNetwork returns framed connections over plain TCP between the addresses a
// deployment's hosts listen on. It authenticates no peer.
func NewNetwork() platform.Network { return NewNetworkOver(TCP()) }

// NewNetworkOver returns framed connections over a transport a deployment
// supplies: the fabric its hosts already authenticate one another on.
func NewNetworkOver(transport platform.Transport) platform.Network {
	return real.NewNetwork(real.NetworkConfig{Transport: transport})
}

// TCP is the default transport: plain TCP, which authenticates no peer. A
// transport of a deployment's own can wrap it.
func TCP() platform.Transport { return real.TCP{} }

// newGCS serves a bucket and prefix through Google Cloud Storage. An empty
// endpoint uses the ambient Google credentials; a non-empty one points the
// client at an emulator and sends none. The returned closer releases the
// client, and the store must not be used after it.
func newGCS(ctx context.Context, endpoint, bucket, prefix string) (platform.ObjectStore, io.Closer, error) {
	client, err := real.NewGCSClient(ctx, endpoint)
	if err != nil {
		return nil, nil, err
	}
	store, err := real.NewGCSObjectStore(client, bucket, prefix)
	if err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	return store, client, nil
}

// newS3 serves a bucket and prefix through Amazon S3, with the ambient AWS
// configuration. A non-empty endpoint points the client at an S3-compatible
// server instead. The returned closer releases nothing: an S3 client holds no
// resources of its own. It is returned so a command closes every store alike.
func newS3(ctx context.Context, endpoint, bucket, prefix string) (platform.ObjectStore, io.Closer, error) {
	client, err := real.NewS3Client(ctx, endpoint)
	if err != nil {
		return nil, nil, err
	}
	store, err := real.NewS3ObjectStore(client, bucket, prefix)
	if err != nil {
		return nil, nil, err
	}
	return store, noClose{}, nil
}

// NewGCENetworkDisks reaches Compute Engine's persistent disks and Hyperdisks
// with the ambient Google credentials. A volume is named by its CSI volume
// handle, projects/<project>/zones/<zone>/disks/<name>, or by its name alone in
// project and zone; a machine is an instance's name. A non-empty endpoint
// points the client at a server that answers for the API.
func NewGCENetworkDisks(ctx context.Context, project, zone, endpoint string) (platform.NetworkDisks, error) {
	return real.NewGCEDisks(ctx, project, zone, endpoint)
}

// NewGCEDevices opens the Compute Engine disks attached to this instance, at
// google-<name> in dir, the kernel's /dev/disk/by-id where dir is empty, each
// for this process alone. A container that sees the node's /dev mounted
// elsewhere names that mount's disk/by-id. Only Linux has them.
func NewGCEDevices(dir string) platform.Devices {
	if dir == "" {
		dir = "/dev/disk/by-id"
	}
	return real.NewDevices(dir, "google-", real.GCEDeviceName)
}

// NewEBSDevices opens the EBS volumes attached to this Nitro instance, each by
// its volume ID (vol-0123abcd), at nvme-Amazon_Elastic_Block_Store_vol0123abcd
// in dir, the kernel's /dev/disk/by-id where dir is empty, each for this
// process alone. Only Linux has them.
func NewEBSDevices(dir string) platform.Devices {
	if dir == "" {
		dir = "/dev/disk/by-id"
	}
	return real.NewDevices(dir, real.EBSDevicePrefix, real.EBSDeviceName)
}

// noClose is a closer with nothing to release.
type noClose struct{}

func (noClose) Close() error { return nil }

// ObjectStoreConfig names the object store a deployment runs on: its provider,
// its bucket and prefix, an emulator's endpoint where one stands in for the
// provider, and the bounds every request to it waits within.
type ObjectStoreConfig struct {
	// Provider is "gcs" or "s3". Empty is "gcs".
	Provider                 string
	Endpoint, Bucket, Prefix string
	// Bounds is how long a request waits for the store's first byte and
	// between two bytes of a body. Zero bounds are the defaults.
	Bounds bounded.Bounds
}

// ObjectStoreFromEnvironment reads the object store a command runs on:
// SPROUTFS_OBJECT_STORE, which is gcs or s3 and gcs when unset;
// SPROUTFS_BUCKET, which is required; SPROUTFS_PREFIX; the provider's
// emulator endpoint, SPROUTFS_GCS_ENDPOINT or SPROUTFS_S3_ENDPOINT; and the
// bounds of its requests, SPROUTFS_STORE_FIRST_BYTE_TIMEOUT and
// SPROUTFS_STORE_STALL_TIMEOUT, each a duration such as 10s, and the
// default when unset. Every command of a deployment reads the same names
// through here.
func ObjectStoreFromEnvironment(lookup func(string) string) (ObjectStoreConfig, error) {
	text := func(name string) string { return strings.TrimSpace(lookup(name)) }
	config := ObjectStoreConfig{Provider: text("SPROUTFS_OBJECT_STORE"), Bucket: text("SPROUTFS_BUCKET"),
		Prefix: text("SPROUTFS_PREFIX")}
	var errs []error
	bound := func(name string) time.Duration {
		value := text(name)
		if value == "" {
			return 0
		}
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			errs = append(errs, fmt.Errorf("%s is %q, want a positive duration such as 10s", name, value))
			return 0
		}
		return parsed
	}
	config.Bounds = bounded.Bounds{FirstByte: bound("SPROUTFS_STORE_FIRST_BYTE_TIMEOUT"),
		Stall: bound("SPROUTFS_STORE_STALL_TIMEOUT")}
	switch config.Provider {
	case "", "gcs":
		config.Provider = "gcs"
		config.Endpoint = text("SPROUTFS_GCS_ENDPOINT")
	case "s3":
		config.Endpoint = text("SPROUTFS_S3_ENDPOINT")
	default:
		errs = append(errs, fmt.Errorf("SPROUTFS_OBJECT_STORE is %q, want gcs or s3", config.Provider))
	}
	if config.Bucket == "" {
		errs = append(errs, errors.New("SPROUTFS_BUCKET is required"))
	}
	return config, errors.Join(errs...)
}

// ObjectStoreFromURL reads the object store a URL names: gs://bucket/prefix
// for Google Cloud Storage and s3://bucket/prefix for Amazon S3 or an
// S3-compatible server. The prefix may be empty. An endpoint query parameter
// points the client at an emulator or an S3-compatible server, as
// SPROUTFS_GCS_ENDPOINT and SPROUTFS_S3_ENDPOINT do for the deployment's own
// store. Nothing else in the URL is read, so a URL naming more is refused.
func ObjectStoreFromURL(raw string) (ObjectStoreConfig, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ObjectStoreConfig{}, fmt.Errorf("object store URL %q: %w", raw, err)
	}
	config := ObjectStoreConfig{Bucket: parsed.Host, Prefix: strings.TrimPrefix(parsed.Path, "/"),
		Endpoint: parsed.Query().Get("endpoint")}
	switch parsed.Scheme {
	case "gs":
		config.Provider = "gcs"
	case "s3":
		config.Provider = "s3"
	default:
		return ObjectStoreConfig{}, fmt.Errorf("object store URL %q: want gs://bucket/prefix or s3://bucket/prefix", raw)
	}
	query := parsed.Query()
	query.Del("endpoint")
	if config.Bucket == "" || parsed.User != nil || parsed.Port() != "" || parsed.Fragment != "" || len(query) > 0 {
		return ObjectStoreConfig{}, fmt.Errorf("object store URL %q: want a bucket, a prefix and at most an endpoint", raw)
	}
	return config, nil
}

// NewObjectStore opens the object store a configuration names, with every
// request to it under the configuration's bounds on the wall clock. It is the
// only way a process opens one, so no process reaches a store with a request
// that may wait on it for ever. The returned closer releases the provider's
// client, and the store must not be used after it.
func NewObjectStore(ctx context.Context, config ObjectStoreConfig) (*bounded.Store, io.Closer, error) {
	if err := config.Bounds.Validate(); err != nil {
		return nil, nil, err
	}
	var store platform.ObjectStore
	var closer io.Closer
	var err error
	switch config.Provider {
	case "", "gcs":
		store, closer, err = newGCS(ctx, config.Endpoint, config.Bucket, config.Prefix)
	case "s3":
		store, closer, err = newS3(ctx, config.Endpoint, config.Bucket, config.Prefix)
	default:
		return nil, nil, fmt.Errorf("object store provider %q: want gcs or s3", config.Provider)
	}
	if err != nil {
		return nil, nil, err
	}
	bound, err := bounded.New(store, config.Bounds, nil)
	if err != nil {
		return nil, nil, errors.Join(err, closer.Close())
	}
	return bound, closer, nil
}

// NewClock returns the operating system's clock: the one a deployment runs on.
// It is named here with the other adapters so a command chooses it in the same
// place, though the implementation is in platform itself — a wall clock binds
// to nothing outside the standard library, and it is what every configuration's
// nil Clock already means.
func NewClock() platform.Clock { return platform.WallClock() }

// NewEntropy returns the operating system's entropy, which is what a writer
// nonce and a checkpoint interval's jitter are drawn from outside a simulation.
func NewEntropy() platform.Entropy { return platform.SystemEntropy() }
