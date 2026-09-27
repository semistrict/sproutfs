//go:build !linux || !(amd64 || arm64)

package main

import (
	"fmt"
	"runtime"
)

// The KVM commands stand in here as the rest of grow_elsewhere.go does: a
// guest that runs VMs of its own is Linux on x86_64, or arm64 under Lima.

func unsupported(what string) error {
	return fmt.Errorf("%s is a Linux ioctl on /dev/kvm, and this is %s/%s", what, runtime.GOOS, runtime.GOARCH)
}

func createVM() (string, error)              { return "", unsupported("creating a VM") }
func runL2() (string, error)                 { return "", unsupported("running an L2") }
func offeredControls() (string, error)       { return "", unsupported("reading VMX controls") }
func startL2Loop(kvmOptions) (string, error) { return "", unsupported("running an L2") }
func serveL2Loop(kvmOptions) error           { return unsupported("running an L2") }
