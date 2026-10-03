package checkpoint

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
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
//	trailer, the region's last 32 bytes:
//	0  4 magic "SFCT"
//	4  1 format version, 1
//	5  3 zero
//	8  8 the region's sequence number
//	16 4 the number of table entries
//	20 4 the table's length
//	24 4 CRC32C of the table and the trailer's first 24 bytes
//	28 4 zero
//
// A region is written whole in the order its items arrive, and its table only
// once those items are synced, so a table never names an item that is not on
// the disk. A region whose table is lost can still be read back by scanning
// its headers from its start.
const (
	diskFormatVersion = 1
	diskItemFixed     = 40
	diskTableFixed    = 34
	diskTrailerSize   = 32
	// maximumDiskItem is the longest envelope an item holds: what its length
	// field and the index's 24 bits of length both carry. A segment's or a
	// 2 MiB page's envelope is far shorter.
	maximumDiskItem = 1<<24 - 1
)

var (
	diskItemMagic  = [4]byte{'S', 'F', 'C', 'I'}
	diskTableMagic = [4]byte{'S', 'F', 'C', 'T'}
	diskChecksum   = crc32.MakeTable(crc32.Castagnoli)
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

// itemKey is the key an item's header names, read without trusting the rest
// of the item. It reports false for a header that does not hold together.
func itemKey(item []byte) (diskKey, bool) {
	if len(item) < diskItemFixed || [4]byte(item[0:4]) != diskItemMagic || item[4] != diskFormatVersion ||
		item[5] > 1 || item[9] != 0 {
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
// end.
func encodeTable(sequence uint64, items []tableItem) []byte {
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
	binary.LittleEndian.PutUint32(trailer[16:], uint32(len(items)))
	binary.LittleEndian.PutUint32(trailer[20:], uint32(size))
	binary.LittleEndian.PutUint32(trailer[24:], crc32.Checksum(table[:size+24], diskChecksum))
	return table
}

// errNoTable reports a region whose end holds no table that checks out.
var errNoTable = errors.New("checkpoint: the disk region has no intact table")

// readRegionTable reads back the table that closes the region of regionBytes
// at base: its sequence number and every item it names. A region with no
// table, or one that fails its checksum, reports errNoTable.
func readRegionTable(ctx context.Context, file platform.File, base, regionBytes int64) (uint64, []tableItem, error) {
	trailer := make([]byte, diskTrailerSize)
	if err := readFull(ctx, file, trailer, base+regionBytes-diskTrailerSize); err != nil {
		return 0, nil, err
	}
	size := int64(binary.LittleEndian.Uint32(trailer[20:]))
	if [4]byte(trailer[0:4]) != diskTableMagic || trailer[4] != diskFormatVersion ||
		size > regionBytes-diskTrailerSize {
		return 0, nil, errNoTable
	}
	table := make([]byte, size+diskTrailerSize)
	if err := readFull(ctx, file, table, base+regionBytes-int64(len(table))); err != nil {
		return 0, nil, err
	}
	if crc32.Checksum(table[:size+24], diskChecksum) != binary.LittleEndian.Uint32(table[size+24:]) {
		return 0, nil, errNoTable
	}
	count := int(binary.LittleEndian.Uint32(table[size+16:]))
	items := make([]tableItem, 0, count)
	for at := int64(0); len(items) < count; {
		if at+diskTableFixed > size {
			return 0, nil, errNoTable
		}
		entry := table[at:size]
		vm := int64(binary.LittleEndian.Uint16(entry[6:]))
		volume := int64(binary.LittleEndian.Uint16(entry[8:]))
		if diskTableFixed+vm+volume > int64(len(entry)) {
			return 0, nil, errNoTable
		}
		names := entry[diskTableFixed:]
		items = append(items, tableItem{
			key: diskKey{cacheKey: cacheKey{Identity: control.Identity{
				Ref:    control.Ref{VM: string(names[:vm]), Sequence: binary.LittleEndian.Uint64(entry[10:])},
				Volume: string(names[vm : vm+volume]), Page: binary.LittleEndian.Uint64(entry[18:])},
				segment: entry[0] == 1},
				span: binary.LittleEndian.Uint16(entry[4:])},
			code:   diskCode{stripe: entry[1], k: entry[2], m: entry[3]},
			offset: binary.LittleEndian.Uint32(entry[26:]), length: binary.LittleEndian.Uint32(entry[30:])})
		at += diskTableFixed + vm + volume
	}
	return binary.LittleEndian.Uint64(table[size+8:]), items, nil
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
