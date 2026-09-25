//! Minimal KVM adapter: guest code lives outside the managed memory regions and never
//! prefaults their data. The same VM/vCPU and memory slots survive refault tests.

use kvm_bindings::kvm_userspace_memory_region;
use kvm_ioctls::{Kvm, VcpuExit, VcpuFd, VmFd};
use sproutfs_vm_memory::MemoryRegion;
use std::io;

const CODE: u64 = 0x1000;
const DATA: [u64; 2] = [0x1000_0000, 0x2000_0000];
const OUTPUT: u64 = 0x3000_0000;

struct Code(*mut libc::c_void, usize);
impl Drop for Code {
    fn drop(&mut self) {
        unsafe {
            libc::munmap(self.0, self.1);
        }
    }
}

pub struct Machine {
    // Field drop order releases KVM references before unmapping code.
    vcpu: VcpuFd,
    _vm: VmFd,
    _code: Code,
    memory_regions: Vec<MemoryRegion>,
}

type Result<T> = std::result::Result<T, Box<dyn std::error::Error>>;

impl Machine {
    pub fn new(memory_regions: &[MemoryRegion]) -> Result<Self> {
        let kvm = Kvm::new()?;
        let vm = kvm.create_vm()?;
        let size = unsafe { libc::sysconf(libc::_SC_PAGESIZE) } as usize;
        let addr = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                size,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_PRIVATE | libc::MAP_ANONYMOUS,
                -1,
                0,
            )
        };
        if addr == libc::MAP_FAILED {
            return Err(io::Error::last_os_error().into());
        }
        let code = Code(addr, size);
        #[cfg(target_arch = "aarch64")]
        let instructions: &[u32] = &[
            0x39400020, // ldrb w0, [x1]
            0x39000040, // strb w0, [x2] -> MMIO output
            0x14000000, // b . (not reached)
            0xd503201f, // nop / alignment
            0x39000023, // strb w3, [x1]
            0x39400020, // ldrb w0, [x1]
            0x39000040, // strb w0, [x2] -> MMIO output
            0x14000000,
        ];
        #[cfg(target_arch = "aarch64")]
        for (i, instruction) in instructions.iter().enumerate() {
            unsafe {
                addr.cast::<u32>().add(i).write(instruction.to_le());
            }
        }
        #[cfg(target_arch = "x86_64")]
        {
            // 32-bit flat mode. EDI addresses the managed byte; BL is the value.
            let read = [0x8a, 0x07, 0xee, 0xf4]; // mov al,[edi]; out dx,al; hlt
            let write = [0x88, 0x1f, 0x8a, 0x07, 0xee, 0xf4]; // mov [edi],bl; read/output
            unsafe {
                std::ptr::copy_nonoverlapping(read.as_ptr(), addr.cast::<u8>(), read.len());
                std::ptr::copy_nonoverlapping(
                    write.as_ptr(),
                    addr.cast::<u8>().add(16),
                    write.len(),
                );
            }
        }
        // Code is populated before slot registration. Managed data is never touched.
        unsafe {
            vm.set_user_memory_region(kvm_userspace_memory_region {
                slot: 0,
                guest_phys_addr: CODE,
                memory_size: size as u64,
                userspace_addr: addr as u64,
                flags: 0,
            })?;
        }
        for (i, r) in memory_regions.iter().enumerate() {
            assert!(i < DATA.len() && r.len as u64 <= 0x1000_0000);
            unsafe {
                vm.set_user_memory_region(kvm_userspace_memory_region {
                    slot: (i + 1) as u32,
                    guest_phys_addr: DATA[i],
                    memory_size: r.len as u64,
                    userspace_addr: r.address as u64,
                    flags: 0,
                })?;
            }
        }
        let vcpu = vm.create_vcpu(0)?;
        #[cfg(target_arch = "aarch64")]
        {
            let mut target = kvm_bindings::kvm_vcpu_init::default();
            vm.get_preferred_target(&mut target)?;
            vcpu.vcpu_init(&target)?;
            vcpu.set_one_reg(0x6030_0000_0010_0042, &0x3c5u64.to_le_bytes())?; // PSTATE EL1h, masked exceptions
        }
        #[cfg(target_arch = "x86_64")]
        {
            let mut regs = vcpu.get_sregs()?;
            let data = kvm_bindings::kvm_segment {
                base: 0,
                limit: u32::MAX,
                selector: 16,
                type_: 3,
                present: 1,
                dpl: 0,
                db: 1,
                s: 1,
                l: 0,
                g: 1,
                avl: 0,
                unusable: 0,
                padding: 0,
            };
            regs.cs = kvm_bindings::kvm_segment {
                selector: 8,
                type_: 11,
                ..data
            };
            regs.ds = data;
            regs.es = data;
            regs.fs = data;
            regs.gs = data;
            regs.ss = data;
            regs.cr0 |= 1;
            vcpu.set_sregs(&regs)?;
        }
        Ok(Self {
            vcpu,
            _vm: vm,
            _code: code,
            memory_regions: memory_regions.to_vec(),
        })
    }

    pub fn access(&mut self, memory_region: usize, offset: usize, value: Option<u8>) -> Result<u8> {
        assert!(offset < self.memory_regions[memory_region].len);
        let pc = CODE + if value.is_some() { 16 } else { 0 };
        let address = DATA[memory_region] + offset as u64;
        #[cfg(target_arch = "aarch64")]
        {
            let base = 0x6030_0000_0010_0000;
            self.vcpu.set_one_reg(base + 64, &pc.to_le_bytes())?;
            self.vcpu.set_one_reg(base + 2, &address.to_le_bytes())?; // x1
            self.vcpu.set_one_reg(base + 4, &OUTPUT.to_le_bytes())?; // x2
            self.vcpu
                .set_one_reg(base + 6, &(value.unwrap_or(0) as u64).to_le_bytes())?; // x3
        }
        #[cfg(target_arch = "x86_64")]
        {
            let mut regs = self.vcpu.get_regs()?;
            regs.rip = pc;
            regs.rdi = address;
            regs.rbx = value.unwrap_or(0) as u64;
            regs.rdx = 0x3f8;
            regs.rflags = 2;
            self.vcpu.set_regs(&regs)?;
        }
        let byte = match self.vcpu.run()? {
            VcpuExit::MmioWrite(addr, bytes) if addr == OUTPUT && bytes.len() == 1 => bytes[0],
            VcpuExit::IoOut(0x3f8, bytes) if bytes.len() == 1 => bytes[0],
            exit => return Err(format!("unexpected KVM exit: {exit:?}").into()),
        };
        // Complete the emulated output before changing registers. This preserves
        // the vCPU/secondary mappings across calls without executing the loop.
        self.vcpu.set_kvm_immediate_exit(1);
        let completion = self.vcpu.run();
        if !matches!(completion, Err(ref err) if err.errno() == libc::EINTR) {
            return Err(format!("unexpected KVM completion: {completion:?}").into());
        }
        self.vcpu.set_kvm_immediate_exit(0);
        Ok(byte)
    }
}
