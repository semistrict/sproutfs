package main

import (
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestTheL2ProgramsAreWhatAnAssemblerMakes: every program is the bytes an
// assembler made of the source in its comment, x86 with `.code16` under
// `clang -target x86_64-linux-gnu` and arm64 under `clang -target
// aarch64-linux-gnu`, as objdump printed them.
func TestTheL2ProgramsAreWhatAnAssemblerMakes(t *testing.T) {
	words := func(words ...uint32) string {
		raw := make([]byte, 0, 4*len(words))
		for _, word := range words {
			raw = binary.LittleEndian.AppendUint32(raw, word)
		}
		return hex.EncodeToString(raw)
	}
	for _, c := range []struct {
		name    string
		program []byte
		want    string
	}{
		{"x86 store", x86StoreProgram(), "66c70600204c324f4bf4"},
		{"x86 count", x86CountProgram(), "f46640" + "66a30020" + "b90000" + "e2fe" + "ebf3"},
		{"arm64 store", arm64StoreProgram(),
			words(0x52864981, 0x72a969e1, 0xd2840002, 0xb9000041, 0xd2a00023, 0xb900007f, 0x14000000)},
		{"arm64 count", arm64CountProgram(),
			words(0xd2840002, 0xd2a00023, 0xb900007f, 0x52800000, 0x11000400, 0xb9000040, 0xd2a00024,
				0xf1000484, 0x54ffffe1, 0x17fffffb)},
	} {
		if got := hex.EncodeToString(c.program); got != c.want {
			t.Errorf("the %s program is %s, want %s", c.name, got, c.want)
		}
		// A program is copied to l2Code and stores to l2Data, so it must end
		// before the word it stores into.
		if end := l2Code + len(c.program); end > l2Data {
			t.Errorf("the %s program ends at %#x, past the word it stores into at %#x", c.name, end, l2Data)
		}
	}
	// "L2OK", as the store leaves it in memory.
	if got := string(binary.LittleEndian.AppendUint32(nil, l2Stored)); got != "L2OK" {
		t.Errorf("l2Stored reads %q in memory", got)
	}
}

// TestAControlIsOfferedByTheHighHalfOfItsMSR: a VMX capability MSR's low half
// is the controls that must be 1 and its high half the ones that may be, and
// only the second is an offer.
func TestAControlIsOfferedByTheHighHalfOfItsMSR(t *testing.T) {
	for _, c := range []struct {
		name   string
		values map[uint32]uint64
		want   string
	}{
		{"none", map[uint32]uint64{0x48e: 0, 0x48b: 0, 0x48d: 0},
			"L1 is offered TPR shadow: no, virtualize APIC accesses: no, posted interrupts: no"},
		{"all", map[uint32]uint64{0x48e: 1 << (32 + 21), 0x48b: 1 << 32, 0x48d: 1 << (32 + 7)},
			"L1 is offered TPR shadow: yes, virtualize APIC accesses: yes, posted interrupts: yes"},
		{"only the low halves", map[uint32]uint64{0x48e: 1 << 21, 0x48b: 1, 0x48d: 1 << 7},
			"L1 is offered TPR shadow: no, virtualize APIC accesses: no, posted interrupts: no"},
		// Full true procbased controls with TPR shadow taken out of the offer,
		// beside pinbased controls that offer everything.
		{"only posted interrupts", map[uint32]uint64{0x48e: 0xfff9fffe_0401e172 &^ (1 << (32 + 21)), 0x48b: 0, 0x48d: 0xff_00000016},
			"L1 is offered TPR shadow: no, virtualize APIC accesses: no, posted interrupts: yes"},
	} {
		got, err := describeControls(c.values)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: said %q, want %q", c.name, got, c.want)
		}
	}
	_, err := describeControls(map[uint32]uint64{0x48e: 0, 0x48d: 0})
	if err == nil || err.Error() != "no value for MSR 0x48b, which says whether virtualize APIC accesses is offered" {
		t.Fatalf("describing controls without MSR 0x48b = %v", err)
	}
}

// TestTheKVMCommandLine: what follows `witness kvm`.
func TestTheKVMCommandLine(t *testing.T) {
	for _, c := range []struct {
		args []string
		want kvmOptions
	}{
		{nil, kvmOptions{file: defaultL2File, log: defaultL2Log}},
		{[]string{"run"}, kvmOptions{verb: "run", file: defaultL2File, log: defaultL2Log}},
		{[]string{"controls"}, kvmOptions{verb: "controls", file: defaultL2File, log: defaultL2Log}},
		{[]string{"loop"}, kvmOptions{verb: "loop", file: defaultL2File, log: defaultL2Log}},
		{[]string{"loop", "--file", "/run/l2", "--log=/run/l2.log"}, kvmOptions{verb: "loop", file: "/run/l2", log: "/run/l2.log"}},
		{[]string{"count", "--file=/run/l2"}, kvmOptions{verb: "count", file: "/run/l2", log: defaultL2Log}},
		{[]string{serveLoopVerb, "--file", "/run/l2", "--log", "/run/l2.log"},
			kvmOptions{verb: serveLoopVerb, file: "/run/l2", log: "/run/l2.log"}},
	} {
		got, err := parseKVM(c.args)
		if err != nil {
			t.Fatalf("kvm %v: %v", c.args, err)
		}
		if got != c.want {
			t.Errorf("kvm %v parsed to %+v, want %+v", c.args, got, c.want)
		}
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"run", "--file", "/run/l2"}, "kvm run takes no arguments"},
		{[]string{"controls", "now"}, "kvm controls takes no arguments"},
		{[]string{"halt"}, `no kvm command named "halt"`},
		{[]string{"count", "--log", "/run/l2.log"}, "kvm count has no flag --log"},
		{[]string{"loop", "--seed", "7"}, "kvm loop has no flag --seed"},
		{[]string{"loop", "--file"}, "--file needs a value"},
		{[]string{"loop", "--file="}, "--file needs a value"},
		{[]string{"loop", "/run/l2"}, `unexpected argument "/run/l2"`},
	} {
		_, err := parseKVM(c.args)
		if err == nil || err.Error() != c.want {
			t.Errorf("kvm %v = %v, want the refusal %q", c.args, err, c.want)
		}
	}
}

// TestACountIsReadOutOfTheL2sMemory: `kvm count` reads the word the counting
// program stores into, out of the file that is the L2's memory.
func TestACountIsReadOutOfTheL2sMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l2")
	memory := make([]byte, l2MemoryBytes)
	binary.LittleEndian.PutUint32(memory[l2Data:], 4_000_000_007)
	if err := os.WriteFile(path, memory, 0o600); err != nil {
		t.Fatal(err)
	}
	count, err := readCount(path)
	if err != nil {
		t.Fatal(err)
	}
	if count != 4_000_000_007 {
		t.Fatalf("read a count of %d, want 4000000007", count)
	}
	if err := os.WriteFile(path, memory[:l2Data], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCount(path); err == nil || err.Error() != path+" is too short to be the memory of a counting L2" {
		t.Fatalf("reading a count out of a file that ends before it = %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := readCount(missing); err == nil ||
		err.Error() != "the counting L2's memory: open "+missing+": no such file or directory" {
		t.Fatalf("reading a count out of no file = %v", err)
	}
}
