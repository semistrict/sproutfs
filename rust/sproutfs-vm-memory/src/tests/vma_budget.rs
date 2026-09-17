use super::*;
use std::io::Write;
use std::sync::atomic::{AtomicU64, Ordering};

fn maps(contents: &[u8]) -> BufReader<File> {
    static NEXT: AtomicU64 = AtomicU64::new(0);
    let path = std::env::temp_dir().join(format!(
        "sproutfs-vma-unit-{}-{}",
        std::process::id(),
        NEXT.fetch_add(1, Ordering::Relaxed)
    ));
    let mut file = File::options()
        .read(true)
        .write(true)
        .create_new(true)
        .open(&path)
        .unwrap();
    std::fs::remove_file(path).unwrap();
    file.write_all(contents).unwrap();
    BufReader::new(file)
}

#[test]
fn disabled_budget_needs_no_maps_and_admits_any_request() {
    let mut budget = VmaBudget::new(0).unwrap();
    assert!(budget.maps.is_none());
    for replacements in [0, 1, usize::MAX] {
        budget.admit(replacements).unwrap();
    }
}

#[test]
fn small_nonzero_limits_are_invalid() {
    for limit in [1, 127] {
        assert_eq!(
            VmaBudget::new(limit).err().unwrap().kind(),
            io::ErrorKind::InvalidInput
        );
    }
}

#[test]
fn cumulative_budget_admits_the_exact_limit_and_refuses_overflow() {
    let mut budget = VmaBudget {
        limit: 128,
        estimate: 2,
        maps: None,
    };
    // Each replacement reserves six VMAs, including temporary edge splits.
    budget.admit(21).unwrap();
    budget.admit(0).unwrap();
    for replacements in [1, usize::MAX] {
        assert_eq!(
            budget.admit(replacements).unwrap_err().raw_os_error(),
            Some(libc::ENOSPC)
        );
    }
    assert_eq!(budget.estimate, 128);
}

// A refused mapping sends the pager to revoke, which is the only thing that
// frees the budget it ran out of. A revocation charged like a mapping could not
// be admitted from what a refusal leaves, and the guest would wait on a command
// that can never be admitted: the range a revocation replaces becomes a trap
// that merges with the traps around it, so it installs nothing and is admitted
// whatever the budget holds.
#[test]
fn a_refused_mapping_still_admits_the_revocation_that_frees_the_budget() {
    let mut budget = VmaBudget {
        limit: 128,
        estimate: 128,
        maps: None,
    };
    for kind in [wire::MAP, wire::MAP_ZERO] {
        assert_eq!(
            budget.admit_command([kind]).unwrap_err().raw_os_error(),
            Some(libc::ENOSPC)
        );
    }
    budget.admit_command([wire::REVOKE]).unwrap();
    // A batch of them is admitted however large, and a mixed one costs only
    // what it installs: two mappings here, which the exhausted budget refuses.
    budget
        .admit_command(std::iter::repeat_n(wire::REVOKE, 1024))
        .unwrap();
    assert_eq!(
        budget
            .admit_command([wire::REVOKE, wire::MAP, wire::REVOKE, wire::MAP_ZERO])
            .unwrap_err()
            .raw_os_error(),
        Some(libc::ENOSPC)
    );
    assert_eq!(budget.estimate, 128);
}

#[test]
fn refresh_recovers_merged_mappings_and_rewinds_on_every_attempt() {
    let mut budget = VmaBudget {
        limit: 128,
        estimate: 125,
        maps: Some(maps(b"first\nsecond\n")),
    };
    budget.admit(2).unwrap();
    assert_eq!(budget.estimate, 14);
    budget.admit(19).unwrap();
    assert_eq!(budget.estimate, 128);
    budget.admit(21).unwrap();
    assert_eq!(budget.estimate, 128);
    assert_eq!(
        budget.admit(22).unwrap_err().raw_os_error(),
        Some(libc::ENOSPC)
    );
}

#[test]
fn refresh_counts_an_unterminated_final_mapping_and_stops_at_the_limit() {
    let mut budget = VmaBudget {
        limit: 128,
        estimate: 125,
        maps: Some(maps(b"first\nsecond")),
    };
    budget.admit(21).unwrap();
    assert_eq!(budget.estimate, 128);
    let content = "mapping\n".repeat(256);
    let mut full = VmaBudget {
        limit: 128,
        estimate: 128,
        maps: Some(maps(content.as_bytes())),
    };
    assert_eq!(
        full.admit(1).unwrap_err().raw_os_error(),
        Some(libc::ENOSPC)
    );
    assert_eq!(full.estimate, 128);
}
