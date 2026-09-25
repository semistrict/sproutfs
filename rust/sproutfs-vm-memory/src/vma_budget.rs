use crate::wire;
use std::fs::File;
use std::io::{self, BufRead, BufReader, Seek, SeekFrom};

#[cfg(test)]
#[path = "tests/vma_budget.rs"]
mod tests;

// Allow a temporary VMA and edge splits while setting flags and replacing
// the destination.
// Count that conservative cost until it approaches the admission limit, then
// refresh from the kernel to account for merging. The embedder must separately
// budget its own concurrent mappings.
//
// The budget is opt-in: a zero limit disables it entirely and reads nothing
// from /proc, which is what a jailed process without /proc needs in order to
// attach at all. A nonzero limit is the admission bound the embedder chose;
// /proc/sys/vm/max_map_count only sanity-checks it and /proc/self/maps only
// lets the estimate recover merged mappings. Without /proc/self/maps the
// estimate is cumulative and therefore strictly conservative.
pub(crate) struct VmaBudget {
    limit: usize,
    estimate: usize,
    maps: Option<BufReader<File>>,
}

impl VmaBudget {
    pub(crate) fn new(requested: u64) -> io::Result<Self> {
        if requested == 0 {
            return Ok(Self {
                limit: 0,
                estimate: 0,
                maps: None,
            });
        }
        let limit =
            usize::try_from(requested).map_err(|_| io::Error::from(io::ErrorKind::InvalidInput))?;
        if limit < 128 {
            return Err(io::ErrorKind::InvalidInput.into());
        }
        if let Some(kernel) = std::fs::read_to_string("/proc/sys/vm/max_map_count")
            .ok()
            .and_then(|raw| raw.trim().parse::<usize>().ok())
        {
            if limit > kernel / 2 {
                return Err(io::ErrorKind::InvalidInput.into());
            }
        }
        // Open before the embedder installs the serving thread's syscall filter.
        let maps = File::open("/proc/self/maps").ok().map(BufReader::new);
        let mut budget = Self {
            limit,
            estimate: 0,
            maps,
        };
        budget.refresh()?;
        Ok(budget)
    }

    fn refresh(&mut self) -> io::Result<()> {
        let limit = self.limit;
        let Some(maps) = self.maps.as_mut() else {
            return Ok(());
        };
        maps.seek(SeekFrom::Start(0))?;
        let mut line = Vec::new();
        let mut count = 0;
        while maps.read_until(b'\n', &mut line)? != 0 {
            count += 1;
            line.clear();
            if count >= limit {
                break;
            }
        }
        self.estimate = count;
        Ok(())
    }

    /// admit_command reserves what one command's merged replacements cost.
    ///
    /// A replacement that installs a mapping — a range mapped from the backing,
    /// a range mapped as zeros — is admitted against the limit and reserves the
    /// cost below. A revocation installs none: the range it replaces becomes
    /// the trap mapping the memory region was attached as, which merges with the traps
    /// around it, so it can only lower the count and is admitted whatever the
    /// budget holds. That is what makes the budget recoverable. A refused
    /// mapping sends the pager to revoke, which is the only thing that frees
    /// the budget for it, and a refusal leaves too little of the limit for a
    /// revocation charged like a mapping — so the pager would have nothing left
    /// to try and the guest would wait for a command that can never be
    /// admitted. What a revocation's own replacement costs transiently is
    /// covered by the kernel headroom the limit leaves, which is why a limit
    /// above half of `/proc/sys/vm/max_map_count` is refused at construction.
    pub(crate) fn admit_command(
        &mut self,
        replacements: impl IntoIterator<Item = u64>,
    ) -> io::Result<()> {
        self.admit(
            replacements
                .into_iter()
                .filter(|kind| *kind != wire::REVOKE)
                .count(),
        )
    }

    /// admit reserves the budget for the replacements of one command that
    /// install a mapping.
    pub(crate) fn admit(&mut self, replacements: usize) -> io::Result<()> {
        if self.limit == 0 {
            return Ok(());
        }
        let cost = replacements.saturating_mul(6);
        if self.estimate.saturating_add(cost) > self.limit {
            self.refresh()?;
        }
        if self.estimate.saturating_add(cost) > self.limit {
            return Err(io::Error::from_raw_os_error(libc::ENOSPC));
        }
        self.estimate += cost;
        Ok(())
    }
}
