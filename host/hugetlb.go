package host

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/vmmemory"
)

// hugeTLB is what this host may take of the node's 2 MiB HugeTLB pool, in
// bytes: the least its cgroup and every cgroup above it allow, and the pool
// itself. An arena at a 2 MiB page is huge pages, taken one slot at a time as
// its pages are first written, so a host whose arenas are larger than either
// starts and runs until a guest has written enough, and then a fault cannot
// allocate its page and the VM is lost. The host finds out at start instead.
type hugeTLB struct {
	// allotment is math.MaxInt64 where no cgroup bounds it.
	allotment, pool int64
}

// hugeTLBBytes is what the 2 MiB arenas of these pagers take of the pool at
// most: their resident pages.
func hugeTLBBytes(configs ...vmmemory.Config) int64 {
	var need int64
	for _, cfg := range configs {
		if cfg.PageSize == checkpoint.PageSize2MiB {
			need += int64(cfg.ResidentPages) * int64(cfg.PageSize)
		}
	}
	return need
}

// admit refuses arenas that need more of the pool than this host may take.
func (h hugeTLB) admit(need int64) error {
	if need <= min(h.allotment, h.pool) {
		return nil
	}
	allotment := "no cgroup bounds"
	if h.allotment != math.MaxInt64 {
		allotment = fmt.Sprintf("its cgroup allows %d", h.allotment)
	}
	return fmt.Errorf("%w: the arenas of 2 MiB pages need %d bytes of HugeTLB pages; %s, and the node's pool holds %d",
		ErrInvalidConfig, need, allotment, h.pool)
}

// readHugeTLB reads this process's share of the 2 MiB pool from the files
// under root, which is "/" outside a test: the cgroup it is in, from
// /proc/self/cgroup, each hugetlb.2MB.max from that cgroup up to the root
// of the cgroup v2 hierarchy, and the pool's nr_hugepages. A cgroup without
// the hugetlb controller bounds nothing.
func readHugeTLB(root fs.FS) (hugeTLB, error) {
	h := hugeTLB{allotment: math.MaxInt64}
	pages, err := readInt(root, "sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages")
	if err != nil {
		return h, fmt.Errorf("the node's 2 MiB HugeTLB pool: %w", err)
	}
	h.pool = pages * checkpoint.PageSize2MiB
	group, err := cgroupOf(root)
	if err != nil {
		return h, err
	}
	for dir := group; ; dir = path.Dir(dir) {
		limit, err := fs.ReadFile(root, path.Join("sys/fs/cgroup", dir, "hugetlb.2MB.max"))
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return h, fmt.Errorf("cgroup %s: %w", dir, err)
		case strings.TrimSpace(string(limit)) == "max":
		default:
			bound, err := strconv.ParseInt(strings.TrimSpace(string(limit)), 10, 64)
			if err != nil {
				return h, fmt.Errorf("cgroup %s: hugetlb.2MB.max: %w", dir, err)
			}
			h.allotment = min(h.allotment, bound)
		}
		if dir == "." {
			return h, nil
		}
	}
}

// cgroupOf is this process's cgroup v2 path, relative to the hierarchy's root.
func cgroupOf(root fs.FS) (string, error) {
	data, err := fs.ReadFile(root, "proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("this process's cgroup: %w", err)
	}
	lines := bufio.NewScanner(bytes.NewReader(data))
	for lines.Scan() {
		if group, ok := strings.CutPrefix(lines.Text(), "0::/"); ok {
			if group == "" {
				return ".", nil
			}
			return path.Clean(group), nil
		}
	}
	return "", fmt.Errorf("this process's cgroup: no cgroup v2 entry in %q", data)
}

func readInt(root fs.FS, name string) (int64, error) {
	data, err := fs.ReadFile(root, name)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
}

// checkHugeTLB refuses a host whose 2 MiB arenas the node cannot give it.
// Arenas all at 4 KiB take nothing of the pool, and are not checked.
func checkHugeTLB(configs ...vmmemory.Config) error {
	need := hugeTLBBytes(configs...)
	if need == 0 {
		return nil
	}
	h, err := readHugeTLB(os.DirFS("/"))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	return h.admit(need)
}
