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
    let mapping = Mapping::new(PAGE_SIZE, Some((&backing, 0))).unwrap();
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
            let region = Frame::read(&mut socket).unwrap();
            assert_eq!((region.kind, region.len), (wire::REGION, PAGE_SIZE as u64));
            Frame {
                kind: wire::ATTACH,
                id: wire::VERSION,
                offset: PAGE_SIZE as u64,
                len: PAGE_SIZE as u64,
                backing: BACKING_HUGETLB,
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
            RegionSpec {
                kind: RegionKind::Ram,
                len: PAGE_SIZE,
            },
        )
        .unwrap();
        let region = session.region();
        // Name the owned anonymous range so an unrelated allocation reusing its
        // address after drop cannot be mistaken for a leaked mapping.
        let marker = c"sproutfs-session-lifetime-test";
        let result = unsafe {
            libc::prctl(
                libc::PR_SET_VMA,
                libc::PR_SET_VMA_ANON_NAME,
                region.address,
                region.len,
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
fn invalid_region_specs_are_rejected_before_kernel_or_socket_access() {
    let page = PAGE_SIZE;
    let valid = RegionSpec {
        kind: RegionKind::Ram,
        len: page,
    };
    for spec in [
        RegionSpec { len: 0, ..valid },
        RegionSpec {
            len: page - 1,
            ..valid
        },
        RegionSpec {
            len: page + 1,
            ..valid
        },
        RegionSpec {
            kind: RegionKind::Pmem,
            len: 1,
        },
    ] {
        let error = Session::connect("/nonexistent-sproutfs-unit-test.sock", spec)
            .err()
            .expect("an invalid region was accepted");
        assert_eq!(
            error.kind(),
            io::ErrorKind::InvalidInput,
            "{spec:?}: {error}"
        );
    }
}
