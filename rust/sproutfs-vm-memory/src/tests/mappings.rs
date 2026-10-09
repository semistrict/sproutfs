use super::{Mapped, Mappings};
use crate::wire;
use crate::wire::Frame;

const PAGE: u64 = 4096;

fn map(file: u64, page: u64, pages: u64, at: u64, immutable: bool) -> Frame {
    Frame {
        kind: wire::MAP,
        offset: page * PAGE,
        len: pages * PAGE,
        backing: at * PAGE,
        flags: (file << 1) | if immutable { wire::IMMUTABLE } else { 0 },
        ..Frame::default()
    }
}

fn zero(page: u64, pages: u64) -> Frame {
    Frame {
        kind: wire::MAP_ZERO,
        offset: page * PAGE,
        len: pages * PAGE,
        flags: wire::IMMUTABLE,
        ..Frame::default()
    }
}

fn revoke(page: u64, pages: u64) -> Frame {
    Frame {
        kind: wire::REVOKE,
        offset: page * PAGE,
        len: pages * PAGE,
        ..Frame::default()
    }
}

/// What a page is mapped as, page by page, the way the kernel decides whether
/// two pages side by side are one mapping: the same kind, the same file, and
/// the second page the file's next page.
fn per_page(frames: &[Frame], pages: usize) -> usize {
    let mut model = vec![Mapped::Trap; pages];
    for c in frames {
        let start = (c.offset / PAGE) as usize;
        for (k, page) in (start..start + (c.len / PAGE) as usize).enumerate() {
            model[page] = match c.kind {
                wire::MAP => Mapped::File {
                    file: wire::map_file(c.flags),
                    delta: (c.backing / PAGE) as i64 + k as i64 - page as i64,
                },
                wire::MAP_ZERO => Mapped::Zero,
                _ => Mapped::Trap,
            };
        }
    }
    1 + model.windows(2).filter(|pair| pair[0] != pair[1]).count()
}

// A memory region is one trap mapping when it attaches and again when every
// page has been revoked, whatever was mapped in between.
#[test]
fn a_region_is_one_mapping_until_something_is_mapped_into_it() {
    let mut mappings = Mappings::new(16);
    assert_eq!(mappings.count(), 1);
    mappings.record(map(0, 3, 2, 3, false), PAGE);
    assert_eq!(mappings.count(), 3);
    mappings.record(revoke(0, 16), PAGE);
    assert_eq!(mappings.count(), 1);
}

// Pages side by side are one mapping only when they continue one file: the
// next page of the same file merges, whatever its protection, the same page of
// the file again does not, another file does not, and the zero page is counted
// apart from the traps it may merge with.
#[test]
fn the_count_is_what_an_independent_per_page_model_says_the_kernel_keeps() {
    const PAGES: usize = 32;
    let script = [
        map(0, 0, 4, 0, false),
        map(0, 4, 1, 4, false),
        map(0, 5, 1, 4, false),
        map(1, 6, 1, 6, true),
        map(1, 7, 1, 7, true),
        map(1, 8, 1, 8, false),
        map(2, 9, 1, 10, true),
        zero(10, 2),
        zero(12, 1),
        revoke(4, 1),
        map(0, 20, 8, 100, false),
        revoke(22, 2),
        map(0, 22, 2, 102, false),
        revoke(0, 12),
        map(0, 30, 2, 0, true),
        revoke(12, 20),
    ];
    let mut mappings = Mappings::new(PAGES);
    for (step, c) in script.iter().enumerate() {
        mappings.record(*c, PAGE);
        assert_eq!(
            mappings.count(),
            per_page(&script[..=step], PAGES),
            "after step {step}: {c:?}"
        );
    }
    assert_eq!(mappings.count(), 1);
}
