//go:build !(linux && (amd64 || arm64))

package vmmachine

import (
	"context"
	"errors"
	"runtime"
)

// Start refuses: Firecracker runs on linux/amd64 and linux/arm64 only.
func (f *Firecracker) Start(context.Context, *Launch) (VMM, error) {
	return nil, errors.New("vmmachine: Firecracker runs on linux/amd64 or linux/arm64, not " +
		runtime.GOOS + "/" + runtime.GOARCH)
}
