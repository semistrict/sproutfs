use std::io;
use std::mem;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::ptr;

use linux_raw_sys::general as u;

/// The pages this transport maps. A session states which of them its region
/// runs, and the arena it attaches is the memory that page is made of: the
/// host's 2 MiB HugeTLB pool, or an ordinary shared memfd over the host's own
/// 4 KiB pages.
pub const MIN_PAGE_SIZE: usize = 4 << 10;
pub const MAX_PAGE_SIZE: usize = 2 << 20;

/// The kinds of memory an arena is made of, as the attachment states them.
pub const BACKING_HUGETLB: u64 = 1;
pub const BACKING_MEMFD: u64 = 2;

const HUGETLBFS_MAGIC: u64 = 0x958458f6;
const TMPFS_MAGIC: u64 = 0x01021994;

/// The arena a region of this page must be attached over, or `None` for a page
/// this transport does not map. The two are one statement: a 2 MiB page is a
/// page of the pool and a 4 KiB page is ordinary memory, so a session that
/// stated one and attached the other is refused before anything is mapped.
pub(crate) fn backing_for(page_size: usize) -> Option<u64> {
    match page_size {
        MAX_PAGE_SIZE => Some(BACKING_HUGETLB),
        MIN_PAGE_SIZE => Some(BACKING_MEMFD),
        _ => None,
    }
}

/// Checks a received arena descriptor against the backing kind the attachment
/// claimed and the offset space it reported, before the descriptor is mapped.
///
/// `len` is the arena's addresses, which is the file's length. The file is
/// sparse — a pager that places a private page at the offset it has within its
/// 2 MiB range owns 512 consecutive offsets per range and puts memory at a
/// handful of them — so this checks the length and never the blocks behind it.
pub(crate) fn check_backing(fd: &OwnedFd, kind: u64, len: u64) -> io::Result<()> {
    let refuse = |what: String| io::Error::new(io::ErrorKind::InvalidData, what);
    let mut stat: libc::stat = unsafe { std::mem::zeroed() };
    called();
    if unsafe { libc::fstat(fd.as_raw_fd(), &mut stat) } != 0 {
        return Err(last("fstat"));
    }
    if len > i64::MAX as u64 || stat.st_size as u64 != len {
        return Err(refuse(format!(
            "the attached arena has {} bytes of offsets, the attachment said {len}",
            stat.st_size
        )));
    }
    let mut fs: libc::statfs = unsafe { std::mem::zeroed() };
    called();
    if unsafe { libc::fstatfs(fd.as_raw_fd(), &mut fs) } != 0 {
        return Err(last("fstatfs"));
    }
    let (magic, block) = match kind {
        BACKING_HUGETLB => (HUGETLBFS_MAGIC, MAX_PAGE_SIZE),
        BACKING_MEMFD => (TMPFS_MAGIC, MIN_PAGE_SIZE),
        other => return Err(refuse(format!("unknown arena kind {other}"))),
    };
    if fs.f_type as u64 != magic || fs.f_bsize as usize != block {
        return Err(refuse(format!(
            "arena kind {kind} is a filesystem of type {magic:#x} with {block}-byte blocks, \
             the descriptor is type {:#x} with {}-byte blocks",
            fs.f_type as u64, fs.f_bsize as i64
        )));
    }
    Ok(())
}

/// Names the kernel call a failure came out of.
///
/// A mutation that fails is terminal and sends no acknowledgement, so the
/// pager learns nothing but that the connection went and the errno is the whole
/// of what this client's embedder can print. Replacing one run is five
/// different calls over three different ranges and most of them can answer the
/// same errno, so the name is what turns that line into somewhere to look.
pub(crate) fn failed(call: &str, err: io::Error) -> io::Error {
    io::Error::new(err.kind(), format!("{call}: {err}"))
}

fn last(call: &str) -> io::Error {
    failed(call, io::Error::last_os_error())
}

pub(crate) fn ioctl<T>(fd: &OwnedFd, call: &str, number: u32, value: &mut T) -> io::Result<()> {
    // asm-generic ioctl encoding, shared by the supported x86_64/aarch64 targets.
    let request =
        (3u64 << 30) | ((mem::size_of::<T>() as u64) << 16) | (0xaa << 8) | u64::from(number);
    called();
    // SAFETY: value has the UAPI layout corresponding to this private call site.
    if unsafe { libc::ioctl(fd.as_raw_fd(), request as _, value) } < 0 {
        return Err(last(call));
    }
    Ok(())
}

pub(crate) struct Uffd(pub OwnedFd);

/// Every feature a session can need, whichever geometry its region turns out to
/// run. The page is the attachment's to state and the UFFD is created before
/// it, so the set is negotiated once here: a host runs a pager of each kind, so
/// a kernel that serves only one of the two geometries cannot run this build at
/// all, and saying so at session setup is what a guest gets instead of faults
/// the kernel will not deliver.
const REQUIRED_FEATURES: [(&str, u32); 7] = [
    ("UFFD_FEATURE_EVENT_REMAP", u::UFFD_FEATURE_EVENT_REMAP),
    (
        "UFFD_FEATURE_PAGEFAULT_FLAG_WP",
        u::UFFD_FEATURE_PAGEFAULT_FLAG_WP,
    ),
    (
        "UFFD_FEATURE_MISSING_HUGETLBFS",
        u::UFFD_FEATURE_MISSING_HUGETLBFS,
    ),
    (
        "UFFD_FEATURE_MINOR_HUGETLBFS",
        u::UFFD_FEATURE_MINOR_HUGETLBFS,
    ),
    ("UFFD_FEATURE_MISSING_SHMEM", u::UFFD_FEATURE_MISSING_SHMEM),
    ("UFFD_FEATURE_MINOR_SHMEM", u::UFFD_FEATURE_MINOR_SHMEM),
    (
        "UFFD_FEATURE_WP_HUGETLBFS_SHMEM",
        u::UFFD_FEATURE_WP_HUGETLBFS_SHMEM,
    ),
];

fn required_features() -> u64 {
    REQUIRED_FEATURES
        .iter()
        .fold(0u64, |set, (_, bit)| set | u64::from(*bit))
}

/// Opens a userfaultfd, through the syscall where the deployment permits it and
/// through /dev/userfaultfd otherwise.
fn open_uffd() -> io::Result<OwnedFd> {
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
        return Err(last("userfaultfd"));
    }
    Ok(unsafe { OwnedFd::from_raw_fd(raw) })
}

/// Names the required features this kernel does not have. UFFD_API may be
/// called once per descriptor, and asking for a feature the kernel lacks fails
/// the whole call, so the enquiry is a second descriptor asking for nothing —
/// which is only ever on the failure path, where the point is to say which
/// feature is missing rather than that some feature is.
fn missing_features() -> io::Result<Vec<&'static str>> {
    let fd = open_uffd()?;
    let mut api = u::uffdio_api {
        api: u::UFFD_API as u64,
        features: 0,
        ioctls: 0,
    };
    ioctl(&fd, "UFFDIO_API", u::_UFFDIO_API, &mut api)?;
    Ok(REQUIRED_FEATURES
        .iter()
        .filter(|(_, bit)| api.features & u64::from(*bit) == 0)
        .map(|(name, _)| *name)
        .collect())
}

impl Uffd {
    pub fn new() -> io::Result<Self> {
        let fd = open_uffd()?;
        let required = required_features();
        let mut api = u::uffdio_api {
            api: u::UFFD_API as u64,
            features: required,
            ioctls: 0,
        };
        let enabled = ioctl(&fd, "UFFDIO_API", u::_UFFDIO_API, &mut api)
            .is_ok_and(|()| api.features & required == required);
        if !enabled {
            let missing = missing_features()?;
            return Err(io::Error::new(
                io::ErrorKind::Unsupported,
                format!(
                    "this kernel lacks the userfaultfd features managed memory needs: {}",
                    if missing.is_empty() {
                        "the API negotiation itself failed".to_owned()
                    } else {
                        missing.join(", ")
                    }
                ),
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
        ioctl(&self.0, "UFFDIO_REGISTER", u::_UFFDIO_REGISTER, &mut reg)?;
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
        ioctl(
            &self.0,
            "UFFDIO_WRITEPROTECT",
            u::_UFFDIO_WRITEPROTECT,
            &mut wp,
        )
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
    //
    // The destination is reserved at the source's own phase within the largest
    // page this transport maps, so that a range prepared from the middle of the
    // source keeps the anonymous offset origin the region was attached with and
    // the trap a revocation installs still merges with the traps around it. At
    // a 2 MiB page every offset is a whole page and the phase is zero; at 4 KiB
    // it is what keeps a revoked page from costing a mapping of its own.
    pub fn prepare(&self, offset: usize, len: usize) -> io::Result<Mapping> {
        if offset.checked_add(len).is_none_or(|end| end > self.0.len) {
            return Err(io::ErrorKind::InvalidInput.into());
        }
        let source = unsafe { self.0.addr.cast::<u8>().add(offset) }.cast();
        let destination = Mapping::anonymous(len, offset % MAX_PAGE_SIZE)?;
        called();
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
            return Err(last("mremap"));
        }
        Ok(destination)
    }
}

/// One batch's staging area: the reservation a contiguous span of a batch's
/// MAP runs is built in, away from the live region. The span is advised,
/// registered and write-protected once for all of its runs rather than once
/// each, which is what makes a run of a scattered batch cheap.
///
/// Only the mremap that puts a run in the region stays the run's own. mremap
/// moves one mapping, and two runs of a batch are never one: runs adjacent in
/// both the region and the arena have already been merged by the caller, so the
/// ones left here are adjacent in the region alone and the kernel keeps them
/// apart. Making them one would mean the pager saying so on the wire.
///
/// Runs leave the span front to back, so what is left to release is always the
/// tail that has not left. No address inside the span is ever both released
/// here and available to another thread's mapping.
pub(crate) struct Staging(Mapping);

impl Staging {
    /// Reserves a span of len bytes, aligned for the largest page this
    /// transport maps: a HugeTLB arena can be placed only at such an address.
    pub fn new(len: usize) -> io::Result<Self> {
        Ok(Self(Mapping::anonymous(len, 0)?))
    }

    /// Places one run's arena bytes at offset within the span.
    pub fn place(&self, offset: usize, len: usize, backing: &OwnedFd, at: u64) -> io::Result<()> {
        if len == 0 || offset.checked_add(len).is_none_or(|end| end > self.0.len) {
            return Err(io::ErrorKind::InvalidInput.into());
        }
        // SAFETY: the sum above is within the reservation this owns.
        let target = unsafe { self.0.addr.cast::<u8>().add(offset) }.cast();
        called();
        let addr = unsafe {
            libc::mmap(
                target,
                len,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_SHARED | libc::MAP_FIXED,
                backing.as_raw_fd(),
                at as libc::off_t,
            )
        };
        if addr == libc::MAP_FAILED {
            return Err(last("mmap"));
        }
        Ok(())
    }

    /// Arms the whole span at once, once every run of it is placed: one
    /// MADV_DONTFORK, one registration, and for a read-only span one
    /// write-protect. Each of the three takes a range and walks the mappings in
    /// it, so a span costs what one run of it used to.
    pub fn arm(&self, uffd: &Uffd, protect: bool) -> io::Result<()> {
        self.0.advise(false)?;
        uffd.register(&self.0, true)?;
        if protect {
            uffd.protect(&self.0)?;
        }
        Ok(())
    }

    /// Moves the front len bytes of the span to target, which is one run of it.
    pub fn move_front(&mut self, len: usize, target: *mut libc::c_void) -> io::Result<()> {
        if len == 0 || len > self.0.len {
            return Err(io::ErrorKind::InvalidInput.into());
        }
        called();
        let got = unsafe {
            libc::mremap(
                self.0.addr,
                len,
                len,
                libc::MREMAP_MAYMOVE | libc::MREMAP_FIXED,
                target,
            )
        };
        if got == libc::MAP_FAILED {
            return Err(last("mremap"));
        }
        // SAFETY: the run just left the span, so the span is what follows it.
        self.0.addr = unsafe { self.0.addr.cast::<u8>().add(len) }.cast();
        self.0.len -= len;
        Ok(())
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
    /// Reserves an anonymous range of len bytes whose address sits at `phase`
    /// within the largest page this transport maps. The whole region and the
    /// trap source take phase zero; a range prepared out of the middle of the
    /// source takes its own, so its anonymous offset origin is preserved.
    pub fn anonymous(len: usize, phase: usize) -> io::Result<Self> {
        if len == 0 || len % MIN_PAGE_SIZE != 0 || phase % MIN_PAGE_SIZE != 0 {
            return Err(io::ErrorKind::InvalidInput.into());
        }
        let reserved_len = len
            .checked_add(MAX_PAGE_SIZE)
            .ok_or(io::ErrorKind::InvalidInput)?;
        called();
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
            return Err(last("mmap"));
        }
        let base = reservation as usize;
        let aligned = ((base + MAX_PAGE_SIZE - 1) & !(MAX_PAGE_SIZE - 1)) + phase;
        let aligned = if aligned - base > MAX_PAGE_SIZE {
            aligned - MAX_PAGE_SIZE
        } else {
            aligned
        };
        let prefix = aligned - base;
        unsafe {
            if prefix != 0 {
                called();
                libc::munmap(reservation, prefix);
            }
            if prefix != MAX_PAGE_SIZE {
                called();
                libc::munmap((aligned + len) as *mut _, MAX_PAGE_SIZE - prefix);
            }
        }
        let mapping = Self {
            addr: aligned as *mut _,
            len,
        };
        mapping.advise(true)?;
        Ok(mapping)
    }

    pub fn new(len: usize, backing: Option<(&OwnedFd, u64)>) -> io::Result<Self> {
        if len == 0 || len % MIN_PAGE_SIZE != 0 {
            return Err(io::ErrorKind::InvalidInput.into());
        }
        let Some((fd, offset)) = backing else {
            return Self::anonymous(len, 0);
        };
        // Build away from the live address. No client can access this mapping
        // before registration and protection have completed.
        called();
        let addr = unsafe {
            libc::mmap(
                ptr::null_mut(),
                len,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_SHARED,
                fd.as_raw_fd(),
                offset as libc::off_t,
            )
        };
        if addr == libc::MAP_FAILED {
            return Err(last("mmap"));
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
            called();
            if unsafe { libc::madvise(self.addr, self.len, advice) } != 0 {
                return Err(last("madvise"));
            }
        }
        Ok(())
    }

    pub fn replace(mut self, target: *mut libc::c_void) -> io::Result<()> {
        // The Go UFFD reader must drain EVENT_REMAP independently of waiting
        // for this command's acknowledgement; mremap waits for that event read.
        called();
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
            return Err(last("mremap"));
        }
        self.len = 0; // the region, not this temporary owner, now owns the range
        Ok(())
    }

    pub fn populate_zero(&self) -> io::Result<()> {
        // Only freshly created anonymous mappings may call this, before UFFD
        // registration or exposure. Linux installs its shared zero page; no
        // content scan or private data-page allocation is involved.
        called();
        if unsafe { libc::madvise(self.addr, self.len, libc::MADV_POPULATE_READ) } != 0 {
            return Err(last("madvise"));
        }
        Ok(())
    }
}

impl Drop for Mapping {
    fn drop(&mut self) {
        if self.len != 0 {
            // No external users may remain when the owning session is dropped.
            called();
            unsafe {
                libc::munmap(self.addr, self.len);
            }
        }
    }
}

#[inline]
fn called() {
    #[cfg(test)]
    calls::COUNT.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
}

/// Counts the kernel calls this module makes, so a test can say what one
/// mapping command costs the client. It is compiled into test builds only:
/// nothing a session does may depend on it.
#[cfg(test)]
pub(crate) mod calls {
    use std::sync::atomic::{AtomicU64, Ordering};

    pub(super) static COUNT: AtomicU64 = AtomicU64::new(0);

    /// How many kernel calls this module has made.
    pub fn taken() -> u64 {
        COUNT.load(Ordering::SeqCst)
    }
}
