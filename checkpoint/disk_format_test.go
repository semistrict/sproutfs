package checkpoint

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// An item reads back as what was written at the edges of its format: no
// names and no bytes, and names as long as their length fields carry. Names a
// byte longer are not storable. A header whose names run past the item, or an
// item a byte short, is damage.
func TestDiskItemsHoldTogetherAtTheirLimits(t *testing.T) {
	for _, key := range []diskKey{
		{},
		{cacheKey: cacheKey{Identity: control.Identity{Ref: control.Ref{VM: strings.Repeat("v", 0xffff), Sequence: 9},
			Volume: strings.Repeat("r", 0xffff), Page: 1 << 40}, segment: true}, span: 1},
	} {
		if !storable(key) {
			t.Fatalf("names of %d and %d bytes are not storable", len(key.Ref.VM), len(key.Volume))
		}
		for _, data := range [][]byte{nil, []byte("envelope")} {
			item := encodeItem(key, wholeEnvelope, data)
			if int64(len(item)) != itemHeaderBytes(key)+int64(len(data)) {
				t.Fatalf("an item is %d bytes, want its header and %d", len(item), len(data))
			}
			parsed, err := parseItem(item, false)
			if err != nil || parsed.key != key || parsed.code != wholeEnvelope || !bytes.Equal(parsed.data, data) {
				t.Fatalf("an item of %d bytes reads back as %d bytes, %v", len(data), len(parsed.data), err)
			}
			if _, err := parseItem(item[:len(item)-1], false); !errors.Is(err, errItemDamaged) {
				t.Fatalf("an item a byte short reads back %v, want %v", err, errItemDamaged)
			}
		}
	}
	longer := diskKey{cacheKey: cacheKey{Identity: control.Identity{Ref: control.Ref{VM: strings.Repeat("v", 0x10000)}}}}
	if storable(longer) {
		t.Fatal("a VM identity of 65,536 bytes is storable")
	}
	longer = diskKey{cacheKey: cacheKey{Identity: control.Identity{Volume: strings.Repeat("r", 0x10000)}}}
	if storable(longer) {
		t.Fatal("a volume name of 65,536 bytes is storable")
	}
	item := encodeItem(keyOf("va", 1), wholeEnvelope, []byte("abc"))
	if _, ok := itemKey(item[:itemHeaderBytes(keyOf("va", 1))-1]); ok {
		t.Fatal("a header whose names run past the item names a key")
	}
}

// diskTableFile is a simulated file holding one region of testRegionBytes,
// with table written at its end.
func diskTableFile(t *testing.T, table []byte) platform.File {
	t.Helper()
	runtime := sim.New(sim.Config{})
	file, err := runtime.NewDisk("host", sim.DiskConfig{}).Open(t.Context(), "cache",
		platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt(t.Context(), table, testRegionBytes-int64(len(table))); err != nil {
		t.Fatal(err)
	}
	return file
}

// resum puts back a table's checksum after a test changed it.
func resum(table []byte) {
	size := len(table) - diskTrailerSize
	binary.LittleEndian.PutUint32(table[size+24:], crc32.Checksum(table[:size+24], diskChecksum))
}

// A table reads back whole when it fills its region up to the trailer, and
// when its last entry has no names. One that says it is longer than its
// region, counts more entries than it holds, or names past its end, under a
// checksum that holds, has no table.
func TestDiskTablesHoldTogetherAtTheirLimits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 736 entries of 89 bytes are the 65,504 bytes before the trailer.
		vm := strings.Repeat("v", 51)
		var full []tableItem
		for page := range uint64(736) {
			full = append(full, tableItem{key: diskKey{cacheKey: cacheKey{Identity: control.Identity{
				Ref: control.Ref{VM: vm, Sequence: 1}, Volume: "ram0", Page: page}}, span: 512},
				code: wholeEnvelope, offset: uint32(page), length: 7})
		}
		bare := []tableItem{{key: keyOf("va", 3), code: wholeEnvelope, offset: 0, length: 9},
			{key: diskKey{}, code: wholeEnvelope, offset: 55, length: 0}}
		for _, items := range [][]tableItem{full, bare} {
			table := encodeTable(4, items)
			sequence, got, err := readRegionTable(t.Context(), diskTableFile(t, table), 0, testRegionBytes)
			if err != nil || sequence != 4 || len(got) != len(items) || got[len(got)-1] != items[len(items)-1] {
				t.Fatalf("a table of %d entries reads back as region %d with %d entries, %v", len(items), sequence,
					len(got), err)
			}
		}
		for _, broken := range []struct {
			name   string
			damage func(table []byte)
		}{
			{"longer than its region", func(table []byte) {
				binary.LittleEndian.PutUint32(table[len(table)-diskTrailerSize+20:], testRegionBytes-diskTrailerSize+8)
			}},
			{"more entries than it holds", func(table []byte) {
				binary.LittleEndian.PutUint32(table[len(table)-diskTrailerSize+16:], 3)
				resum(table)
			}},
			{"a VM identity past its end", func(table []byte) {
				binary.LittleEndian.PutUint16(table[40+6:], 200)
				resum(table)
			}},
			{"a volume name past its end", func(table []byte) {
				binary.LittleEndian.PutUint16(table[40+8:], 200)
				resum(table)
			}},
		} {
			table := encodeTable(4, bare)
			broken.damage(table)
			if _, _, err := readRegionTable(t.Context(), diskTableFile(t, table), 0, testRegionBytes); !errors.Is(err, errNoTable) {
				t.Fatalf("a table %s reads back %v, want %v", broken.name, err, errNoTable)
			}
		}
	})
}

// A read past the end of the file is the file's end, not bytes.
func TestDiskReadFullStopsAtTheEnd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		file := diskTableFile(t, []byte("ten bytes!"))
		if err := readFull(t.Context(), file, make([]byte, 20), testRegionBytes-10); !errors.Is(err, io.EOF) {
			t.Fatalf("reading past the end returned %v, want %v", err, io.EOF)
		}
		if err := readFull(t.Context(), file, make([]byte, 10), testRegionBytes-10); err != nil {
			t.Fatalf("reading to the end returned %v", err)
		}
	})
}
