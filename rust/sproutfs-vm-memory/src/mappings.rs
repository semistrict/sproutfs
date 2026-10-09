use crate::runs::Runs;
use crate::wire;
use crate::wire::Frame;

#[cfg(test)]
#[path = "tests/mappings.rs"]
mod tests;

/// What one page of the memory region is mapped as, which decides whether the
/// kernel keeps it in one mapping with the page beside it.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Mapped {
    /// A fault trap. Every trap is prepared out of one anonymous source at the
    /// offset it has in the memory region, so traps side by side merge.
    Trap,
    /// The zero page, mapped immutable. It is counted apart from the traps,
    /// which it may merge with, because counting a mapping the kernel merged
    /// only refuses a command early.
    Zero,
    /// A file of this session. `delta` is the page of the file less the page
    /// of the memory region, so two pages side by side are one stretch of the
    /// file exactly when it is the same, and the kernel merges them. Whether a
    /// page is immutable does not part them: the private file is mapped shared
    /// either way and every other file private and immutable, and what makes a
    /// page immutable is write protection in its page table, not its mapping.
    File { file: u64, delta: i64 },
}

/// The memory region's pages by what each is mapped as. Two runs side by side
/// never hold the same thing, so the runs are the mappings the kernel keeps
/// for the memory region: what the VMA budget admits against, read from what
/// this session did rather than from /proc, which a jailed VMM does not have.
pub(crate) struct Mappings(Runs<Mapped>);

impl Mappings {
    /// A memory region attached as one trap mapping of `pages` pages.
    pub(crate) fn new(pages: usize) -> Self {
        let mut runs = Runs::new(Mapped::Trap);
        runs.set(0, pages, Mapped::Trap);
        Self(runs)
    }

    /// How many mappings the memory region is.
    pub(crate) fn count(&self) -> usize {
        self.0.len()
    }

    /// Records what one command that the session applied mapped, a command
    /// counted in pages of `page_size` bytes.
    pub(crate) fn record(&mut self, c: Frame, page_size: u64) {
        let start = (c.offset / page_size) as usize;
        let end = ((c.offset + c.len) / page_size) as usize;
        let mapped = match c.kind {
            wire::MAP => Mapped::File {
                file: wire::map_file(c.flags),
                delta: (c.backing / page_size) as i64 - start as i64,
            },
            wire::MAP_ZERO => Mapped::Zero,
            _ => Mapped::Trap,
        };
        self.0.set(start, end, mapped);
    }
}
