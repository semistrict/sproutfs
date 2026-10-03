package checkpoint

import (
	"context"
	"errors"
	"fmt"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/stripe"
)

// What the disk keeps of an envelope is the stripes of it that its cache is
// ranked for. The list of caches the host holds carries the deployment's
// code, and ranks every window's caches: stripe i of the window's envelopes
// goes on rank ((i - 1) mod n) + 1 of the n caches ranked for it, counting
// from one, so a list shorter than the code is wide takes the stripes round
// its caches, and a cache may hold several indices of one window. A host
// alone in its list holds every stripe, under the code of one host, 1+0: its
// one stripe is the envelope whole. A host whose list does not rank its cache
// for a window keeps nothing of it.
//
// That holds only for the windows inside the share the cluster cache is
// turned on for (CacheConfig.ClusterPercent), chosen by a hash of the window.
// Every other window is kept whole under 1+0, whatever the list says, as a
// host kept it before there were stripes, so a deployment rolls the cluster
// cache out a share of windows at a time and loses nothing before then.
//
// A read rebuilds the envelope from the stripes of the list's code the disk
// holds, any k distinct indices of them. A stripe of another code is a miss,
// so a deployment that changes its code refills from the store and reads no
// wrong bytes. Each stripe's key and checksum are checked as it is read, and
// the envelope it rebuilds is checked by the caller's check, which is the
// envelope's own SHA-256. A stripe found wrong is forgotten. Nothing here
// reads from a peer or sends one a stripe: until the cluster fills and reads
// across hosts, a page this host cannot rebuild from its own disk is read
// from the store.

// The probes the disk's stripes mark.
const (
	// ProbeDiskSeveralStripes is a write that kept several indices of one
	// envelope, under a list shorter than the code is wide.
	ProbeDiskSeveralStripes = "checkpoint/disk-several-stripes"
	// ProbeDiskStripesDecoded is a read that rebuilt an envelope with a data
	// stripe missing, from a parity stripe.
	ProbeDiskStripesDecoded = "checkpoint/disk-stripes-decoded"
	// ProbeDiskTooFewStripes is a read that found stripes of the code, but
	// fewer than k.
	ProbeDiskTooFewStripes = "checkpoint/disk-too-few-stripes"
	// ProbeDiskStripeOfAnotherCode is a read that found stripes of the page
	// only under another code.
	ProbeDiskStripeOfAnotherCode = "checkpoint/disk-stripe-of-another-code"
	// ProbeDiskWrongStripe is a stripe found wrong, by rebuilding the
	// envelope from other stripes, and forgotten.
	ProbeDiskWrongStripe = "checkpoint/disk-wrong-stripe-found"
)

// The fault-injection sites of the disk's stripes.
const (
	// buggifyDiskWrongStripe hands a read a stripe whose checksum holds and
	// whose bytes are wrong.
	buggifyDiskWrongStripe = "checkpoint/disk-wrong-stripe"
	// buggifyDiskCodeChanged reads under another code than the list's, as a
	// host does after its deployment's code changed.
	buggifyDiskCodeChanged = "checkpoint/disk-code-changed"
	// buggifyDiskShortList places a window by the list less every other
	// cache, as a host that has heard of no other cache yet does: a list
	// shorter than the code is wide.
	buggifyDiskShortList = "checkpoint/disk-short-list"
)

// wholeCode is the code of a host alone: each envelope whole.
var wholeCode = rank.CodeFor(1)

// follow has the disk place what it keeps by the list caches returns, and
// read under its code. It is called once, as the host starts following the
// list of caches, before any read or fill.
func (d *cacheDisk) follow(caches func() rank.List) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.caches = caches
}

// list is the list of caches the disk places by, and whether it follows one.
func (d *cacheDisk) list() (rank.List, bool) {
	d.mu.Lock()
	caches := d.caches
	d.mu.Unlock()
	if caches == nil {
		return rank.List{}, false
	}
	list := caches()
	return list, list.Code().Validate() == nil
}

// listFor is the list of caches key's window is placed by, and whether it is
// placed by one: a disk that follows a list places by it the windows inside
// the share the cluster cache is turned on for, and keeps every other window
// whole. ignoreShare is the guard that places every window by the list.
func (d *cacheDisk) listFor(key diskKey, ignoreShare bool) (rank.List, bool) {
	list, ok := d.list()
	if !ok || !ignoreShare && !key.rankWindow().InShare(d.clusterPercent) {
		return rank.List{}, false
	}
	return list, true
}

// code is the code the disk reads key under: the list's for a window it
// places by the list, and 1+0 for every other.
func (d *cacheDisk) code(ctx context.Context, key diskKey) rank.Code {
	code := wholeCode
	if list, ok := d.listFor(key, sim.Bug(ctx, "diskcache-share-ignored")); ok {
		code = list.Code()
	}
	if sim.Buggify(ctx, buggifyDiskCodeChanged, 0.05) {
		code = anotherCode(code)
	}
	return code
}

// anotherCode is a code of the table other than code.
func anotherCode(code rank.Code) rank.Code {
	for hosts := 1; ; hosts++ {
		if other := rank.CodeFor(hosts); other != code {
			return other
		}
	}
}

// placement is the code the disk keeps key's envelope under, and the indices
// of it this cache holds: for a window it places by the list, those the list
// puts on this cache, none where the list does not rank it; for every other
// window, the envelope whole.
func (d *cacheDisk) placement(ctx context.Context, key diskKey) (rank.Code, []int) {
	list, ok := d.listFor(key, sim.Bug(ctx, "diskcache-share-ignored"))
	if !ok {
		return wholeCode, []int{0}
	}
	if sim.Buggify(ctx, buggifyDiskShortList, 0.25) {
		for _, cache := range list.Caches() {
			if cache.Identity != d.identity {
				list = list.Without(cache.Identity)
			}
		}
	}
	var indices []int
	for index, holder := range list.Holders(key.rankWindow()) {
		if holder.Identity != d.identity ||
			index >= list.Len() && sim.Bug(ctx, "diskcache-stripes-not-round") {
			continue
		}
		indices = append(indices, index)
	}
	return list.Code(), indices
}

// write keeps the stripes of envelope this cache holds under key, by the
// list it follows, unless the disk already holds them. A write the disk
// refuses reports ErrDiskRefused. A write the disk fails is logged and
// forgotten, and reports nothing: the store still holds the bytes.
func (d *cacheDisk) write(ctx context.Context, key diskKey, envelope []byte, kind WriteKind) error {
	code, indices := d.placement(ctx, key)
	if len(indices) == 0 {
		return nil
	}
	stripes, err := stripe.Split(code, envelope)
	if err != nil {
		return d.refuse("%d bytes do not split under %s: %v", len(envelope), code, err)
	}
	kept := make([]stripe.Stripe, len(indices))
	for at, index := range indices {
		kept[at] = stripes[index]
	}
	if len(kept) > 1 {
		sim.Probe(ctx, ProbeDiskSeveralStripes)
	}
	_, err = d.writeStripes(ctx, key, kept, kind)
	if errors.Is(err, errDiskFailed) {
		// The disk logged it; the store still holds the bytes.
		return nil
	}
	return err
}

// errDiskFailed reports a write the disk failed, which it has logged and
// forgotten: the store still holds what it would have kept.
var errDiskFailed = errors.New("checkpoint: the page cache's disk failed a write")

// writeStripes keeps stripes of key's envelope, as items next to each other
// in one write, leaving out those the disk already holds, and reports how
// many it wrote. A write the disk refuses reports ErrDiskRefused. A write the
// disk fails is logged and forgotten, and reports errDiskFailed.
func (d *cacheDisk) writeStripes(ctx context.Context, key diskKey, stripes []stripe.Stripe, kind WriteKind) (int, error) {
	for _, s := range stripes {
		if !storableStripe(s) {
			return 0, d.refuse("%v cannot be stored", s)
		}
	}
	size, table := itemsBytes(key, stripes)
	if !storable(key) || size+table+diskTrailerSize > d.regionBytes {
		return 0, d.refuse("%d bytes do not fit in a region of %d", size, d.regionBytes)
	}
	if err := d.lockWriter(ctx); err != nil {
		return 0, err
	}
	defer d.unlockWriter()
	d.mu.Lock()
	var missing []stripe.Stripe
	for _, s := range stripes {
		if _, found := d.index.lookup(key, codeOf(s), false, false); !found {
			missing = append(missing, s)
		}
	}
	indexed, stopped := d.index.used, d.stopped
	d.mu.Unlock()
	if len(missing) == 0 {
		return 0, nil
	}
	if stopped {
		return 0, d.refuse("the cache is closed")
	}
	if indexed+int64(len(missing))*maximumInsertCharge > d.indexLimit {
		return 0, d.refuse("the index holds %d bytes of %d", indexed, d.indexLimit)
	}
	size, _ = itemsBytes(key, missing)
	if !d.budget.Admit(size, kind) {
		return 0, d.refuse("the write budget refused %d bytes", size)
	}
	stored, err := d.append(ctx, key, missing, kind)
	switch {
	case err != nil:
		return 0, err
	case !stored:
		return 0, errDiskFailed
	}
	return len(missing), nil
}

// holdsStripe reports whether the index holds the stripe code names of key's
// envelope.
func (d *cacheDisk) holdsStripe(key diskKey, code diskCode) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, found := d.index.lookup(key, code, false, false)
	return found
}

// windowKey is the disk's key for page at of window.
func windowKey(window rank.Window, at uint32) diskKey {
	return diskKey{cacheKey: cacheKey{Identity: control.Identity{Ref: window.Ref, Volume: window.Volume,
		Page: window.Page(at)}, segment: window.Segment}, span: uint16(max(window.Pages, 1))}
}

// heldPages is the pages of window, among pages, the disk holds a stripe of
// under code, of any index; pages nil asks for every page of the window.
func (d *cacheDisk) heldPages(window rank.Window, pages []uint32, code rank.Code) []uint32 {
	if pages == nil {
		pages = make([]uint32, max(window.Pages, 1))
		for at := range pages {
			pages[at] = uint32(at)
		}
	}
	var held []uint32
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, page := range pages {
		key := windowKey(window, page)
		for index := range code.Width() {
			if _, found := d.index.lookup(key, indexOf(code, index), false, false); found {
				held = append(held, page)
				break
			}
		}
	}
	return held
}

// holdsAnyOf reports whether the disk holds a stripe of any of pages of
// window under code; pages nil asks of every page.
func (d *cacheDisk) holdsAnyOf(window rank.Window, pages []uint32, code rank.Code) bool {
	return len(d.heldPages(window, pages, code)) > 0
}

// forgetStripe drops one stripe from the index, as a reader that found it
// wrong asks.
func (d *cacheDisk) forgetStripe(ctx context.Context, key diskKey, code diskCode, cause error) bool {
	d.mu.Lock()
	location, found := d.index.lookup(key, code, false, false)
	d.mu.Unlock()
	if !found {
		return false
	}
	d.forget(ctx, location, key, cause)
	return true
}

// has reports whether the disk holds every stripe of key's envelope this
// cache is placed to hold, and holds some.
func (d *cacheDisk) has(ctx context.Context, key diskKey) bool {
	code, indices := d.placement(ctx, key)
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, index := range indices {
		if _, found := d.index.lookup(key, indexOf(code, index), false, false); !found {
			return false
		}
	}
	return len(indices) > 0
}

// lookupStripes is the stripes of key's envelope under code the index holds, as
// locations whose regions are held for a read, and their indices.
func (d *cacheDisk) lookupStripes(ctx context.Context, key diskKey, code rank.Code) ([]diskLocation, []diskCode) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var locations []diskLocation
	var codes []diskCode
	for index := range code.Width() {
		want := indexOf(code, index)
		location, found := d.index.lookup(key, want, sim.Bug(ctx, "diskcache-mix-codes"),
			sim.Bug(ctx, "diskcache-one-stripe-a-page"))
		if !found {
			continue
		}
		location.entry.region.readers++
		locations, codes = append(locations, location), append(codes, want)
	}
	return locations, codes
}

// readStripe reads back stripe index of key's envelope under code. Anything
// but a hit is a miss.
func (d *cacheDisk) readStripe(ctx context.Context, key diskKey, code rank.Code, index int) (stripe.Stripe, diskReadOutcome) {
	want := indexOf(code, index)
	d.mu.Lock()
	location, found := d.index.lookup(key, want, false, false)
	if found {
		location.entry.region.readers++
	}
	d.mu.Unlock()
	if !found {
		return stripe.Stripe{}, diskAbsent
	}
	return d.readItem(ctx, key, want, location)
}

// read rebuilds the envelope the disk holds under key from the stripes of the
// list's code it holds, and checks it with check: nil accepts any envelope.
// Anything but a hit is a miss. A stripe the rebuild finds wrong is
// forgotten, and so is every stripe of key that rebuilds nothing that passes,
// because a copy that failed once is not asked for again.
func (d *cacheDisk) read(ctx context.Context, key diskKey, check func([]byte) error) ([]byte, diskReadOutcome) {
	code := d.code(ctx, key)
	locations, codes := d.lookupStripes(ctx, key, code)
	var stripes []stripe.Stripe
	var read []diskLocation
	outcome := diskHit
	for at, location := range locations {
		s, found := d.readItem(ctx, key, codes[at], location)
		if found != diskHit {
			outcome = found
			continue
		}
		stripes, read = append(stripes, s), append(read, location)
	}
	if len(stripes) < code.K {
		switch {
		case outcome != diskHit:
			return nil, outcome
		case len(stripes) > 0:
			sim.Probe(ctx, ProbeDiskTooFewStripes)
		default:
			d.mu.Lock()
			other := d.index.holdsAny(key)
			d.mu.Unlock()
			if other {
				sim.Probe(ctx, ProbeDiskStripeOfAnotherCode)
			}
		}
		return nil, diskAbsent
	}
	joined, err := stripe.Join(ctx, code, stripes, check)
	if context.Cause(ctx) != nil {
		// A check the caller gave up on says nothing about the stripes.
		return nil, diskFailed
	}
	wrong := joined.Wrong
	if len(wrong) > 0 {
		sim.Probe(ctx, ProbeDiskWrongStripe)
	} else if errors.Is(err, stripe.ErrWrong) {
		// Which of them is wrong cannot be told, so none is kept.
		wrong = make([]int, len(stripes))
		for at := range wrong {
			wrong[at] = at
		}
	}
	if !sim.Bug(ctx, "diskcache-keep-wrong-stripe") {
		for _, at := range wrong {
			d.forget(ctx, read[at], key, fmt.Errorf("stripe %d of %s rebuilt no envelope that passes its check",
				stripes[at].Index, code))
		}
	}
	if err != nil {
		return nil, diskWrongStripe
	}
	for _, at := range joined.Used {
		if stripes[at].Index >= code.K {
			sim.Probe(ctx, ProbeDiskStripesDecoded)
			break
		}
	}
	return joined.Envelope, diskHit
}

// decoded is the decoded bytes of the envelope the disk holds under key,
// rebuilt and checked as one from the store is: it decodes under codecs to at
// most maximum bytes, its SHA-256 holds, and valid takes what it decodes to.
func (d *cacheDisk) decoded(ctx context.Context, key diskKey, codecs *blob.Codecs, maximum int,
	valid func([]byte) bool) ([]byte, bool) {
	var data []byte
	_, outcome := d.read(ctx, key, func(envelope []byte) error {
		decoded, err := codecs.Decode(ctx, envelope, maximum)
		if err != nil {
			return err
		}
		if !valid(decoded) {
			return ErrCorrupt
		}
		data = decoded
		return nil
	})
	return data, outcome == diskHit
}

// served counts one read of an envelope that came back intact, and one read
// of each stripe of it the disk holds under the list's code.
func (d *cacheDisk) served(key diskKey) {
	code := wholeCode
	if list, ok := d.listFor(key, false); ok {
		code = list.Code()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hits++
	for index := range code.Width() {
		if location, found := d.index.lookup(key, indexOf(code, index), false, false); found {
			d.index.read(location)
		}
	}
}
