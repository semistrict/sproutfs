package checkpoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/internal/checkpoint/internal/part"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
)

// Config supplies shared object storage and the upload budget a publication
// obeys. The zero Concurrency takes its documented default.
type Config struct {
	// ObjectStore holds every checkpoint object; ObjectPrefix is the
	// deployment's namespace within it.
	ObjectStore  platform.ObjectStore
	ObjectPrefix platform.ObjectPrefix
	// Cache optionally shares decoded pages between the stores and checkpoints
	// of one host. Nil leaves reads uncached.
	Cache *Cache
	// Codecs is the compression pool this store's publications encode through
	// and its reads decode through. Its two halves are separate, so a guest's
	// page fault does not queue behind a checkpoint's encoding. Nil takes the
	// package-wide default.
	Codecs *blob.Codecs
	// Concurrency is the number of objects this store uploads at a time,
	// host-wide: every publication it begins draws on the one budget, so a
	// manager whose VMs all become dirty at once does not multiply it by the
	// number of them. Default 8.
	Concurrency int
	// PartBytes is the encoded member size one part fills to before it is
	// sealed and uploaded, which bounds both a publication's memory and the
	// bytes one interrupted upload repeats. Default 64 MiB.
	PartBytes int
	// MaxBuilders is how many publications may hold a part builder at a time,
	// host-wide. A builder holds up to PartBytes of encoded members, and a part
	// it has sealed is held until its upload finishes, under one of the
	// Concurrency slots — so what publication costs a host, however many of its
	// VMs become dirty at once, is MaxBuilders plus Concurrency times PartBytes:
	// the builders, and the sealed parts in flight. Zero takes Concurrency.
	MaxBuilders int
	// MaxDeletes is how many objects reclamation deletes at a time. Deletions
	// have a budget of their own so that a reclamation sweep cannot take the
	// slots a checkpoint needs to become durable. Zero takes a small default.
	MaxDeletes int
	// MaxIndexBytes bounds the root one publication may write, which is what a
	// checkpoint is read back through: a publication whose root would exceed it
	// is refused. Zero takes maximumRootSize, which is the format's own bound
	// and what every reader fetches. It is a knob rather than a constant because
	// the refusal above it is a path nothing reached while the only value was
	// one no volume this design targets could produce.
	MaxIndexBytes int
}

const (
	defaultConcurrency = 8
	// defaultDeleteConcurrency is deliberately small: reclamation is never
	// urgent, and everything it misses is merely unreferenced.
	defaultDeleteConcurrency = 2
	maximumConcurrency       = 1024
)

// Store reads and publishes checkpoints in one deployment's object namespace.
// Its methods are safe for concurrent use.
type Store struct {
	objects   platform.ObjectStore
	prefix    string
	cache     *Cache
	partBytes int
	// maxRootBytes is Config.MaxIndexBytes, or the package maximum.
	maxRootBytes int
	// indexTail is how much of the end of an index object an open reads first:
	// defaultIndexTail, and less only where a test wants a root that does not
	// fit it.
	indexTail int64
	// slots is the upload budget every publication this store begins shares.
	// Its capacity is Config.Concurrency.
	slots chan struct{}
	// builders is the budget for part builders, which is what bounds the memory
	// publication costs this host: its capacity is Config.MaxBuilders, and a
	// publication holds one slot from its first member until it ends.
	builders chan struct{}
	// deletes is reclamation's own budget, so a sweep never waits on, or waits
	// out, the uploads that make a checkpoint durable.
	deletes chan struct{}
	// codecs is what this store encodes and decodes envelopes through.
	codecs *blob.Codecs
	// builderMu guards liveBuilders, which is what the budget is holding.
	builderMu    sync.Mutex
	liveBuilders int
	// protectedMu guards protected, which memoises the checkpoints one pinned
	// checkpoint's index names. Indexes are immutable and pins are permanent,
	// so one read serves every later reclamation and no entry ever goes stale.
	// It holds one entry per pinned checkpoint of the VMs this store has swept,
	// which MaximumPins bounds per VM.
	protectedMu sync.Mutex
	protected   map[control.Ref][]control.Ref
}

// NewStore validates the configuration and returns a store over it.
func NewStore(config Config) (*Store, error) {
	if config.ObjectStore == nil || config.Concurrency < 0 || config.Concurrency > maximumConcurrency ||
		config.MaxBuilders < 0 || config.MaxBuilders > maximumConcurrency ||
		config.MaxDeletes < 0 || config.MaxDeletes > maximumConcurrency ||
		config.PartBytes < 0 || config.PartBytes > partTargetBytes ||
		config.MaxIndexBytes < 0 || config.MaxIndexBytes > maximumRootSize {
		return nil, ErrInvalidConfig
	}
	if config.PartBytes == 0 {
		config.PartBytes = partTargetBytes
	}
	if config.MaxIndexBytes == 0 {
		config.MaxIndexBytes = maximumRootSize
	}
	prefix := config.ObjectPrefix.String()
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if config.Concurrency == 0 {
		config.Concurrency = defaultConcurrency
	}
	if config.MaxBuilders == 0 {
		config.MaxBuilders = config.Concurrency
	}
	if config.MaxDeletes == 0 {
		config.MaxDeletes = min(defaultDeleteConcurrency, config.Concurrency)
	}
	if config.Codecs == nil {
		config.Codecs = blob.Default()
	}
	store := &Store{objects: config.ObjectStore, prefix: prefix, cache: config.Cache,
		partBytes: config.PartBytes, maxRootBytes: config.MaxIndexBytes,
		indexTail: defaultIndexTail,
		slots:     make(chan struct{}, config.Concurrency),
		builders:  make(chan struct{}, config.MaxBuilders),
		deletes:   make(chan struct{}, config.MaxDeletes),
		codecs:    config.Codecs,
		protected: make(map[control.Ref][]control.Ref)}
	if _, err := store.indexKey(control.Ref{VM: "vm", Sequence: 1}); err != nil {
		return nil, ErrInvalidConfig
	}
	return store, nil
}

// checkpointPrefix is the key prefix every object of one checkpoint shares.
func (s *Store) checkpointPrefix(ref control.Ref) string {
	return s.prefix + "vm/" + ref.VM + "/ckpt/" + strconv.FormatUint(ref.Sequence, 10) + "/"
}

// indexKey names one checkpoint's index object: the object holding the
// segments it changed and the root that ends it. Its create-if-absent PUT is
// the publication's commit, so while it is there the checkpoint is published,
// and until it is there the checkpoint is absent.
func (s *Store) indexKey(ref control.Ref) (platform.ObjectKey, error) {
	if !validName(ref.VM) {
		return platform.ObjectKey{}, ErrInvalidConfig
	}
	return platform.NewObjectKey(s.checkpointPrefix(ref) + "index")
}

// partKey names one part of a checkpoint's data, numbered from zero. How
// many there are is what the root says.
func (s *Store) partKey(ref control.Ref, number uint32) (platform.ObjectKey, error) {
	if !validName(ref.VM) {
		return platform.ObjectKey{}, ErrInvalidConfig
	}
	return platform.NewObjectKey(s.checkpointPrefix(ref) + "part/" + strconv.FormatUint(uint64(number), 10))
}

// supersededPartKey names the object a checkpoint's root lived in while it was
// the last member of the last part of a "pack". Nothing writes one; a
// deployment holding them is refused with the layout version that part carries
// named, which is what Open does when it finds no index object.
func (s *Store) supersededPartKey(ref control.Ref) (platform.ObjectKey, error) {
	if !validName(ref.VM) {
		return platform.ObjectKey{}, ErrInvalidConfig
	}
	return platform.NewObjectKey(s.checkpointPrefix(ref) + "pack/last")
}

// Open reads and validates the root of a published checkpoint. It is one GET of
// the end of the checkpoint's index object: the record that closes it says
// where the root before it is, the tail read holds the whole of any root but
// the very largest — one more GET fetches the rest of one that long — and the
// segments ahead of the root are fetched as they are needed. So what an open
// costs does not grow with what the checkpoint changed.
//
// A checkpoint with no index object never committed, and is absent. A
// deployment written when the root was a member of a part is refused with the
// version it was written under named rather than reported absent.
func (s *Store) Open(ctx context.Context, ref control.Ref) (*Index, error) {
	key, err := s.indexKey(ref)
	if err != nil {
		return nil, err
	}
	tail, size, err := s.readSuffix(ctx, key, s.indexTail)
	if err != nil {
		if errors.Is(err, platform.ErrNotFound) {
			return nil, s.refuseSupersededParts(ctx, ref, err)
		}
		return nil, err
	}
	if len(tail) < indexRecordSize || !isIndexRecord(tail[len(tail)-indexRecordSize:]) {
		// An object at this key that does not end as one of these does is one
		// an older build wrote, and what this read owes is the version to name.
		return nil, s.refuseSupersededIndex(ctx, key)
	}
	offset, length, err := decodeIndexRecord(tail[len(tail)-indexRecordSize:], size)
	if err != nil {
		return nil, err
	}
	// The root ends where the closing record begins, so the tail holds the end
	// of it and, for all but the largest, the whole of it.
	held := tail[:len(tail)-indexRecordSize]
	var encoded []byte
	if length <= uint64(len(held)) {
		encoded = held[uint64(len(held))-length:]
	} else {
		head, err := s.readRange(ctx, key, offset, length-uint64(len(held)), maximumRootExtent)
		if err != nil {
			return nil, err
		}
		encoded = append(head, held...)
	}
	root, err := s.codecs.Decode(ctx, encoded, maximumRootSize)
	if err != nil {
		return nil, errors.Join(ErrCorrupt, err)
	}
	return decodeRoot(s, ref, root)
}

// refuseSupersededParts reports why a checkpoint with no index object is not
// there: a deployment written while the root was the last member of a
// checkpoint's last part has that part under the same checkpoint, and is
// refused with the layout version its trailer carries named. Anything else is
// simply absent, which is what an interrupted publication leaves.
func (s *Store) refuseSupersededParts(ctx context.Context, ref control.Ref, absent error) error {
	key, err := s.supersededPartKey(ref)
	if err != nil {
		return absent
	}
	tail, _, err := s.readSuffix(ctx, key, part.TrailerSize)
	if err != nil {
		return absent
	}
	version, found := part.TrailerVersion(tail)
	if !found {
		return absent
	}
	return fmt.Errorf("%w: checkpoint part format version %d, which this build does not read: "+
		"a checkpoint is an index object and its parts", ErrCorrupt, version)
}

// refuseSupersededIndex names the version an index object this build did not
// write was written under. One that begins with a record says so there. While
// the root was the whole of the index object it was one envelope holding a
// message that carried its own format version, and that field is what says so.
// Nothing an older build wrote is larger than supersededIndexSize.
func (s *Store) refuseSupersededIndex(ctx context.Context, key platform.ObjectKey) error {
	data, _, err := platform.ReadObject(ctx, s.objects, key, 1, supersededIndexSize, ErrCorrupt)
	if err != nil {
		return err
	}
	if isIndexRecord(data) {
		if _, _, err := decodeIndexRecord(data, uint64(len(data))); err != nil {
			return err
		}
		return ErrCorrupt
	}
	root, err := s.codecs.Decode(ctx, data, supersededIndexSize)
	if err != nil {
		return ErrCorrupt
	}
	if _, err := decodeRoot(s, control.Ref{}, root); err != nil {
		return err
	}
	return ErrCorrupt
}

// Root publishes the first checkpoint of a new VM: every named volume exists at
// its given size and page size, and reads as zeroes. Sizes must be whole
// numbers of sectors and page sizes must be ones [GeometryFor] accepts, because
// this is where a volume's geometry is chosen and it is fixed from here on. It
// is an index object holding a root and no segments, and no parts at all.
func (s *Store) Root(ctx context.Context, ref control.Ref, volumes map[string]VolumeSpec) (*Index, error) {
	if !validName(ref.VM) || ref.Sequence == 0 {
		return nil, ErrInvalidConfig
	}
	index := newIndex(s, ref)
	for name, spec := range volumes {
		if !validName(name) || spec.Size%SectorSize != 0 {
			return nil, ErrInvalidConfig
		}
		geometry, err := GeometryFor(spec.PageSize)
		if err != nil {
			return nil, err
		}
		index.volumes[name] = &volumeTable{size: spec.Size, geometry: geometry,
			segments: make(map[uint64]segmentEntry)}
		index.names = append(index.names, name)
	}
	slices.Sort(index.names)
	index.checkpoints[ref] = checkpointCost{}
	data, err := newIndexObject(s).seal(ctx, index)
	if err != nil {
		return nil, err
	}
	if err := s.putIndexObject(ctx, ref, data); err != nil {
		return nil, err
	}
	return index, nil
}

// acquire takes one slot of the store's host-wide upload budget, waiting under
// ctx. Every caller releases what it took.
func (s *Store) acquire(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	select {
	case s.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (s *Store) release() { <-s.slots }

// acquireBuilder takes one slot of the store's host-wide budget for part
// builders, waiting under ctx. A publication holds it from the first member it
// writes until it finishes or is abandoned.
func (s *Store) acquireBuilder(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	select {
	case s.builders <- struct{}{}:
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	s.builderMu.Lock()
	s.liveBuilders++
	s.builderMu.Unlock()
	return nil
}

// acquireDelete takes one slot of reclamation's own budget, waiting under ctx.
func (s *Store) acquireDelete(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	select {
	case s.deletes <- struct{}{}:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (s *Store) releaseDelete() { <-s.deletes }

func (s *Store) releaseBuilder() {
	s.builderMu.Lock()
	s.liveBuilders--
	s.builderMu.Unlock()
	<-s.builders
}

// digestAttribute is the object attribute every object this store writes
// carries: the digest of its logical contents. It is what a retry compares
// against, so settling that a publication already landed costs one HEAD per
// object rather than reading every part back.
const digestAttribute = "sproutfs-digest"

// digestOf is the attribute value for one object's logical bytes.
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// putObject writes one immutable object create-if-absent, carrying the digest
// of what it holds. An object already there is settled from its own metadata:
// the digest attribute says whether it holds what this caller is publishing, so
// a retried publication is idempotent and reusing a reference for other
// contents is a conflict, at the cost of one HEAD rather than reading the whole
// object back. An object written without the attribute is compared through
// same, which is the only path that reads one.
func (s *Store) putObject(ctx context.Context, key platform.ObjectKey, data []byte, digest string, same func([]byte) error) error {
	_, err := s.objects.Put(ctx, platform.PutRequest{
		Key: key, Body: bytes.NewReader(data), Size: int64(len(data)),
		Attributes: map[string]string{digestAttribute: digest},
		Conditions: platform.PutConditions{IfNoneMatch: true},
	})
	if err == nil {
		return nil
	}
	if !errors.Is(err, platform.ErrPrecondition) {
		return err
	}
	// A retry of a publication finds its own byte-identical objects and carries
	// on. Giving up instead is what a real publication does when the store
	// stops answering partway: the checkpoint fails, the parent stays readable
	// and selected, and the next attempt must still succeed.
	if sim.Buggify(ctx, "checkpoint/give-up-on-existing-part", 0.5) {
		return errors.Join(ErrConflict, err)
	}
	metadata, headErr := s.objects.Head(ctx, key)
	if headErr != nil {
		return errors.Join(ErrConflict, headErr)
	}
	if found, stated := metadata.Attributes[digestAttribute]; stated {
		if found == digest {
			return nil
		}
		return ErrConflict
	}
	existing, _, readErr := platform.ReadObject(ctx, s.objects, key, 0, maximumPartSize, ErrCorrupt)
	if readErr != nil {
		return errors.Join(ErrConflict, readErr)
	}
	return same(existing)
}

// ReadState returns the VMM state published with a checkpoint.
func (s *Store) ReadState(ctx context.Context, index *Index) ([]byte, error) {
	if index.state.isZero() {
		return nil, ErrNoState
	}
	return s.readMember(ctx, index.state, maximumStateSize)
}

// Read fills dst from a volume of a published checkpoint. The range is served
// as runs of pages, one per maximumRunBytes of it: the segments locating a
// run's pages are read once each however many pages they locate, its members
// are fetched in as few ranged reads as the layout allows, and a page with no
// member reads as zeroes and costs no request at all. On error dst may be
// partially filled.
func (s *Store) Read(ctx context.Context, index *Index, volume string, offset uint64, dst []byte) error {
	return s.ReadPages(ctx, index, volume, offset, dst, nil)
}

// ReadPages is Read of only the pages of the range that wanted marks — one
// element per page the range touches, or nil for every one of them. The bytes
// of a page nothing wants are left as the caller had them and cost neither a
// request nor a segment lookup, and the pages that are wanted are still grouped
// into as few ranged reads as the layout allows: a hole in the middle of a run
// is read through where reading through it is cheaper than a second round trip,
// exactly as a hole the volume itself has is.
//
// That is what a pager's window read is. A fault's window is one run of pages
// of which the ones the memory region already holds resident need no bytes, and asking
// for the run with those pages left out costs what the run costs — where asking
// for each stretch of it separately costs a request per stretch.
func (s *Store) ReadPages(ctx context.Context, index *Index, volume string, offset uint64, dst []byte, wanted []bool) error {
	table := index.volumes[volume]
	if table == nil {
		return ErrUnknownVolume
	}
	length := uint64(len(dst))
	if offset > table.size || length > table.size-offset {
		return ErrInvalidRange
	}
	if length == 0 {
		return context.Cause(ctx)
	}
	first := table.geometry.PageOf(offset)
	if wanted != nil && uint64(len(wanted)) != table.geometry.PageOf(offset+length-1)-first+1 {
		return ErrInvalidRange
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	for cursor := offset; cursor < offset+length; {
		limit := min(offset+length, runLimit(table.geometry, cursor))
		run, err := s.resolveRun(ctx, index, volume, cursor, dst[cursor-offset:limit-offset], wanted, first)
		if err != nil {
			return err
		}
		if len(run) > 0 {
			if err := s.readRun(ctx, table.geometry, volume, run); err != nil {
				return err
			}
		}
		cursor = limit
	}
	return nil
}

// resolveRun reports where the bytes of every page of a range live, filling the
// pages that have no member with zeroes as it goes. One segment is held across
// the pages it locates, so a run costs one lookup of each segment it crosses
// rather than one per page. A page wanted does not mark is skipped whole: its
// bytes are the caller's and its segment is never looked up. first is the page
// wanted is indexed from, which is the first page of the whole read and not of
// this run.
func (s *Store) resolveRun(ctx context.Context, index *Index, volume string, offset uint64, dst []byte,
	wanted []bool, first uint64) ([]pageRead, error) {
	table := index.volumes[volume]
	geometry := table.geometry
	end := offset + uint64(len(dst))
	var run []pageRead
	var held *segment
	var current uint64
	for cursor := offset; cursor < end; {
		number := geometry.PageOf(cursor)
		start, span := geometry.PageSpan(table.size, number)
		limit := min(end, start+span)
		if wanted != nil && !wanted[number-first] {
			cursor = limit
			continue
		}
		if held == nil || current != geometry.SegmentOf(number) {
			loaded, err := index.segmentAt(ctx, volume, geometry.SegmentOf(number))
			if err != nil {
				return nil, err
			}
			held, current = loaded, geometry.SegmentOf(number)
		}
		target := dst[cursor-offset : limit-offset]
		if at, found := held.pages[geometry.OffsetIn(number)]; found {
			run = append(run, pageRead{number: number, at: at, within: cursor - start, dst: target})
		} else {
			clear(target)
		}
		cursor = limit
	}
	return run, nil
}

// loadPage fetches one page's decoded bytes, through the shared cache when one
// is configured. The cache is keyed by the page's identity — the
// checkpoint the page was first published under, which the index carries as the
// member's origin — rather than by the checkpoint whose part currently holds it,
// so a fork hits its parent's entries and compaction moving the bytes costs
// neither a refetch nor a second entry.
func (s *Store) loadPage(ctx context.Context, geometry Geometry, volume string, number uint64, at location) ([]byte, func(), error) {
	fetch := func(ctx context.Context) ([]byte, error) {
		data, err := s.readMember(ctx, at, int64(geometry.PageSize))
		if err != nil {
			return nil, err
		}
		if len(data) == 0 || len(data)%SectorSize != 0 {
			return nil, ErrCorrupt
		}
		return data, nil
	}
	if s.cache == nil {
		data, err := fetch(ctx)
		return data, func() {}, err
	}
	return s.cache.get(ctx, pageKey(identityOf(volume, number, at)), fetch)
}

// loadSegment fetches one segment's encoded page table out of the index object
// of the checkpoint that wrote it, through the shared cache when one is
// configured. It is keyed by the segment's identity — that checkpoint, this
// volume and this number — so two roots addressing the same segment share one
// copy however each of them found it. The caller decodes the bytes it borrows
// and releases them.
func (s *Store) loadSegment(ctx context.Context, volume string, number uint64, at segmentAddress) ([]byte, func(), error) {
	fetch := func(ctx context.Context) ([]byte, error) {
		key, err := s.indexKey(at.ref)
		if err != nil {
			return nil, err
		}
		encoded, err := s.readRange(ctx, key, at.offset, at.length, maximumSegmentExtent)
		if err != nil {
			if errors.Is(err, platform.ErrNotFound) || errors.Is(err, platform.ErrInvalidRange) {
				return nil, errors.Join(ErrCorrupt, err)
			}
			return nil, err
		}
		data, err := s.codecs.Decode(ctx, encoded, maximumSegmentSize)
		if err != nil {
			return nil, errors.Join(ErrCorrupt, err)
		}
		return data, nil
	}
	if s.cache == nil {
		data, err := fetch(ctx)
		return data, func() {}, err
	}
	return s.cache.get(ctx, segmentCacheKey(volume, number, at.ref), fetch)
}

// readMember fetches one member of a part by range and decodes its envelope,
// which is what makes a page one read whatever else the part holds.
func (s *Store) readMember(ctx context.Context, at location, maximum int64) ([]byte, error) {
	key, err := s.partKey(at.ref, at.part)
	if err != nil {
		return nil, err
	}
	encoded, err := s.readRange(ctx, key, at.offset, at.length, int64(maximum)+blob.HeaderSize)
	if err != nil {
		if errors.Is(err, platform.ErrNotFound) || errors.Is(err, platform.ErrInvalidRange) {
			return nil, errors.Join(ErrCorrupt, err)
		}
		return nil, err
	}
	data, err := s.codecs.Decode(ctx, encoded, int(maximum))
	if err != nil {
		return nil, errors.Join(ErrCorrupt, err)
	}
	return data, nil
}

// readRange reads exactly length bytes at offset of one object. The response
// must echo the key and return exactly the bytes asked for; anything else is a
// corrupt read rather than a short one.
func (s *Store) readRange(ctx context.Context, key platform.ObjectKey, offset, length uint64, maximum int64) ([]byte, error) {
	if length == 0 || length > uint64(maximum) {
		return nil, ErrCorrupt
	}
	result, err := s.objects.Get(ctx, platform.GetRequest{
		Key: key, Range: &platform.ByteRange{Offset: int64(offset), Length: int64(length)}})
	if err != nil {
		return nil, err
	}
	defer result.Body.Close()
	if result.Metadata.Key != key || result.Metadata.ETag == "" || result.ContentLength != int64(length) {
		return nil, ErrCorrupt
	}
	data, err := io.ReadAll(io.LimitReader(result.Body, int64(length)+1))
	if err != nil {
		return nil, err
	}
	if uint64(len(data)) != length {
		return nil, ErrCorrupt
	}
	return data, nil
}

// readSuffix reads the last suffix bytes of one object and reports the size of
// the whole object with them; an object shorter than the suffix comes back
// whole. The response must echo the key and return exactly as many bytes as its
// own size says a suffix that long holds; anything else is a corrupt read
// rather than a short one.
func (s *Store) readSuffix(ctx context.Context, key platform.ObjectKey, suffix int64) ([]byte, uint64, error) {
	result, err := s.objects.Get(ctx, platform.GetRequest{
		Key: key, Range: &platform.ByteRange{Suffix: suffix}})
	if err != nil {
		return nil, 0, err
	}
	defer result.Body.Close()
	if result.Metadata.Key != key || result.Metadata.ETag == "" || result.Metadata.Size < 0 ||
		result.ContentLength != min(result.Metadata.Size, suffix) {
		return nil, 0, ErrCorrupt
	}
	data, err := io.ReadAll(io.LimitReader(result.Body, result.ContentLength+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(data)) != result.ContentLength {
		return nil, 0, ErrCorrupt
	}
	return data, uint64(result.Metadata.Size), nil
}

// partTable is what one part says about itself: the members it names, the size
// of its member memory region, and, in a checkpoint's last part alone, how many parts
// the checkpoint has.
type partTable struct {
	members []part.Member
	body    uint64
	parts   uint32
}

// partTailSize is the tail of a part one read fetches: every table a writer
// produces fits in maximumTableSize, and the trailer naming it is the last
// TrailerSize bytes of the part. A part shorter than that is read whole.
const partTailSize = maximumTableSize + part.TrailerSize

// readPartTable reads one part's tail and decodes the trailer and the table out
// of it. The tail is bounded, so this is one request rather than a Head for the
// part's size and a range read of each. A member naming a volume no key could
// hold is corrupt here rather than unknown later.
func (s *Store) readPartTable(ctx context.Context, ref control.Ref, number uint32) (partTable, error) {
	key, err := s.partKey(ref, number)
	if err != nil {
		return partTable{}, err
	}
	tail, size, err := s.readSuffix(ctx, key, partTailSize)
	if err != nil {
		return partTable{}, err
	}
	if size < part.TrailerSize || size > maximumPartSize {
		return partTable{}, ErrCorrupt
	}
	trailer, err := part.DecodeTrailer(tail[uint64(len(tail))-part.TrailerSize:], size)
	if err != nil {
		return partTable{}, errors.Join(ErrCorrupt, err)
	}
	// The trailer has located the table within the part; what this read holds
	// is the part's last len(tail) bytes, so a table starting before them is
	// one the bound a writer respects says cannot exist.
	base := size - uint64(len(tail))
	if trailer.TableOffset < base {
		return partTable{}, ErrCorrupt
	}
	table := tail[trailer.TableOffset-base:][:trailer.TableLength]
	members, err := part.DecodeTable(table, trailer.TableOffset)
	if err != nil {
		return partTable{}, errors.Join(ErrCorrupt, err)
	}
	for _, item := range members {
		if !item.State && !validName(item.Volume) {
			return partTable{}, ErrCorrupt
		}
	}
	return partTable{members: members, body: trailer.TableOffset, parts: trailer.Parts}, nil
}

// putIndexObject writes one checkpoint's index object create-if-absent, which
// is the publication's commit. A retry of the same publication finds its own
// byte-identical object and carries on; a different one under the same
// reference is a conflict.
func (s *Store) putIndexObject(ctx context.Context, ref control.Ref, data []byte) error {
	key, err := s.indexKey(ref)
	if err != nil {
		return err
	}
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	return s.putObject(ctx, key, data, digestOf(data), func(existing []byte) error {
		if !equalParts(existing, data) {
			return ErrConflict
		}
		return nil
	})
}
