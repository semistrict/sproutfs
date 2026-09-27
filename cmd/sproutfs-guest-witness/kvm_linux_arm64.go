package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unsafe"
)

// The arm64 ioctls: struct kvm_vcpu_init is 32 bytes, struct kvm_one_reg 16.
const (
	kvmArmPreferredTarget = 0x8020AEAF // _IOR(KVMIO, 0xaf, struct kvm_vcpu_init)
	kvmArmVCPUInit        = 0x4020AEAE // _IOW(KVMIO, 0xae, struct kvm_vcpu_init)
	kvmSetOneReg          = 0x4010AEAC // _IOW(KVMIO, 0xac, struct kvm_one_reg)
)

// arm64PC is the id of the PC in KVM_SET_ONE_REG: KVM_REG_ARM64,
// KVM_REG_SIZE_U64 and KVM_REG_ARM_CORE with the offset of regs.pc in struct
// kvm_regs in 32-bit words, which is 64.
const arm64PC = 0x6030000000100040

// entry is the PC KVM_SET_ONE_REG reads through the address it is given. It is
// a package variable because a variable is certain not to move while the
// kernel reads it, and the address is inside another structure the Go runtime
// does not see as a pointer.
var entry uint64 = l2Code

func storeProgram() []byte { return arm64StoreProgram() }
func countProgram() []byte { return arm64CountProgram() }

func prepareVM(int) error { return nil }

// prepareVCPU initialises the vCPU as the processor this VM prefers, which
// resets it at EL1 with its MMU off and every interrupt masked, and points its
// PC at l2Code.
func prepareVCPU(vm, vcpu int) error {
	var init [32]byte
	if _, err := ioctlPointer(vm, kvmArmPreferredTarget, unsafe.Pointer(&init[0])); err != nil {
		return fmt.Errorf("asking for the L2's preferred processor: %w", err)
	}
	if _, err := ioctlPointer(vcpu, kvmArmVCPUInit, unsafe.Pointer(&init[0])); err != nil {
		return fmt.Errorf("initialising the L2's vCPU: %w", err)
	}
	var reg [16]byte
	binary.LittleEndian.PutUint64(reg[0:], arm64PC)
	binary.LittleEndian.PutUint64(reg[8:], uint64(uintptr(unsafe.Pointer(&entry))))
	if _, err := ioctlPointer(vcpu, kvmSetOneReg, unsafe.Pointer(&reg[0])); err != nil {
		return fmt.Errorf("pointing the L2's vCPU at its program: %w", err)
	}
	return nil
}

// stopped reports the store to l2Doorbell both programs stop with: struct
// kvm_run's mmio is phys_addr, data[8], len and is_write.
func stopped(run []byte, reason uint32) bool {
	return reason == exitMMIO && binary.LittleEndian.Uint64(run[kvmRunData:]) == l2Doorbell &&
		run[kvmRunData+20] == 1
}

// offeredControls is VMX's, and a nested VM is x86's alone.
func offeredControls() (string, error) {
	return "", errors.New("VMX controls are x86's, and this is arm64")
}
