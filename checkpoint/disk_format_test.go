package checkpoint

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"slices"
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
	binary.LittleEndian.PutUint32(table[size+32:], crc32.Checksum(table[:size+32], diskChecksum))
}

// A table reads back whole when it fills its region up to the trailer, and
// when its last entry has no names, and an item as long as an item can be.
// One that says it is longer than its region, counts more entries than it
// holds, names past its end, names an item longer than an item can be, or is
// of another format, under a checksum that holds, is torn, and so is one that
// fails its checksum. A region whose end holds no trailer has no table.
func TestDiskTablesHoldTogetherAtTheirLimits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 24 entries of 2,729 bytes are the 65,496 bytes before the trailer.
		vm := strings.Repeat("v", 2691)
		var full []tableItem
		for page := range uint64(24) {
			full = append(full, tableItem{key: diskKey{cacheKey: cacheKey{Identity: control.Identity{
				Ref: control.Ref{VM: vm, Sequence: 1}, Volume: "ram0", Page: page}}, span: 512},
				code: wholeEnvelope, offset: uint32(page), length: 7})
		}
		bare := []tableItem{{key: keyOf("va", 3), code: wholeEnvelope, offset: 0, length: 9},
			{key: diskKey{}, code: wholeEnvelope, offset: 55, length: 0}}
		longest := []tableItem{{key: keyOf("va", 3), code: wholeEnvelope, offset: 0, length: maximumDiskItem}}
		for _, items := range [][]tableItem{full, bare, longest} {
			table := encodeTable(4, 9, items)
			got, err := readRegionTable(t.Context(), diskTableFile(t, table), 0, testRegionBytes)
			if err != nil || got.sequence != 4 || got.generation != 9 || !slices.Equal(got.items, items) {
				t.Fatalf("a table of %d entries reads back as region %d of generation %d with %d entries, %v",
					len(items), got.sequence, got.generation, len(got.items), err)
			}
		}
		for _, broken := range []struct {
			name   string
			damage func(table []byte)
		}{
			{"longer than its region", func(table []byte) {
				binary.LittleEndian.PutUint32(table[len(table)-diskTrailerSize+28:], testRegionBytes-diskTrailerSize+8)
			}},
			{"more entries than it holds", func(table []byte) {
				binary.LittleEndian.PutUint32(table[len(table)-diskTrailerSize+24:], 3)
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
			{"an item longer than an item can be", func(table []byte) {
				binary.LittleEndian.PutUint32(table[30:], maximumDiskItem+1)
				resum(table)
			}},
			{"another format", func(table []byte) {
				table[len(table)-diskTrailerSize+4] = diskFormatVersion + 1
				resum(table)
			}},
			{"a checksum that fails", func(table []byte) {
				table[0] ^= 1
			}},
		} {
			table := encodeTable(4, 9, bare)
			broken.damage(table)
			if _, err := readRegionTable(t.Context(), diskTableFile(t, table), 0, testRegionBytes); !errors.Is(err, errTornTable) {
				t.Fatalf("a table %s reads back %v, want %v", broken.name, err, errTornTable)
			}
		}
		if _, err := readRegionTable(t.Context(), diskTableFile(t, make([]byte, 100)), 0, testRegionBytes); !errors.Is(err, errNoTable) {
			t.Fatalf("a region that ends in zeros reads back %v, want %v", err, errNoTable)
		}
	})
}

// diskHeaderFile is a simulated file that begins with data.
func diskHeaderFile(t *testing.T, data []byte) platform.File {
	t.Helper()
	file, err := sim.New(sim.Config{}).NewDisk("host", sim.DiskConfig{}).Open(t.Context(), "cache",
		platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt(t.Context(), data, 0); err != nil {
		t.Fatal(err)
	}
	return file
}

// A header reads back as what was written, with names as long as a region
// holds and with none. One whose magic is absent, or whose file is shorter
// than its fixed part, is no header. One of another version is of another
// format. One that fails its checksum, says it is longer than its limit, or
// is a byte short is damaged.
func TestDiskHeadersHoldTogether(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		identity := CacheIdentity{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
		for _, header := range []diskHeader{
			{regionBytes: testRegionBytes, identity: identity, generation: 7, deployment: testDeployment},
			{regionBytes: 1 << 30, generation: 1<<64 - 1},
			{regionBytes: testRegionBytes, identity: identity, deployment: CacheDeployment{
				Store: strings.Repeat("s", 21828), Bucket: strings.Repeat("b", 21828), Prefix: strings.Repeat("p", 21828)}},
		} {
			encoded := encodeDiskHeader(header)
			if int64(len(encoded)) != diskHeaderBytes(header.deployment) {
				t.Fatalf("a header is %d bytes, want %d", len(encoded), diskHeaderBytes(header.deployment))
			}
			got, err := readDiskHeader(t.Context(), diskHeaderFile(t, encoded), testRegionBytes)
			if err != nil || got != header {
				t.Fatalf("a header reads back as %+v, %v, want %+v", got, err, header)
			}
		}
		header := diskHeader{regionBytes: testRegionBytes, identity: identity, generation: 7, deployment: testDeployment}
		for _, broken := range []struct {
			name   string
			damage func(encoded []byte) []byte
			want   error
		}{
			{"with no magic", func(encoded []byte) []byte { encoded[0] = 0; return encoded }, errNoHeader},
			{"shorter than its fixed part", func(encoded []byte) []byte { return encoded[:diskHeaderFixed-1] },
				errNoHeader},
			{"of another version", func(encoded []byte) []byte { encoded[4] = diskFormatVersion + 1; return encoded },
				errHeaderFormat},
			{"with a byte changed", func(encoded []byte) []byte { encoded[len(encoded)-5] ^= 1; return encoded },
				errHeaderDamaged},
			{"a byte short", func(encoded []byte) []byte { return encoded[:len(encoded)-1] }, errHeaderDamaged},
			{"longer than its limit", func(encoded []byte) []byte {
				binary.LittleEndian.PutUint16(encoded[44:], 0xffff)
				return encoded
			}, errHeaderDamaged},
		} {
			_, err := readDiskHeader(t.Context(), diskHeaderFile(t, broken.damage(encodeDiskHeader(header))),
				testRegionBytes)
			if !errors.Is(err, broken.want) {
				t.Fatalf("a header %s reads back %v, want %v", broken.name, err, broken.want)
			}
		}
	})
}

// An identity reads as its sixteen bytes in hexadecimal.
func TestCacheIdentityReadsAsHex(t *testing.T) {
	identity := CacheIdentity{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10}
	if got := identity.String(); got != "0123456789abcdeffedcba9876543210" {
		t.Fatalf("an identity reads as %q", got)
	}
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
