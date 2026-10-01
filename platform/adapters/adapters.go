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
	"strings"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/internal/real"
)

// NewDisk opens the host filesystem rooted at root. The root must exist.
func NewDisk(root string) (platform.Disk, error) { return real.NewDisk(root) }

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

// NewGCS serves a bucket and prefix through Google Cloud Storage. An empty
// endpoint uses the ambient Google credentials; a non-empty one points the
// client at an emulator and sends none. The returned closer releases the
// client, and the store must not be used after it.
func NewGCS(ctx context.Context, endpoint, bucket, prefix string) (platform.ObjectStore, io.Closer, error) {
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

// NewS3 serves a bucket and prefix through Amazon S3, with the ambient AWS
// configuration. A non-empty endpoint points the client at an S3-compatible
// server instead. The returned closer releases nothing: an S3 client holds no
// resources of its own. It is returned so a command closes every store alike.
func NewS3(ctx context.Context, endpoint, bucket, prefix string) (platform.ObjectStore, io.Closer, error) {
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

// noClose is a closer with nothing to release.
type noClose struct{}

func (noClose) Close() error { return nil }

// ObjectStoreConfig names the object store a deployment runs on: its provider,
// its bucket and prefix, and an emulator's endpoint where one stands in for
// the provider.
type ObjectStoreConfig struct {
	// Provider is "gcs" or "s3". Empty is "gcs".
	Provider                 string
	Endpoint, Bucket, Prefix string
}

// ObjectStoreFromEnvironment reads the object store a command runs on:
// SPROUTFS_OBJECT_STORE, which is gcs or s3 and gcs when unset;
// SPROUTFS_BUCKET, which is required; SPROUTFS_PREFIX; and the provider's
// emulator endpoint, SPROUTFS_GCS_ENDPOINT or SPROUTFS_S3_ENDPOINT. Every
// command of a deployment reads the same names through here.
func ObjectStoreFromEnvironment(lookup func(string) string) (ObjectStoreConfig, error) {
	text := func(name string) string { return strings.TrimSpace(lookup(name)) }
	config := ObjectStoreConfig{Provider: text("SPROUTFS_OBJECT_STORE"), Bucket: text("SPROUTFS_BUCKET"),
		Prefix: text("SPROUTFS_PREFIX")}
	var errs []error
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

// NewObjectStore opens the object store a configuration names.
func NewObjectStore(ctx context.Context, config ObjectStoreConfig) (platform.ObjectStore, io.Closer, error) {
	switch config.Provider {
	case "", "gcs":
		return NewGCS(ctx, config.Endpoint, config.Bucket, config.Prefix)
	case "s3":
		return NewS3(ctx, config.Endpoint, config.Bucket, config.Prefix)
	}
	return nil, nil, fmt.Errorf("object store provider %q: want gcs or s3", config.Provider)
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
