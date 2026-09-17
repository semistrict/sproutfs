use super::Generations;

#[test]
fn untouched_ranges_require_the_first_generation() {
    let history = Generations::default();
    assert!(history.accepts(5, 12, 1));
    for generation in [0, 2, u64::MAX] {
        assert!(!history.accepts(5, 12, generation));
    }
}

#[test]
fn overlapping_updates_preserve_both_edges_and_gaps() {
    let mut history = Generations::default();
    history.set(2, 8, 1);
    history.set(4, 6, 2);
    history.set(10, 12, 3);
    for (start, end, next) in [
        (0, 2, 1),
        (2, 4, 2),
        (4, 6, 3),
        (6, 8, 2),
        (8, 10, 1),
        (10, 12, 4),
    ] {
        assert!(history.accepts(start, end, next), "{start}..{end}");
        assert!(!history.accepts(start, end, next + 1));
    }
    // A command spanning different histories must be rejected as a whole.
    for next in 1..=4 {
        assert!(!history.accepts(0, 12, next));
    }
    history.set(4, 6, 1);
    assert!(history.accepts(2, 8, 2));
    assert!(!history.accepts(2, 9, 2));
}

#[test]
fn generation_exhaustion_never_accepts_a_wrapped_command() {
    let mut history = Generations::default();
    history.set(3, 6, u64::MAX);
    for generation in [0, 1, u64::MAX] {
        assert!(!history.accepts(3, 6, generation));
    }
    assert!(history.accepts(0, 3, 1));
    assert!(history.accepts(6, 9, 1));
}

#[test]
fn fragmented_history_matches_an_independent_per_page_model() {
    let mut history = Generations::default();
    let mut pages = [0u64; 24];
    // Include disjoint, nested, bridging and repeated replacements.
    for (start, end, generation) in [
        (2, 8, 1),
        (14, 20, 1),
        (4, 6, 2),
        (6, 17, 3),
        (0, 24, 4),
        (3, 21, 5),
        (0, 3, 5),
        (21, 24, 5),
        (8, 16, 6),
        (8, 16, 7),
        (0, 1, 8),
        (23, 24, 8),
    ] {
        history.set(start, end, generation);
        pages[start..end].fill(generation);
        for left in 0..pages.len() {
            for right in left + 1..=pages.len() {
                for next in 0..=9 {
                    let expected = pages[left..right]
                        .iter()
                        .all(|value| value.checked_add(1) == Some(next));
                    assert_eq!(
                        history.accepts(left, right, next),
                        expected,
                        "after {start}..{end}={generation}, command {left}..{right}={next}"
                    );
                }
            }
        }
    }
}
