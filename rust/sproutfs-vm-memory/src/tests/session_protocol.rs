use crate::{
    BACKING_HUGETLB, BACKING_MEMFD, MAX_PAGE_SIZE, MIN_PAGE_SIZE, RegionKind, RegionSpec, Session,
    wire::{self, Frame},
};
use std::io;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::os::unix::net::{UnixListener, UnixStream};
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, Instant};

#[path = "session_mapping.rs"]
mod mapping;

/// Most of these tests are about the protocol rather than about the geometry,
/// so they run at a PMEM session's page; the ones that are about the geometry
/// name both explicitly.
const PAGE_SIZE: usize = MAX_PAGE_SIZE;

struct Peer {
    directory: std::path::PathBuf,
    listener: UnixListener,
}

impl Peer {
    fn new() -> Self {
        static NEXT: AtomicU64 = AtomicU64::new(0);
        let directory = std::env::temp_dir().join(format!(
            "sproutfs-protocol-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        std::fs::create_dir(&directory).unwrap();
        let listener = UnixListener::bind(directory.join("control.sock")).unwrap();
        listener.set_nonblocking(true).unwrap();
        Self {
            directory,
            listener,
        }
    }

    fn accept(&self, spec: RegionSpec) -> (UnixStream, OwnedFd) {
        let deadline = Instant::now() + Duration::from_secs(5);
        let mut socket = loop {
            match self.listener.accept() {
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
        let (hello, uffd) = Frame::receive_fd(&mut socket).unwrap();
        assert_eq!(
            hello,
            Frame {
                kind: wire::HELLO,
                id: wire::VERSION,
                ..Frame::default()
            }
        );
        let region = Frame::read(&mut socket).unwrap();
        assert_ne!(region.offset, 0);
        // The region is reserved before the page is known, so it is reserved at
        // the largest page this transport maps, which is aligned for both.
        assert_eq!(region.offset % MAX_PAGE_SIZE as u64, 0);
        assert_eq!(
            region,
            Frame {
                kind: wire::REGION,
                offset: region.offset,
                len: spec.len as u64,
                flags: spec.kind as u64,
                ..Frame::default()
            }
        );
        (socket, uffd)
    }
}

impl Drop for Peer {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(self.directory.join("control.sock"));
        let _ = std::fs::remove_dir(&self.directory);
    }
}

/// An arena of one page of the given geometry: the HugeTLB pool's memory for a
/// 2 MiB page and an ordinary memfd for a 4 KiB one, which is exactly what the
/// attachment claims and what the client checks the descriptor against.
fn arena(page_size: usize) -> OwnedFd {
    let mut flags = libc::MFD_CLOEXEC;
    if page_size == MAX_PAGE_SIZE {
        flags |= libc::MFD_HUGETLB | libc::MFD_HUGE_2MB;
    }
    let raw = unsafe { libc::memfd_create(c"sproutfs-protocol".as_ptr(), flags) };
    assert!(raw >= 0, "memfd: {}", io::Error::last_os_error());
    let backing = unsafe { OwnedFd::from_raw_fd(raw) };
    // Handshake validation never touches or reserves physical huge pages.
    assert_eq!(
        unsafe { libc::ftruncate(backing.as_raw_fd(), page_size as _) },
        0
    );
    backing
}

fn backing() -> OwnedFd {
    arena(PAGE_SIZE)
}

/// The attachment a well-behaved pager of this page sends: the page, the arena,
/// and the memory that arena is made of.
fn attachment_for(page_size: usize) -> Frame {
    Frame {
        kind: wire::ATTACH,
        id: wire::VERSION,
        offset: page_size as u64,
        len: page_size as u64,
        backing: if page_size == MAX_PAGE_SIZE {
            BACKING_HUGETLB
        } else {
            BACKING_MEMFD
        },
        ..Frame::default()
    }
}

fn attachment() -> Frame {
    attachment_for(PAGE_SIZE)
}

fn ready() -> Frame {
    Frame {
        kind: wire::READY,
        id: 1,
        ..Frame::default()
    }
}

// Exercise the real connect boundary. A rejected handshake must produce an
// error even though the peer offers a complete path to successful readiness.
fn handshake(attach: Frame, ready: Frame, spec: RegionSpec) -> io::Result<Session> {
    let backing = backing();
    handshake_with_backing(attach, ready, spec, &backing)
}

fn handshake_with_backing(
    attach: Frame,
    ready: Frame,
    spec: RegionSpec,
    backing: &OwnedFd,
) -> io::Result<Session> {
    let peer = Peer::new();
    std::thread::scope(|scope| {
        let server = scope.spawn(|| {
            let (mut socket, _uffd) = peer.accept(spec);
            attach.send_fd(&mut socket, backing).unwrap();
            // Rejection can close the socket before READY is written.
            if ready.write(&mut socket).is_ok() {
                Frame::read(&mut socket).ok()
            } else {
                None
            }
        });
        let result = Session::connect(peer.directory.join("control.sock"), spec);
        let acknowledged = server.join().unwrap();
        if result.is_ok() {
            assert_eq!(
                acknowledged,
                Some(Frame {
                    kind: wire::ACK,
                    id: ready.id,
                    ..Frame::default()
                })
            );
        }
        result
    })
}

#[test]
#[ignore = "requires native HugeTLB/UFFD support and permission to create kernel-mode UFFD"]
fn attachment_rejects_invalid_fields_before_exposing_the_region() {
    let spec = RegionSpec {
        kind: RegionKind::Ram,
        len: PAGE_SIZE,
    };
    let valid = attachment();
    for invalid in [
        Frame {
            kind: wire::HELLO,
            ..valid
        },
        Frame {
            id: wire::VERSION + 1,
            ..valid
        },
        Frame { len: 0, ..valid },
        Frame {
            len: 2 * PAGE_SIZE as u64,
            ..valid
        },
        Frame {
            len: PAGE_SIZE as u64 - 1,
            ..valid
        },
        Frame {
            len: u64::MAX,
            ..valid
        },
        // The geometry itself: a page this transport does not map, a page the
        // stated arena kind is not the memory of, and an unknown arena kind.
        Frame { offset: 1, ..valid },
        Frame {
            offset: 64 << 10,
            ..valid
        },
        Frame {
            offset: MIN_PAGE_SIZE as u64,
            ..valid
        },
        Frame {
            backing: BACKING_MEMFD,
            ..valid
        },
        Frame {
            backing: 0,
            ..valid
        },
        Frame {
            backing: 3,
            ..valid
        },
        Frame {
            generation: 1,
            ..valid
        },
    ] {
        let error = handshake(invalid, ready(), spec)
            .err()
            .expect("invalid attachment accepted");
        assert_eq!(
            error.kind(),
            io::ErrorKind::InvalidData,
            "{invalid:?}: {error}"
        );
    }
}

#[test]
#[ignore = "requires native HugeTLB/UFFD support and permission to create kernel-mode UFFD"]
fn readiness_rejects_zero_ids_and_nonzero_reserved_fields() {
    let spec = RegionSpec {
        kind: RegionKind::Pmem,
        len: PAGE_SIZE,
    };
    let valid = ready();
    for invalid in [
        Frame { id: 0, ..valid },
        Frame { offset: 1, ..valid },
        Frame { len: 1, ..valid },
        Frame {
            backing: 1,
            ..valid
        },
        Frame {
            generation: 1,
            ..valid
        },
        Frame { flags: 1, ..valid },
    ] {
        let error = handshake(attachment(), invalid, spec)
            .err()
            .expect("invalid readiness accepted");
        assert_eq!(
            error.kind(),
            io::ErrorKind::InvalidData,
            "{invalid:?}: {error}"
        );
    }
}

// A session maps exactly one region, whichever kind it is, and exposes it at a
// page-aligned address of its own.
#[test]
#[ignore = "requires native HugeTLB/UFFD support and permission to create kernel-mode UFFD"]
fn a_handshake_maps_the_one_region_it_asked_for() {
    for kind in [RegionKind::Ram, RegionKind::Pmem] {
        let spec = RegionSpec {
            kind,
            len: 2 * PAGE_SIZE,
        };
        let session = handshake(attachment(), ready(), spec).unwrap();
        let region = session.region();
        assert_eq!((region.kind, region.len), (spec.kind, spec.len));
        assert_ne!(region.address, 0);
        assert_eq!(region.address % PAGE_SIZE, 0);
        assert_eq!(session.page_size(), PAGE_SIZE);
    }
}

// The page is the session's, not the library's: a RAM session runs 4 KiB over
// an ordinary memfd and a PMEM session runs 2 MiB over the pool, and a client
// that reserved its region before either was stated serves whichever it is
// given.
#[test]
#[ignore = "requires native HugeTLB/UFFD support and permission to create kernel-mode UFFD"]
fn a_session_runs_the_page_its_attachment_states() {
    for (page_size, kind) in [
        (MIN_PAGE_SIZE, RegionKind::Ram),
        (MAX_PAGE_SIZE, RegionKind::Pmem),
    ] {
        let spec = RegionSpec {
            kind,
            len: 4 * page_size,
        };
        let arena = arena(page_size);
        let attach = Frame {
            len: 4 * page_size as u64,
            ..attachment_for(page_size)
        };
        assert_eq!(
            unsafe { libc::ftruncate(arena.as_raw_fd(), 4 * page_size as libc::off_t) },
            0
        );
        let session = handshake_with_backing(attach, ready(), spec, &arena).unwrap();
        assert_eq!(session.page_size(), page_size);
        let region = session.region();
        assert_eq!(region.len, spec.len);
        assert_eq!(region.address % MAX_PAGE_SIZE, 0);
    }
}

// A geometry the two ends do not agree on is refused before the region is
// exposed: a region that is not whole pages of the page the session states, and
// an arena that is not the memory that page is made of.
#[test]
#[ignore = "requires native HugeTLB/UFFD support and permission to create kernel-mode UFFD"]
fn a_mismatched_geometry_is_refused_before_the_region_is_exposed() {
    // Whole 4 KiB pages, but not whole 2 MiB ones.
    let ragged = RegionSpec {
        kind: RegionKind::Pmem,
        len: MAX_PAGE_SIZE + MIN_PAGE_SIZE,
    };
    let error = handshake(attachment(), ready(), ragged)
        .err()
        .expect("a region that is not whole pages of its session was accepted");
    assert_eq!(error.kind(), io::ErrorKind::InvalidData, "{error}");
    assert!(
        error.to_string().contains("not whole"),
        "the refusal must say what it refused: {error}"
    );

    // A 4 KiB session whose arena is the HugeTLB pool, and a 2 MiB session
    // whose arena is ordinary memory. The attachment is well formed either
    // way; it is the descriptor that does not match what it claims.
    for (stated, attached) in [
        (MIN_PAGE_SIZE, MAX_PAGE_SIZE),
        (MAX_PAGE_SIZE, MIN_PAGE_SIZE),
    ] {
        let spec = RegionSpec {
            kind: RegionKind::Ram,
            len: MAX_PAGE_SIZE,
        };
        let wrong = arena(attached);
        assert_eq!(
            unsafe { libc::ftruncate(wrong.as_raw_fd(), MAX_PAGE_SIZE as libc::off_t) },
            0
        );
        let attach = Frame {
            len: MAX_PAGE_SIZE as u64,
            ..attachment_for(stated)
        };
        let error = handshake_with_backing(attach, ready(), spec, &wrong)
            .err()
            .expect("an arena that is not the memory of its page was accepted");
        assert_eq!(error.kind(), io::ErrorKind::InvalidData, "{error}");
    }
}

// mremap waits for its UFFD event to be consumed. The real pager drains this
// descriptor independently of control ACKs; the protocol peer must do so too.
struct RemapEvents {
    stop: std::sync::Arc<std::sync::atomic::AtomicBool>,
    reader: Option<std::thread::JoinHandle<io::Result<()>>>,
}

impl RemapEvents {
    fn new(uffd: OwnedFd) -> Self {
        let stop = std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false));
        let stopped = stop.clone();
        let reader = std::thread::spawn(move || {
            while !stopped.load(Ordering::Acquire) {
                let mut message = [0u64; 4];
                let n = unsafe { libc::read(uffd.as_raw_fd(), message.as_mut_ptr().cast(), 32) };
                if n < 0 {
                    let error = io::Error::last_os_error();
                    if error.kind() == io::ErrorKind::WouldBlock {
                        std::thread::sleep(Duration::from_millis(1));
                        continue;
                    }
                    if error.kind() == io::ErrorKind::Interrupted {
                        continue;
                    }
                    return Err(error);
                }
                if n != 32
                    || message[0].to_ne_bytes()[0] != linux_raw_sys::general::UFFD_EVENT_REMAP as u8
                {
                    return Err(io::Error::other(
                        "unexpected UFFD event without a memory access",
                    ));
                }
            }
            Ok(())
        });
        Self {
            stop,
            reader: Some(reader),
        }
    }
}

impl Drop for RemapEvents {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Release);
        let result = self.reader.take().unwrap().join();
        if !std::thread::panicking() {
            result.unwrap().unwrap();
        }
    }
}

fn serve(script: impl FnOnce(&mut UnixStream) + Send) -> io::Result<()> {
    serve_region(
        RegionSpec {
            kind: RegionKind::Ram,
            len: 2 * PAGE_SIZE,
        },
        script,
    )
}

fn serve_region(spec: RegionSpec, script: impl FnOnce(&mut UnixStream) + Send) -> io::Result<()> {
    serve_attachment(spec, attachment(), script)
}

fn serve_attachment(
    spec: RegionSpec,
    attach: Frame,
    script: impl FnOnce(&mut UnixStream) + Send,
) -> io::Result<()> {
    let peer = Peer::new();
    let backing = backing();
    std::thread::scope(|scope| {
        let server = scope.spawn(|| {
            let (mut socket, uffd) = peer.accept(spec);
            let _events = RemapEvents::new(uffd);
            attach.send_fd(&mut socket, &backing).unwrap();
            ready().write(&mut socket).unwrap();
            assert_eq!(
                Frame::read(&mut socket).unwrap(),
                Frame {
                    kind: wire::ACK,
                    id: 1,
                    ..Frame::default()
                }
            );
            script(&mut socket);
        });
        let result = Session::connect(peer.directory.join("control.sock"), spec)
            .and_then(|mut session| session.run());
        server.join().unwrap();
        result
    })
}

fn acknowledge(socket: &mut UnixStream, command: Frame, errno: i32) {
    command.write(socket).unwrap();
    assert_eq!(
        Frame::read(socket).unwrap(),
        Frame {
            kind: wire::ACK,
            id: command.id,
            generation: command.generation,
            flags: errno as u64,
            ..Frame::default()
        },
        "command {command:?}"
    );
}

fn stop(socket: &mut UnixStream) {
    acknowledge(
        socket,
        Frame {
            kind: wire::STOP,
            id: 100,
            ..Frame::default()
        },
        0,
    );
}

#[test]
#[ignore = "requires native HugeTLB/UFFD support and permission to create kernel-mode UFFD"]
fn mapping_commands_reject_invalid_ranges_flags_and_stale_ids() {
    serve(|socket| {
        let revoke = Frame {
            kind: wire::REVOKE,
            id: 2,
            len: PAGE_SIZE as u64,
            generation: 1,
            ..Frame::default()
        };
        let map = Frame {
            kind: wire::MAP,
            ..revoke
        };
        let zero = Frame {
            kind: wire::MAP_ZERO,
            flags: wire::SHARED,
            ..revoke
        };
        for invalid in [
            Frame {
                kind: u64::MAX,
                ..revoke
            },
            Frame { len: 0, ..revoke },
            Frame {
                offset: 1,
                ..revoke
            },
            Frame {
                offset: 4096,
                ..revoke
            },
            Frame {
                offset: 1 << 20,
                ..revoke
            },
            Frame { len: 4096, ..map },
            Frame {
                len: 1 << 20,
                ..map
            },
            Frame {
                len: PAGE_SIZE as u64 - 1,
                ..revoke
            },
            Frame {
                offset: 2 * PAGE_SIZE as u64,
                ..revoke
            },
            Frame {
                offset: u64::MAX - PAGE_SIZE as u64 + 1,
                ..revoke
            },
            Frame { flags: 2, ..map },
            Frame { flags: 1, ..revoke },
            Frame {
                backing: PAGE_SIZE as u64,
                ..revoke
            },
            Frame { flags: 0, ..zero },
            Frame {
                backing: PAGE_SIZE as u64,
                ..zero
            },
            Frame { backing: 1, ..map },
            Frame {
                backing: 4096,
                ..map
            },
            Frame {
                backing: 1 << 20,
                ..map
            },
            Frame {
                backing: PAGE_SIZE as u64,
                ..map
            },
            Frame {
                backing: u64::MAX - PAGE_SIZE as u64 + 1,
                ..map
            },
        ] {
            acknowledge(socket, invalid, libc::EINVAL);
        }
        for invalid in [
            Frame { id: 0, ..revoke },
            Frame { id: 1, ..revoke },
            Frame {
                generation: 0,
                ..revoke
            },
            Frame {
                generation: 2,
                ..revoke
            },
        ] {
            acknowledge(socket, invalid, libc::ESTALE);
        }
        // Rejections leave both the command ID and generation available. An
        // exact immediate retry is acknowledged without advancing generation.
        acknowledge(socket, revoke, 0);
        acknowledge(socket, revoke, 0);
        acknowledge(socket, Frame { id: 3, ..revoke }, libc::ESTALE);
        acknowledge(
            socket,
            Frame {
                id: 3,
                generation: 2,
                ..revoke
            },
            0,
        );
        stop(socket);
    })
    .unwrap();
}

fn batch(socket: &mut UnixStream, command: Frame, runs: &[Frame]) -> Option<Frame> {
    command.write(socket).unwrap();
    for run in runs {
        if run.write(socket).is_err() {
            break;
        }
    }
    Frame::read(socket).ok()
}

#[test]
#[ignore = "requires native HugeTLB/UFFD support and permission to create kernel-mode UFFD"]
fn batches_reject_invalid_headers_before_waiting_for_runs() {
    let valid = Frame {
        kind: wire::MAP_BATCH,
        id: 2,
        len: 1,
        ..Frame::default()
    };
    for invalid in [
        Frame { len: 0, ..valid },
        Frame { len: 1025, ..valid },
        Frame { offset: 1, ..valid },
        Frame {
            backing: 1,
            ..valid
        },
        Frame {
            generation: 1,
            ..valid
        },
        Frame { flags: 1, ..valid },
    ] {
        let result = serve(|socket| {
            invalid.write(socket).unwrap();
            // A malformed header is self-contained: reject it before reading
            // a body. Half-close makes an erroneous read return EOF, not hang.
            socket.shutdown(std::net::Shutdown::Write).unwrap();
            assert!(
                Frame::read(socket).is_err(),
                "invalid batch acknowledged: {invalid:?}"
            );
        });
        let error = result.expect_err("invalid batch accepted");
        assert_eq!(
            error.kind(),
            io::ErrorKind::InvalidData,
            "{invalid:?}: {error}"
        );
    }
}

#[test]
#[ignore = "requires native HugeTLB/UFFD support and permission to create kernel-mode UFFD"]
fn batches_require_matching_ids_and_ordered_disjoint_ranges() {
    let run = Frame {
        kind: wire::REVOKE,
        id: 2,
        len: PAGE_SIZE as u64,
        generation: 1,
        ..Frame::default()
    };
    let spec = RegionSpec {
        kind: RegionKind::Ram,
        len: 4 * PAGE_SIZE,
    };
    for invalid in [
        vec![Frame { id: 3, ..run }],
        vec![
            Frame {
                offset: PAGE_SIZE as u64,
                ..run
            },
            run,
        ],
        vec![run, run],
        vec![
            Frame {
                len: 2 * PAGE_SIZE as u64,
                ..run
            },
            Frame {
                offset: PAGE_SIZE as u64,
                ..run
            },
        ],
    ] {
        let result = serve_region(spec, |socket| {
            let command = Frame {
                kind: wire::MAP_BATCH,
                id: 2,
                len: invalid.len() as u64,
                ..Frame::default()
            };
            if batch(socket, command, &invalid).is_some() {
                // Allow an incorrectly accepted batch to finish normally; the
                // caller must still assert rejection, not merely disconnection.
                stop(socket);
            }
        });
        let error = result.expect_err("invalid batch accepted");
        assert_eq!(
            error.kind(),
            io::ErrorKind::InvalidData,
            "{invalid:?}: {error}"
        );
    }
    serve_region(spec, |socket| {
        // The first two runs are adjacent and merge into one mapping operation;
        // the third is separated by an untouched page and stays its own.
        let runs = [
            run,
            Frame {
                offset: PAGE_SIZE as u64,
                ..run
            },
            Frame {
                offset: 3 * PAGE_SIZE as u64,
                ..run
            },
        ];
        let command = Frame {
            kind: wire::MAP_BATCH,
            id: 2,
            len: 3,
            ..Frame::default()
        };
        assert_eq!(
            batch(socket, command, &runs),
            Some(Frame {
                kind: wire::ACK,
                id: 2,
                ..Frame::default()
            })
        );
        // Each original range advances even when adjacent runs merge into one
        // mapping operation; the following generation must be accepted.
        for (index, previous) in runs.into_iter().enumerate() {
            acknowledge(
                socket,
                Frame {
                    id: 3 + index as u64,
                    generation: 2,
                    ..previous
                },
                0,
            );
        }
        stop(socket);
    })
    .unwrap();
}

#[test]
#[ignore = "requires native HugeTLB/UFFD support, kernel-mode UFFD permission, and 4 GiB virtual address space"]
fn batch_accepts_the_protocol_limit_of_1024_runs() {
    // The ranges reserve virtual addresses only. Contiguous trap replacements
    // merge, so this neither allocates huge pages nor touches 2 GiB of memory.
    let spec = RegionSpec {
        kind: RegionKind::Ram,
        len: 1024 * PAGE_SIZE,
    };
    serve_region(spec, |socket| {
        let runs: Vec<_> = (0..1024)
            .map(|page| Frame {
                kind: wire::REVOKE,
                id: 2,
                offset: page * PAGE_SIZE as u64,
                len: PAGE_SIZE as u64,
                generation: 1,
                ..Frame::default()
            })
            .collect();
        assert_eq!(
            batch(
                socket,
                Frame {
                    kind: wire::MAP_BATCH,
                    id: 2,
                    len: 1024,
                    ..Frame::default()
                },
                &runs
            ),
            Some(Frame {
                kind: wire::ACK,
                id: 2,
                ..Frame::default()
            })
        );
        // Check generation advancement at both ends of the accepted batch.
        for (id, page) in [(3, 0), (4, 1023)] {
            acknowledge(
                socket,
                Frame {
                    kind: wire::REVOKE,
                    id,
                    offset: page * PAGE_SIZE as u64,
                    len: PAGE_SIZE as u64,
                    generation: 2,
                    ..Frame::default()
                },
                0,
            );
        }
        stop(socket);
    })
    .unwrap();
}

#[test]
#[ignore = "requires native HugeTLB/UFFD support and permission to create kernel-mode UFFD"]
fn batch_budget_rejection_acknowledges_the_command_and_errno() {
    let spec = RegionSpec {
        kind: RegionKind::Ram,
        len: 64 * PAGE_SIZE,
    };
    serve_attachment(
        spec,
        Frame {
            flags: 128,
            ..attachment()
        },
        |socket| {
            // Separate every replacement with an untouched page, so the batch
            // cannot merge into a single cheap mapping operation. Only the
            // replacements that install a mapping are admitted against the
            // budget, so those are what a rejected batch is made of.
            let runs: Vec<_> = (0..32)
                .map(|page| Frame {
                    kind: wire::MAP_ZERO,
                    id: 2,
                    offset: 2 * page * PAGE_SIZE as u64,
                    len: PAGE_SIZE as u64,
                    generation: 1,
                    flags: wire::SHARED,
                    ..Frame::default()
                })
                .collect();
            let ack = batch(
                socket,
                Frame {
                    kind: wire::MAP_BATCH,
                    id: 2,
                    len: 32,
                    ..Frame::default()
                },
                &runs,
            );
            assert_eq!(
                ack,
                Some(Frame {
                    kind: wire::ACK,
                    id: 2,
                    flags: libc::ENOSPC as u64,
                    ..Frame::default()
                })
            );
            stop(socket);
        },
    )
    .unwrap();
}
