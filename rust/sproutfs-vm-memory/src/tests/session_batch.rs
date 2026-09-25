use super::*;

/// The arena offset of every mapping of the session's arena, in address order.
/// A span puts its runs in one reservation and moves them out front to back, so
/// this is what says each of them reached the memory region page it names.
fn arena_offsets() -> Vec<u64> {
    let mut offsets = Vec::new();
    for line in std::fs::read_to_string("/proc/self/maps").unwrap().lines() {
        if !line.contains("/memfd:sproutfs-protocol") {
            continue;
        }
        let mut fields = line.split_whitespace();
        let (_range, _perms) = (fields.next().unwrap(), fields.next().unwrap());
        offsets.push(u64::from_str_radix(fields.next().unwrap(), 16).unwrap());
    }
    offsets
}

/// One mapping command of a populate, or of a read-ahead window landing in
/// scattered arena slots: pages next to each other in the memory region, each from a
/// different part of the arena, so no two of them merge. This is the shape the
/// pager sends most of, and what it costs the client is counted here.
const RUNS: usize = 64;

/// What one such run costs: the arena mapping and its MADV_DONTFORK, the UFFD
/// registration, the write-protect, and the mremap that puts it in the memory region.
/// The first three of those are the span's rather than the run's; the mremap is
/// the run's own and cannot be anything else, because mremap moves one mapping
/// and two runs of a batch are never one.
const SPAN_CALLS: u64 = 8;
const RUN_CALLS: u64 = 2;

#[test]
#[ignore = "requires native UFFD support and permission to create kernel-mode UFFD"]
fn a_batch_of_scattered_runs_costs_one_span_and_one_mremap_a_run() {
    let page = MIN_PAGE_SIZE;
    let spec = MemoryRegionSpec {
        kind: MemoryRegionKind::Ram,
        len: RUNS * page,
    };
    let arena = arena(page);
    assert_eq!(
        unsafe { libc::ftruncate(arena.as_raw_fd(), (RUNS * page) as libc::off_t) },
        0
    );
    let attach = Frame {
        len: (RUNS * page) as u64,
        ..attachment_for(page)
    };
    serve_backing(spec, attach, &arena, |socket| {
        // Run k puts arena page RUNS-1-k at memory region page k: adjacent in the
        // memory region, never adjacent in the arena.
        let runs: Vec<_> = (0..RUNS)
            .map(|k| Frame {
                kind: wire::MAP,
                id: 2,
                offset: (k * page) as u64,
                len: page as u64,
                backing: ((RUNS - 1 - k) * page) as u64,
                flags: wire::SHARED,
                generation: 1,
            })
            .collect();
        let before = crate::linux::calls::taken();
        assert_eq!(
            batch(
                socket,
                Frame {
                    kind: wire::MAP_BATCH,
                    id: 2,
                    len: RUNS as u64,
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
        assert_eq!(
            crate::linux::calls::taken() - before,
            SPAN_CALLS + RUN_CALLS * RUNS as u64,
            "kernel calls for one batch of {RUNS} scattered runs"
        );
        // Every run of the span reached the memory region page it names, in the order
        // the batch gave them: memory region page k holds arena page RUNS-1-k.
        let want: Vec<u64> = (0..RUNS).map(|k| ((RUNS - 1 - k) * page) as u64).collect();
        assert_eq!(
            arena_offsets(),
            want,
            "the arena offsets the memory region maps"
        );
        stop(socket);
    })
    .unwrap();
}

/// An arena that will not be mapped writable: an ordinary memfd sealed against
/// writing, which the kernel refuses every shared writable mmap of. It is how a
/// test makes a span's own kernel call fail over a wire the pager could not
/// have sent any differently.
fn sealed_arena(len: usize) -> OwnedFd {
    let raw = unsafe {
        libc::memfd_create(
            c"sproutfs-protocol".as_ptr(),
            libc::MFD_CLOEXEC | libc::MFD_ALLOW_SEALING,
        )
    };
    assert!(raw >= 0, "memfd: {}", io::Error::last_os_error());
    let arena = unsafe { OwnedFd::from_raw_fd(raw) };
    assert_eq!(
        unsafe { libc::ftruncate(arena.as_raw_fd(), len as libc::off_t) },
        0
    );
    assert_eq!(
        unsafe { libc::fcntl(arena.as_raw_fd(), libc::F_ADD_SEALS, libc::F_SEAL_WRITE) },
        0,
        "seal: {}",
        io::Error::last_os_error()
    );
    arena
}

/// A mutation that fails sends no acknowledgement: the session ends and the
/// pager sees only that the connection went, so the one thing that can say what
/// happened is the error this client's embedder prints. It has to name the
/// kernel call and the run, because a span is one reservation holding many runs
/// and five different calls, and an errno alone tells a reader neither which of
/// them refused nor which page of the memory region it was about.
#[test]
#[ignore = "requires native UFFD support and permission to create kernel-mode UFFD"]
fn a_span_the_kernel_refuses_names_the_call_and_the_run() {
    let page = MIN_PAGE_SIZE;
    let spec = MemoryRegionSpec {
        kind: MemoryRegionKind::Ram,
        len: RUNS * page,
    };
    let arena = sealed_arena(RUNS * page);
    let attach = Frame {
        len: (RUNS * page) as u64,
        ..attachment_for(page)
    };
    let error = serve_backing(spec, attach, &arena, |socket| {
        let runs: Vec<_> = (0..RUNS)
            .map(|k| Frame {
                kind: wire::MAP,
                id: 2,
                offset: (k * page) as u64,
                len: page as u64,
                backing: ((RUNS - 1 - k) * page) as u64,
                flags: wire::SHARED,
                generation: 1,
            })
            .collect();
        assert_eq!(
            batch(
                socket,
                Frame {
                    kind: wire::MAP_BATCH,
                    id: 2,
                    len: RUNS as u64,
                    ..Frame::default()
                },
                &runs
            ),
            None,
            "a span the kernel refused was acknowledged"
        );
    })
    .expect_err("a span the kernel refused ran to completion");
    let said = error.to_string();
    for want in ["mmap", &format!("run 0 of {RUNS}")] {
        assert!(said.contains(want), "{said:?} does not name {want}");
    }
}

/// A run of a batch the client refuses ends the session without an
/// acknowledgement too, and one batch carries up to 1024 runs: the failure has
/// to say which of them it was and what about it was refused, or the pager is
/// left with one errno for a thousand frames.
#[test]
#[ignore = "requires native UFFD support and permission to create kernel-mode UFFD"]
fn a_refused_run_of_a_batch_names_itself() {
    let page = MIN_PAGE_SIZE;
    let spec = MemoryRegionSpec {
        kind: MemoryRegionKind::Ram,
        len: RUNS * page,
    };
    let arena = arena(page);
    assert_eq!(
        unsafe { libc::ftruncate(arena.as_raw_fd(), (RUNS * page) as libc::off_t) },
        0
    );
    let attach = Frame {
        len: (RUNS * page) as u64,
        ..attachment_for(page)
    };
    // Run 5 maps from one page past the end of the arena's offsets, which is
    // the frame this client refuses.
    let error = serve_backing(spec, attach, &arena, |socket| {
        let runs: Vec<_> = (0..RUNS)
            .map(|k| Frame {
                kind: wire::MAP,
                id: 2,
                offset: (k * page) as u64,
                len: page as u64,
                backing: if k == 5 { (RUNS * page) as u64 } else { 0 },
                flags: wire::SHARED,
                generation: 1,
            })
            .collect();
        batch(
            socket,
            Frame {
                kind: wire::MAP_BATCH,
                id: 2,
                len: RUNS as u64,
                ..Frame::default()
            },
            &runs,
        );
    })
    .expect_err("a batch with a refused run ran to completion");
    let said = error.to_string();
    for want in [
        &format!("run 5 of {RUNS}"),
        "arena offset 262144",
        "Invalid argument",
    ] {
        assert!(said.contains(want), "{said:?} does not name {want}");
    }
}
