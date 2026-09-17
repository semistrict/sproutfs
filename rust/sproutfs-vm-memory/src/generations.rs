use std::collections::BTreeMap;

#[cfg(test)]
#[path = "tests/generations.rs"]
mod tests;

// Default generation zero is implicit. Revoke retains history so a later
// mapping cannot accept a stale command after the payload has been discarded.
#[derive(Default)]
pub(crate) struct Generations(BTreeMap<usize, (usize, u64)>);

impl Generations {
    pub(crate) fn accepts(&self, start: usize, end: usize, next: u64) -> bool {
        let mut cursor = start;
        while cursor < end {
            let (generation, stop) = self.run(cursor, end);
            if generation.checked_add(1) != Some(next) {
                return false;
            }
            cursor = stop;
        }
        true
    }

    fn run(&self, start: usize, end: usize) -> (u64, usize) {
        if let Some((_, &(stop, generation))) = self.0.range(..=start).next_back() {
            if stop > start {
                return (generation, end.min(stop));
            }
        }
        let stop = self.0.range(start..end).next().map_or(end, |(&p, _)| p);
        (0, stop)
    }

    fn split(&mut self, position: usize) {
        if let Some((&start, &(end, generation))) = self.0.range(..position).next_back() {
            if end > position {
                self.0.insert(start, (position, generation));
                self.0.insert(position, (end, generation));
            }
        }
    }

    pub(crate) fn set(&mut self, mut start: usize, mut end: usize, generation: u64) {
        self.split(start);
        self.split(end);
        // Only the replaced ranges are visited; unrelated history stays put.
        while let Some((&key, _)) = self.0.range(start..end).next() {
            self.0.remove(&key);
        }
        if let Some((&left, &(stop, value))) = self.0.range(..start).next_back() {
            if stop == start && value == generation {
                self.0.remove(&left);
                start = left;
            }
        }
        if let Some(&(stop, value)) = self.0.get(&end) {
            if value == generation {
                self.0.remove(&end);
                end = stop;
            }
        }
        self.0.insert(start, (end, generation));
    }
}
