//! Memory mappings owned by one process and controlled by an external Go pager.
//!
//! The pager owns shared backing and all policy. This library only creates
//! one region, transfers UFFD, and executes versioned mapping commands. It has
//! no Firecracker dependency. Read the safety contract on [`Session::region`].

#![cfg(target_os = "linux")]
#[cfg(not(any(target_arch = "x86_64", target_arch = "aarch64")))]
compile_error!("sproutfs-vm-memory currently supports Linux x86_64 and aarch64");

mod control;
mod generations;
mod linux;
pub use linux::PAGE_SIZE;
mod vma_budget;
mod wire;

#[cfg(test)]
#[path = "tests/session.rs"]
mod tests;

pub use control::{Control, PendingSeal};

use std::io;
use std::os::fd::{AsRawFd, OwnedFd};
use std::os::unix::net::UnixStream;
use std::path::Path;

use linux::{Mapping, TrapSource, Uffd};
use wire::Frame;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
#[repr(u64)]
pub enum RegionKind {
    Pmem = 1,
    Ram = 2,
}

#[derive(Clone, Copy, Debug)]
pub struct RegionSpec {
    pub kind: RegionKind,
    pub len: usize,
}

/// An address descriptor, not a borrow of the bytes. Its owner is the session.
#[derive(Clone, Copy, Debug)]
pub struct Region {
    pub kind: RegionKind,
    pub address: usize,
    pub len: usize,
}

struct OwnedRegion {
    mapping: Mapping,
    traps: TrapSource,
    descriptor: Region,
    generations: generations::Generations,
}

/// Keeps UFFD and the region alive, including after a control error.
///
/// A control failure is terminal: the embedder must stop all memory users before
/// dropping the session. Never close UFFD and continue running with this range.
pub struct Session {
    socket: UnixStream,
    requests: std::sync::Arc<control::Requests>,
    uffd: Uffd,
    backing: OwnedFd,
    backing_len: u64,
    region: OwnedRegion,
    last_command: Option<Frame>,
    terminal: bool,
    vmas: vma_budget::VmaBudget,
}

impl Drop for Session {
    fn drop(&mut self) {
        self.requests.close();
    }
}

impl Session {
    pub fn connect(path: impl AsRef<Path>, spec: RegionSpec) -> io::Result<Self> {
        if spec.len == 0 || spec.len % linux::PAGE_SIZE != 0 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "expected a nonempty page-aligned region",
            ));
        }
        let uffd = Uffd::new()?;
        let traps = TrapSource::new(spec.len)?;
        let mapping = traps.prepare(0, spec.len)?;
        uffd.register(&mapping, false)?;
        let descriptor = Region {
            kind: spec.kind,
            address: mapping.addr as usize,
            len: spec.len,
        };
        let region = OwnedRegion {
            mapping,
            traps,
            descriptor,
            generations: generations::Generations::default(),
        };
        let mut socket = UnixStream::connect(path)?;
        Frame {
            kind: wire::HELLO,
            id: wire::VERSION,
            ..Frame::default()
        }
        .send_fd(&mut socket, &uffd.0)?;
        Frame {
            kind: wire::REGION,
            offset: descriptor.address as u64,
            len: descriptor.len as u64,
            flags: descriptor.kind as u64,
            ..Frame::default()
        }
        .write(&mut socket)?;
        let (attach, backing) = Frame::receive_fd(&mut socket)?;
        let mut stat: libc::stat = unsafe { std::mem::zeroed() };
        if unsafe { libc::fstat(backing.as_raw_fd(), &mut stat) } != 0 {
            return Err(io::Error::last_os_error());
        }
        if attach.kind != wire::ATTACH
            || attach.id != wire::VERSION
            || attach.len == 0
            || attach.len != stat.st_size as u64
            || attach.len > i64::MAX as u64
            || attach.len % linux::PAGE_SIZE as u64 != 0
            || attach.offset != 0
            || attach.backing != 0
            || attach.generation != 0
        {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "invalid backing attachment",
            ));
        }
        let mut fs: libc::statfs = unsafe { std::mem::zeroed() };
        if unsafe { libc::fstatfs(backing.as_raw_fd(), &mut fs) } != 0 {
            return Err(io::Error::last_os_error());
        }
        if fs.f_type as u64 != 0x958458f6 || fs.f_bsize as usize != linux::PAGE_SIZE {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "backing must use explicit 2 MiB HugeTLB pages",
            ));
        }
        let vmas = vma_budget::VmaBudget::new(attach.flags)?;
        let mut session = Self {
            vmas,
            requests: control::Requests::new(socket.try_clone()?)?,
            socket,
            uffd,
            backing,
            backing_len: attach.len,
            region,
            last_command: None,
            terminal: false,
        };
        // Service eager maps before exposing addresses to an embedder. The Go
        // side also installs page tables before sending READY.
        session.run_inner(true)?;
        Ok(session)
    }

    /// Returns the stable address for the embedding process to register/use.
    ///
    /// Dereferencing it is unsafe. All users must stop before session drop;
    /// ordinary Rust references may not span mapping changes. CPU accesses may
    /// race replacement, but external kernel/device pins require coordination
    /// by the embedder. The Firecracker adapter owns that coordination.
    pub fn region(&self) -> Region {
        self.region.descriptor
    }

    /// Returns a device-side handle. The mapping service must run independently
    /// while a caller waits for a seal.
    pub fn control(&self) -> Control {
        Control(self.requests.clone())
    }

    /// Serves mapping commands on a dedicated thread, separate from memory users.
    ///
    /// On STOP, all external users must already have stopped. On error, retain
    /// the session until those users have been terminated; the UFFD stays open.
    pub fn run(&mut self) -> io::Result<()> {
        if self.terminal {
            return Err(io::Error::other("session is terminal"));
        }
        let result = self.run_inner(false);
        self.terminal = true;
        self.requests.close();
        result
    }

    fn run_inner(&mut self, attaching: bool) -> io::Result<()> {
        loop {
            let command = Frame::read(&mut self.socket)?;
            if command.kind == wire::READY {
                if !attaching
                    || command.id == 0
                    || self.last_command.is_some_and(|last| command.id <= last.id)
                    || command.offset != 0
                    || command.len != 0
                    || command.backing != 0
                    || command.generation != 0
                    || command.flags != 0
                {
                    return Err(io::ErrorKind::InvalidData.into());
                }
                self.requests.send(Frame {
                    kind: wire::ACK,
                    id: command.id,
                    ..Frame::default()
                })?;
                self.last_command = Some(command);
                return Ok(());
            }
            if command.kind == wire::MAP_BATCH {
                self.map_batch(command)?;
                continue;
            }
            if command.kind == wire::RESULT {
                self.requests.complete(command)?;
                continue;
            }
            if command.kind == wire::STOP {
                if attaching {
                    return Err(io::ErrorKind::InvalidData.into());
                }
                self.requests.send(Frame {
                    kind: wire::ACK,
                    id: command.id,
                    ..Frame::default()
                })?;
                return Ok(());
            }
            let status = if self.last_command == Some(command) {
                0 // only the immediately preceding identical command is retryable
            } else if let Err(errno) = self.validate(command) {
                errno
            } else if let Err(err) = self.vmas.admit_command([command.kind]) {
                err.raw_os_error().unwrap_or(libc::EIO)
            } else {
                // After mutation begins, failure is terminal, not a rejected
                // command: mremap may already have removed the destination.
                self.apply(command)?;
                self.last_command = Some(command);
                0
            };
            self.requests.send(Frame {
                kind: wire::ACK,
                id: command.id,
                generation: command.generation,
                flags: status as u64,
                ..Frame::default()
            })?;
        }
    }

    fn map_batch(&mut self, command: Frame) -> io::Result<()> {
        if command.len == 0
            || command.len > wire::MAX_BATCH_RUNS
            || command.offset != 0
            || command.backing != 0
            || command.generation != 0
            || command.flags != 0
        {
            return Err(io::ErrorKind::InvalidData.into());
        }
        let mut runs: Vec<Frame> = Vec::with_capacity(command.len as usize);
        for _ in 0..command.len {
            let run = Frame::read(&mut self.socket)?;
            self.validate(run).map_err(io::Error::from_raw_os_error)?;
            if (run.kind != wire::MAP && run.kind != wire::MAP_ZERO && run.kind != wire::REVOKE)
                || run.id != command.id
                || runs
                    .last()
                    .is_some_and(|last| run.offset < last.offset + last.len)
            {
                return Err(io::ErrorKind::InvalidData.into());
            }
            runs.push(run);
        }
        // Validate the complete bounded, disjoint batch before any mutation.
        // A partial mapping failure terminates the session and receives no ACK.
        let mut merged: Vec<Frame> = Vec::new();
        for run in &runs {
            if let Some(last) = merged.last_mut().filter(|last| {
                last.kind == run.kind
                    && last.flags == run.flags
                    && last.offset + last.len == run.offset
                    && (run.kind != wire::MAP || last.backing + last.len == run.backing)
            }) {
                last.len += run.len;
            } else {
                merged.push(*run);
            }
        }
        if let Err(err) = self.vmas.admit_command(merged.iter().map(|run| run.kind)) {
            return self.requests.send(Frame {
                kind: wire::ACK,
                id: command.id,
                flags: err.raw_os_error().unwrap_or(libc::EIO) as u64,
                ..Frame::default()
            });
        }
        for run in merged {
            self.replace(run)?;
        }
        for run in runs {
            self.record_generation(run);
        }
        self.last_command = Some(command);
        self.requests.send(Frame {
            kind: wire::ACK,
            id: command.id,
            ..Frame::default()
        })
    }

    fn validate(&self, c: Frame) -> Result<(), i32> {
        if c.kind != wire::MAP && c.kind != wire::REVOKE && c.kind != wire::MAP_ZERO {
            return Err(libc::EINVAL);
        }
        if c.id == 0 || self.last_command.is_some_and(|last| c.id <= last.id) {
            return Err(libc::ESTALE);
        }
        let r = &self.region;
        let size = linux::PAGE_SIZE as u64;
        if c.len == 0
            || c.offset % size != 0
            || c.len % size != 0
            || c.offset
                .checked_add(c.len)
                .is_none_or(|end| end > r.descriptor.len as u64)
            || c.flags > wire::SHARED
            || (c.kind == wire::REVOKE && (c.flags != 0 || c.backing != 0))
            || (c.kind == wire::MAP_ZERO && (c.flags != wire::SHARED || c.backing != 0))
        {
            return Err(libc::EINVAL);
        }
        if c.kind == wire::MAP
            && (c.backing % size != 0
                || c.backing
                    .checked_add(c.len)
                    .is_none_or(|end| end > self.backing_len))
        {
            return Err(libc::EINVAL);
        }
        if !r.generations.accepts(
            (c.offset / size) as usize,
            ((c.offset + c.len) / size) as usize,
            c.generation,
        ) {
            return Err(libc::ESTALE);
        }
        Ok(())
    }

    fn apply(&mut self, c: Frame) -> io::Result<()> {
        self.replace(c)?;
        self.record_generation(c);
        Ok(())
    }

    fn replace(&mut self, c: Frame) -> io::Result<()> {
        let shared_mapping = c.kind == wire::MAP;
        let mapping = if shared_mapping {
            Mapping::new(c.len as usize, Some((&self.backing, c.backing)))?
        } else {
            self.region
                .traps
                .prepare(c.offset as usize, c.len as usize)?
        };
        if c.kind == wire::MAP_ZERO {
            mapping.populate_zero()?;
        }
        self.uffd.register(&mapping, shared_mapping)?;
        if (shared_mapping && c.flags == wire::SHARED) || c.kind == wire::MAP_ZERO {
            self.uffd.protect(&mapping)?;
        }
        let r = &mut self.region;
        let target = unsafe { r.mapping.addr.cast::<u8>().add(c.offset as usize) }.cast();
        mapping.replace(target)
    }

    fn record_generation(&mut self, c: Frame) {
        let size = linux::PAGE_SIZE as u64;
        self.region.generations.set(
            (c.offset / size) as usize,
            ((c.offset + c.len) / size) as usize,
            c.generation,
        );
    }
}
