package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// A nested VM's guest is L1, and a VM that guest runs is L2. `witness kvm`
// says only that L1 can create a VM; these say what a nested VM is for: that
// L2 runs, that L1 is not offered the VMX controls that make the host's KVM
// write L1's memory behind the host page tables (TASK-57), and that an L2 left
// running goes on running across whatever the host does to L1.
//
// L2 is a few instructions in a few pages of L1's memory, at guest-physical 0,
// with one vCPU. On x86 it runs in 16-bit real mode, the state a vCPU is
// created in, so nothing needs building first: no descriptor tables, no page
// tables. On arm64, which only Lima runs (a nested VM is x86's alone), it runs
// at EL1 with its MMU off. Both are the smallest program that says it ran.

const (
	// l2MemoryBytes is L2's whole memory: one memory slot at guest-physical 0.
	// It is 64 KiB so that it is whole pages whatever L1's page size is.
	l2MemoryBytes = 64 << 10
	// l2Code is where L2's program starts, and l2Data the 32-bit word it stores
	// into. Both are guest-physical addresses and offsets into the slot.
	l2Code = 0x1000
	l2Data = 0x2000
	// l2Stored is what the program that stops stores: "L2OK" in memory.
	l2Stored uint32 = 0x4b4f324c
	// l2Doorbell is where an arm64 L2 stores to stop. It is past the slot, so
	// the store is MMIO, which KVM hands to the witness: arm64 has no HLT, and
	// KVM keeps a WFI to itself.
	l2Doorbell = l2MemoryBytes
)

// The two programs, for x86 in 16-bit real mode. Each is checked against what
// an assembler makes of the same source in l2_test.go.

// x86StoreProgram stores l2Stored at l2Data and halts:
//
//	mov dword [0x2000], 0x4b4f324c
//	hlt
func x86StoreProgram() []byte {
	program := []byte{0x66, 0xc7, 0x06}
	program = binary.LittleEndian.AppendUint16(program, l2Data)
	program = binary.LittleEndian.AppendUint32(program, l2Stored)
	return append(program, x86Halt)
}

// x86CountProgram halts once, which tells the witness that L2 has entered,
// and then counts in EAX forever, storing each count at l2Data. The count is
// kept in a register and only copied out, so an L2 whose registers were lost
// would start again from nothing and be seen to go backwards. Between counts
// it spins 65536 times on LOOP, so the 32-bit count takes days to wrap:
//
//	      hlt
//	next: inc eax
//	      mov [0x2000], eax
//	      mov cx, 0
//	spin: loop spin
//	      jmp next
func x86CountProgram() []byte {
	program := []byte{x86Halt}
	next := len(program)
	program = append(program, 0x66, 0x40)
	program = append(program, 0x66, 0xa3)
	program = binary.LittleEndian.AppendUint16(program, l2Data)
	program = append(program, 0xb9, 0x00, 0x00)
	spin := len(program)
	program = append(program, 0xe2, x86Back(spin, spin+2))
	return append(program, 0xeb, x86Back(next, len(program)+2))
}

// x86Halt is HLT, which KVM hands to the witness when there is no in-kernel
// interrupt controller to wait on.
const x86Halt = 0xf4

// x86Back is the 8-bit displacement of a jump back to target from the
// instruction that ends at end.
func x86Back(target, end int) byte {
	return byte(int8(target - end))
}

// The same two programs for arm64, as instruction words.

// arm64StoreProgram stores l2Stored at l2Data and stops at the doorbell:
//
//	      movz w1, #0x324c
//	      movk w1, #0x4b4f, lsl #16
//	      movz x2, #0x2000
//	      str  w1, [x2]
//	      movz x3, #0x1, lsl #16
//	      str  wzr, [x3]
//	spin: b    spin
func arm64StoreProgram() []byte {
	return arm64Words(
		arm64Movz(32, 1, uint16(l2Stored&0xffff), 0),
		arm64Movk(32, 1, uint16(l2Stored>>16), 16),
		arm64Movz(64, 2, l2Data, 0),
		arm64StrW(1, 2),
		arm64Movz(64, 3, l2Doorbell>>16, 16),
		arm64StrW(arm64Zero, 3),
		arm64B(0),
	)
}

// arm64CountProgram rings the doorbell once, which tells the witness that L2
// has entered, and then counts in w0 as x86CountProgram does:
//
//	      movz x2, #0x2000
//	      movz x3, #0x1, lsl #16
//	      str  wzr, [x3]
//	      movz w0, #0
//	next: add  w0, w0, #1
//	      str  w0, [x2]
//	      movz x4, #0x1, lsl #16
//	spin: subs x4, x4, #1
//	      b.ne spin
//	      b    next
func arm64CountProgram() []byte {
	return arm64Words(
		arm64Movz(64, 2, l2Data, 0),
		arm64Movz(64, 3, l2Doorbell>>16, 16),
		arm64StrW(arm64Zero, 3),
		arm64Movz(32, 0, 0, 0),
		arm64AddW(0, 0, 1),
		arm64StrW(0, 2),
		arm64Movz(64, 4, 1, 16),
		arm64SubsX(4, 4, 1),
		arm64Bne(-1),
		arm64B(-5),
	)
}

// arm64Zero is register 31 where an instruction reads it as the zero register.
const arm64Zero = 31

func arm64Words(words ...uint32) []byte {
	program := make([]byte, 0, 4*len(words))
	for _, word := range words {
		program = binary.LittleEndian.AppendUint32(program, word)
	}
	return program
}

// arm64Wide is MOVZ or MOVK: bits is 32 or 64, shift a multiple of 16.
func arm64Wide(opcode uint32, bits int, rd int, imm uint16, shift int) uint32 {
	if bits == 64 {
		opcode |= 1 << 31
	}
	return opcode | uint32(shift/16)<<21 | uint32(imm)<<5 | uint32(rd)
}

func arm64Movz(bits, rd int, imm uint16, shift int) uint32 {
	return arm64Wide(0x52800000, bits, rd, imm, shift)
}

func arm64Movk(bits, rd int, imm uint16, shift int) uint32 {
	return arm64Wide(0x72800000, bits, rd, imm, shift)
}

// arm64StrW stores the 32-bit register rt at the address in rn.
func arm64StrW(rt, rn int) uint32 { return 0xb9000000 | uint32(rn)<<5 | uint32(rt) }

func arm64AddW(rd, rn int, imm uint16) uint32 {
	return 0x11000000 | uint32(imm)<<10 | uint32(rn)<<5 | uint32(rd)
}

func arm64SubsX(rd, rn int, imm uint16) uint32 {
	return 0xf1000000 | uint32(imm)<<10 | uint32(rn)<<5 | uint32(rd)
}

// arm64B and arm64Bne branch by a number of instructions.
func arm64B(by int) uint32   { return 0x14000000 | uint32(by)&0x3ffffff }
func arm64Bne(by int) uint32 { return 0x54000001 | (uint32(by)&0x7ffff)<<5 }

// vmxControl is one VMX control a nested VM's guest must not be offered: with
// it L1 can name a page of its own that L0's KVM maps for good and writes
// behind the host page tables, where the pager's write-protection cannot see
// the writes (see vmmachine's nested.go and TASK-57).
type vmxControl struct {
	name string
	// msr is the VMX capability MSR that offers it, and bit its bit in the
	// control word. The MSR's high half is the controls that may be 1.
	msr uint32
	bit uint
}

// The three that the audit of Linux's arch/x86/kvm/vmx/nested.c found
// (TASK-57): each names a page L0 maps in nested_get_vmcs12_pages.
var pinningControls = []vmxControl{
	{name: "TPR shadow", msr: 0x48e, bit: 21},              // MSR_IA32_VMX_TRUE_PROCBASED_CTLS
	{name: "virtualize APIC accesses", msr: 0x48b, bit: 0}, // MSR_IA32_VMX_PROCBASED_CTLS2
	{name: "posted interrupts", msr: 0x48d, bit: 7},        // MSR_IA32_VMX_TRUE_PINBASED_CTLS
}

// describeControls is the one line `witness kvm controls` answers with, from
// the value of each MSR pinningControls names.
func describeControls(values map[uint32]uint64) (string, error) {
	said := make([]string, 0, len(pinningControls))
	for _, control := range pinningControls {
		value, ok := values[control.msr]
		if !ok {
			return "", fmt.Errorf("no value for MSR %#x, which says whether %s is offered", control.msr, control.name)
		}
		offered := "no"
		if value>>(32+control.bit)&1 == 1 {
			offered = "yes"
		}
		said = append(said, control.name+": "+offered)
	}
	return "L1 is offered " + strings.Join(said, ", "), nil
}

// kvmOptions is one `witness kvm` command line: the verb and the flags.
type kvmOptions struct {
	verb string
	// file is the counting L2's memory, which `count` reads, and log where
	// the resident witness running it writes why it stopped.
	file, log string
}

const (
	// defaultL2File and defaultL2Log are on the guest's devtmpfs, which is
	// guest memory: they go wherever the guest's RAM goes, and nothing of a
	// disk checkpoint holds them. The root is a disk.
	defaultL2File = "/dev/sproutfs-l2"
	defaultL2Log  = "/dev/sproutfs-l2.log"
	// serveLoopVerb is the internal verb the resident half of `kvm loop` runs.
	serveLoopVerb = "serve-loop"
)

// parseKVM reads what follows `witness kvm`.
func parseKVM(args []string) (kvmOptions, error) {
	parsed := kvmOptions{file: defaultL2File, log: defaultL2Log}
	if len(args) == 0 {
		return parsed, nil
	}
	parsed.verb, args = args[0], args[1:]
	switch parsed.verb {
	case "run", "controls":
		if len(args) != 0 {
			return kvmOptions{}, fmt.Errorf("kvm %s takes no arguments", parsed.verb)
		}
		return parsed, nil
	case "loop", "count", serveLoopVerb:
	default:
		return kvmOptions{}, fmt.Errorf("no kvm command named %q", parsed.verb)
	}
	for len(args) > 0 {
		argument := args[0]
		args = args[1:]
		name, value, inline := strings.Cut(strings.TrimPrefix(argument, "--"), "=")
		if !strings.HasPrefix(argument, "--") {
			return kvmOptions{}, fmt.Errorf("unexpected argument %q", argument)
		}
		if !inline {
			if len(args) == 0 {
				return kvmOptions{}, fmt.Errorf("--%s needs a value", name)
			}
			value, args = args[0], args[1:]
		}
		if value == "" {
			return kvmOptions{}, fmt.Errorf("--%s needs a value", name)
		}
		switch {
		case name == "file":
			parsed.file = value
		case name == "log" && parsed.verb != "count":
			parsed.log = value
		default:
			return kvmOptions{}, fmt.Errorf("kvm %s has no flag --%s", parsed.verb, name)
		}
	}
	return parsed, nil
}

// kvm runs one `witness kvm` command.
func kvm(args []string) error {
	parsed, err := parseKVM(args)
	if err != nil {
		return fmt.Errorf("%w\n\n%s", err, usage)
	}
	var said string
	switch parsed.verb {
	case "":
		said, err = createVM()
	case "run":
		said, err = runL2()
	case "controls":
		said, err = offeredControls()
	case "loop":
		said, err = startL2Loop(parsed)
	case serveLoopVerb:
		return serveL2Loop(parsed)
	case "count":
		var count uint32
		count, err = readCount(parsed.file)
		said = fmt.Sprintf("L2 has counted to %d", count)
	}
	if err != nil {
		return err
	}
	fmt.Println(said)
	return nil
}

// readCount is what a counting L2 has stored at l2Data of its memory, the
// file a resident witness mapped it from. It reads the file rather than
// asking the witness: the file is L2's memory, so this is what L2 wrote.
func readCount(path string) (uint32, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("the counting L2's memory: %w", err)
	}
	defer file.Close()
	var word [4]byte
	if _, err := file.ReadAt(word[:], l2Data); err != nil {
		if errors.Is(err, io.EOF) {
			return 0, fmt.Errorf("%s is too short to be the memory of a counting L2", path)
		}
		return 0, fmt.Errorf("reading %s: %w", path, err)
	}
	return binary.LittleEndian.Uint32(word[:]), nil
}
