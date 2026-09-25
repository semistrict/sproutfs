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
	"io"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/internal/real"
)

// NewDisk opens the host filesystem rooted at root. The root must exist.
func NewDisk(root string) (platform.Disk, error) { return real.NewDisk(root) }

// NewNetwork returns the TCP transport: framed connections between the
// addresses a deployment's hosts listen on.
func NewNetwork() platform.Network { return real.NewNetwork(real.NetworkConfig{}) }

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

// NewClock returns the operating system's clock: the one a deployment runs on.
// It is named here with the other adapters so a command chooses it in the same
// place, though the implementation is in platform itself — a wall clock binds
// to nothing outside the standard library, and it is what every configuration's
// nil Clock already means.
func NewClock() platform.Clock { return platform.WallClock() }

// NewEntropy returns the operating system's entropy, which is what a writer
// nonce and a checkpoint interval's jitter are drawn from outside a simulation.
func NewEntropy() platform.Entropy { return platform.SystemEntropy() }
