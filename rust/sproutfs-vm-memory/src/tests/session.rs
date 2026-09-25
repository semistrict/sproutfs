use super::*;
use std::os::fd::AsRawFd;

#[path = "session_protocol.rs"]
mod protocol;

/// These lifetime tests are about mappings and descriptors rather than about
/// geometry, so they run at the larger of the two pages; the geometry itself is
/// exercised in session_protocol.rs.
const PAGE_SIZE: usize = MAX_PAGE_SIZE;

#[test]
fn mapping_drop_releases_its_backing() {
    use std::os::fd::FromRawFd;

    let raw = unsafe {
        libc::memfd_create(
            c"sproutfs-mapping-lifetime-test".as_ptr(),
            libc::MFD_CLOEXEC,
        )
    };
    assert!(raw >= 0, "memfd: {}", io::Error::last_os_error());
    let backing = unsafe { OwnedFd::from_raw_fd(raw) };
    assert_eq!(
        unsafe { libc::ftruncate(backing.as_raw_fd(), PAGE_SIZE as _) },
        0
    );
    let mapping = Mapping::new(
        PAGE_SIZE,
        Some(FileRange {
            fd: &backing,
            at: 0,
            writable: true,
        }),
    )
    .unwrap();
    let marker = "/memfd:sproutfs-mapping-lifetime-test";
    assert!(
        std::fs::read_to_string("/proc/self/maps")
            .unwrap()
            .contains(marker)
    );
    drop(mapping);
    // Keep the descriptor alive. Other tests may reuse the freed address, but
    // only this mapping owns the named backing we are checking for.
    assert!(
        !std::fs::read_to_string("/proc/self/maps")
            .unwrap()
            .contains(marker)
    );
}

/// An ordinary memfd of the given pages and name, and a second descriptor of
/// it opened read-only through /proc/self/fd, as the pager opens every file but
/// the private one.
fn memfd_and_read_only(name: &std::ffi::CStr, pages: usize) -> (OwnedFd, OwnedFd) {
    use std::os::fd::FromRawFd;
    let raw = unsafe { libc::memfd_create(name.as_ptr(), libc::MFD_CLOEXEC) };
    assert!(raw >= 0, "memfd: {}", io::Error::last_os_error());
    let file = unsafe { OwnedFd::from_raw_fd(raw) };
    assert_eq!(
        unsafe { libc::ftruncate(file.as_raw_fd(), (pages * MIN_PAGE_SIZE) as _) },
        0
    );
    let path = std::ffi::CString::new(format!("/proc/self/fd/{raw}")).unwrap();
    let raw = unsafe { libc::open(path.as_ptr(), libc::O_RDONLY | libc::O_CLOEXEC) };
    assert!(raw >= 0, "reopen: {}", io::Error::last_os_error());
    (file, unsafe { OwnedFd::from_raw_fd(raw) })
}

/// The permissions /proc/self/maps shows for the mapping at address.
fn permissions(address: usize) -> String {
    let maps = std::fs::read_to_string("/proc/self/maps").unwrap();
    for line in maps.lines() {
        let mut fields = line.split_whitespace();
        let (range, perms) = (fields.next().unwrap(), fields.next().unwrap());
        let (start, end) = range.split_once('-').unwrap();
        let (start, end) = (
            usize::from_str_radix(start, 16).unwrap(),
            usize::from_str_radix(end, 16).unwrap(),
        );
        if start <= address && address < end {
            return perms.to_owned();
        }
    }
    panic!("nothing is mapped at {address:#x}");
}

// The private file is mapped shared, so the guest's stores reach the pager's
// page. A read-only file is mapped private, because the kernel will not register
// a shared mapping of a read-only file with userfaultfd, and the kernel lets
// that private mapping be made from the read-only descriptor.
#[test]
fn a_read_only_file_is_mapped_private_and_the_private_file_shared() {
    let (file, read_only) = memfd_and_read_only(c"sproutfs-mapping-kinds-test", 2);
    let shared = Mapping::new(
        MIN_PAGE_SIZE,
        Some(FileRange {
            fd: &file,
            at: 0,
            writable: true,
        }),
    )
    .unwrap();
    let private = Mapping::new(
        MIN_PAGE_SIZE,
        Some(FileRange {
            fd: &read_only,
            at: MIN_PAGE_SIZE as u64,
            writable: false,
        }),
    )
    .unwrap();
    assert_eq!(permissions(shared.addr as usize), "rw-s");
    assert_eq!(permissions(private.addr as usize), "rw-p");
    // A shared mapping of the read-only descriptor is what the kernel refuses.
    let refused = Mapping::new(
        MIN_PAGE_SIZE,
        Some(FileRange {
            fd: &read_only,
            at: 0,
            writable: true,
        }),
    )
    .err()
    .expect("a writable shared mapping of a read-only descriptor");
    assert_eq!(refused.raw_os_error(), None);
    assert_eq!(refused.to_string(), "mmap: Permission denied (os error 13)");
}

// A file's descriptor must be what its FILE frame says: the memory the
// attachment names, at least the length stated, and open read-write for the
// private file and read-only for every other. The identity it reports is the
// file's, whichever descriptor of it was checked.
#[test]
fn a_file_is_checked_against_what_its_frame_states() {
    let (file, read_only) = memfd_and_read_only(c"sproutfs-file-check-test", 2);
    let len = 2 * MIN_PAGE_SIZE as u64;
    let writable = linux::check_file(&file, BACKING_MEMFD, len, true).unwrap();
    let readable = linux::check_file(&read_only, BACKING_MEMFD, len, false).unwrap();
    assert_eq!(writable, readable);
    assert_eq!(
        linux::check_file(&read_only, BACKING_MEMFD, MIN_PAGE_SIZE as u64, false).unwrap(),
        writable,
        "a file longer than stated is taken at the stated length"
    );
    let (other, _) = memfd_and_read_only(c"sproutfs-file-check-other", 2);
    assert_ne!(
        linux::check_file(&other, BACKING_MEMFD, len, true).unwrap(),
        writable
    );
    for (name, fd, kind, len, want_writable, error) in [
        (
            "a read-write descriptor stated read-only",
            &file,
            BACKING_MEMFD,
            len,
            false,
            "the file's descriptor is open with access mode 2, its frame said writable=false",
        ),
        (
            "a read-only descriptor stated read-write",
            &read_only,
            BACKING_MEMFD,
            len,
            true,
            "the file's descriptor is open with access mode 0, its frame said writable=true",
        ),
        (
            "a file shorter than stated",
            &read_only,
            BACKING_MEMFD,
            3 * MIN_PAGE_SIZE as u64,
            false,
            "the file has 8192 bytes of offsets, its frame said 12288",
        ),
        (
            "ordinary memory stated as the HugeTLB pool",
            &read_only,
            BACKING_HUGETLB,
            len,
            false,
            "arena kind 1 is a filesystem of type 0x958458f6 with 2097152-byte blocks, \
             the descriptor is type 0x1021994 with 4096-byte blocks",
        ),
    ] {
        let err = linux::check_file(fd, kind, len, want_writable).expect_err(name);
        assert_eq!(
            (err.kind(), err.to_string()),
            (io::ErrorKind::InvalidData, error.to_owned()),
            "{name}"
        );
    }
}

#[test]
#[ignore = "requires native HugeTLB/UFFD support and permission to create kernel-mode UFFD"]
fn session_drop_releases_mappings_and_closes_retained_controls() {
    use std::io::Read;
    use std::os::fd::FromRawFd;
    use std::os::unix::net::UnixListener;
    use std::sync::mpsc;
    use std::time::{Duration, Instant};

    struct SocketDirectory(std::path::PathBuf);
    impl Drop for SocketDirectory {
        fn drop(&mut self) {
            let _ = std::fs::remove_file(self.0.join("control.sock"));
            let _ = std::fs::remove_dir(&self.0);
        }
    }
    let directory = SocketDirectory(
        std::env::temp_dir().join(format!("sproutfs-session-lifecycle-{}", std::process::id())),
    );
    std::fs::create_dir(&directory.0).unwrap();
    let path = directory.0.join("control.sock");
    let listener = UnixListener::bind(&path).unwrap();
    listener.set_nonblocking(true).unwrap();
    let raw = unsafe {
        libc::memfd_create(
            c"sproutfs-lifecycle".as_ptr(),
            libc::MFD_CLOEXEC | libc::MFD_HUGETLB | libc::MFD_HUGE_2MB,
        )
    };
    assert!(raw >= 0, "memfd: {}", io::Error::last_os_error());
    let backing = unsafe { OwnedFd::from_raw_fd(raw) };
    // No physical huge pages are touched or reserved by this handshake.
    assert_eq!(
        unsafe { libc::ftruncate(backing.as_raw_fd(), PAGE_SIZE as _) },
        0
    );
    std::thread::scope(|scope| {
        let (sent, received) = mpsc::sync_channel(1);
        let peer = scope.spawn(move || {
            let deadline = Instant::now() + Duration::from_secs(5);
            let mut socket = loop {
                match listener.accept() {
                    Ok((socket, _)) => break socket,
                    Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                        assert!(Instant::now() < deadline, "session did not connect");
                        std::thread::sleep(Duration::from_millis(1));
                    }
                    Err(error) => panic!("accept: {error}"),
                }
            };
            socket
                .set_read_timeout(Some(Duration::from_secs(5)))
                .unwrap();
            socket
                .set_write_timeout(Some(Duration::from_secs(5)))
                .unwrap();
            let (hello, _uffd) = Frame::receive_fd(&mut socket).unwrap();
            assert_eq!((hello.kind, hello.id), (wire::HELLO, wire::VERSION));
            let memory_region = Frame::read(&mut socket).unwrap();
            assert_eq!(
                (memory_region.kind, memory_region.len),
                (wire::MEMORY_REGION, PAGE_SIZE as u64)
            );
            Frame {
                kind: wire::ATTACH,
                id: wire::VERSION,
                offset: PAGE_SIZE as u64,
                backing: BACKING_HUGETLB,
                ..Frame::default()
            }
            .write(&mut socket)
            .unwrap();
            Frame {
                kind: wire::FILE,
                id: wire::PRIVATE_FILE,
                len: PAGE_SIZE as u64,
                backing: BACKING_HUGETLB,
                flags: wire::FILE_WRITABLE,
                ..Frame::default()
            }
            .send_fd(&mut socket, &backing)
            .unwrap();
            Frame {
                kind: wire::READY,
                id: 1,
                ..Frame::default()
            }
            .write(&mut socket)
            .unwrap();
            let ready = Frame::read(&mut socket).unwrap();
            assert_eq!((ready.kind, ready.id), (wire::ACK, 1));
            let seal = Frame::read(&mut socket).unwrap();
            assert_eq!(seal.kind, wire::SEAL);
            sent.send(()).unwrap();
            let mut byte = [0];
            assert_eq!(
                socket.read(&mut byte).unwrap(),
                0,
                "session drop must shut down control"
            );
        });
        let session = Session::connect(
            &path,
            MemoryRegionSpec {
                kind: MemoryRegionKind::Ram,
                len: PAGE_SIZE,
            },
        )
        .unwrap();
        let memory_region = session.memory_region();
        // Name the owned anonymous range so an unrelated allocation reusing its
        // address after drop cannot be mistaken for a leaked mapping.
        let marker = c"sproutfs-session-lifetime-test";
        let result = unsafe {
            libc::prctl(
                libc::PR_SET_VMA,
                libc::PR_SET_VMA_ANON_NAME,
                memory_region.address,
                memory_region.len,
                marker.as_ptr(),
            )
        };
        assert_eq!(result, 0, "name mapping: {}", io::Error::last_os_error());
        assert!(
            std::fs::read_to_string("/proc/self/maps")
                .unwrap()
                .contains(marker.to_str().unwrap())
        );
        let control = session.control();
        let pending = control.start_seal().unwrap();
        received.recv_timeout(Duration::from_secs(5)).unwrap();
        drop(session);
        assert!(
            !std::fs::read_to_string("/proc/self/maps")
                .unwrap()
                .contains(marker.to_str().unwrap())
        );
        assert_eq!(
            pending
                .wait(Duration::from_secs(1))
                .unwrap_err()
                .raw_os_error(),
            Some(libc::EPIPE)
        );
        assert_eq!(
            control.start_seal().err().unwrap().kind(),
            io::ErrorKind::BrokenPipe
        );
        peer.join().unwrap();
    });
}

#[test]
fn invalid_memory_region_specs_are_rejected_before_kernel_or_socket_access() {
    let page = PAGE_SIZE;
    let valid = MemoryRegionSpec {
        kind: MemoryRegionKind::Ram,
        len: page,
    };
    for spec in [
        MemoryRegionSpec { len: 0, ..valid },
        MemoryRegionSpec {
            len: page - 1,
            ..valid
        },
        MemoryRegionSpec {
            len: page + 1,
            ..valid
        },
        MemoryRegionSpec {
            kind: MemoryRegionKind::Pmem,
            len: 1,
        },
    ] {
        let error = Session::connect("/nonexistent-sproutfs-unit-test.sock", spec)
            .err()
            .expect("an invalid memory region was accepted");
        assert_eq!(
            error.kind(),
            io::ErrorKind::InvalidInput,
            "{spec:?}: {error}"
        );
    }
}
