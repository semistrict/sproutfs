package journal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// The campaign's shape: three VMs commit at once to one journal, each trimming
// its entries as a checkpoint would cover them, until the power is lost at a
// point the seed draws. The next holder reads the journal back under a newer
// lease, as a recovery would, and then commits at the next epoch. Four holders
// in turn, and a fifth that only reads back.
const (
	campaignSeeds   = 24
	campaignHolders = 4
	campaignVMs     = 3
	campaignCommits = 10
	campaignRing    = 512 << 10
)

// campaignProbes are the probes the campaign must reach across its seeds. A
// lease lost while the journal is open, and a fenced commit, need a holder
// that keeps running after it is replaced; their own tests reach them.
var campaignProbes = []string{ProbeFormatted, ProbeLeaseRefused, ProbeTornEntry, ProbeFailedRangePadded,
	ProbeRingEndPadded, ProbeFull, ProbeGrouped}

// Under every fault the journal's sites inject, and power losses that tear
// and garble what was not synced, every entry a commit was answered for and
// no checkpoint covered reads back at the position it was answered with.
// Every entry that reads back is one a commit was made of. No position is
// answered twice, and each holder answers past everything answered before.
// Every site fires and every probe is reached across the seeds.
func TestAJournalKeepsEveryAnsweredEntryThroughItsFaults(t *testing.T) {
	fired := make(map[string]uint64)
	reached := make(map[string]uint64)
	for seed := uint64(1); seed <= campaignSeeds; seed++ {
		synctest.Test(t, func(t *testing.T) {
			runtime := sim.New(sim.Config{Seed: seed, Buggify: true})
			c := &campaign{t: t, ctx: sim.WithRuntime(t.Context(), runtime), runtime: runtime,
				attempted: make(map[string]Entry), answered: make(map[uint64]Entry),
				covered: make(map[string]uint64)}
			c.disk, c.handle = device(t, c.ctx, runtime, campaignRing, sim.DiskConfig{PowerLossFaults: true})
			for holder := uint64(1); holder <= campaignHolders; holder++ {
				j := c.open(holder)
				c.commitUntilThePowerIsLost(j, holder)
			}
			c.open(campaignHolders + 1)
			for site, n := range runtime.FiredSites() {
				fired[site] += n
			}
			for probe, n := range runtime.Probes() {
				reached[probe] += n
			}
		})
	}
	t.Logf("sites fired: %v", fired)
	t.Logf("probes reached: %v", reached)
	for _, site := range Sites() {
		if fired[site] == 0 {
			t.Errorf("the site %s never fired", site)
		}
	}
	for _, probe := range campaignProbes {
		if reached[probe] == 0 {
			t.Errorf("the probe %s was never reached", probe)
		}
	}
}

// campaign is one seed's run.
type campaign struct {
	t       *testing.T
	ctx     context.Context
	runtime *sim.Runtime
	disk    *sim.Disk
	handle  func() platform.File

	generation uint64

	mu sync.Mutex
	// attempted is every entry a commit was made of, by its name.
	attempted map[string]Entry
	// answered is every entry a commit was answered for, by its position.
	answered map[uint64]Entry
	// covered is each VM's covered position at the epoch it last ran.
	covered map[string]uint64
	// highest is the highest position answered so far.
	highest uint64
}

func vmName(vm int) string { return fmt.Sprintf("vm-%d", vm) }

func memberOf(holder uint64) rank.Identity { return rank.Identity{0xa0, byte(holder)} }

// open opens the journal as holder, under a lease of its own, as a member the
// membership assigned the disk to at generation holder. Every holder after
// the first reads back each VM's entries of the epochs before its own, checks
// them, and drops them, as the recovery's first checkpoint would. A holder of
// an older lease is then refused.
func (c *campaign) open(holder uint64) *Journal {
	t := c.t
	t.Helper()
	lease := Lease{Assigned: holder, Member: memberOf(holder)}
	var j *Journal
	// A header write can fail under its site, or the device can fail the
	// open's reads and writes, and the open fails with them.
	for attempt := 0; j == nil; attempt++ {
		opened, err := Open(c.ctx, c.handle(), Config{Identity: diskOne, Lease: lease,
			Entropy: c.runtime.NewEntropy(fmt.Sprintf("journal/%d/%d", holder, attempt))})
		switch {
		case err == nil:
			j = opened
		case !deviceFailed(err) || attempt == 10:
			t.Fatalf("holder %d opening the journal: %v", holder, err)
		}
	}
	closeAtEnd(t, c.ctx, j)
	if holder == 1 {
		c.generation = j.Generation()
		return j
	}
	if j.Formatted() || j.Generation() != c.generation {
		t.Fatalf("holder %d found the journal formatted %v under generation %d, want %d", holder, j.Formatted(),
			j.Generation(), c.generation)
	}
	for vm := range campaignVMs {
		for epoch := uint64(1); epoch < holder; epoch++ {
			c.verify(j, vmName(vm), epoch, holder)
		}
		j.Trim(vmName(vm), nil)
	}
	stale := Lease{Assigned: holder - 1, Member: memberOf(holder - 1)}
	_, err := Open(c.ctx, c.handle(), Config{Identity: diskOne, Lease: stale})
	// The device may fail the open's read of the header, which is made again.
	for attempt := 0; deviceFailed(err) && attempt < 10; attempt++ {
		_, err = Open(c.ctx, c.handle(), Config{Identity: diskOne, Lease: stale})
	}
	if !errors.Is(err, ErrLeased) {
		t.Fatalf("opening the journal under the lease before holder %d's: %v, want ErrLeased", holder, err)
	}
	return j
}

// verify reads back vm's entries of epoch as a reader at holder's epoch, and
// checks them against what was committed and answered.
func (c *campaign) verify(j *Journal, vm string, epoch, holder uint64) {
	t := c.t
	t.Helper()
	after := uint64(0)
	if epoch == holder-1 {
		after = c.covered[vm]
	}
	var read map[uint64]bool
	var err error
	// A read the device fails is made again, as a recovery makes its read
	// again: an entry it visited before the failure is visited again.
	for attempt := 0; attempt == 0 || deviceFailed(err) && attempt < 10; attempt++ {
		read = make(map[uint64]bool)
		err = j.Read(c.ctx, ReadRequest{VM: vm, Epoch: epoch, After: after, Generation: c.generation, Reader: holder},
			func(e Entry) error {
				read[e.Position] = true
				want, ok := c.attempted[nameOf(e)]
				if !ok || !sameContent(e, want) {
					return fmt.Errorf("an entry no commit was made of read back: %s", describe([]Entry{e}))
				}
				if answered, ok := c.answered[e.Position]; ok && !sameContent(e, answered) {
					return fmt.Errorf("%s read back at %d, where %s was answered", describe([]Entry{e}), e.Position,
						describe([]Entry{answered}))
				}
				return nil
			})
	}
	if err != nil {
		t.Fatalf("holder %d reading %s's epoch %d: %v", holder, vm, epoch, err)
	}
	// The epochs before the last holder's were read back, and dropped, by
	// the holders before.
	if epoch != holder-1 {
		return
	}
	for _, position := range slices.Sorted(maps.Keys(c.answered)) {
		e := c.answered[position]
		if e.VM == vm && e.Epoch == epoch && position > after && !read[position] {
			t.Fatalf("holder %d lost %s, answered at %d after %s's covered position %d", holder,
				describe([]Entry{e}), position, vm, after)
		}
	}
}

// deviceFailed reports an error the journal's device made: an I/O error, or a
// filesystem out of space.
func deviceFailed(err error) bool {
	return errors.Is(err, platform.ErrInjectedFault) || errors.Is(err, platform.ErrNoSpace)
}

// nameOf is the name a campaign entry's data begins with.
func nameOf(e Entry) string {
	name, _, _ := bytes.Cut(e.Data, []byte{0})
	return string(name)
}

func sameContent(a, b Entry) bool {
	return a.VM == b.VM && a.Volume == b.Volume && a.Epoch == b.Epoch && slices.Equal(a.Blocks, b.Blocks) &&
		bytes.Equal(a.Data, b.Data)
}

// campaignEntry is the n-th entry vm commits under holder: one to three
// blocks, named in its first bytes and filled after them from its name.
func (c *campaign) campaignEntry(holder uint64, vm, n int) Entry {
	name := fmt.Sprintf("%d/%d/%d", holder, vm, n)
	blocks := make([]uint64, 1+c.runtime.Random("journal-campaign").Intn(name+"/blocks", 3))
	for i := range blocks {
		blocks[i] = uint64(n*3 + i)
	}
	data := make([]byte, len(blocks)*BlockBytes)
	for i := range data {
		data[i] = byte(i*31) ^ byte(holder*7+uint64(vm)*13+uint64(n))
	}
	copy(data, name)
	data[len(name)] = 0
	return Entry{VM: vmName(vm), Volume: "disk", Epoch: holder, Blocks: blocks, Data: data}
}

// commitUntilThePowerIsLost has every VM commit at once until a seeded number
// of commits were tried, then has the disk lose its power and lets the holder go.
func (c *campaign) commitUntilThePowerIsLost(j *Journal, holder uint64) {
	t := c.t
	t.Helper()
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	tried, finished := make(chan struct{}), make(chan struct{}, campaignVMs)
	answeredNow := make(chan uint64, campaignVMs*campaignCommits)
	var wg sync.WaitGroup
	for vm := range campaignVMs {
		wg.Go(func() {
			defer func() { finished <- struct{}{} }()
			var answered []uint64
			for n := range campaignCommits {
				e := c.campaignEntry(holder, vm, n)
				c.mu.Lock()
				c.attempted[nameOf(e)] = e
				c.mu.Unlock()
				positions, err := commitOf(ctx, j, e)
				if err == nil {
					c.answer(e, positions[0], answeredNow)
					answered = append(answered, positions[0])
					// A checkpoint covers all but the last answered entry.
					if len(answered) > 1 {
						c.cover(j, e.VM, holder, answered[len(answered)-2])
					}
				}
				select {
				case tried <- struct{}{}:
				case <-ctx.Done():
					return
				}
			}
		})
	}
	lossAt := 1 + c.runtime.Random("journal-campaign").Intn(fmt.Sprintf("power-loss/%d", holder),
		campaignVMs*campaignCommits-5)
	for tries, done := 0, 0; tries < lossAt && done < campaignVMs; {
		select {
		case <-tried:
			tries++
		case <-finished:
			done++
		}
	}
	if err := c.disk.PowerLoss(c.ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	wg.Wait()
	if _, err := j.Close(c.ctx); !errors.Is(err, platform.ErrStaleHandle) {
		t.Fatalf("closing holder %d's journal after its power loss: %v, want ErrStaleHandle", holder, err)
	}
	close(answeredNow)
	c.mu.Lock()
	defer c.mu.Unlock()
	for position := range answeredNow {
		if position <= c.highestBefore(holder) {
			t.Fatalf("holder %d answered at %d, not past %d, the highest answered before it", holder, position,
				c.highestBefore(holder))
		}
	}
}

// answer records an answered entry. No position is answered twice.
func (c *campaign) answer(e Entry, position uint64, answered chan<- uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if earlier, ok := c.answered[position]; ok {
		c.t.Errorf("%s was answered at %d, where %s was answered before", describe([]Entry{e}), position,
			describe([]Entry{earlier}))
	}
	e.Position = position
	c.answered[position] = e
	answered <- position
}

// highestBefore is the highest position answered by the holders before
// holder. The caller holds mu.
func (c *campaign) highestBefore(holder uint64) uint64 {
	var highest uint64
	for position, e := range c.answered {
		if e.Epoch < holder {
			highest = max(highest, position)
		}
	}
	return highest
}

// cover trims vm's entries at holder's epoch up to position, as a checkpoint
// that covers them does.
func (c *campaign) cover(j *Journal, vm string, holder, position uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.covered[vm] = max(c.covered[vm], position)
	j.Trim(vm, map[uint64]uint64{holder: c.covered[vm]})
}
