use super::*;

/// The arena offset of every mapping of the session's arena, in address order.
/// A span puts its runs in one reservation and moves them out front to back, so
/// this is what says each of them reached the region page it names.
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
/// scattered arena slots: pages next to each other in the region, each from a
/// different part of the arena, so no two of them merge. This is the shape the
/// pager sends most of, and what it costs the client is counted here.
const RUNS: usize = 64;

/// What one such run costs: the arena mapping and its MADV_DONTFORK, the UFFD
/// registration, the write-protect, and the mremap that puts it in the region.
/// The first three of those are the span's rather than the run's; the mremap is
/// the run's own and cannot be anything else, because mremap moves one mapping
/// and two runs of a batch are never one.
const SPAN_CALLS: u64 = 8;
const RUN_CALLS: u64 = 2;

#[test]
#[ignore = "requires native UFFD support and permission to create kernel-mode UFFD"]
fn a_batch_of_scattered_runs_costs_one_span_and_one_mremap_a_run() {
    let page = MIN_PAGE_SIZE;
    let spec = RegionSpec {
        kind: RegionKind::Ram,
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
        // Run k puts arena page RUNS-1-k at region page k: adjacent in the
        // region, never adjacent in the arena.
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
        // Every run of the span reached the region page it names, in the order
        // the batch gave them: region page k holds arena page RUNS-1-k.
        let want: Vec<u64> = (0..RUNS).map(|k| ((RUNS - 1 - k) * page) as u64).collect();
        assert_eq!(arena_offsets(), want, "the arena offsets the region maps");
        stop(socket);
    })
    .unwrap();
}
