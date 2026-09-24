use super::*;

fn pair() -> (Control, UnixStream) {
    let (client, peer) = UnixStream::pair().unwrap();
    peer.set_read_timeout(Some(Duration::from_secs(1))).unwrap();
    (Control(Requests::new(client).unwrap()), peer)
}

fn completion(request: Frame, errno: i32) -> Frame {
    Frame {
        kind: wire::RESULT,
        id: request.id,
        flags: errno as u64,
        ..Frame::default()
    }
}

// A VM's regions are separate sessions, so their seals are separate requests
// that complete in whichever order the host answers them.
#[test]
fn independent_sessions_seal_in_either_order() {
    let (one_control, mut one_peer) = pair();
    let (two_control, mut two_peer) = pair();
    let first = one_control.start_seal().unwrap();
    let second = two_control.start_seal().unwrap();
    let one = Frame::read(&mut one_peer).unwrap();
    let two = Frame::read(&mut two_peer).unwrap();
    let request = Frame {
        kind: wire::SEAL,
        id: 1,
        ..Frame::default()
    };
    assert_eq!(one, request);
    assert_eq!(two, request);
    two_control
        .0
        .complete(completion(two, libc::ENOSPC))
        .unwrap();
    one_control.0.complete(completion(one, 0)).unwrap();
    first.wait(Duration::from_secs(1)).unwrap();
    assert_eq!(
        second
            .wait(Duration::from_secs(1))
            .unwrap_err()
            .raw_os_error(),
        Some(libc::ENOSPC)
    );
    let retry = two_control.start_seal().unwrap();
    let request = Frame::read(&mut two_peer).unwrap();
    assert!(request.id > two.id);
    two_control.0.complete(completion(request, 0)).unwrap();
    retry.wait(Duration::from_secs(1)).unwrap();
}

#[test]
fn a_session_has_only_one_pending_request() {
    let (control, mut peer) = pair();
    let pending = control.start_seal().unwrap();
    let request = Frame::read(&mut peer).unwrap();
    assert_eq!(
        control.start_seal().err().unwrap().kind(),
        io::ErrorKind::WouldBlock
    );
    control.0.complete(completion(request, 0)).unwrap();
    pending.wait(Duration::from_secs(1)).unwrap();
    let next = control.start_seal().unwrap();
    let request = Frame::read(&mut peer).unwrap();
    control.0.complete(completion(request, 0)).unwrap();
    next.wait(Duration::from_secs(1)).unwrap();
}

#[test]
fn malformed_or_stale_completions_do_not_consume_the_pending_request() {
    let (control, mut peer) = pair();
    let pending = control.start_seal().unwrap();
    let response = completion(Frame::read(&mut peer).unwrap(), 0);
    for invalid in [
        Frame {
            kind: wire::ACK,
            ..response
        },
        Frame {
            id: response.id + 1,
            ..response
        },
        Frame {
            offset: 1,
            ..response
        },
        Frame { len: 1, ..response },
        Frame {
            backing: 1,
            ..response
        },
        Frame {
            generation: 1,
            ..response
        },
        Frame {
            flags: i32::MAX as u64 + 1,
            ..response
        },
    ] {
        assert_eq!(
            control.0.complete(invalid).unwrap_err().kind(),
            io::ErrorKind::InvalidData,
            "{invalid:?}"
        );
    }
    control.0.complete(response).unwrap();
    pending.wait(Duration::from_secs(1)).unwrap();
    assert_eq!(
        control.0.complete(response).unwrap_err().kind(),
        io::ErrorKind::InvalidData
    );
}

#[test]
fn disconnect_wakes_the_waiter_and_refuses_new_requests() {
    let (control, _peer) = pair();
    let pending = control.start_seal().unwrap();
    control.disconnect();
    control.disconnect();
    assert_eq!(
        pending
            .wait(Duration::from_secs(1))
            .unwrap_err()
            .raw_os_error(),
        Some(libc::EPIPE)
    );
    assert_eq!(
        control.start_seal().err().unwrap().kind(),
        io::ErrorKind::BrokenPipe
    );
}

#[test]
fn abandoning_a_request_terminates_the_session() {
    let (control, _peer) = pair();
    drop(control.start_seal().unwrap());
    assert_eq!(
        control.start_seal().err().unwrap().kind(),
        io::ErrorKind::BrokenPipe
    );
}

#[test]
fn timeout_counts_time_since_submission_and_terminates_the_session() {
    let (control, _peer) = pair();
    let mut pending = control.start_seal().unwrap();
    pending.started = Instant::now() - Duration::from_secs(2);
    assert_eq!(
        pending.wait(Duration::from_secs(1)).unwrap_err().kind(),
        io::ErrorKind::TimedOut
    );
    assert_eq!(
        control.start_seal().err().unwrap().kind(),
        io::ErrorKind::BrokenPipe
    );
}

#[test]
fn id_exhaustion_cannot_send_a_wrapped_id() {
    let (control, mut peer) = pair();
    control.0.writer.lock().unwrap().sequence = u64::MAX - 1;
    let final_request = control.start_seal().unwrap();
    let request = Frame::read(&mut peer).unwrap();
    assert_eq!(request.id, u64::MAX);
    control.0.complete(completion(request, 0)).unwrap();
    final_request.wait(Duration::from_secs(1)).unwrap();
    assert!(control.start_seal().is_err());
    peer.set_nonblocking(true).unwrap();
    assert_eq!(
        Frame::read(&mut peer).unwrap_err().kind(),
        io::ErrorKind::WouldBlock
    );
}

#[test]
fn every_representable_positive_errno_is_a_valid_completion() {
    let (control, mut peer) = pair();
    for errno in [libc::EIO, i32::MAX] {
        let pending = control.start_seal().unwrap();
        let request = Frame::read(&mut peer).unwrap();
        control.0.complete(completion(request, errno)).unwrap();
        assert_eq!(
            pending
                .wait(Duration::from_secs(1))
                .unwrap_err()
                .raw_os_error(),
            Some(errno)
        );
    }
}

type Answers = mpsc::Receiver<Result<(), Option<i32>>>;

/// Starts one flush whose answer arrives on the returned channel, as the errno
/// it failed with. The channel disconnects without a message if the answer is
/// dropped unanswered.
fn start_flush(control: &Control) -> io::Result<Answers> {
    let (sender, receiver) = mpsc::channel();
    control.start_flush(move |result| {
        sender
            .send(result.map_err(|err| err.raw_os_error()))
            .unwrap()
    })?;
    Ok(receiver)
}

fn flush(id: u64) -> Frame {
    Frame {
        kind: wire::FLUSH,
        id,
        ..Frame::default()
    }
}

fn unanswered(answers: &Answers) {
    assert_eq!(answers.try_recv(), Err(mpsc::TryRecvError::Empty));
}

/// The answer ran and is gone, so it can never run again.
fn answered_once(answers: &Answers) {
    assert_eq!(answers.try_recv(), Err(mpsc::TryRecvError::Disconnected));
}

// A guest's flush waits for the host: the request goes out at once, and its
// answer is the RESULT that echoes it, however long the host takes.
#[test]
fn a_flush_is_pending_until_its_result() {
    let (control, mut peer) = pair();
    let answers = start_flush(&control).unwrap();
    assert_eq!(Frame::read(&mut peer).unwrap(), flush(1));
    unanswered(&answers);
    control.0.complete(completion(flush(1), 0)).unwrap();
    assert_eq!(answers.try_recv(), Ok(Ok(())));
    answered_once(&answers);
}

// Flushes and seals are one sequence of requests, so the pager sees their IDs
// rise whatever the mix, and it answers them in whatever order it finishes
// them. Each flush is answered once, with its own result.
#[test]
fn flushes_and_seals_share_one_sequence_and_complete_in_any_order() {
    let (control, mut peer) = pair();
    let first = start_flush(&control).unwrap();
    let seal = control.start_seal().unwrap();
    let second = start_flush(&control).unwrap();
    let sent: Vec<Frame> = (0..3).map(|_| Frame::read(&mut peer).unwrap()).collect();
    assert_eq!(
        sent,
        [
            flush(1),
            Frame {
                kind: wire::SEAL,
                id: 2,
                ..Frame::default()
            },
            flush(3)
        ]
    );
    control.0.complete(completion(flush(3), libc::EIO)).unwrap();
    assert_eq!(second.try_recv(), Ok(Err(Some(libc::EIO))));
    unanswered(&first);
    control.0.complete(completion(sent[1], 0)).unwrap();
    seal.wait(Duration::from_secs(1)).unwrap();
    unanswered(&first);
    control.0.complete(completion(flush(1), 0)).unwrap();
    assert_eq!(first.try_recv(), Ok(Ok(())));
    assert_eq!(
        control
            .0
            .complete(completion(flush(1), 0))
            .unwrap_err()
            .kind(),
        io::ErrorKind::InvalidData,
        "a flush was answered twice"
    );
    answered_once(&first);
    answered_once(&second);
}

// A session that ends leaves nobody to answer its flushes, so each fails with
// EPIPE rather than holding its guest's flush forever, and a flush asked for
// afterwards is refused without its answer ever running.
#[test]
fn a_disconnect_fails_every_pending_flush() {
    let (control, mut peer) = pair();
    let first = start_flush(&control).unwrap();
    let second = start_flush(&control).unwrap();
    Frame::read(&mut peer).unwrap();
    Frame::read(&mut peer).unwrap();
    control.disconnect();
    assert_eq!(first.try_recv(), Ok(Err(Some(libc::EPIPE))));
    assert_eq!(second.try_recv(), Ok(Err(Some(libc::EPIPE))));
    answered_once(&first);
    answered_once(&second);
    let (sender, receiver) = mpsc::channel::<()>();
    assert_eq!(
        control
            .start_flush(move |_| sender.send(()).unwrap())
            .unwrap_err()
            .kind(),
        io::ErrorKind::BrokenPipe
    );
    assert_eq!(receiver.try_recv(), Err(mpsc::TryRecvError::Disconnected));
}

// A flush the socket would not take was never sent, so nothing will answer it:
// its caller is told by the error and its answer never runs. The session is
// over, and the flushes it had already sent fail with it.
#[test]
fn a_flush_that_cannot_be_sent_is_refused_and_ends_the_session() {
    let (control, peer) = pair();
    let sent = start_flush(&control).unwrap();
    drop(peer);
    let (sender, receiver) = mpsc::channel::<()>();
    assert!(
        control
            .start_flush(move |_| sender.send(()).unwrap())
            .is_err()
    );
    assert_eq!(receiver.try_recv(), Err(mpsc::TryRecvError::Disconnected));
    assert_eq!(sent.try_recv(), Ok(Err(Some(libc::EPIPE))));
}
