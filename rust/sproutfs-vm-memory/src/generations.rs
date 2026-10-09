use crate::runs::Runs;

#[cfg(test)]
#[path = "tests/generations.rs"]
mod tests;

// Default generation zero is implicit. Revoke retains history so a later
// mapping cannot accept a stale command after the payload has been discarded.
pub(crate) struct Generations(Runs<u64>);

impl Default for Generations {
    fn default() -> Self {
        Self(Runs::new(0))
    }
}

impl Generations {
    pub(crate) fn accepts(&self, start: usize, end: usize, next: u64) -> bool {
        let mut cursor = start;
        while cursor < end {
            let (generation, stop) = self.0.at(cursor, end);
            if generation.checked_add(1) != Some(next) {
                return false;
            }
            cursor = stop;
        }
        true
    }

    pub(crate) fn set(&mut self, start: usize, end: usize, generation: u64) {
        self.0.set(start, end, generation);
    }
}
