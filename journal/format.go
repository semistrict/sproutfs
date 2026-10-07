package journal

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/semistrict/sproutfs/rank"
	"github.com/zeebo/xxh3"
)

// The journal disk begins with two header slots of 4 KiB. A header is
// rewritten into the slot that does not hold the current one, so a torn header
// write leaves the older header in place. The rest of the disk is the ring.
//
// A slot:
//
//	0   4  magic "SFJH"
//	4   1  format version, 1
//	5   1  empty: 1 when its last holder closed it with no live entry
//	6   2  zero
//	8   8  the slot's counter; the valid slot with the higher counter wins
//	16  16 the disk's identity
//	32  8  the generation, drawn when the journal is formatted
//	40  8  where the ring starts
//	48  8  the ring's length
//	56  8  the tail hint: a position at or before the oldest live entry
//	64  8  the lease: the membership generation that assigned the disk
//	72  16 the lease: the member's identity
//	88  16 XXH3-128 of the slot's first 88 bytes
//
// An entry:
//
//	0   4  magic "SFJE"
//	4   1  format version, 1
//	5   1  kind: 0 pad, 1 blocks
//	6   2  zero
//	8   8  position
//	16  8  the journal's generation
//	24  8  the VM's epoch
//	32  4  the entry's length, from its first byte to its checksum's last
//	36  4  the number of blocks
//	40  2  length of the VM identity
//	42  2  length of the volume's name
//	44  4  zero
//	48     the VM identity and the volume's name, padded to 8
//	       the block numbers, 8 bytes each
//	       the blocks, 4 KiB each
//	end-16 16 XXH3-128 of everything before it
//
// Every integer is little-endian.

const (
	// BlockBytes is the size of a block: 4 KiB of a page.
	BlockBytes = 4096
	// MaxEntryBytes bounds an entry of blocks. A pad before it at the ring's
	// end is shorter than it, so nothing on the ring is longer than
	// maxStoredBytes.
	MaxEntryBytes = maxStoredBytes - BlockBytes
	// MaxBatchBytes is the room a batch takes commits into. A batch holds at
	// least one commit, however large.
	MaxBatchBytes = 8 << 20

	formatVersion = 1

	slotBytes   = 4096
	slotLength  = 104
	ringOffset  = 2 * slotBytes
	minRingSize = 16 << 10

	entryHeadBytes = 48
	checksumBytes  = 16
	// minEntryBytes is the smallest entry: a pad with nothing in it.
	minEntryBytes = entryHeadBytes + checksumBytes
	// maxStoredBytes bounds every entry on the ring, pads included.
	maxStoredBytes = 16 << 20
	maxNameBytes   = 1<<16 - 1

	kindPad    = 0
	kindBlocks = 1
)

var (
	slotMagic  = [4]byte{'S', 'F', 'J', 'H'}
	entryMagic = [4]byte{'S', 'F', 'J', 'E'}
)

// ErrVersion reports a journal disk written under another format version. The
// error names the version.
var ErrVersion = errors.New("journal: the disk is of another format version")

// errNoSlot reports a slot that holds no header: blank, torn or damaged.
var errNoSlot = errors.New("journal: no header in the slot")

// slot is one header slot.
type slot struct {
	empty      bool
	counter    uint64
	identity   rank.Identity
	generation uint64
	ringStart  int64
	ringLength int64
	tail       uint64
	lease      Lease
}

func checksum(data []byte) [16]byte { return xxh3.Hash128(data).Bytes() }

// encodeSlot is a whole slot, padded with zeros to 4 KiB.
func encodeSlot(s slot) []byte {
	b := make([]byte, slotBytes)
	copy(b[0:4], slotMagic[:])
	b[4] = formatVersion
	if s.empty {
		b[5] = 1
	}
	binary.LittleEndian.PutUint64(b[8:], s.counter)
	copy(b[16:32], s.identity[:])
	binary.LittleEndian.PutUint64(b[32:], s.generation)
	binary.LittleEndian.PutUint64(b[40:], uint64(s.ringStart))
	binary.LittleEndian.PutUint64(b[48:], uint64(s.ringLength))
	binary.LittleEndian.PutUint64(b[56:], s.tail)
	binary.LittleEndian.PutUint64(b[64:], s.lease.Assigned)
	copy(b[72:88], s.lease.Member[:])
	sum := checksum(b[:88])
	copy(b[88:slotLength], sum[:])
	return b
}

// decodeSlot reads one slot back. A slot whose magic or checksum is wrong
// holds no header, errNoSlot. One that holds together under another version
// is ErrVersion, with the version named.
func decodeSlot(b []byte) (slot, error) {
	if len(b) < slotLength || [4]byte(b[0:4]) != slotMagic || checksum(b[:88]) != [16]byte(b[88:slotLength]) {
		return slot{}, errNoSlot
	}
	if b[4] != formatVersion {
		return slot{}, fmt.Errorf("%w: version %d, not %d", ErrVersion, b[4], formatVersion)
	}
	if b[5] > 1 || binary.LittleEndian.Uint16(b[6:]) != 0 {
		return slot{}, errNoSlot
	}
	s := slot{
		empty:      b[5] == 1,
		counter:    binary.LittleEndian.Uint64(b[8:]),
		identity:   rank.Identity(b[16:32]),
		generation: binary.LittleEndian.Uint64(b[32:]),
		ringStart:  int64(binary.LittleEndian.Uint64(b[40:])),
		ringLength: int64(binary.LittleEndian.Uint64(b[48:])),
		tail:       binary.LittleEndian.Uint64(b[56:]),
		lease:      Lease{Assigned: binary.LittleEndian.Uint64(b[64:]), Member: rank.Identity(b[72:88])},
	}
	return s, nil
}

// Entry is the changed blocks of one memory region, taken by one capture.
type Entry struct {
	// VM is the identity of the VM whose disk the blocks are of.
	VM string
	// Volume is the name of the volume the blocks are of.
	Volume string
	// Epoch is the VM's epoch when its blocks were taken.
	Epoch uint64
	// Blocks are the block numbers: each block's byte offset in the volume
	// divided by BlockBytes.
	Blocks []uint64
	// Data holds the blocks, BlockBytes each, in the order Blocks names them.
	Data []byte
	// Position is where the entry is in its journal. Read sets it; a commit
	// ignores it.
	Position uint64
}

// EntryBytes is how many bytes an entry of blocks blocks takes in the ring.
func EntryBytes(vm, volume string, blocks int) int64 {
	return entryHeadBytes + align8(int64(len(vm)+len(volume))) + int64(blocks)*(8+BlockBytes) + checksumBytes
}

func align8(n int64) int64 { return (n + 7) &^ 7 }

// size is the bytes e takes in the ring.
func (e Entry) size() int64 { return EntryBytes(e.VM, e.Volume, len(e.Blocks)) }

// check reports an entry this format cannot hold.
func (e Entry) check() error {
	switch {
	case len(e.VM) > maxNameBytes || len(e.Volume) > maxNameBytes:
		return fmt.Errorf("%w: a VM identity of %d bytes or a volume name of %d", ErrTooLarge, len(e.VM),
			len(e.Volume))
	case len(e.Data) != len(e.Blocks)*BlockBytes:
		return fmt.Errorf("journal: an entry of %d blocks holds %d bytes", len(e.Blocks), len(e.Data))
	case e.size() > MaxEntryBytes:
		return fmt.Errorf("%w: an entry of %d bytes", ErrTooLarge, e.size())
	}
	return nil
}

// appendEntry appends e, encoded at position under generation, to dst.
func appendEntry(dst []byte, e Entry, position, generation uint64) []byte {
	start := len(dst)
	dst = appendHead(dst, kindBlocks, position, generation, e.Epoch, e.size(), len(e.Blocks), len(e.VM),
		len(e.Volume))
	dst = append(dst, e.VM...)
	dst = append(dst, e.Volume...)
	dst = append(dst, make([]byte, align8(int64(len(e.VM)+len(e.Volume)))-int64(len(e.VM)+len(e.Volume)))...)
	for _, block := range e.Blocks {
		dst = binary.LittleEndian.AppendUint64(dst, block)
	}
	dst = append(dst, e.Data...)
	sum := checksum(dst[start:])
	return append(dst, sum[:]...)
}

// appendPad appends a pad of length bytes at position under generation.
func appendPad(dst []byte, length int64, position, generation uint64) []byte {
	start := len(dst)
	dst = appendHead(dst, kindPad, position, generation, 0, length, 0, 0, 0)
	dst = append(dst, make([]byte, length-minEntryBytes)...)
	sum := checksum(dst[start:])
	return append(dst, sum[:]...)
}

func appendHead(dst []byte, kind byte, position, generation, epoch uint64, length int64, blocks, vm,
	volume int) []byte {
	dst = append(dst, entryMagic[:]...)
	dst = append(dst, formatVersion, kind, 0, 0)
	dst = binary.LittleEndian.AppendUint64(dst, position)
	dst = binary.LittleEndian.AppendUint64(dst, generation)
	dst = binary.LittleEndian.AppendUint64(dst, epoch)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(length))
	dst = binary.LittleEndian.AppendUint32(dst, uint32(blocks))
	dst = binary.LittleEndian.AppendUint16(dst, uint16(vm))
	dst = binary.LittleEndian.AppendUint16(dst, uint16(volume))
	return append(dst, 0, 0, 0, 0)
}

// head is an entry's first 48 bytes, read back.
type head struct {
	kind       byte
	position   uint64
	generation uint64
	epoch      uint64
	length     int64
	blocks     int
	vm, volume int
}

// errNoEntry reports bytes that are not an entry: where reading back stops.
var errNoEntry = errors.New("journal: no entry")

// decodeHead reads an entry's head. It holds together on its own, but says
// nothing of the bytes after it until the checksum is checked.
func decodeHead(b []byte) (head, error) {
	if len(b) < entryHeadBytes || [4]byte(b[0:4]) != entryMagic || b[4] != formatVersion ||
		binary.LittleEndian.Uint16(b[6:]) != 0 || binary.LittleEndian.Uint32(b[44:]) != 0 {
		return head{}, errNoEntry
	}
	h := head{
		kind:       b[5],
		position:   binary.LittleEndian.Uint64(b[8:]),
		generation: binary.LittleEndian.Uint64(b[16:]),
		epoch:      binary.LittleEndian.Uint64(b[24:]),
		length:     int64(binary.LittleEndian.Uint32(b[32:])),
		blocks:     int(binary.LittleEndian.Uint32(b[36:])),
		vm:         int(binary.LittleEndian.Uint16(b[40:])),
		volume:     int(binary.LittleEndian.Uint16(b[42:])),
	}
	switch h.kind {
	case kindPad:
		if h.epoch != 0 || h.blocks != 0 || h.vm != 0 || h.volume != 0 || h.length < minEntryBytes ||
			h.length%8 != 0 || h.length > maxStoredBytes {
			return head{}, errNoEntry
		}
	case kindBlocks:
		if h.length != entryHeadBytes+align8(int64(h.vm+h.volume))+int64(h.blocks)*(8+BlockBytes)+checksumBytes ||
			h.length > MaxEntryBytes {
			return head{}, errNoEntry
		}
	default:
		return head{}, errNoEntry
	}
	return h, nil
}

// checkEntry reads back a whole entry, its checksum included, and returns
// its head. An entry whose head holds together and whose checksum does not
// was torn or damaged.
func checkEntry(b []byte) (head, error) {
	h, err := decodeHead(b)
	if err != nil {
		return head{}, err
	}
	if int64(len(b)) != h.length || checksum(b[:h.length-checksumBytes]) != [16]byte(b[h.length-checksumBytes:]) {
		return h, errTorn
	}
	for at := int64(entryHeadBytes + h.vm + h.volume); at < entryHeadBytes+align8(int64(h.vm+h.volume)); at++ {
		if b[at] != 0 {
			return h, errNoEntry
		}
	}
	return h, nil
}

// errTorn reports an entry whose head holds together and whose bytes do not
// match its checksum.
var errTorn = fmt.Errorf("%w: its checksum does not match", errNoEntry)

// entryVM is the VM identity in an entry checkEntry passed.
func entryVM(b []byte, h head) string { return string(b[entryHeadBytes : entryHeadBytes+h.vm]) }

// decodeEntry reads back a whole entry, its checksum included. The entry's
// names, block numbers and data are copied out of b.
func decodeEntry(b []byte) (head, Entry, error) {
	h, err := checkEntry(b)
	if err != nil || h.kind == kindPad {
		return h, Entry{}, err
	}
	at := int64(entryHeadBytes + h.vm)
	e := Entry{VM: entryVM(b, h), Volume: string(b[at : at+int64(h.volume)]), Epoch: h.epoch, Position: h.position}
	at = entryHeadBytes + align8(int64(h.vm+h.volume))
	e.Blocks = make([]uint64, h.blocks)
	for i := range e.Blocks {
		e.Blocks[i] = binary.LittleEndian.Uint64(b[at:])
		at += 8
	}
	e.Data = append([]byte(nil), b[at:at+int64(h.blocks)*BlockBytes]...)
	return h, e, nil
}
