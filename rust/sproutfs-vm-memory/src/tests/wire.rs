use super::*;
use std::net::Shutdown;
use std::time::Duration;

// This independently specified fixture catches field-order and endianness
// changes even if the encoder and decoder are changed together.
const BYTES: [u8; FRAME_BYTES] = [
    1, 0, 0, 0, 0, 0, 0, 0, 8, 7, 6, 5, 4, 3, 2, 1, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 32, 0, 0, 0, 0,
    0, 0, 0, 64, 0, 0, 0, 0, 0, 255, 255, 255, 255, 255, 255, 255, 255, 1, 0, 0, 0, 0, 0, 0, 128,
];

fn frame() -> Frame {
    Frame {
        kind: 1,
        id: 0x0102030405060708,
        offset: 256,
        len: 2 << 20,
        backing: 4 << 20,
        generation: u64::MAX,
        flags: (1 << 63) | 1,
    }
}

fn pair() -> (UnixStream, UnixStream) {
    let (left, right) = UnixStream::pair().unwrap();
    for socket in [&left, &right] {
        socket
            .set_read_timeout(Some(Duration::from_secs(1)))
            .unwrap();
        socket
            .set_write_timeout(Some(Duration::from_secs(1)))
            .unwrap();
    }
    (left, right)
}

#[test]
fn frame_encoding_and_decoding_match_the_protocol_fixture() {
    assert_eq!(frame().bytes(), BYTES);
    assert_eq!(Frame::decode(BYTES), frame());
    let (mut writer, mut reader) = pair();
    frame().write(&mut writer).unwrap();
    let mut actual = [0; FRAME_BYTES];
    reader.read_exact(&mut actual).unwrap();
    assert_eq!(actual, BYTES);
}

#[test]
fn frame_reads_reassemble_fragments_and_reject_truncation() {
    let (mut writer, mut reader) = pair();
    let sender = std::thread::spawn(move || {
        for fragment in BYTES.chunks(3) {
            writer.write_all(fragment).unwrap();
        }
    });
    assert_eq!(Frame::read(&mut reader).unwrap(), frame());
    sender.join().unwrap();
    for length in [0, 1, FRAME_BYTES - 1] {
        let (mut writer, mut reader) = pair();
        writer.write_all(&BYTES[..length]).unwrap();
        writer.shutdown(Shutdown::Write).unwrap();
        assert_eq!(
            Frame::read(&mut reader).unwrap_err().kind(),
            io::ErrorKind::UnexpectedEof
        );
    }
}

#[test]
fn received_descriptor_is_independent_and_close_on_exec() {
    let (mut writer, mut reader) = pair();
    let (sent, mut peer) = pair();
    let descriptor: OwnedFd = sent.into();
    frame().send_fd(&mut writer, &descriptor).unwrap();
    let (actual, received) = Frame::receive_fd(&mut reader).unwrap();
    assert_eq!(actual, frame());
    // SAFETY: received owns a live descriptor, and F_GETFD takes no pointer.
    let flags = unsafe { libc::fcntl(received.as_raw_fd(), libc::F_GETFD) };
    assert!(flags >= 0 && flags & libc::FD_CLOEXEC != 0);
    drop(descriptor);
    let mut socket = UnixStream::from(received);
    socket.write_all(b"still owned").unwrap();
    let mut bytes = [0; 11];
    peer.read_exact(&mut bytes).unwrap();
    assert_eq!(&bytes, b"still owned");
}

// Send deliberately malformed or fragmented ancillary input without using the
// production sender, which correctly always sends one descriptor and 64 bytes.
fn send_rights(socket: &UnixStream, bytes: &[u8], descriptors: &[&OwnedFd]) {
    assert!(descriptors.len() <= 32);
    let mut iov = libc::iovec {
        iov_base: bytes.as_ptr().cast_mut().cast(),
        iov_len: bytes.len(),
    };
    let mut control = [0usize; 32];
    // SAFETY: every pointer refers to a live buffer until sendmsg returns;
    // the ancillary allocation is larger than every descriptor array below.
    unsafe {
        let mut msg: libc::msghdr = mem::zeroed();
        msg.msg_iov = &mut iov;
        msg.msg_iovlen = 1;
        msg.msg_control = control.as_mut_ptr().cast();
        // SCM_RIGHTS carries i32 descriptors, not Rust pointer-sized references.
        let payload = mem::size_of::<i32>() * descriptors.len();
        msg.msg_controllen = libc::CMSG_SPACE(payload as u32) as usize;
        let header = libc::CMSG_FIRSTHDR(&msg);
        (*header).cmsg_level = libc::SOL_SOCKET;
        (*header).cmsg_type = libc::SCM_RIGHTS;
        (*header).cmsg_len = libc::CMSG_LEN(payload as u32) as usize;
        for (index, descriptor) in descriptors.iter().enumerate() {
            libc::CMSG_DATA(header)
                .cast::<i32>()
                .add(index)
                .write_unaligned(descriptor.as_raw_fd());
        }
        assert_eq!(
            libc::sendmsg(socket.as_raw_fd(), &msg, libc::MSG_NOSIGNAL),
            bytes.len() as isize
        );
    }
}

#[test]
fn descriptor_receive_reassembles_a_frame_after_ancillary_delivery() {
    let (mut writer, mut reader) = pair();
    let (sent, _peer) = pair();
    let descriptor: OwnedFd = sent.into();
    send_rights(&writer, &BYTES[..1], &[&descriptor]);
    writer.write_all(&BYTES[1..]).unwrap();
    let (actual, _descriptor) = Frame::receive_fd(&mut reader).unwrap();
    assert_eq!(actual, frame());
}

#[test]
fn descriptor_receive_rejects_missing_multiple_and_truncated_rights() {
    let (mut writer, mut reader) = pair();
    writer.write_all(&BYTES).unwrap();
    assert_eq!(
        Frame::receive_fd(&mut reader).unwrap_err().kind(),
        io::ErrorKind::InvalidData
    );
    for count in [2, 32] {
        let (writer, mut reader) = pair();
        let (sent, _peer) = pair();
        let descriptor: OwnedFd = sent.into();
        send_rights(&writer, &BYTES, &vec![&descriptor; count]);
        assert_eq!(
            Frame::receive_fd(&mut reader).unwrap_err().kind(),
            io::ErrorKind::InvalidData
        );
    }
}

#[test]
fn descriptor_receive_rejects_a_truncated_frame_and_a_closed_peer() {
    let (writer, mut reader) = pair();
    let (sent, _peer) = pair();
    let descriptor: OwnedFd = sent.into();
    send_rights(&writer, &BYTES[..1], &[&descriptor]);
    writer.shutdown(Shutdown::Write).unwrap();
    assert_eq!(
        Frame::receive_fd(&mut reader).unwrap_err().kind(),
        io::ErrorKind::UnexpectedEof
    );
    let (writer, mut reader) = pair();
    drop(writer);
    assert_eq!(
        Frame::receive_fd(&mut reader).unwrap_err().kind(),
        io::ErrorKind::UnexpectedEof
    );
}

/// A pager that closes the socket instead of attaching backing is the one
/// failure this handshake has no other evidence of: the region was refused —
/// the host's logical-page cap is full, say — and the descriptor never came.
/// It must not be reported as a malformed ancillary message, which names the
/// wire and sends the reader looking at the wrong end of the connection.
#[test]
fn descriptor_receive_names_the_pager_closing_before_attach() {
    let (writer, mut reader) = pair();
    drop(writer);
    let err = Frame::receive_fd(&mut reader).unwrap_err();
    assert_eq!(err.kind(), io::ErrorKind::UnexpectedEof);
    assert_eq!(
        err.to_string(),
        "the pager closed the session before attaching backing"
    );
}

/// The one message these three shared sent a reader looking at the wire for a
/// buffer this side sized, for a pager that answered without attaching
/// anything, and for a pager that attached more than a session takes. Each has
/// a different place to look, so each says which it is.
#[test]
fn descriptor_receive_names_which_attachment_failure_it_is() {
    let (mut writer, mut reader) = pair();
    writer.write_all(&BYTES).unwrap();
    let none = Frame::receive_fd(&mut reader).unwrap_err();
    assert_eq!(none.kind(), io::ErrorKind::InvalidData);
    assert_eq!(
        none.to_string(),
        "the pager answered without a backing descriptor"
    );

    let (writer, mut reader) = pair();
    let (sent, _peer) = pair();
    let descriptor: OwnedFd = sent.into();
    send_rights(&writer, &BYTES, &[&descriptor, &descriptor]);
    let many = Frame::receive_fd(&mut reader).unwrap_err();
    assert_eq!(many.kind(), io::ErrorKind::InvalidData);
    assert_eq!(
        many.to_string(),
        "the pager attached 2 backing descriptors, expected one"
    );

    let (writer, mut reader) = pair();
    send_rights(&writer, &BYTES, &vec![&descriptor; 32]);
    let truncated = Frame::receive_fd(&mut reader).unwrap_err();
    assert_eq!(truncated.kind(), io::ErrorKind::InvalidData);
    assert_eq!(
        truncated.to_string(),
        "the pager's backing descriptors did not fit this session's ancillary buffer"
    );
}

#[test]
fn descriptor_send_reports_a_closed_peer() {
    let (mut writer, reader) = pair();
    let (sent, _peer) = pair();
    let descriptor: OwnedFd = sent.into();
    drop(reader);
    assert_eq!(
        frame()
            .send_fd(&mut writer, &descriptor)
            .unwrap_err()
            .kind(),
        io::ErrorKind::BrokenPipe
    );
}

#[test]
fn descriptor_receive_reports_would_block_without_retrying() {
    let (_writer, mut reader) = pair();
    reader.set_nonblocking(true).unwrap();
    assert_eq!(
        Frame::receive_fd(&mut reader).unwrap_err().kind(),
        io::ErrorKind::WouldBlock
    );
}

#[test]
fn descriptor_receive_ignores_other_socket_ancillary_messages() {
    let (mut writer, mut reader) = pair();
    let enabled: libc::c_int = 1;
    // SAFETY: the option pointer references a live integer of the supplied size.
    assert_eq!(
        unsafe {
            libc::setsockopt(
                reader.as_raw_fd(),
                libc::SOL_SOCKET,
                libc::SO_PASSCRED,
                (&enabled as *const libc::c_int).cast(),
                mem::size_of_val(&enabled) as libc::socklen_t,
            )
        },
        0
    );
    let (sent, _peer) = pair();
    let descriptor: OwnedFd = sent.into();
    frame().send_fd(&mut writer, &descriptor).unwrap();
    let (actual, _received) = Frame::receive_fd(&mut reader).unwrap();
    assert_eq!(actual, frame());
}
