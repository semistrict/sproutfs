use crate::wire;
use std::io;

#[cfg(test)]
#[path = "tests/vma_budget.rs"]
mod tests;

/// What one replacement that installs a mapping may cost while it is applied:
/// the mapping it is built in, the splits at both edges of the range it
/// replaces, and the reservation it is staged in.
const TRANSIENT_VMAS: usize = 6;

/// The admission bound on the mappings one memory region makes.
///
/// The budget is opt-in: a zero limit disables it. A nonzero limit is the bound
/// the embedder chose, and what it bounds is the memory region's own mappings,
/// which the session counts from what it applied (`Mappings`), so it needs no
/// /proc and a jailed VMM keeps it as well as any other. The embedder budgets
/// its own mappings and those of its other sessions; `/proc/sys/vm/max_map_count`,
/// where it can be read, only sanity-checks the limit.
pub(crate) struct VmaBudget {
    limit: usize,
}

impl VmaBudget {
    pub(crate) fn new(requested: u64) -> io::Result<Self> {
        if requested == 0 {
            return Ok(Self { limit: 0 });
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
        Ok(Self { limit })
    }

    /// admit_command admits one command over a memory region that is `mapped`
    /// mappings now, by what its replacements cost while they are applied.
    ///
    /// A replacement that installs a mapping — a range mapped from the backing,
    /// a range mapped as zeros — is admitted against the limit. A revocation
    /// is admitted whatever the budget holds: the range it replaces becomes
    /// the trap mapping the memory region was attached as, which merges with
    /// the traps around it, and a refused mapping sends the pager to revoke,
    /// which is the only thing that frees the budget for it. Refusing the
    /// revocation too would leave the guest waiting for a command that can
    /// never be admitted. A revocation in the middle of a run of a file splits
    /// it, and what that and a revocation's own replacement cost is covered by
    /// the kernel headroom the limit leaves, which is why a limit above half of
    /// `/proc/sys/vm/max_map_count` is refused at construction.
    pub(crate) fn admit_command(
        &self,
        mapped: usize,
        replacements: impl IntoIterator<Item = u64>,
    ) -> io::Result<()> {
        self.admit(
            mapped,
            replacements
                .into_iter()
                .filter(|kind| *kind != wire::REVOKE)
                .count(),
        )
    }

    /// admit admits the replacements of one command that install a mapping
    /// over a memory region that is `mapped` mappings now.
    pub(crate) fn admit(&self, mapped: usize, replacements: usize) -> io::Result<()> {
        if self.limit == 0 {
            return Ok(());
        }
        let cost = replacements.saturating_mul(TRANSIENT_VMAS);
        if mapped.saturating_add(cost) > self.limit {
            return Err(io::Error::from_raw_os_error(libc::ENOSPC));
        }
        Ok(())
    }
}
