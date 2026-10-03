package checkpoint

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// The page cache's disk is a log of fixed-size disk regions. Each item in a
// region is one envelope, stored with a header that names it:
//
//	offset size
//	0      4    magic "SFCI"
//	4      1    format version, 1
//	5      1    kind: 0 a page, 1 a segment
//	6      1    the item's stripe index in its code
//	7      1    k, the code's data stripes
//	8      1    m, the code's parity stripes
//	9      1    zero
//	10     2    the pages one window of its volume spans
//	12     2    length of the checkpoint's VM identity
//	14     2    length of the volume's name
//	16     8    the checkpoint's sequence
//	24     8    the page, or the segment's number
//	32     4    the length of the bytes that follow the names
//	36     4    CRC32C of the header with this field zero, the names and the bytes
//	40          the VM identity, the volume's name, then the bytes
//
// A whole envelope is stripe 0 of the code 1+0. Striping changes the code and
// the index, and nothing else of the format.
//
// A closed region ends with its table, then a fixed trailer:
//
//	table entry: kind, stripe, k, m (1 each), span, VM and volume lengths (2
//	each), sequence and page (8 each), the item's offset in the region and
//	its length (4 each), then the VM identity and the volume's name.
//
//	trailer, the region's last 40 bytes:
//	0  4 magic "SFCT"
//	4  1 format version, 1
//	5  3 zero
//	8  8 the region's sequence number
//	16 8 the file's generation
//	24 4 the number of table entries
//	28 4 the table's length
//	32 4 CRC32C of the table and the trailer's first 32 bytes
//	36 4 zero
//
// A region is written whole in the order its items arrive, and its table only
// once those items are synced, so a table never names an item that is not on
// the disk. A region whose table is lost can still be read back by scanning
// its headers from its start.
//
// The file begins with its header, alone in the file's first region-sized
// span, so every region starts on a region boundary:
//
//	0  4  magic "SFCH"
//	4  1  format version, 1
//	5  3  zero
//	8  8  the region size
//	16 16 the cache's identity
//	32 8  the file's generation
//	40 2  the length of the object store's kind
//	42 2  the length of the bucket's name
//	44 2  the length of the prefix
//	46 2  zero
//	48    the kind, the bucket's name and the prefix, then a CRC32C of all
//	      that comes before it (4)
//
// The identity is drawn when the file is made, and kept while the file is.
// Every table carries the file's generation. A file made again over an old one
// takes a new generation, so a table the old file left behind is never read
// as one of the new file's, even where the device kept it.
const (
	diskFormatVersion = 1
	diskItemFixed     = 40
	diskTableFixed    = 34
	diskTrailerSize   = 40
	diskHeaderFixed   = 48
	// maximumDiskItem is the longest envelope an item holds: what its length
	// field and the index's 24 bits of length both carry. A segment's or a
	// 2 MiB page's envelope is far shorter.
	maximumDiskItem = 1<<24 - 1
)

var (
	diskItemMagic   = [4]byte{'S', 'F', 'C', 'I'}
	diskTableMagic  = [4]byte{'S', 'F', 'C', 'T'}
	diskHeaderMagic = [4]byte{'S', 'F', 'C', 'H'}
	diskChecksum    = crc32.MakeTable(crc32.Castagnoli)
)

// diskCode is where an item sits in its envelope's erasure code: stripe of k
// data stripes and m parity stripes.
type diskCode struct {
	stripe, k, m uint8
}

// wholeEnvelope is the code of an item that is its envelope whole.
var wholeEnvelope = diskCode{stripe: 0, k: 1, m: 0}

// errItemKey and errItemDamaged are the two ways an item read back fails its
// checks: it names another key, or it is not what was written.
var (
	errItemKey     = errors.New("checkpoint: the disk's item names another key")
	errItemDamaged = errors.New("checkpoint: the disk's item fails its checksum")
)

// itemHeaderBytes is the length of an item's header under key.
func itemHeaderBytes(key diskKey) int64 {
	return diskItemFixed + int64(len(key.Ref.VM)+len(key.Volume))
}

// tableEntryBytes is what one item under key adds to its region's table.
func tableEntryBytes(key diskKey) int64 {
	return diskTableFixed + int64(len(key.Ref.VM)+len(key.Volume))
}

// storable reports whether key's names fit the format's length fields.
func storable(key diskKey) bool {
	return len(key.Ref.VM) <= 0xffff && len(key.Volume) <= 0xffff
}

func kindOf(key diskKey) uint8 {
	if key.segment {
		return 1
	}
	return 0
}

// encodeItem is one item as it lies on the disk: its header, then data.
func encodeItem(key diskKey, code diskCode, data []byte) []byte {
	header := itemHeaderBytes(key)
	item := make([]byte, header+int64(len(data)))
	copy(item[0:4], diskItemMagic[:])
	item[4] = diskFormatVersion
	item[5] = kindOf(key)
	item[6], item[7], item[8] = code.stripe, code.k, code.m
	binary.LittleEndian.PutUint16(item[10:], key.span)
	binary.LittleEndian.PutUint16(item[12:], uint16(len(key.Ref.VM)))
	binary.LittleEndian.PutUint16(item[14:], uint16(len(key.Volume)))
	binary.LittleEndian.PutUint64(item[16:], key.Ref.Sequence)
	binary.LittleEndian.PutUint64(item[24:], key.Page)
	binary.LittleEndian.PutUint32(item[32:], uint32(len(data)))
	at := copy(item[diskItemFixed:], key.Ref.VM)
	at += copy(item[diskItemFixed+at:], key.Volume)
	copy(item[diskItemFixed+at:], data)
	binary.LittleEndian.PutUint32(item[36:], crc32.Checksum(item, diskChecksum))
	return item
}

// parsedItem is what an item read back says of itself.
type parsedItem struct {
	key  diskKey
	code diskCode
	data []byte
}

// itemLength is the whole length of the item whose header begins with fixed,
// by what the header says. It reports false for a header that does not hold
// together.
func itemLength(fixed []byte) (int64, bool) {
	if len(fixed) < diskItemFixed || [4]byte(fixed[0:4]) != diskItemMagic || fixed[4] != diskFormatVersion ||
		fixed[5] > 1 || fixed[9] != 0 {
		return 0, false
	}
	return diskItemFixed + int64(binary.LittleEndian.Uint16(fixed[12:])) +
		int64(binary.LittleEndian.Uint16(fixed[14:])) + int64(binary.LittleEndian.Uint32(fixed[32:])), true
}

// itemKey is the key an item's header names, read without trusting the rest
// of the item. It reports false for a header that does not hold together.
func itemKey(item []byte) (diskKey, bool) {
	if _, ok := itemLength(item); !ok {
		return diskKey{}, false
	}
	vm := int(binary.LittleEndian.Uint16(item[12:]))
	volume := int(binary.LittleEndian.Uint16(item[14:]))
	if len(item) < diskItemFixed+vm+volume {
		return diskKey{}, false
	}
	names := item[diskItemFixed:]
	return diskKey{cacheKey: cacheKey{Identity: control.Identity{
		Ref:    control.Ref{VM: string(names[:vm]), Sequence: binary.LittleEndian.Uint64(item[16:])},
		Volume: string(names[vm : vm+volume]), Page: binary.LittleEndian.Uint64(item[24:])},
		segment: item[5] == 1},
		span: binary.LittleEndian.Uint16(item[10:])}, true
}

// parseItem reads an item's header and bytes back without trusting either: a
// header that does not hold together, a length that is not the item's, or
// bytes that fail the checksum, are damage. skipChecksum is the guard that
// leaves the checksum unchecked.
func parseItem(item []byte, skipChecksum bool) (parsedItem, error) {
	key, ok := itemKey(item)
	if !ok {
		return parsedItem{}, errItemDamaged
	}
	header := itemHeaderBytes(key)
	if int64(len(item)) != header+int64(binary.LittleEndian.Uint32(item[32:])) {
		return parsedItem{}, errItemDamaged
	}
	if !skipChecksum {
		stored := binary.LittleEndian.Uint32(item[36:])
		sum := crc32.Update(0, diskChecksum, item[:36])
		sum = crc32.Update(sum, diskChecksum, []byte{0, 0, 0, 0})
		if crc32.Update(sum, diskChecksum, item[diskItemFixed:]) != stored {
			return parsedItem{}, errItemDamaged
		}
	}
	return parsedItem{key: key, code: diskCode{stripe: item[6], k: item[7], m: item[8]}, data: item[header:]}, nil
}

// tableItem is one item a region's table names: its key, code, and where it
// lies in the region.
type tableItem struct {
	key            diskKey
	code           diskCode
	offset, length uint32
}

// encodeTable is a region's table and trailer, which close the region at its
// end, under the file's generation.
func encodeTable(sequence, generation uint64, items []tableItem) []byte {
	var size int64
	for _, item := range items {
		size += tableEntryBytes(item.key)
	}
	table := make([]byte, size+diskTrailerSize)
	at := 0
	for _, item := range items {
		entry := table[at:]
		entry[0] = kindOf(item.key)
		entry[1], entry[2], entry[3] = item.code.stripe, item.code.k, item.code.m
		binary.LittleEndian.PutUint16(entry[4:], item.key.span)
		binary.LittleEndian.PutUint16(entry[6:], uint16(len(item.key.Ref.VM)))
		binary.LittleEndian.PutUint16(entry[8:], uint16(len(item.key.Volume)))
		binary.LittleEndian.PutUint64(entry[10:], item.key.Ref.Sequence)
		binary.LittleEndian.PutUint64(entry[18:], item.key.Page)
		binary.LittleEndian.PutUint32(entry[26:], item.offset)
		binary.LittleEndian.PutUint32(entry[30:], item.length)
		names := copy(entry[diskTableFixed:], item.key.Ref.VM)
		names += copy(entry[diskTableFixed+names:], item.key.Volume)
		at += diskTableFixed + names
	}
	trailer := table[size:]
	copy(trailer[0:4], diskTableMagic[:])
	trailer[4] = diskFormatVersion
	binary.LittleEndian.PutUint64(trailer[8:], sequence)
	binary.LittleEndian.PutUint64(trailer[16:], generation)
	binary.LittleEndian.PutUint32(trailer[24:], uint32(len(items)))
	binary.LittleEndian.PutUint32(trailer[28:], uint32(size))
	binary.LittleEndian.PutUint32(trailer[32:], crc32.Checksum(table[:size+32], diskChecksum))
	return table
}

// errNoTable reports a region whose end holds no table at all: a region that
// was open, or one whose table never reached the disk. errTornTable reports a
// region whose end holds a table's trailer, and a table that fails its
// checksum or does not hold together.
var (
	errNoTable   = errors.New("checkpoint: the disk region has no table")
	errTornTable = errors.New("checkpoint: the disk region's table is torn")
)

// regionTable is what a region's table says: the region's place in the order
// of the log, the generation of the file it was written in, and every item it
// names, in the order they lie.
type regionTable struct {
	sequence, generation uint64
	items                []tableItem
}

// readRegionTable reads back the table that closes the region of regionBytes
// at base. A region with no trailer reports errNoTable, and one whose table
// fails its checksum or does not hold together reports errTornTable. A failed
// read reports its own error.
func readRegionTable(ctx context.Context, file platform.File, base, regionBytes int64) (regionTable, error) {
	trailer := make([]byte, diskTrailerSize)
	if err := readFull(ctx, file, trailer, base+regionBytes-diskTrailerSize); err != nil {
		return regionTable{}, err
	}
	if [4]byte(trailer[0:4]) != diskTableMagic {
		return regionTable{}, errNoTable
	}
	size := int64(binary.LittleEndian.Uint32(trailer[28:]))
	if trailer[4] != diskFormatVersion || size > regionBytes-diskTrailerSize {
		return regionTable{}, errTornTable
	}
	table := make([]byte, size+diskTrailerSize)
	if err := readFull(ctx, file, table, base+regionBytes-int64(len(table))); err != nil {
		return regionTable{}, err
	}
	if sim.Buggify(ctx, buggifyDiskTornTableOnOpen, 0.25) {
		clear(table[:len(table)/2])
	}
	if crc32.Checksum(table[:size+32], diskChecksum) != binary.LittleEndian.Uint32(table[size+32:]) {
		return regionTable{}, errTornTable
	}
	count := int(binary.LittleEndian.Uint32(table[size+24:]))
	items := make([]tableItem, 0, min(count, int(size/diskTableFixed)))
	for at := int64(0); len(items) < count; {
		if at+diskTableFixed > size {
			return regionTable{}, errTornTable
		}
		entry := table[at:size]
		vm := int64(binary.LittleEndian.Uint16(entry[6:]))
		volume := int64(binary.LittleEndian.Uint16(entry[8:]))
		if diskTableFixed+vm+volume > int64(len(entry)) {
			return regionTable{}, errTornTable
		}
		names := entry[diskTableFixed:]
		item := tableItem{
			key: diskKey{cacheKey: cacheKey{Identity: control.Identity{
				Ref:    control.Ref{VM: string(names[:vm]), Sequence: binary.LittleEndian.Uint64(entry[10:])},
				Volume: string(names[vm : vm+volume]), Page: binary.LittleEndian.Uint64(entry[18:])},
				segment: entry[0] == 1},
				span: binary.LittleEndian.Uint16(entry[4:])},
			code:   diskCode{stripe: entry[1], k: entry[2], m: entry[3]},
			offset: binary.LittleEndian.Uint32(entry[26:]), length: binary.LittleEndian.Uint32(entry[30:])}
		if item.length > maximumDiskItem {
			return regionTable{}, errTornTable
		}
		items = append(items, item)
		at += diskTableFixed + vm + volume
	}
	return regionTable{sequence: binary.LittleEndian.Uint64(table[size+8:]),
		generation: binary.LittleEndian.Uint64(table[size+16:]), items: items}, nil
}

// scanRegion reads the items of the region of regionBytes at base back from
// their headers, from its start, without its table. It reports every item
// that reads back intact, in the order they lie. An item that fails its
// checksum is stepped over by the length its header gives; the scan stops at
// the first header that does not hold together or would run into the
// trailer. A failed read stops it, and is reported with what was found.
func scanRegion(ctx context.Context, file platform.File, base, regionBytes int64) ([]tableItem, error) {
	var items []tableItem
	limit := regionBytes - diskTrailerSize
	fixed := make([]byte, diskItemFixed)
	for at := int64(0); at+diskItemFixed <= limit; {
		if err := readFull(ctx, file, fixed, base+at); err != nil {
			return items, err
		}
		size, ok := itemLength(fixed)
		if !ok || at+size > limit {
			break
		}
		item := make([]byte, size)
		if err := readFull(ctx, file, item, base+at); err != nil {
			return items, err
		}
		if parsed, err := parseItem(item, false); err == nil {
			items = append(items, tableItem{key: parsed.key, code: parsed.code, offset: uint32(at),
				length: uint32(len(parsed.data))})
		}
		at += size
	}
	return items, nil
}

// CacheIdentity names one page cache's disk. It is drawn when the cache's
// file is made, and kept for as long as the file is, across every restart of
// the host over it.
type CacheIdentity [16]byte

func (i CacheIdentity) String() string { return hex.EncodeToString(i[:]) }

// CacheDeployment is the deployment a page cache's disk belongs to: the kind
// of object store, its bucket and the prefix of the deployment's objects, as
// the host knows them. A page's identity is unique only within one
// deployment, so a file of another deployment is emptied.
type CacheDeployment struct {
	Store, Bucket, Prefix string
}

// storable reports whether the deployment's names fit the header's length
// fields.
func (d CacheDeployment) storable() bool {
	return len(d.Store) <= 0xffff && len(d.Bucket) <= 0xffff && len(d.Prefix) <= 0xffff
}

// diskHeader is what a cache file's header says.
type diskHeader struct {
	regionBytes int64
	identity    CacheIdentity
	generation  uint64
	deployment  CacheDeployment
}

// diskHeaderBytes is the length of the header naming deployment.
func diskHeaderBytes(deployment CacheDeployment) int64 {
	return diskHeaderFixed + int64(len(deployment.Store)+len(deployment.Bucket)+len(deployment.Prefix)) + 4
}

// encodeDiskHeader is the header as it lies at the start of the file.
func encodeDiskHeader(header diskHeader) []byte {
	deployment := header.deployment
	encoded := make([]byte, diskHeaderBytes(deployment))
	copy(encoded[0:4], diskHeaderMagic[:])
	encoded[4] = diskFormatVersion
	binary.LittleEndian.PutUint64(encoded[8:], uint64(header.regionBytes))
	copy(encoded[16:32], header.identity[:])
	binary.LittleEndian.PutUint64(encoded[32:], header.generation)
	binary.LittleEndian.PutUint16(encoded[40:], uint16(len(deployment.Store)))
	binary.LittleEndian.PutUint16(encoded[42:], uint16(len(deployment.Bucket)))
	binary.LittleEndian.PutUint16(encoded[44:], uint16(len(deployment.Prefix)))
	at := diskHeaderFixed + copy(encoded[diskHeaderFixed:], deployment.Store)
	at += copy(encoded[at:], deployment.Bucket)
	at += copy(encoded[at:], deployment.Prefix)
	binary.LittleEndian.PutUint32(encoded[at:], crc32.Checksum(encoded[:at], diskChecksum))
	return encoded
}

// The ways a file's header is refused before what it says is compared: there
// is none, it is damaged, or it is of another format.
var (
	errNoHeader      = errors.New("checkpoint: the page cache's disk has no header")
	errHeaderDamaged = errors.New("checkpoint: the page cache's disk's header is damaged")
	errHeaderFormat  = errors.New("checkpoint: the page cache's disk is of another format")
)

// readDiskHeader reads back the header at the start of file, which is never
// longer than limit.
func readDiskHeader(ctx context.Context, file platform.File, limit int64) (diskHeader, error) {
	fixed := make([]byte, diskHeaderFixed)
	if err := readFull(ctx, file, fixed, 0); errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return diskHeader{}, errNoHeader
	} else if err != nil {
		return diskHeader{}, err
	}
	if [4]byte(fixed[0:4]) != diskHeaderMagic {
		return diskHeader{}, errNoHeader
	}
	if fixed[4] != diskFormatVersion {
		return diskHeader{}, fmt.Errorf("%w: version %d", errHeaderFormat, fixed[4])
	}
	store := int64(binary.LittleEndian.Uint16(fixed[40:]))
	bucket := int64(binary.LittleEndian.Uint16(fixed[42:]))
	prefix := int64(binary.LittleEndian.Uint16(fixed[44:]))
	size := diskHeaderFixed + store + bucket + prefix + 4
	if size > limit {
		return diskHeader{}, errHeaderDamaged
	}
	encoded := make([]byte, size)
	if err := readFull(ctx, file, encoded, 0); errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return diskHeader{}, errHeaderDamaged
	} else if err != nil {
		return diskHeader{}, err
	}
	if sim.Buggify(ctx, buggifyDiskTornHeader, 0.5) {
		clear(encoded[len(encoded)/2:])
	}
	if crc32.Checksum(encoded[:size-4], diskChecksum) != binary.LittleEndian.Uint32(encoded[size-4:]) {
		return diskHeader{}, errHeaderDamaged
	}
	names := encoded[diskHeaderFixed : size-4]
	header := diskHeader{regionBytes: int64(binary.LittleEndian.Uint64(encoded[8:])),
		generation: binary.LittleEndian.Uint64(encoded[32:]),
		deployment: CacheDeployment{Store: string(names[:store]), Bucket: string(names[store : store+bucket]),
			Prefix: string(names[store+bucket:])}}
	copy(header.identity[:], encoded[16:32])
	return header, nil
}

// readFull reads exactly len(buffer) bytes at offset.
func readFull(ctx context.Context, file platform.File, buffer []byte, offset int64) error {
	n, err := file.ReadAt(ctx, buffer, offset)
	if n == len(buffer) && (err == nil || errors.Is(err, io.EOF)) {
		return nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return err
}
