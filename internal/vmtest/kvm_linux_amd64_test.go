package vmtest

import (
	"encoding/binary"
	"fmt"
	"unsafe"
)

const (
	kvmGetRegs  = 2<<30 | 144<<16 | 0xae81
	kvmSetRegs  = 1<<30 | 144<<16 | 0xae82
	kvmGetSRegs = 2<<30 | 312<<16 | 0xae83
	kvmSetSRegs = 1<<30 | 312<<16 | 0xae84
	// outputPort is where the guest writes the byte it read.
	outputPort = 0x3f8
)

// segment is struct kvm_segment.
type segment struct {
	base                                              uint64
	limit                                             uint32
	selector                                          uint16
	kind, present, dpl, db, s, l, g, avl, unusable, _ uint8
}

// descriptorTable is struct kvm_dtable.
type descriptorTable struct {
	base  uint64
	limit uint16
	_     [3]uint16
}

// specialRegisters is struct kvm_sregs.
type specialRegisters struct {
	cs, ds, es, fs, gs, ss, tr, ldt     segment
	gdt, idt                            descriptorTable
	cr0, cr2, cr3, cr4, cr8, efer, apic uint64
	interrupts                          [4]uint64
}

// registers is struct kvm_regs.
type registers struct {
	rax, rbx, rcx, rdx, rsi, rdi, rsp, rbp uint64
	r8, r9, r10, r11, r12, r13, r14, r15   uint64
	rip, rflags                            uint64
}

// The sizes the ioctl numbers above state.
var (
	_ [312 - unsafe.Sizeof(specialRegisters{})]byte
	_ [unsafe.Sizeof(specialRegisters{}) - 312]byte
	_ [144 - unsafe.Sizeof(registers{})]byte
	_ [unsafe.Sizeof(registers{}) - 144]byte
)

// guestInstructions runs in 32-bit flat mode. EDI addresses the byte and BL
// is the value to store. From 16 bytes in, it first stores BL there.
func guestInstructions() []byte {
	code := make([]byte, 22)
	copy(code, []byte{0x8a, 0x07, 0xee, 0xf4})                  // mov al,[edi]; out dx,al; hlt
	copy(code[16:], []byte{0x88, 0x1f, 0x8a, 0x07, 0xee, 0xf4}) // mov [edi],bl; then the same
	return code
}

func (m *machine) initVCPU() error {
	var special specialRegisters
	if _, err := kvmIoctl(m.vcpu, kvmGetSRegs, unsafe.Pointer(&special)); err != nil {
		return fmt.Errorf("KVM_GET_SREGS: %w", err)
	}
	data := segment{limit: ^uint32(0), selector: 16, kind: 3, present: 1, db: 1, s: 1, g: 1}
	code := data
	code.selector, code.kind = 8, 11
	special.cs = code
	special.ds, special.es, special.fs, special.gs, special.ss = data, data, data, data, data
	special.cr0 |= 1
	if _, err := kvmIoctl(m.vcpu, kvmSetSRegs, unsafe.Pointer(&special)); err != nil {
		return fmt.Errorf("KVM_SET_SREGS: %w", err)
	}
	return nil
}

func (m *machine) setAccess(pc, address uint64, value byte) error {
	var r registers
	if _, err := kvmIoctl(m.vcpu, kvmGetRegs, unsafe.Pointer(&r)); err != nil {
		return fmt.Errorf("KVM_GET_REGS: %w", err)
	}
	r.rip, r.rdi, r.rbx, r.rdx, r.rflags = pc, address, uint64(value), outputPort, 2
	if _, err := kvmIoctl(m.vcpu, kvmSetRegs, unsafe.Pointer(&r)); err != nil {
		return fmt.Errorf("KVM_SET_REGS: %w", err)
	}
	return nil
}

// output is the byte the guest wrote to outputPort.
func (m *machine) output() (byte, error) {
	run := m.run
	reason := binary.LittleEndian.Uint32(run[kvmRunExitReason:])
	// struct kvm_run's io: direction, size, port, count, data_offset.
	direction, size, port := run[32], run[33], binary.LittleEndian.Uint16(run[34:])
	count, offset := binary.LittleEndian.Uint32(run[36:]), binary.LittleEndian.Uint64(run[40:])
	if reason != kvmExitIO || direction != 1 || size != 1 || port != outputPort || count != 1 {
		return 0, fmt.Errorf("unexpected KVM exit %d: io direction %d size %d port %#x count %d", reason, direction, size, port, count)
	}
	return run[offset], nil
}
