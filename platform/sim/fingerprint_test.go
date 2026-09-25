package sim_test

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// errand is a small deterministic piece of work over a simulated disk and
// object store, which is what the fingerprints below are taken over. sizes is
// how many bytes each write and each put carries.
func errand(t *testing.T, runtime *sim.Runtime, ctx context.Context, sizes []int, readFirst bool) {
	t.Helper()
	disk := runtime.NewDisk("disk", sim.DiskConfig{})
	file, err := disk.Open(ctx, "data", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	key, err := platform.NewObjectKey("object")
	if err != nil {
		t.Fatal(err)
	}
	for index, size := range sizes {
		page := bytes.Repeat([]byte{byte(index + 1)}, size)
		read := func() {
			if _, err := file.ReadAt(ctx, make([]byte, size), 0); err != nil {
				t.Fatal(err)
			}
		}
		if readFirst && index > 0 {
			read()
		}
		if _, err := file.WriteAt(ctx, page, 0); err != nil {
			t.Fatal(err)
		}
		if !readFirst && index > 0 {
			read()
		}
		if _, err := runtime.ObjectStore().Put(ctx, platform.PutRequest{
			Key: key, Body: bytes.NewReader(page), Size: int64(len(page))}); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Sync(ctx); err != nil {
		t.Fatal(err)
	}
}

func fingerprints(t *testing.T, sizes []int, readFirst bool) (strict, work uint64) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 1})
		errand(t, runtime, t.Context(), sizes, readFirst)
		strict, work = runtime.Fingerprint(), runtime.WorkFingerprint(nil)
	})
	return strict, work
}

// The same work at the same simulated instants digests the same, and more work
// does not.
func TestFingerprintFollowsWhatTheDependenciesDid(t *testing.T) {
	strict, work := fingerprints(t, []int{64, 64}, false)
	again, workAgain := fingerprints(t, []int{64, 64}, false)
	if strict != again || work != workAgain {
		t.Fatalf("one workload digested as %#x/%#x then %#x/%#x", strict, work, again, workAgain)
	}
	longer, longerWork := fingerprints(t, []int{64, 64, 64}, false)
	if longer == strict || longerWork == work {
		t.Fatal("an extra write, read and put digested the same")
	}
	larger, largerWork := fingerprints(t, []int{64, 128}, false)
	if larger == strict || largerWork == work {
		t.Fatal("a write and a put of twice the bytes digested the same")
	}
}

// The strict fingerprint is what only a harness that controls completion order
// can promise: it sees the order one resource's own operations arrived in, and
// the simulated instant each of them finished at. The work fingerprint sees
// neither, which is exactly why a campaign that races two callers can assert it
// where the strict one would be a coin toss.
func TestTheTwoFingerprintsDifferOnOrderWithinAResource(t *testing.T) {
	writeFirst, writeFirstWork := fingerprints(t, []int{64, 64}, false)
	readFirst, readFirstWork := fingerprints(t, []int{64, 64}, true)
	if writeFirst == readFirst {
		t.Fatal("one file's reads and writes in a different order digested the same strictly")
	}
	if writeFirstWork != readFirstWork {
		t.Fatal("the same operations in a different order did not digest as the same work")
	}
}

// A campaign that has to exclude a class of event says so with a predicate, and
// what it excludes is then invisible to the digest and nothing else is.
func TestWorkFingerprintKeepsOnlyWhatTheCallerSelects(t *testing.T) {
	runtime := sim.New(sim.Config{Seed: 1})
	synctest.Test(t, func(t *testing.T) {
		errand(t, runtime, t.Context(), []int{64, 64}, false)
	})
	notADisk := func(e sim.Event) bool { return e.Kind != "disk" }
	if runtime.WorkFingerprint(notADisk) == runtime.WorkFingerprint(nil) {
		t.Fatal("excluding every disk event changed nothing")
	}
	everything := func(sim.Event) bool { return true }
	if runtime.WorkFingerprint(everything) != runtime.WorkFingerprint(nil) {
		t.Fatal("a predicate that keeps every event digested differently from nil")
	}
}

// A buggified site that fired is part of what the run did, so two runs of one
// seed that injected different faults cannot look alike.
func TestFingerprintCoversTheFaultsThatFired(t *testing.T) {
	digest := func(buggify bool) uint64 {
		var value uint64
		synctest.Test(t, func(t *testing.T) {
			runtime := sim.New(sim.Config{Seed: 1, Buggify: buggify})
			ctx := sim.WithRuntime(t.Context(), runtime)
			for range 4 {
				sim.Buggify(ctx, "vmmemory/evict-past-a-free-slot", 0.5)
			}
			value = runtime.Fingerprint()
		})
		return value
	}
	if digest(true) == digest(false) {
		t.Fatal("a run whose sites fired digested like a run with the switch off")
	}
}

// An empty trace has a fingerprint of its own rather than a zero nobody notices.
func TestAnEmptyTraceStillHasAFingerprint(t *testing.T) {
	runtime := sim.New(sim.Config{Seed: 1})
	if runtime.Fingerprint() == 0 {
		t.Fatal("an empty trace digested as zero")
	}
	if runtime.Fingerprint() != runtime.Fingerprint() {
		t.Fatal("the digest of an empty trace is not stable")
	}
}
