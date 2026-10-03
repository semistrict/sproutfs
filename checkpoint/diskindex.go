package checkpoint

import (
	"encoding/binary"
	"hash/fnv"
	"math/bits"
	"slices"

	"github.com/semistrict/sproutfs/control"
)

// The disk's index says where each item is. One entry an item would cost more
// memory than a host has at a 4 KiB page, so the index is kept per window: the
// pages of one volume, in one aligned 2 MiB span, that one checkpoint
// published. A segment is a window of its own. A window is keyed by an 8-byte
// hash of its identity, and the key every item carries in its header catches
// two windows that share a hash.
//
// An entry is a run of one window's items that lie next to each other in one
// region, in page order. It holds the region, the run's first offset, which
// pages are present and each one's length, so an item's offset is the run's
// first offset plus the items before it. A window filled at two different
// times, or interleaved with another window's items, has an entry for each
// run. An entry with few items lists (page, length) pairs; one with many keeps
// a bitmap of its window's pages and the lengths of the ones present.

// diskWindowBytes is the span of a volume one window covers.
const diskWindowBytes = 2 << 20

// diskKey is what the disk names one envelope by: the cache key, and how many
// of its volume's pages one window spans, which is the one thing about a
// volume's geometry the window needs.
type diskKey struct {
	cacheKey
	// span is 1 for a segment and at a 2 MiB page, and 512 at 4 KiB.
	span uint16
}

// windowSpan is how many pages of a volume of geometry one window spans.
func windowSpan(geometry Geometry) uint16 {
	return uint16(max(1, diskWindowBytes/geometry.PageSize))
}

// pageDiskKey is the disk's key for one page of a volume of geometry.
func pageDiskKey(identity control.Identity, geometry Geometry) diskKey {
	return diskKey{cacheKey: pageKey(identity), span: windowSpan(geometry)}
}

// segmentDiskKey is the disk's key for one segment, a window of its own.
func segmentDiskKey(volume string, number uint64, ref control.Ref) diskKey {
	return diskKey{cacheKey: segmentCacheKey(volume, number, ref), span: 1}
}

// window is the hash of the window key falls in, and key's place within it.
func (k diskKey) window() (uint64, uint16) {
	span := uint64(max(k.span, 1))
	hash := fnv.New64a()
	var fixed [27]byte
	if k.segment {
		fixed[0] = 1
	}
	binary.LittleEndian.PutUint64(fixed[1:], k.Ref.Sequence)
	binary.LittleEndian.PutUint64(fixed[9:], k.Page/span)
	binary.LittleEndian.PutUint16(fixed[17:], uint16(span))
	binary.LittleEndian.PutUint64(fixed[19:], uint64(len(k.Ref.VM)))
	hash.Write(fixed[:])
	hash.Write([]byte(k.Ref.VM))
	hash.Write([]byte(k.Volume))
	return hash.Sum64(), uint16(k.Page % span)
}

// itemWord is what the index keeps of one item: its length in 24 bits, a
// read counter in 7, and whether the item was forgotten. A forgotten item
// keeps its length, because the items after it in its run are found by it.
type itemWord uint32

const (
	wordLengthMask = 1<<24 - 1
	wordReadShift  = 24
	wordReadsMax   = 127
	wordForgotten  = 1 << 31
)

func newItemWord(length int64) itemWord { return itemWord(length) }

func (w itemWord) length() int64    { return int64(w & wordLengthMask) }
func (w itemWord) reads() int       { return int(w>>wordReadShift) & wordReadsMax }
func (w itemWord) forgotten() bool  { return w&wordForgotten != 0 }
func (w itemWord) forget() itemWord { return w | wordForgotten }

// read counts one more read, up to the counter's ceiling.
func (w itemWord) read() itemWord {
	if w.reads() == wordReadsMax {
		return w
	}
	return w + 1<<wordReadShift
}

// listedItem is one item of an entry that lists its items.
type listedItem struct {
	page uint16
	word itemWord
}

// denseItems is how many items an entry lists before it keeps a bitmap
// instead: past it the bitmap's 64 bytes cost less than a list's 8 an item.
const denseItems = 16

// Index memory is counted, not measured: an entry's fixed cost, and what its
// items cost in whichever form it holds them.
const (
	windowEntryCharge = 96
	listedItemCharge  = 8
	bitmapCharge      = 64
	denseItemCharge   = 4
	// maximumInsertCharge is the most one insert can add.
	maximumInsertCharge = windowEntryCharge + bitmapCharge + denseItemCharge*denseItems
)

// windowEntry is one run of a window's items in one region.
type windowEntry struct {
	hash   uint64
	region *diskRegion
	// first is the file offset of the run's first item and end the offset past
	// its last; header is the length of each item's header, which is the same
	// for every item of one window.
	first, end, header int64
	// list holds the items while there are few. Past that, present marks the
	// window's pages the run holds, and words holds their items in page order.
	list    []listedItem
	present *[8]uint64
	words   []itemWord
	// live counts the items not forgotten, and removed marks an entry the
	// index no longer holds.
	live    int
	removed bool
}

// wordAt is the word of the entry's at-th item.
func (e *windowEntry) wordAt(at int) itemWord {
	if e.present == nil {
		return e.list[at].word
	}
	return e.words[at]
}

func (e *windowEntry) setWord(at int, word itemWord) {
	if e.present == nil {
		e.list[at].word = word
		return
	}
	e.words[at] = word
}

// last is the highest page the entry holds.
func (e *windowEntry) last() uint16 {
	if e.present == nil {
		return e.list[len(e.list)-1].page
	}
	for word := len(e.present) - 1; word >= 0; word-- {
		if e.present[word] != 0 {
			return uint16(word*64 + 63 - bits.LeadingZeros64(e.present[word]))
		}
	}
	panic("checkpoint: a window entry keeps a bitmap of no pages")
}

// find reports the position of page in the entry, its word and its offset.
func (e *windowEntry) find(page uint16) (int, itemWord, int64, bool) {
	offset := e.first
	if e.present == nil {
		for at, item := range e.list {
			if item.page == page {
				return at, item.word, offset, true
			}
			offset += e.header + item.word.length()
		}
		return 0, 0, 0, false
	}
	if e.present[page/64]&(1<<(page%64)) == 0 {
		return 0, 0, 0, false
	}
	at := 0
	for word := range int(page / 64) {
		at += bits.OnesCount64(e.present[word])
	}
	at += bits.OnesCount64(e.present[page/64] & (1<<(page%64) - 1))
	for _, word := range e.words[:at] {
		offset += e.header + word.length()
	}
	return at, e.words[at], offset, true
}

// add appends an item past every one the entry holds, and reports what that
// cost the index.
func (e *windowEntry) add(page uint16, word itemWord) int64 {
	e.live++
	e.end += e.header + word.length()
	if e.present != nil {
		e.present[page/64] |= 1 << (page % 64)
		e.words = append(e.words, word)
		return denseItemCharge
	}
	e.list = append(e.list, listedItem{page: page, word: word})
	if len(e.list) <= denseItems {
		return listedItemCharge
	}
	before := e.charge() - listedItemCharge
	e.present = new([8]uint64)
	e.words = make([]itemWord, 0, len(e.list))
	for _, item := range e.list {
		e.present[item.page/64] |= 1 << (item.page % 64)
		e.words = append(e.words, item.word)
	}
	e.list = nil
	return e.charge() - before
}

// charge is what the entry costs the index.
func (e *windowEntry) charge() int64 {
	if e.present != nil {
		return windowEntryCharge + bitmapCharge + denseItemCharge*int64(len(e.words))
	}
	return windowEntryCharge + listedItemCharge*int64(len(e.list))
}

// diskIndex is every window entry the disk holds, by window hash.
type diskIndex struct {
	windows map[uint64][]*windowEntry
	// used is what the entries cost, and live the items not forgotten.
	used int64
	live int
}

func newDiskIndex() diskIndex { return diskIndex{windows: make(map[uint64][]*windowEntry)} }

// diskLocation is where the index says one item lies.
type diskLocation struct {
	entry  *windowEntry
	at     int
	word   itemWord
	offset int64
}

// size is the item's length on the disk, header and bytes.
func (l diskLocation) size() int64 { return l.entry.header + l.word.length() }

// lookup finds the item the index holds for key. Two windows that share a
// hash share entries, so what it finds may be another window's item; the key
// in the item's header is what tells them apart.
func (x *diskIndex) lookup(key diskKey) (diskLocation, bool) {
	hash, page := key.window()
	header := itemHeaderBytes(key)
	for _, entry := range x.windows[hash] {
		if entry.header != header {
			continue
		}
		if at, word, offset, found := entry.find(page); found && !word.forgotten() {
			return diskLocation{entry: entry, at: at, word: word, offset: offset}, true
		}
	}
	return diskLocation{}, false
}

// insert names an item of length bytes at offset in region, extending the run
// the window's last item there ends at when it can.
func (x *diskIndex) insert(key diskKey, region *diskRegion, offset, length int64) {
	hash, page := key.window()
	header := itemHeaderBytes(key)
	word := newItemWord(length)
	for _, entry := range x.windows[hash] {
		if entry.region == region && entry.end == offset && entry.header == header && entry.last() < page {
			x.used += entry.add(page, word)
			x.live++
			return
		}
	}
	entry := &windowEntry{hash: hash, region: region, first: offset, end: offset, header: header}
	x.used += windowEntryCharge + entry.add(page, word)
	x.live++
	x.windows[hash] = append(x.windows[hash], entry)
	region.entries = append(region.entries, entry)
}

// forget marks one item gone, and drops its entry once nothing in it is left.
func (x *diskIndex) forget(location diskLocation) {
	entry := location.entry
	if entry.removed {
		return
	}
	word := entry.wordAt(location.at)
	if word.forgotten() {
		return
	}
	entry.setWord(location.at, word.forget())
	entry.live--
	x.live--
	if entry.live == 0 {
		x.remove(entry)
	}
}

// read counts one read of an item.
func (x *diskIndex) read(location diskLocation) {
	if location.entry.removed {
		return
	}
	location.entry.setWord(location.at, location.entry.wordAt(location.at).read())
}

// remove drops one entry from the index.
func (x *diskIndex) remove(entry *windowEntry) {
	if entry.removed {
		return
	}
	entry.removed = true
	x.used -= entry.charge()
	x.live -= entry.live
	entries := slices.DeleteFunc(x.windows[entry.hash], func(other *windowEntry) bool { return other == entry })
	if len(entries) == 0 {
		delete(x.windows, entry.hash)
	} else {
		x.windows[entry.hash] = entries
	}
}

// dropRegion drops every entry that names region.
func (x *diskIndex) dropRegion(region *diskRegion) {
	for _, entry := range region.entries {
		x.remove(entry)
	}
	region.entries = nil
}

// clear drops every entry.
func (x *diskIndex) clear() {
	for _, entries := range x.windows {
		for _, entry := range entries {
			entry.removed = true
		}
	}
	*x = newDiskIndex()
}

// each calls visit for every item of an entry that is not forgotten, in the
// order they lie, with its location.
func (e *windowEntry) each(visit func(page uint16, location diskLocation)) {
	offset := e.first
	step := func(at int, page uint16, word itemWord) {
		if !word.forgotten() {
			visit(page, diskLocation{entry: e, at: at, word: word, offset: offset})
		}
		offset += e.header + word.length()
	}
	if e.present == nil {
		for at, item := range e.list {
			step(at, item.page, item.word)
		}
		return
	}
	at := 0
	for word, set := range e.present {
		for set != 0 {
			bit := bits.TrailingZeros64(set)
			step(at, uint16(word*64+bit), e.words[at])
			at++
			set &^= 1 << bit
		}
	}
}
