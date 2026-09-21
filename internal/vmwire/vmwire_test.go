package vmwire_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/vmwire"
)

// The geometry is the session's, so it is on the wire, and both ends check it
// before a byte of guest memory exists. These are the checks themselves; the
// Linux suites are what run them against a real client.

func TestVersionSevenCarriesTheGeometry(t *testing.T) {
	if vmwire.Version != 7 {
		t.Fatalf("the mapping protocol is at version %d, want 7: a version 6 peer reads a"+
			" 2 MiB page number as a 4 KiB one", vmwire.Version)
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
	ram := vmwire.AttachFrame(checkpoint.PageSize4KiB, 64<<20, vmwire.BackingMemfd, 1024)
	if err := vmwire.CheckAttach(ram); err != nil {
		t.Fatalf("a 4 KiB RAM attachment was refused: %v", err)
	}
	pmem := vmwire.AttachFrame(checkpoint.PageSize2MiB, 64<<20, vmwire.BackingHugeTLB, 0)
	if err := vmwire.CheckAttach(pmem); err != nil {
		t.Fatalf("a 2 MiB PMEM attachment was refused: %v", err)
	}
	if ram.Offset != checkpoint.PageSize4KiB || ram.Length != 64<<20 ||
		ram.Backing != vmwire.BackingMemfd || ram.Flags != 1024 || ram.ID != vmwire.Version {
		t.Fatalf("an attachment encodes its geometry as %+v", ram)
	}

	for _, c := range []struct {
		name  string
		frame vmwire.Frame
		want  string
	}{
		{"a version 6 peer", func() vmwire.Frame { f := ram; f.ID = 6; return f }(), "version 6"},
		{"a page this transport does not map",
			func() vmwire.Frame { f := ram; f.Offset = 64 << 10; return f }(), "not 65536"},
		{"a 4 KiB page claiming the HugeTLB pool",
			func() vmwire.Frame { f := ram; f.Backing = vmwire.BackingHugeTLB; return f }(),
			"arena kind 2, not 1"},
		{"a 2 MiB page claiming ordinary memory",
			func() vmwire.Frame { f := pmem; f.Backing = vmwire.BackingMemfd; return f }(),
			"arena kind 1, not 2"},
		{"an arena that is not whole pages",
			func() vmwire.Frame { f := pmem; f.Length = 3 << 20; return f }(), "not whole 2097152-byte pages"},
		{"an empty arena", func() vmwire.Frame { f := ram; f.Length = 0; return f }(), "not whole"},
		{"a frame that is not an attachment",
			func() vmwire.Frame { f := ram; f.Kind = vmwire.Region; return f }(), "kind 2"},
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
