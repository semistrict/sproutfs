package vmmemory

import (
	"testing"
	"unsafe"
)

// TestTheZirconCoresPerPageValuesKeepTheirSizes holds what the zircon core
// keeps for each page a region touches to the sizes it was measured at. A
// fault at random makes a frame and a binding, and both live as long as the
// page: a field that pushes either into the next size class costs every page
// of every region, in memory and in the garbage collector's marking.
func TestTheZirconCoresPerPageValuesKeepTheirSizes(t *testing.T) {
	if got := unsafe.Sizeof(zframe{}); got != 96 {
		t.Errorf("a frame is %d bytes, want 96", got)
	}
	if got := unsafe.Sizeof(zbinding{}); got != 64 {
		t.Errorf("a binding is %d bytes, want 64", got)
	}
}
