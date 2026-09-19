package main

import (
	"strings"
	"testing"
)

// TestAWitnessFileOnPMEMMustBeDAX: the guest's disk is a PMEM device so that
// the guest maps the host's resident pages and keeps no page cache of its own.
// A witness file on such a device that the kernel does not report as DAX is a
// guest holding a second copy of what the host already shares, and the witness
// is the one thing in a production guest placed to say so.
func TestAWitnessFileOnPMEMMustBeDAX(t *testing.T) {
	err := daxRequired("/root/witness.dat", "pmem0", 0)
	if err == nil {
		t.Fatal("a file on pmem0 that is not DAX was accepted")
	}
	if !strings.Contains(err.Error(), "/root/witness.dat is on pmem0 and is not DAX") {
		t.Fatalf("error %q", err)
	}
}

// TestAWitnessFileOnPMEMThatIsDAXIsAccepted.
func TestAWitnessFileOnPMEMThatIsDAXIsAccepted(t *testing.T) {
	if err := daxRequired("/root/witness.dat", "pmem0", statxAttrDAX); err != nil {
		t.Fatal(err)
	}
}

// TestAWitnessFileOffPMEMIsNotAskedForDAX: a witness run on a developer's
// machine, or over a tmpfs, has no PMEM under it and nothing to require.
func TestAWitnessFileOffPMEMIsNotAskedForDAX(t *testing.T) {
	for _, device := range []string{"vda", "nvme0n1p2", "", "dm-0"} {
		if err := daxRequired("/tmp/witness.dat", device, 0); err != nil {
			t.Fatalf("device %q: %v", device, err)
		}
	}
}
