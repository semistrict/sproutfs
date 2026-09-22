//! Fixed-size managed-memory control frames. See docs/vm-memory.md in the parent
//! project. A session maps one region, so no frame names one.

use std::io::{self, Read, Write};
use std::mem;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::os::unix::net::UnixStream;

#[cfg(test)]
#[path = "tests/wire.rs"]
mod tests;

/// Version 8 made the attachment's length the arena's offset space rather than
/// its capacity. The arena is a sparse file whose offsets are not its pages: a
/// pager that puts a private page at the offset it has within its 2 MiB range
/// owns 512 consecutive offsets per range whatever memory it holds there. So
/// the number this client checks the descriptor's size against, and bounds a
/// MAP's arena offset by, is the addresses; a version 7 peer would read it as
/// the memory behind them.
///
/// Version 7 gave the attachment the geometry: the page this session's region
/// runs and the kind of memory its arena is made of. The page is no longer one
/// number both ends know, so a version 6 peer is refused by version too — its
/// page numbers name other pages.
pub(crate) const VERSION: u64 = 8;
/// The encoded size of one frame.
pub(crate) const FRAME_BYTES: usize = 56;
pub(crate) const HELLO: u64 = 1;
pub(crate) const REGION: u64 = 2;
pub(crate) const ATTACH: u64 = 3;
pub(crate) const MAP: u64 = 4;
pub(crate) const REVOKE: u64 = 5;
pub(crate) const ACK: u64 = 6;
pub(crate) const STOP: u64 = 7;
/// Asks the host to take the session's checkpoint. There is no durability
/// request in this protocol: a guest flush makes nothing durable and the
/// device completes it itself.
pub(crate) const SEAL: u64 = 8;
pub(crate) const RESULT: u64 = 9;
pub(crate) const MAP_BATCH: u64 = 10;
pub(crate) const READY: u64 = 11;
pub(crate) const MAP_ZERO: u64 = 12;
pub(crate) const MAX_BATCH_RUNS: u64 = 1024;
pub(crate) const SHARED: u64 = 1;

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

    pub fn read(socket: &mut UnixStream) -> io::Result<Self> {
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

    pub fn receive_fd(socket: &mut UnixStream) -> io::Result<(Self, OwnedFd)> {
        let mut bytes = [0; FRAME_BYTES];
        let mut iov = libc::iovec {
            iov_base: bytes.as_mut_ptr().cast(),
            iov_len: bytes.len(),
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
        let mut fds = Vec::new();
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
        // Every way this can go wrong says something different about where to
        // look, and one message for all of them sent a reader to the wire when
        // the answer was at the other end of the connection.
        //
        // Truncated ancillary data is this side's buffer, not the pager's
        // message: the kernel dropped descriptors that did not fit.
        if msg.msg_flags & libc::MSG_CTRUNC != 0 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "the pager's backing descriptors did not fit this session's ancillary buffer",
            ));
        }
        // An orderly close with nothing on it is its own failure: the pager
        // refused this region — its logical-page cap is full, say — and closed
        // instead of attaching backing. It is the end of the connection the
        // answer had to come from, not a malformed message.
        if n == 0 {
            return Err(io::Error::new(
                io::ErrorKind::UnexpectedEof,
                if fds.is_empty() {
                    "the pager closed the session before attaching backing"
                } else {
                    "the pager closed the session after its backing descriptor and before the frame"
                },
            ));
        }
        // The pager answered, so this is the message itself: either it carried
        // no descriptor, or it carried more than the one a session attaches.
        if fds.len() != 1 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                if fds.is_empty() {
                    "the pager answered without a backing descriptor".to_owned()
                } else {
                    format!(
                        "the pager attached {} backing descriptors, expected one",
                        fds.len()
                    )
                },
            ));
        }
        socket.read_exact(&mut bytes[n..])?;
        Ok((Self::decode(bytes), fds.pop().unwrap()))
    }
}
