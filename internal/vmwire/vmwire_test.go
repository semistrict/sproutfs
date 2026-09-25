package vmwire_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/internal/vmwire"
)

// The geometry is the session's, so it is on the wire, and both ends check it
// before a byte of guest memory exists. These are the checks themselves; the
// Linux suites are what run them against a real client.

// Version 10 moved the arena off ATTACH and into files. FILE hands the client a
// file and a MAP names one. A version 9 peer would read ATTACH as the arena and a
// MAP's file number as a protection flag, so the two are told apart by the
// version before any guest memory exists.
//
// Version 9 before it added FLUSH, which RESULT answers.
func TestVersionTenMapsFiles(t *testing.T) {
	if vmwire.Version != 10 {
		t.Fatalf("the mapping protocol is at version %d, want 10: a version 9 peer reads"+
			" a MAP's file number as a protection flag", vmwire.Version)
	}
	for _, c := range []struct {
		name        string
		kind, value uint64
	}{
		{"flush", vmwire.Flush, 13},
		{"file", vmwire.File, 14},
		{"drop_file", vmwire.DropFile, 15},
	} {
		if c.kind != c.value {
			t.Fatalf("%s is frame kind %d, want %d", c.name, c.kind, c.value)
		}
		if name := vmwire.KindName(c.kind); name != c.name {
			t.Fatalf("frame kind %d is logged as %q, want %q", c.kind, name, c.name)
		}
	}
}

// A MAP's flags carry the file it maps from above the immutable bit. The
// private file is file 0, so a MAP of it encodes as it did before files.
func TestAMapNamesItsFileAboveTheImmutableBit(t *testing.T) {
	for _, c := range []struct {
		file      uint64
		immutable bool
		flags     uint64
	}{
		{vmwire.PrivateFile, false, 0},
		{vmwire.PrivateFile, true, 1},
		{vmwire.SharedFile, true, 3},
		{2, true, 5},
		{1 << 40, true, 1<<41 | 1},
	} {
		if got := vmwire.MapFlags(c.file, c.immutable); got != c.flags {
			t.Fatalf("a MAP of file %d immutable=%t has flags %d, want %d", c.file, c.immutable, got, c.flags)
		}
		if got := vmwire.MapFile(c.flags); got != c.file {
			t.Fatalf("flags %d name file %d, want %d", c.flags, got, c.file)
		}
	}
}

// A FILE states its number, its size, what it is made of and whether its
// descriptor is read-write, and only the private file is. A DROP_FILE names a
// file and nothing else.
func TestAFileStatesWhatTheClientChecks(t *testing.T) {
	const page = checkpoint.PageSize4KiB
	private := vmwire.FileFrame(vmwire.PrivateFile, 64<<20, vmwire.BackingMemfd, true)
	if want := (vmwire.Frame{Kind: vmwire.File, ID: 0, Length: 64 << 20, Backing: vmwire.BackingMemfd,
		Flags: vmwire.FileWritable}); private != want {
		t.Fatalf("the private file encodes as %+v, want %+v", private, want)
	}
	shared := vmwire.FileFrame(vmwire.SharedFile, 8<<20, vmwire.BackingMemfd, false)
	if want := (vmwire.Frame{Kind: vmwire.File, ID: 1, Length: 8 << 20, Backing: vmwire.BackingMemfd}); shared != want {
		t.Fatalf("the shared file encodes as %+v, want %+v", shared, want)
	}
	if drop := vmwire.DropFileFrame(3); drop != (vmwire.Frame{Kind: vmwire.DropFile, ID: 3}) {
		t.Fatalf("a drop of file 3 encodes as %+v", drop)
	}
	for _, f := range []vmwire.Frame{private, shared, vmwire.FileFrame(7, page, vmwire.BackingMemfd, false)} {
		if err := vmwire.CheckFile(f, page, vmwire.BackingMemfd); err != nil {
			t.Fatalf("%+v was refused: %v", f, err)
		}
	}
	for _, c := range []struct {
		name  string
		frame vmwire.Frame
		want  string
	}{
		{"a read-only private file", vmwire.FileFrame(vmwire.PrivateFile, page, vmwire.BackingMemfd, false),
			"invalid managed-memory file 0: writable is false, and only file 0 is writable"},
		{"a writable shared file", vmwire.FileFrame(vmwire.SharedFile, page, vmwire.BackingMemfd, true),
			"invalid managed-memory file 1: writable is true, and only file 0 is writable"},
		{"a file of the other arena kind", vmwire.FileFrame(vmwire.SharedFile, page, vmwire.BackingHugeTLB, false),
			"invalid managed-memory file 1: arena kind 1, the attachment said 2"},
		{"an empty file", vmwire.FileFrame(vmwire.SharedFile, 0, vmwire.BackingMemfd, false),
			"invalid managed-memory file 1: 0 bytes is not whole 4096-byte pages"},
		{"a file that is not whole pages", vmwire.FileFrame(vmwire.SharedFile, page+1, vmwire.BackingMemfd, false),
			"invalid managed-memory file 1: 4097 bytes is not whole 4096-byte pages"},
		{"a file with an unknown flag", func() vmwire.Frame { f := shared; f.Flags = 2; return f }(),
			"invalid managed-memory file: kind 14 offset 0 generation 0 flags 2"},
		{"a file with an offset", func() vmwire.Frame { f := shared; f.Offset = page; return f }(),
			"invalid managed-memory file: kind 14 offset 4096 generation 0 flags 0"},
		{"a frame that is not a file", func() vmwire.Frame { f := shared; f.Kind = vmwire.Attach; return f }(),
			"invalid managed-memory file: kind 3 offset 0 generation 0 flags 0"},
	} {
		err := vmwire.CheckFile(c.frame, page, vmwire.BackingMemfd)
		if err == nil {
			t.Fatalf("%s was accepted", c.name)
		}
		if err.Error() != c.want {
			t.Fatalf("%s was refused with %q, want %q", c.name, err, c.want)
		}
	}
}

func TestAFrameSurvivesEncoding(t *testing.T) {
	f := vmwire.Frame{Kind: vmwire.MapRange, ID: 9, Offset: 1 << 30, Length: 8 << 20,
		Backing: 3 << 20, Generation: 4, Flags: 1}
	if got := vmwire.Decode(f.Bytes()); got != f {
		t.Fatalf("a frame decoded as %+v, want %+v", got, f)
	}
	if n := len(f.Bytes()); n != vmwire.FrameBytes {
		t.Fatalf("a frame encodes to %d bytes, want %d", n, vmwire.FrameBytes)
	}
	var buffer bytes.Buffer
	if err := vmwire.Write(&buffer, f); err != nil {
		t.Fatal(err)
	}
	got, err := vmwire.Read(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	if got != f {
		t.Fatalf("a frame read back as %+v, want %+v", got, f)
	}
}

// A page and the memory behind it are one statement: 2 MiB is the HugeTLB
// pool's page and 4 KiB is ordinary memory, and nothing else is mapped at all.
func TestTheArenaKindFollowsThePage(t *testing.T) {
	for _, c := range []struct {
		pageSize uint64
		backing  uint64
	}{
		{checkpoint.PageSize4KiB, vmwire.BackingMemfd},
		{checkpoint.PageSize2MiB, vmwire.BackingHugeTLB},
	} {
		got, err := vmwire.BackingFor(c.pageSize)
		if err != nil {
			t.Fatalf("a %d-byte page: %v", c.pageSize, err)
		}
		if got != c.backing {
			t.Fatalf("a %d-byte page is arena kind %d, want %d", c.pageSize, got, c.backing)
		}
	}
	for _, pageSize := range []uint64{0, 1, 4095, 64 << 10, 1 << 30} {
		if _, err := vmwire.BackingFor(pageSize); err == nil {
			t.Fatalf("a %d-byte page was accepted by this transport", pageSize)
		}
	}
}

func TestAnAttachmentStatesAGeometryTheClientChecks(t *testing.T) {
	ram := vmwire.AttachFrame(checkpoint.PageSize4KiB, vmwire.BackingMemfd, 1024)
	if err := vmwire.CheckAttach(ram); err != nil {
		t.Fatalf("a 4 KiB RAM attachment was refused: %v", err)
	}
	pmem := vmwire.AttachFrame(checkpoint.PageSize2MiB, vmwire.BackingHugeTLB, 0)
	if err := vmwire.CheckAttach(pmem); err != nil {
		t.Fatalf("a 2 MiB PMEM attachment was refused: %v", err)
	}
	if want := (vmwire.Frame{Kind: vmwire.Attach, ID: vmwire.Version, Offset: checkpoint.PageSize4KiB,
		Backing: vmwire.BackingMemfd, Flags: 1024}); ram != want {
		t.Fatalf("an attachment encodes its geometry as %+v, want %+v", ram, want)
	}

	for _, c := range []struct {
		name  string
		frame vmwire.Frame
		want  string
	}{
		{"a version 9 peer", func() vmwire.Frame { f := ram; f.ID = 9; return f }(), "version 9"},
		{"a page this transport does not map",
			func() vmwire.Frame { f := ram; f.Offset = 64 << 10; return f }(), "not 65536"},
		{"a 4 KiB page claiming the HugeTLB pool",
			func() vmwire.Frame { f := ram; f.Backing = vmwire.BackingHugeTLB; return f }(),
			"arena kind 2, not 1"},
		{"a 2 MiB page claiming ordinary memory",
			func() vmwire.Frame { f := pmem; f.Backing = vmwire.BackingMemfd; return f }(),
			"arena kind 1, not 2"},
		{"an attachment that states an arena's length, as version 9 did",
			func() vmwire.Frame { f := pmem; f.Length = 64 << 20; return f }(),
			"a length of 67108864, where each file states its own"},
		{"a frame that is not an attachment",
			func() vmwire.Frame { f := ram; f.Kind = vmwire.MemoryRegion; return f }(), "kind 2"},
	} {
		err := vmwire.CheckAttach(c.frame)
		if err == nil {
			t.Fatalf("%s was accepted", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s was refused with %q, want it to name %q", c.name, err, c.want)
		}
	}
}
