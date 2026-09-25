//go:build !(linux && (amd64 || arm64))

package host

import (
	"context"
	"errors"
	"runtime"
)

// Start refuses to pretend elsewhere. Managed VM memory is Linux userfaultfd
// over a HugeTLB arena, and the VMM is x86_64 or aarch64 Firecracker; every
// other platform builds the API and nothing behind it.
func Start(context.Context, SupervisorConfig) (Service, error) {
	return nil, errors.New("host: running VMs requires linux/amd64 or linux/arm64, not " +
		runtime.GOOS + "/" + runtime.GOARCH)
}
