package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The x86 ioctls, with their structures' sizes: struct kvm_sregs is 312
// bytes, struct kvm_regs 144, struct kvm_msr_list and struct kvm_msrs the
// 4 and 8 bytes before their arrays.
const (
	kvmSetTSSAddr             = 0xAE47     // _IO(KVMIO, 0x47)
	kvmGetSregs               = 0x8138AE83 // _IOR(KVMIO, 0x83, struct kvm_sregs)
	kvmSetSregs               = 0x4138AE84 // _IOW(KVMIO, 0x84, struct kvm_sregs)
	kvmSetRegs                = 0x4090AE82 // _IOW(KVMIO, 0x82, struct kvm_regs)
	kvmGetMSRFeatureIndexList = 0xC004AE0A // _IOWR(KVMIO, 0x0a, struct kvm_msr_list)
	kvmGetMSRs                = 0xC008AE88 // _IOWR(KVMIO, 0x88, struct kvm_msrs)
)

// tssAddress is three pages below 4 GiB, where every KVM user puts them: KVM
// runs a real-mode guest in virtual-8086 mode where the processor cannot run
// real mode itself, and needs a TSS for it outside the guest's memory. L1's
// processor is whatever L0 offers it, so this witness does not count on
// unrestricted guest.
const tssAddress = 0xfffbd000

func storeProgram() []byte { return x86StoreProgram() }
func countProgram() []byte { return x86CountProgram() }

func prepareVM(vm int) error {
	if _, err := ioctl(vm, kvmSetTSSAddr, tssAddress); err != nil {
		return fmt.Errorf("giving the L2 its TSS pages: %w", err)
	}
	return nil
}

// prepareVCPU starts the vCPU at l2Code in real mode. A vCPU is created in the
// state a processor resets to, which is real mode with CS at 0xf000 based at
// 0xffff0000; CS is moved to 0 and the rest left as reset left it, DS based at
// 0 among them.
func prepareVCPU(_, vcpu int) error {
	var sregs [312]byte
	if _, err := ioctlPointer(vcpu, kvmGetSregs, unsafe.Pointer(&sregs[0])); err != nil {
		return fmt.Errorf("reading the L2 vCPU's segments: %w", err)
	}
	// CS is the first struct kvm_segment: base, limit, selector.
	binary.LittleEndian.PutUint64(sregs[0:], 0)
	binary.LittleEndian.PutUint16(sregs[12:], 0)
	if _, err := ioctlPointer(vcpu, kvmSetSregs, unsafe.Pointer(&sregs[0])); err != nil {
		return fmt.Errorf("setting the L2 vCPU's segments: %w", err)
	}
	// rax through r15, then rip and rflags, whose bit 1 is always set.
	var regs [144]byte
	binary.LittleEndian.PutUint64(regs[16*8:], l2Code)
	binary.LittleEndian.PutUint64(regs[17*8:], 2)
	if _, err := ioctlPointer(vcpu, kvmSetRegs, unsafe.Pointer(&regs[0])); err != nil {
		return fmt.Errorf("setting the L2 vCPU's registers: %w", err)
	}
	return nil
}

// stopped reports the HLT both programs stop at.
func stopped(_ []byte, reason uint32) bool { return reason == exitHLT }

// offeredControls says which of pinningControls this guest is offered.
//
// It asks this guest's own KVM, through the feature MSRs /dev/kvm reports:
// the VMX capabilities KVM would give a VM of its own. Those are what KVM read
// from the processor it runs on — here, the VMX capability MSRs L0 offers this
// guest — narrowed to what it can nest, and it nests all three of these when
// it is offered them. So a control this reports as not offered is one L0 did
// not offer; the raw values go to stderr for whoever has to read them. The
// guest's root has no rdmsr, and /dev/cpu/*/msr is a module it may not have.
func offeredControls() (string, error) {
	device, err := openKVM()
	if err != nil {
		return "", err
	}
	defer unix.Close(device)
	listed, err := featureMSRs(device)
	if err != nil {
		return "", err
	}
	msrs := make([]byte, 8+16*len(pinningControls))
	binary.LittleEndian.PutUint32(msrs, uint32(len(pinningControls)))
	for i, control := range pinningControls {
		if !listed[control.msr] {
			return "", fmt.Errorf("/dev/kvm lists no feature MSR %#x, which says whether %s is offered", control.msr, control.name)
		}
		binary.LittleEndian.PutUint32(msrs[8+16*i:], control.msr)
	}
	read, err := ioctlPointer(device, kvmGetMSRs, unsafe.Pointer(&msrs[0]))
	if err != nil {
		return "", fmt.Errorf("reading the VMX capability MSRs from /dev/kvm: %w", err)
	}
	if read != uintptr(len(pinningControls)) {
		return "", fmt.Errorf("/dev/kvm read %d of the %d VMX capability MSRs asked for", read, len(pinningControls))
	}
	values := make(map[uint32]uint64, len(pinningControls))
	for i := range pinningControls {
		entry := msrs[8+16*i:]
		msr, value := binary.LittleEndian.Uint32(entry), binary.LittleEndian.Uint64(entry[8:])
		values[msr] = value
		fmt.Fprintf(os.Stderr, "MSR %#x = %#016x\n", msr, value)
	}
	return describeControls(values)
}

// featureMSRs is the set of MSRs /dev/kvm reports as features. The first ask
// is for none, which KVM refuses with E2BIG and the count, unless it has none.
func featureMSRs(device int) (map[uint32]bool, error) {
	var head [4]byte
	_, err := ioctlPointer(device, kvmGetMSRFeatureIndexList, unsafe.Pointer(&head[0]))
	if err != nil && !errors.Is(err, unix.E2BIG) {
		return nil, fmt.Errorf("asking /dev/kvm how many feature MSRs it has: %w", err)
	}
	count := binary.LittleEndian.Uint32(head[:])
	list := make([]byte, 4+4*count)
	binary.LittleEndian.PutUint32(list, count)
	if _, err := ioctlPointer(device, kvmGetMSRFeatureIndexList, unsafe.Pointer(&list[0])); err != nil {
		return nil, fmt.Errorf("listing /dev/kvm's feature MSRs: %w", err)
	}
	listed := make(map[uint32]bool, count)
	for i := range binary.LittleEndian.Uint32(list) {
		listed[binary.LittleEndian.Uint32(list[4+4*i:])] = true
	}
	return listed, nil
}
