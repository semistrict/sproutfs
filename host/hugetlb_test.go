package host

import (
	"errors"
	"math"
	"testing"
	"testing/fstest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/vmmemory"
)

// node is the files a host reads its share of the 2 MiB pool from: a pod's
// container cgroup under its pod's, each with its own bound, and a pool of
// pool pages.
func node(container, pod, pool string) fstest.MapFS {
	files := fstest.MapFS{
		"proc/self/cgroup": {Data: []byte("0::/kubepods.slice/pod1.slice/cri-1.scope\n")},
		"sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages": {Data: []byte(pool + "\n")},
	}
	if container != "" {
		files["sys/fs/cgroup/kubepods.slice/pod1.slice/cri-1.scope/hugetlb.2MB.max"] = &fstest.MapFile{Data: []byte(container + "\n")}
	}
	if pod != "" {
		files["sys/fs/cgroup/kubepods.slice/pod1.slice/hugetlb.2MB.max"] = &fstest.MapFile{Data: []byte(pod + "\n")}
	}
	return files
}

// The bound a host may take is the least of its own cgroup's and every one
// above it: a pod's allotment is set on the pod's cgroup, and a container
// under it may say max.
func TestAHostsHugeTLBAllotmentIsTheLeastOfItsCgroups(t *testing.T) {
	for _, c := range []struct {
		name           string
		container, pod string
		want           int64
	}{
		{"the pod bounds it", "max", "7516192768", 7516192768},
		{"the container bounds it", "4294967296", "7516192768", 4294967296},
		{"no cgroup bounds it", "max", "", math.MaxInt64},
		{"no cgroup has the controller", "", "", math.MaxInt64},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, err := readHugeTLB(node(c.container, c.pod, "8704"))
			if err != nil {
				t.Fatal(err)
			}
			if h.allotment != c.want || h.pool != 8704*checkpoint.PageSize2MiB {
				t.Fatalf("allotment %d and pool %d, want %d and %d", h.allotment, h.pool, c.want, 8704*checkpoint.PageSize2MiB)
			}
		})
	}
}

// Arenas of 2 MiB pages are huge pages, taken as a guest first writes them, so
// arenas the pod's allotment or the node's pool cannot hold would start and run
// until a fault could not allocate its page, and the VM would be lost: seen on
// GCE on 2026-10-08, with a 7 GiB allotment under 16.25 GiB of arenas. The host
// refuses to start instead, and says both numbers.
func TestAHostRefusesArenasItsHugeTLBShareCannotHold(t *testing.T) {
	gib := int64(1) << 30
	ram := vmmemory.Config{PageSize: checkpoint.PageSize2MiB, ResidentPages: int(16 * gib / checkpoint.PageSize2MiB)}
	pmem := vmmemory.Config{PageSize: checkpoint.PageSize2MiB, ResidentPages: int(gib / 4 / checkpoint.PageSize2MiB)}
	need := hugeTLBBytes(ram, pmem)
	if need != 16*gib+gib/4 {
		t.Fatalf("the arenas need %d bytes, want %d", need, 16*gib+gib/4)
	}
	small, err := readHugeTLB(node("max", "7516192768", "8704"))
	if err != nil {
		t.Fatal(err)
	}
	err = small.admit(need)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("arenas past the allotment = %v, want %v", err, ErrInvalidConfig)
	}
	want := "host: invalid configuration: the arenas of 2 MiB pages need 17448304640 bytes of HugeTLB pages; " +
		"its cgroup allows 7516192768, and the node's pool holds 18253611008"
	if err.Error() != want {
		t.Fatalf("the refusal says %q, want %q", err, want)
	}
	pool, err := readHugeTLB(node("max", "18253611008", "4096"))
	if err != nil {
		t.Fatal(err)
	}
	want = "host: invalid configuration: the arenas of 2 MiB pages need 17448304640 bytes of HugeTLB pages; " +
		"its cgroup allows 18253611008, and the node's pool holds 8589934592"
	if err := pool.admit(need); err == nil || err.Error() != want {
		t.Fatalf("arenas past the pool = %v, want %q", err, want)
	}
	enough, err := readHugeTLB(node("max", "18253611008", "8704"))
	if err != nil {
		t.Fatal(err)
	}
	if err := enough.admit(need); err != nil {
		t.Fatalf("arenas the allotment and the pool hold were refused: %v", err)
	}
	// Arenas of 4 KiB pages are ordinary memory and need nothing of the pool.
	ram.PageSize, pmem.PageSize = checkpoint.PageSize4KiB, checkpoint.PageSize4KiB
	if need := hugeTLBBytes(ram, pmem); need != 0 {
		t.Fatalf("arenas of 4 KiB pages need %d bytes of the pool, want none", need)
	}
}
