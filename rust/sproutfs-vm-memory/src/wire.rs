//! Fixed-size managed-memory control frames. See docs/vm-memory.md in the parent
//! project. A session maps one memory region, so no frame names one.

use std::io::{self, Write};
use std::mem;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::os::unix::net::UnixStream;

#[cfg(test)]
#[path = "tests/wire.rs"]
mod tests;

/// Version 10 moved the arena off ATTACH and into files. ATTACH carries no
/// descriptor and no length. The pager hands this client each file it may map
/// in a FILE frame with its descriptor, and a MAP names the file it maps from.
/// File 0 is the region's private file and the only one this client maps
/// writable. A version 9 peer would read ATTACH as the arena and a MAP's file
/// number as a protection flag.
///
/// Version 9 added FLUSH, the request this client sends when its guest flushes
/// the memory region and whose RESULT completes that flush. A version 8 pager ends the
/// session on a control message it does not know, which would end the guest at
/// its first flush, so the two are told apart before a guest runs.
///
/// Version 8 made the attachment's length the arena's offset space rather than
/// its capacity. The arena is a sparse file whose offsets are not its pages: a
/// pager that puts a private page at the offset it has within its 2 MiB range
/// owns 512 consecutive offsets per range whatever memory it holds there. So
/// the number this client checks the descriptor's size against, and bounds a
/// MAP's arena offset by, is the addresses; a version 7 peer would read it as
/// the memory behind them.
///
/// Version 7 gave the attachment the geometry: the page this session's memory region
/// runs and the kind of memory its arena is made of. The page is no longer one
/// number both ends know, so a version 6 peer is refused by version too — its
/// page numbers name other pages.
pub(crate) const VERSION: u64 = 10;
/// The encoded size of one frame.
pub(crate) const FRAME_BYTES: usize = 56;
pub(crate) const HELLO: u64 = 1;
pub(crate) const MEMORY_REGION: u64 = 2;
pub(crate) const ATTACH: u64 = 3;
pub(crate) const MAP: u64 = 4;
pub(crate) const REVOKE: u64 = 5;
pub(crate) const ACK: u64 = 6;
pub(crate) const STOP: u64 = 7;
/// Asks the host to take the session's checkpoint.
pub(crate) const SEAL: u64 = 8;
/// Answers a SEAL or a FLUSH, echoing its request ID.
pub(crate) const RESULT: u64 = 9;
pub(crate) const MAP_BATCH: u64 = 10;
pub(crate) const READY: u64 = 11;
pub(crate) const MAP_ZERO: u64 = 12;
/// Asks the host to make durable a flush the guest made of this session's
/// memory region, under a request ID from the same sequence as SEAL's and with every
/// other field zero. The host answers with RESULT once the flush is durable,
/// which may take a disk checkpoint first, and the device completes the
/// guest's flush then.
pub(crate) const FLUSH: u64 = 13;
/// Hands this client one file it may map, with its descriptor: `id` is the
/// file's number, `len` its size in bytes, `backing` the arena kind, and
/// `flags` FILE_WRITABLE or zero. A FILE that repeats a number with a larger
/// length grows that file.
pub(crate) const FILE: u64 = 14;
/// Closes the descriptor of the file `id` names. The pager sends it once every
/// mapping of that file is revoked.
pub(crate) const DROP_FILE: u64 = 15;
pub(crate) const MAX_BATCH_RUNS: u64 = 1024;
/// The bit of a MAP's flags that maps its pages read-only and write-protected.
/// The bits above it are the file the MAP names. MAP_ZERO carries it alone.
pub(crate) const IMMUTABLE: u64 = 1;
/// The flag of a FILE whose descriptor is read-write.
pub(crate) const FILE_WRITABLE: u64 = 1;
/// The region's private file: the only file whose descriptor is read-write,
/// and the only one a writable MAP may name. Every other file is read-only.
pub(crate) const PRIVATE_FILE: u64 = 0;

/// The file a MAP's flags name.
pub(crate) fn map_file(flags: u64) -> u64 {
    flags >> 1
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub(crate) struct Frame {
    pub kind: u64,
    pub id: u64,
    pub offset: u64,
    pub len: u64,
    pub backing: u64,
    pub generation: u64,
    pub flags: u64,
}

impl Frame {
    fn bytes(self) -> [u8; FRAME_BYTES] {
        let mut bytes = [0; FRAME_BYTES];
        for (field, value) in bytes.chunks_exact_mut(8).zip([
            self.kind,
            self.id,
            self.offset,
            self.len,
            self.backing,
            self.generation,
            self.flags,
        ]) {
            field.copy_from_slice(&value.to_le_bytes());
        }
        bytes
    }

    fn decode(bytes: [u8; FRAME_BYTES]) -> Self {
        let mut fields = bytes
            .chunks_exact(8)
            .map(|b| u64::from_le_bytes(b.try_into().unwrap()));
        Self {
            kind: fields.next().unwrap(),
            id: fields.next().unwrap(),
            offset: fields.next().unwrap(),
            len: fields.next().unwrap(),
            backing: fields.next().unwrap(),
            generation: fields.next().unwrap(),
            flags: fields.next().unwrap(),
        }
    }

    /// Reads one whole frame with a plain read, as the pager a test plays does.
    /// This client reads every frame with receive, which keeps descriptors.
    #[cfg(test)]
    pub fn read(socket: &mut UnixStream) -> io::Result<Self> {
        use std::io::Read;
        let mut bytes = [0; FRAME_BYTES];
        socket.read_exact(&mut bytes)?;
        Ok(Self::decode(bytes))
    }

    pub fn write(self, socket: &mut UnixStream) -> io::Result<()> {
        socket.write_all(&self.bytes())
    }

    pub fn send_fd(self, socket: &mut UnixStream, fd: &OwnedFd) -> io::Result<()> {
        let mut bytes = self.bytes();
        let mut iov = libc::iovec {
            iov_base: bytes.as_mut_ptr().cast(),
            iov_len: bytes.len(),
        };
        // usize storage gives the ancillary buffer cmsghdr alignment.
        let mut control = [0usize; 8];
        // SAFETY: msghdr is a C POD; all pointers below reference live buffers.
        let mut msg: libc::msghdr = unsafe { mem::zeroed() };
        msg.msg_iov = &mut iov;
        msg.msg_iovlen = 1;
        msg.msg_control = control.as_mut_ptr().cast();
        msg.msg_controllen = unsafe { libc::CMSG_SPACE(mem::size_of::<i32>() as _) } as _;
        // SAFETY: the buffer has sufficient space for one SCM_RIGHTS descriptor.
        unsafe {
            let cmsg = libc::CMSG_FIRSTHDR(&msg);
            (*cmsg).cmsg_level = libc::SOL_SOCKET;
            (*cmsg).cmsg_type = libc::SCM_RIGHTS;
            (*cmsg).cmsg_len = libc::CMSG_LEN(mem::size_of::<i32>() as _) as _;
            libc::CMSG_DATA(cmsg)
                .cast::<i32>()
                .write_unaligned(fd.as_raw_fd());
        }
        let sent = loop {
            // SAFETY: all pointers in msg are valid for the duration of sendmsg.
            let n = unsafe { libc::sendmsg(socket.as_raw_fd(), &msg, libc::MSG_NOSIGNAL) };
            if n >= 0 {
                break n as usize;
            }
            let err = io::Error::last_os_error();
            if err.kind() != io::ErrorKind::Interrupted {
                return Err(err);
            }
        };
        if sent == 0 {
            return Err(io::ErrorKind::WriteZero.into());
        }
        socket.write_all(&bytes[sent..])
    }

    /// Reads one whole frame and every descriptor that came with it.
    ///
    /// Every read of a frame is a recvmsg, including the reads that finish a
    /// frame that arrived in pieces. The kernel closes the descriptors a plain
    /// read reaches, so a FILE that followed another frame closely would lose
    /// its descriptor. The kernel ends a read at the message that carries
    /// descriptors, and the pager sends a frame's descriptors with its first
    /// byte, so the descriptors a frame's reads collect are that frame's own.
    pub fn receive(socket: &mut UnixStream) -> io::Result<(Self, Vec<OwnedFd>)> {
        let mut bytes = [0; FRAME_BYTES];
        let mut fds = Vec::new();
        let mut filled = 0;
        while filled < FRAME_BYTES {
            let n = Self::receive_some(socket, &mut bytes[filled..], &mut fds)?;
            // An orderly close with nothing on it is the pager ending the
            // session, which its caller may know more about. One in the middle
            // of a frame is a frame that never came whole.
            if n == 0 {
                return Err(io::Error::new(
                    io::ErrorKind::UnexpectedEof,
                    if filled == 0 && fds.is_empty() {
                        "the pager closed the session"
                    } else {
                        "the pager closed the session partway through a frame"
                    },
                ));
            }
            filled += n;
        }
        Ok((Self::decode(bytes), fds))
    }

    /// Reads one whole frame that must carry exactly one descriptor, as the
    /// pager a test plays reads HELLO.
    #[cfg(test)]
    pub fn receive_fd(socket: &mut UnixStream) -> io::Result<(Self, OwnedFd)> {
        let (frame, mut fds) = Self::receive(socket)?;
        if fds.len() != 1 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!(
                    "the pager sent {} descriptors with frame kind {}, expected one",
                    fds.len(),
                    frame.kind
                ),
            ));
        }
        Ok((frame, fds.pop().unwrap()))
    }

    /// One recvmsg into buffer. It takes the descriptors that came with the
    /// bytes it read into fds and reports how many bytes it read.
    fn receive_some(
        socket: &mut UnixStream,
        buffer: &mut [u8],
        fds: &mut Vec<OwnedFd>,
    ) -> io::Result<usize> {
        let mut iov = libc::iovec {
            iov_base: buffer.as_mut_ptr().cast(),
            iov_len: buffer.len(),
        };
        let mut control = [0usize; 8];
        let mut msg: libc::msghdr = unsafe { mem::zeroed() };
        msg.msg_iov = &mut iov;
        msg.msg_iovlen = 1;
        msg.msg_control = control.as_mut_ptr().cast();
        msg.msg_controllen = mem::size_of_val(&control) as _;
        let n = loop {
            // SAFETY: recvmsg writes only into the live buffers described above.
            let n = unsafe { libc::recvmsg(socket.as_raw_fd(), &mut msg, libc::MSG_CMSG_CLOEXEC) };
            if n >= 0 {
                break n as usize;
            }
            let err = io::Error::last_os_error();
            if err.kind() != io::ErrorKind::Interrupted {
                return Err(err);
            }
        };
        // SAFETY: libc walks kernel-validated ancillary messages within control.
        unsafe {
            let mut cmsg = libc::CMSG_FIRSTHDR(&msg);
            while !cmsg.is_null() {
                if (*cmsg).cmsg_level == libc::SOL_SOCKET && (*cmsg).cmsg_type == libc::SCM_RIGHTS {
                    // cmsg_len is usize in glibc and socklen_t in musl.
                    #[allow(clippy::unnecessary_cast)]
                    let size = (*cmsg).cmsg_len as usize - libc::CMSG_LEN(0) as usize;
                    for i in 0..size / mem::size_of::<i32>() {
                        let raw = libc::CMSG_DATA(cmsg).cast::<i32>().add(i).read_unaligned();
                        fds.push(OwnedFd::from_raw_fd(raw));
                    }
                }
                cmsg = libc::CMSG_NXTHDR(&msg, cmsg);
            }
        }
        // Truncated ancillary data is this side's buffer, not the pager's
        // message: the kernel dropped descriptors that did not fit.
        if msg.msg_flags & libc::MSG_CTRUNC != 0 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "the pager's descriptors did not fit this session's ancillary buffer",
            ));
        }
        Ok(n)
    }
}
