//! Compare first reads of resident memfd pages with and without populated PTEs.
#![cfg_attr(not(target_os = "linux"), allow(dead_code))]

#[cfg(not(target_os = "linux"))]
fn main() {
    eprintln!("the PTE latency measurement needs Linux memfd and rusage fault counts");
    std::process::exit(1);
}

#[cfg(target_os = "linux")]
use std::io;
#[cfg(target_os = "linux")]
use std::time::Instant;

#[cfg(target_os = "linux")]
fn faults() -> i64 {
    let mut usage = unsafe { std::mem::zeroed::<libc::rusage>() };
    assert_eq!(unsafe { libc::getrusage(libc::RUSAGE_SELF, &mut usage) }, 0);
    usage.ru_minflt
}

#[cfg(target_os = "linux")]
fn main() -> io::Result<()> {
    let page = unsafe { libc::sysconf(libc::_SC_PAGESIZE) } as usize;
    let pages = 4096;
    let len = pages * page;
    let fd = unsafe { libc::memfd_create(c"pte-latency".as_ptr(), libc::MFD_CLOEXEC) };
    if fd < 0 || unsafe { libc::ftruncate(fd, len as libc::off_t) } != 0 {
        return Err(io::Error::last_os_error());
    }
    let map = || {
        let p = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                len,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_SHARED,
                fd,
                0,
            )
        };
        assert_ne!(p, libc::MAP_FAILED);
        p
    };
    let backing = map();
    for i in 0..pages {
        unsafe { backing.cast::<u8>().add(i * page).write_volatile(7) };
    }
    for eager in [false, true] {
        let mut elapsed = 0;
        let mut minor = 0;
        let mut populate_ns = 0;
        for _ in 0..16 {
            let p = map();
            if eager {
                let start = Instant::now();
                assert_eq!(
                    unsafe { libc::madvise(p, len, libc::MADV_POPULATE_READ) },
                    0
                );
                populate_ns += start.elapsed().as_nanos();
            }
            let before = faults();
            let start = Instant::now();
            for i in 0..pages {
                assert_eq!(unsafe { p.cast::<u8>().add(i * page).read_volatile() }, 7);
            }
            elapsed += start.elapsed().as_nanos();
            minor += faults() - before;
            assert_eq!(unsafe { libc::munmap(p, len) }, 0);
        }
        println!(
            "pte eager={eager} pages={pages} repeats=16 first_touch_ns={} minor_faults={} populate_ns={}",
            elapsed / 16,
            minor / 16,
            populate_ns / 16
        );
    }
    assert_eq!(unsafe { libc::munmap(backing, len) }, 0);
    assert_eq!(unsafe { libc::close(fd) }, 0);
    Ok(())
}
