use super::*;
use crate::mappings::Mappings;

const PAGE: u64 = 4096;

fn command(kind: u64, page: u64) -> wire::Frame {
    wire::Frame {
        kind,
        offset: page * PAGE,
        len: PAGE,
        backing: if kind == wire::MAP { page * PAGE } else { 0 },
        flags: if kind == wire::MAP_ZERO {
            wire::IMMUTABLE
        } else {
            0
        },
        ..wire::Frame::default()
    }
}

#[test]
fn disabled_budget_admits_any_request() {
    let budget = VmaBudget::new(0).unwrap();
    for replacements in [0, 1, usize::MAX] {
        budget.admit(usize::MAX, replacements).unwrap();
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

// Each replacement that installs a mapping may cost six while it is applied,
// over what the memory region is mapped as now.
#[test]
fn a_command_is_admitted_up_to_the_limit_over_the_mappings_there_are() {
    let budget = VmaBudget { limit: 128 };
    budget.admit(2, 21).unwrap();
    budget.admit(128, 0).unwrap();
    for (mapped, replacements) in [(3, 21), (0, 22), (128, 1), (0, usize::MAX)] {
        assert_eq!(
            budget
                .admit(mapped, replacements)
                .unwrap_err()
                .raw_os_error(),
            Some(libc::ENOSPC),
            "{mapped} mapped, {replacements} replacements"
        );
    }
}

// A refused mapping sends the pager to revoke, which is the only thing that
// frees the budget it ran out of. A revocation charged like a mapping could not
// be admitted from what a refusal leaves, and the guest would wait on a command
// that can never be admitted: the range a revocation replaces becomes a trap
// that merges with the traps around it, so it installs nothing and is admitted
// whatever the budget holds.
#[test]
fn a_refused_mapping_still_admits_the_revocation_that_frees_the_budget() {
    let budget = VmaBudget { limit: 128 };
    for kind in [wire::MAP, wire::MAP_ZERO] {
        assert_eq!(
            budget
                .admit_command(128, [kind])
                .unwrap_err()
                .raw_os_error(),
            Some(libc::ENOSPC)
        );
    }
    budget.admit_command(128, [wire::REVOKE]).unwrap();
    // A batch of them is admitted however large, and a mixed one costs only
    // what it installs: two mappings here, which the exhausted budget refuses.
    budget
        .admit_command(128, std::iter::repeat_n(wire::REVOKE, 1024))
        .unwrap();
    assert_eq!(
        budget
            .admit_command(128, [wire::REVOKE, wire::MAP, wire::REVOKE, wire::MAP_ZERO])
            .unwrap_err()
            .raw_os_error(),
        Some(libc::ENOSPC)
    );
}

// The budget follows what the memory region is mapped as, not what it has
// been: a VMM that maps and revokes far more often than the limit allows
// mappings at once is never refused while it holds few. A jailed VMM has no
// /proc, and this budget was once an estimate that only grew without one: on
// GCE it refused every map after 87,381 of them, with 326 mappings held, and
// the guest froze.
#[test]
fn a_region_that_maps_and_revokes_without_end_is_never_refused_while_it_holds_few() {
    let budget = VmaBudget { limit: 128 };
    let mut mappings = Mappings::new(64);
    for round in 0..100_000u64 {
        let page = round % 64;
        for kind in [wire::MAP, wire::MAP_ZERO, wire::REVOKE] {
            budget
                .admit_command(mappings.count(), [kind])
                .unwrap_or_else(|err| panic!("round {round} {kind}: {err}"));
            mappings.record(command(kind, page), PAGE);
        }
        assert_eq!(mappings.count(), 1);
    }
}

// A region that does hold many mappings is refused once what a command could
// cost would pass the limit, and admitted again once revocations have merged
// them back.
#[test]
fn a_region_holding_many_mappings_is_refused_until_they_are_revoked() {
    let budget = VmaBudget { limit: 128 };
    let mut mappings = Mappings::new(256);
    let mut page = 0;
    // Every other page mapped from the private file at its own page of it
    // backwards, so no two merge.
    while budget.admit_command(mappings.count(), [wire::MAP]).is_ok() {
        mappings.record(
            wire::Frame {
                backing: (255 - page) * PAGE,
                ..command(wire::MAP, page)
            },
            PAGE,
        );
        page += 2;
    }
    // Each map adds itself and the trap after it: admitted up to 122, the
    // last one leaves 124.
    assert_eq!(mappings.count(), 124);
    mappings.record(
        wire::Frame {
            len: page * PAGE,
            ..command(wire::REVOKE, 0)
        },
        PAGE,
    );
    assert_eq!(mappings.count(), 1);
    budget.admit_command(mappings.count(), [wire::MAP]).unwrap();
}
