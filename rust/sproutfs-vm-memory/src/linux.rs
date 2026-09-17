use std::io;
use std::mem;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::ptr;

use linux_raw_sys::general as u;

pub const PAGE_SIZE: usize = 2 << 20;

pub(crate) fn ioctl<T>(fd: &OwnedFd, number: u32, value: &mut T) -> io::Result<()> {
    // asm-generic ioctl encoding, shared by the supported x86_64/aarch64 targets.
    let request =
        (3u64 << 30) | ((mem::size_of::<T>() as u64) << 16) | (0xaa << 8) | u64::from(number);
    // SAFETY: value has the UAPI layout corresponding to this private call site.
    if unsafe { libc::ioctl(fd.as_raw_fd(), request as _, value) } < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

pub(crate) struct Uffd(pub OwnedFd);

impl Uffd {
    pub fn new() -> io::Result<Self> {
        let flags = libc::O_CLOEXEC | libc::O_NONBLOCK;
        // Do not set UFFD_USER_MODE_ONLY: KVM/host kernel access must fault too.
        let mut raw = unsafe { libc::syscall(u::__NR_userfaultfd as libc::c_long, flags) } as i32;
        if raw < 0 {
            let device = std::fs::OpenOptions::new()
                .read(true)
                .write(true)
                .open("/dev/userfaultfd")?;
            // USERFAULTFD_IOC_NEW is _IO(0xaa, 0).
            raw = unsafe { libc::ioctl(device.as_raw_fd(), 0xaa00, flags) };
        }
        if raw < 0 {
            return Err(io::Error::last_os_error());
        }
        let fd = unsafe { OwnedFd::from_raw_fd(raw) };
        let required = u::UFFD_FEATURE_EVENT_REMAP
            | u::UFFD_FEATURE_PAGEFAULT_FLAG_WP
            | u::UFFD_FEATURE_MISSING_HUGETLBFS
            | u::UFFD_FEATURE_MINOR_HUGETLBFS
            | u::UFFD_FEATURE_WP_HUGETLBFS_SHMEM;
        let mut api = u::uffdio_api {
            api: u::UFFD_API as u64,
            features: required as u64,
            ioctls: 0,
        };
        ioctl(&fd, u::_UFFDIO_API, &mut api)?;
        if api.features & required as u64 != required as u64 {
            return Err(io::Error::new(
                io::ErrorKind::Unsupported,
                "kernel lacks required HugeTLB/UFFD features",
            ));
        }
        Ok(Self(fd))
    }

    pub fn register(&self, mapping: &Mapping, shared: bool) -> io::Result<()> {
        let mode = u::UFFDIO_REGISTER_MODE_MISSING
            | u::UFFDIO_REGISTER_MODE_WP
            | if shared {
                u::UFFDIO_REGISTER_MODE_MINOR
            } else {
                0
            };
        let mut reg = u::uffdio_register {
            range: u::uffdio_range {
                start: mapping.addr as u64,
                len: mapping.len as u64,
            },
            mode: mode as u64,
            ioctls: 0,
        };
        ioctl(&self.0, u::_UFFDIO_REGISTER, &mut reg)?;
        let required = (1u64 << u::_UFFDIO_WRITEPROTECT)
            | if shared {
                1u64 << u::_UFFDIO_CONTINUE
            } else {
                1u64 << u::_UFFDIO_COPY
            };
        if reg.ioctls & required != required {
            return Err(io::Error::new(
                io::ErrorKind::Unsupported,
                "range lacks required UFFD ioctls",
            ));
        }
        Ok(())
    }

    pub fn protect(&self, mapping: &Mapping) -> io::Result<()> {
        let mut wp = u::uffdio_writeprotect {
            range: u::uffdio_range {
                start: mapping.addr as u64,
                len: mapping.len as u64,
            },
            mode: 1, // UFFDIO_WRITEPROTECT_MODE_WP (not emitted by linux-raw-sys)
        };
        ioctl(&self.0, u::_UFFDIO_WRITEPROTECT, &mut wp)
    }
}

// One untouched anonymous source per logical region preserves a common
// anonymous offset origin for zero and missing-trap replacements. Creating
// unrelated anonymous mappings for each revoke prevents adjacent VMAs from
// merging, even when only a few arena pages remain resident.
pub(crate) struct TrapSource(Mapping);

impl TrapSource {
    pub fn new(len: usize) -> io::Result<Self> {
        Ok(Self(Mapping::new(len, None)?))
    }

    // The source is never exposed, registered, populated, or written. Moving
    // its empty page tables leaves it empty and mapped for future preparations.
    // DONTUNMAP keeps the address reservation owned throughout the operation.
    pub fn prepare(&self, offset: usize, len: usize) -> io::Result<Mapping> {
        if offset.checked_add(len).is_none_or(|end| end > self.0.len) {
            return Err(io::ErrorKind::InvalidInput.into());
        }
        let source = unsafe { self.0.addr.cast::<u8>().add(offset) }.cast();
        let destination = Mapping::new(len, None)?;
        let addr = unsafe {
            libc::mremap(
                source,
                len,
                len,
                libc::MREMAP_MAYMOVE | libc::MREMAP_FIXED | libc::MREMAP_DONTUNMAP,
                destination.addr,
            )
        };
        if addr == libc::MAP_FAILED {
            return Err(io::Error::last_os_error());
        }
        Ok(destination)
    }
}

pub(crate) struct Mapping {
    pub addr: *mut libc::c_void,
    pub len: usize,
}

// Only the owning session mutates mapping metadata. Access to raw addresses is
// unsafe and must obey the embedding process's lifetime/pinning contract.
unsafe impl Send for Mapping {}

impl Mapping {
    pub fn new(len: usize, backing: Option<(&OwnedFd, u64)>) -> io::Result<Self> {
        if len == 0 || len % PAGE_SIZE != 0 {
            return Err(io::ErrorKind::InvalidInput.into());
        }
        if backing.is_none() {
            let reserved_len = len
                .checked_add(PAGE_SIZE)
                .ok_or(io::ErrorKind::InvalidInput)?;
            let reservation = unsafe {
                libc::mmap(
                    ptr::null_mut(),
                    reserved_len,
                    libc::PROT_READ | libc::PROT_WRITE,
                    libc::MAP_PRIVATE | libc::MAP_ANONYMOUS | libc::MAP_NORESERVE,
                    -1,
                    0,
                )
            };
            if reservation == libc::MAP_FAILED {
                return Err(io::Error::last_os_error());
            }
            let base = reservation as usize;
            let aligned = (base + PAGE_SIZE - 1) & !(PAGE_SIZE - 1);
            let prefix = aligned - base;
            unsafe {
                if prefix != 0 {
                    libc::munmap(reservation, prefix);
                }
                libc::munmap((aligned + len) as *mut _, PAGE_SIZE - prefix);
            }
            let mapping = Self {
                addr: aligned as *mut _,
                len,
            };
            mapping.advise(true)?;
            return Ok(mapping);
        }
        let (fd, offset, flags) = match backing {
            Some((fd, offset)) => (fd.as_raw_fd(), offset as libc::off_t, libc::MAP_SHARED),
            None => (
                -1,
                0,
                libc::MAP_PRIVATE | libc::MAP_ANONYMOUS | libc::MAP_NORESERVE,
            ),
        };
        // Build away from the live address. No client can access this mapping
        // before registration and protection have completed.
        let addr = unsafe {
            libc::mmap(
                ptr::null_mut(),
                len,
                libc::PROT_READ | libc::PROT_WRITE,
                flags,
                fd,
                offset,
            )
        };
        if addr == libc::MAP_FAILED {
            return Err(io::Error::last_os_error());
        }
        let mapping = Self { addr, len };
        mapping.advise(false)?;
        Ok(mapping)
    }

    fn advise(&self, anonymous: bool) -> io::Result<()> {
        for advice in [libc::MADV_NOHUGEPAGE, libc::MADV_DONTFORK] {
            if advice == libc::MADV_NOHUGEPAGE && !anonymous {
                continue;
            }
            if unsafe { libc::madvise(self.addr, self.len, advice) } != 0 {
                return Err(io::Error::last_os_error());
            }
        }
        Ok(())
    }

    pub fn replace(mut self, target: *mut libc::c_void) -> io::Result<()> {
        // The Go UFFD reader must drain EVENT_REMAP independently of waiting
        // for this command's acknowledgement; mremap waits for that event read.
        let got = unsafe {
            libc::mremap(
                self.addr,
                self.len,
                self.len,
                libc::MREMAP_MAYMOVE | libc::MREMAP_FIXED,
                target,
            )
        };
        if got == libc::MAP_FAILED {
            return Err(io::Error::last_os_error());
        }
        self.len = 0; // the region, not this temporary owner, now owns the range
        Ok(())
    }

    pub fn populate_zero(&self) -> io::Result<()> {
        // Only freshly created anonymous mappings may call this, before UFFD
        // registration or exposure. Linux installs its shared zero page; no
        // content scan or private data-page allocation is involved.
        if unsafe { libc::madvise(self.addr, self.len, libc::MADV_POPULATE_READ) } != 0 {
            return Err(io::Error::last_os_error());
        }
        Ok(())
    }
}

impl Drop for Mapping {
    fn drop(&mut self) {
        if self.len != 0 {
            // No external users may remain when the owning session is dropped.
            unsafe {
                libc::munmap(self.addr, self.len);
            }
        }
    }
}
