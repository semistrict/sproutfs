package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"hash/fnv"
	"math/rand/v2"
	"runtime"
	"sync"

	"github.com/semistrict/sproutfs/checkpoint"
)

// faultRunBytes is the aligned run one fault of a pager reads, host/pager.go's
// readAheadBytes: four pages at 2 MiB and 2,048 at 4 KiB. A guest's fault in a
// page it has not touched reads the whole run the page is in.
const faultRunBytes = 8 << 20

// The links a page carries: every page names the next page of one cycle
// through all the guest's pages, and the first page of every fault run also
// names the next run of one cycle through all its runs. A chain follows them,
// so where it reads next is known only once it has read the page it is on.
const (
	pageLinkAt = 0
	runLinkAt  = 8
)

// crc32c is what a reader checks the pages it read by. It runs at many GB/s
// on the hosts' CRC32 instructions, so checking a page costs the restore a
// small part of what reading it costs.
var crc32c = crc32.MakeTable(crc32.Castagnoli)

// guest is a published guest's memory: one volume of pages of noise no
// encoder shrinks, drawn from the VM's name and the page, each carrying its
// links. The publisher and the reader each build it from the VM's name and
// size, and so agree on every byte.
type guest struct {
	vm       string
	pageSize uint64
	pages    uint64
	// nextPage is the page each page links to, and nextRun the run each run
	// links to.
	nextPage []uint32
	nextRun  []uint32

	once sync.Once
	// sums is each page's CRC-32C, which a reader checks what it read
	// against.
	sums []uint32
}

// newGuest is the memory of the guest vm: pages pages of pageSize, a whole
// number of fault runs.
func newGuest(vm string, pageSize, pages uint64) (*guest, error) {
	if pageSize != checkpoint.PageSize2MiB && pageSize != checkpoint.PageSize4KiB {
		return nil, fmt.Errorf("a page of %d bytes: want %d or %d", pageSize, checkpoint.PageSize4KiB,
			checkpoint.PageSize2MiB)
	}
	runPages := faultRunBytes / pageSize
	if pages == 0 || pages%runPages != 0 || pages > 1<<32 {
		return nil, fmt.Errorf("%d pages of %d bytes: want a whole number of %d-page runs", pages, pageSize, runPages)
	}
	seed := fnv.New64a()
	seed.Write([]byte(vm))
	random := rand.New(rand.NewPCG(seed.Sum64(), pageSize))
	return &guest{vm: vm, pageSize: pageSize, pages: pages, nextPage: cycle(random, pages),
		nextRun: cycle(random, pages/runPages)}, nil
}

// cycle is one cycle through n elements in an order random draws: element i
// links to cycle[i], and following the links from any element visits every
// element once before it comes back. It is Sattolo's shuffle.
func cycle(random *rand.Rand, n uint64) []uint32 {
	order := make([]uint32, n)
	for at := range order {
		order[at] = uint32(at)
	}
	for at := n - 1; at > 0; at-- {
		other := random.Uint64N(at)
		order[at], order[other] = order[other], order[at]
	}
	return order
}

// runPages is how many pages one fault run holds.
func (g *guest) runPages() uint64 { return faultRunBytes / g.pageSize }

// unitPages is how many pages one read of unit takes.
func (g *guest) unitPages(unit string) uint64 {
	if unit == unitRun {
		return g.runPages()
	}
	return 1
}

// units is how many reads of unit the guest's memory holds.
func (g *guest) units(unit string) uint64 { return g.pages / g.unitPages(unit) }

// ReadPage is a page of the guest's memory: noise, with its links.
func (g *guest) ReadPage(_ context.Context, _ string, page uint64, dst []byte) error {
	g.noise(page, dst)
	binary.LittleEndian.PutUint64(dst[pageLinkAt:], uint64(g.nextPage[page]))
	if page%g.runPages() == 0 {
		binary.LittleEndian.PutUint64(dst[runLinkAt:], uint64(g.nextRun[page/g.runPages()]))
	}
	return nil
}

// noise fills dst with the page's noise.
func (g *guest) noise(page uint64, dst []byte) {
	state := page*0x9e3779b97f4a7c15 ^ 0xd1b54a32d192ed03
	for _, b := range []byte(g.vm) {
		state = state*31 + uint64(b)
	}
	for at := 0; at+8 <= len(dst); at += 8 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		binary.LittleEndian.PutUint64(dst[at:], state)
	}
}

// sum is the CRC-32C page should read back as. The first call makes every
// page's, on every processor, so no read pays for making what it is checked
// against.
func (g *guest) sum(page uint64) uint32 {
	g.once.Do(func() {
		g.sums = make([]uint32, g.pages)
		workers := runtime.GOMAXPROCS(0)
		var wait sync.WaitGroup
		for worker := range workers {
			wait.Go(func() {
				buffer := make([]byte, g.pageSize)
				for at := uint64(worker); at < g.pages; at += uint64(workers) {
					_ = g.ReadPage(context.Background(), volume, at, buffer)
					g.sums[at] = crc32.Checksum(buffer, crc32c)
				}
			})
		}
		wait.Wait()
	})
	return g.sums[page]
}

// link is the unit a read of unit links to, from the bytes it read.
func link(unit string, read []byte) uint64 {
	if unit == unitRun {
		return binary.LittleEndian.Uint64(read[runLinkAt:])
	}
	return binary.LittleEndian.Uint64(read[pageLinkAt:])
}
