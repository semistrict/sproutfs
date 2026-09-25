package vmtest

import (
	"encoding/binary"
	"fmt"
	"unsafe"
)

const (
	kvmSetOneReg          = 1<<30 | 16<<16 | 0xaeac
	kvmARMVCPUInit        = 1<<30 | 32<<16 | 0xaeae
	kvmARMPreferredTarget = 2<<30 | 32<<16 | 0xaeaf
	// coreRegister is the id of core register 0. A 64-bit register's index
	// is its offset in struct kvm_regs in 32-bit words.
	coreRegister = 0x6030_0000_0010_0000
	registerX1   = coreRegister + 2
	registerX2   = coreRegister + 4
	registerX3   = coreRegister + 6
	registerPC   = coreRegister + 64
	registerPSR  = coreRegister + 66
)

// vcpuInit is struct kvm_vcpu_init.
type vcpuInit struct {
	target   uint32
	features [7]uint32
}

// oneRegister is struct kvm_one_reg. The value is a Go pointer, so it moves
// with the value.
type oneRegister struct {
	id    uint64
	value *uint64
}

// guestInstructions loads the byte x1 addresses and writes it to x2. From 16
// bytes in, it first stores w3 there.
func guestInstructions() []byte {
	var code []byte
	for _, instruction := range []uint32{
		0x39400020, // ldrb w0, [x1]
		0x39000040, // strb w0, [x2]
		0x14000000, // b .
		0xd503201f, // nop
		0x39000023, // strb w3, [x1]
		0x39400020, // ldrb w0, [x1]
		0x39000040, // strb w0, [x2]
		0x14000000, // b .
	} {
		code = binary.LittleEndian.AppendUint32(code, instruction)
	}
	return code
}

func (m *machine) initVCPU() error {
	var init vcpuInit
	if _, err := kvmIoctl(m.vm, kvmARMPreferredTarget, unsafe.Pointer(&init)); err != nil {
		return fmt.Errorf("KVM_ARM_PREFERRED_TARGET: %w", err)
	}
	if _, err := kvmIoctl(m.vcpu, kvmARMVCPUInit, unsafe.Pointer(&init)); err != nil {
		return fmt.Errorf("KVM_ARM_VCPU_INIT: %w", err)
	}
	// EL1h with every exception masked.
	return m.setRegister(registerPSR, 0x3c5)
}

func (m *machine) setRegister(id, value uint64) error {
	register := oneRegister{id: id, value: &value}
	if _, err := kvmIoctl(m.vcpu, kvmSetOneReg, unsafe.Pointer(&register)); err != nil {
		return fmt.Errorf("KVM_SET_ONE_REG %#x: %w", id, err)
	}
	return nil
}

func (m *machine) setAccess(pc, address uint64, value byte) error {
	for _, register := range []struct{ id, value uint64 }{
		{registerPC, pc}, {registerX1, address}, {registerX2, guestOutput}, {registerX3, uint64(value)},
	} {
		if err := m.setRegister(register.id, register.value); err != nil {
			return err
		}
	}
	return nil
}

// output is the byte the guest wrote to guestOutput.
func (m *machine) output() (byte, error) {
	run := m.run
	reason := binary.LittleEndian.Uint32(run[kvmRunExitReason:])
	// struct kvm_run's mmio: phys_addr, data[8], len, is_write.
	physical, length, write := binary.LittleEndian.Uint64(run[32:]), binary.LittleEndian.Uint32(run[48:]), run[52]
	if reason != kvmExitMMIO || physical != guestOutput || length != 1 || write != 1 {
		return 0, fmt.Errorf("unexpected KVM exit %d: mmio %#x length %d write %d", reason, physical, length, write)
	}
	return run[40], nil
}
