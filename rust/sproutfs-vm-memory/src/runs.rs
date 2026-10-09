use std::collections::BTreeMap;

#[cfg(test)]
#[path = "tests/runs.rs"]
mod tests;

/// A value per page of a memory region, kept as maximal runs of pages that hold
/// the same value. A page no run covers holds `default`. Setting a range
/// replaces what it covers and joins it to a neighbour that holds the same
/// value, so two runs side by side never hold one value and the number of runs
/// is the number of places the value changes.
pub(crate) struct Runs<T> {
    runs: BTreeMap<usize, (usize, T)>,
    default: T,
}

impl<T: Copy + PartialEq> Runs<T> {
    pub(crate) fn new(default: T) -> Self {
        Self {
            runs: BTreeMap::new(),
            default,
        }
    }

    /// How many runs are set. A page no run covers is in none of them.
    pub(crate) fn len(&self) -> usize {
        self.runs.len()
    }

    /// The value at `start` and where the stretch holding it ends, no further
    /// than `end`.
    pub(crate) fn at(&self, start: usize, end: usize) -> (T, usize) {
        if let Some((_, &(stop, value))) = self.runs.range(..=start).next_back() {
            if stop > start {
                return (value, end.min(stop));
            }
        }
        let stop = self.runs.range(start..end).next().map_or(end, |(&p, _)| p);
        (self.default, stop)
    }

    fn split(&mut self, position: usize) {
        if let Some((&start, &(end, value))) = self.runs.range(..position).next_back() {
            if end > position {
                self.runs.insert(start, (position, value));
                self.runs.insert(position, (end, value));
            }
        }
    }

    pub(crate) fn set(&mut self, mut start: usize, mut end: usize, value: T) {
        self.split(start);
        self.split(end);
        // Only the replaced ranges are visited; unrelated history stays put.
        while let Some((&key, _)) = self.runs.range(start..end).next() {
            self.runs.remove(&key);
        }
        if let Some((&left, &(stop, held))) = self.runs.range(..start).next_back() {
            if stop == start && held == value {
                self.runs.remove(&left);
                start = left;
            }
        }
        if let Some(&(stop, held)) = self.runs.get(&end) {
            if held == value {
                self.runs.remove(&end);
                end = stop;
            }
        }
        self.runs.insert(start, (end, value));
    }
}
