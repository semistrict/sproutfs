use super::Runs;

// Runs are the places the value changes, whatever order the ranges were set
// in: the number of runs is what an independent page-by-page model counts, and
// each page holds what the model says.
#[test]
fn runs_are_where_an_independent_per_page_model_changes() {
    const PAGES: usize = 24;
    let mut runs = Runs::new(0u8);
    let mut pages = [0u8; PAGES];
    runs.set(0, PAGES, 0);
    for (start, end, value) in [
        (2, 8, 1),
        (14, 20, 1),
        (4, 6, 2),
        (6, 17, 3),
        (8, 9, 1),
        (9, 17, 1),
        (0, 24, 4),
        (3, 21, 5),
        (0, 3, 5),
        (21, 24, 5),
        (8, 16, 6),
        (10, 12, 5),
        (8, 10, 5),
        (12, 16, 5),
    ] {
        runs.set(start, end, value);
        pages[start..end].fill(value);
        let changes = 1 + pages.windows(2).filter(|pair| pair[0] != pair[1]).count();
        assert_eq!(runs.len(), changes, "after {start}..{end}={value}");
        for (page, &value) in pages.iter().enumerate() {
            assert_eq!(runs.at(page, PAGES).0, value, "page {page}");
        }
    }
    assert_eq!(runs.len(), 1);
}

#[test]
fn a_page_no_run_covers_holds_the_default_up_to_the_next_run() {
    let mut runs = Runs::new(7u64);
    runs.set(5, 8, 1);
    assert_eq!(runs.at(0, 10), (7, 5));
    assert_eq!(runs.at(6, 10), (1, 8));
    assert_eq!(runs.at(8, 10), (7, 10));
    assert_eq!(runs.len(), 1);
}
