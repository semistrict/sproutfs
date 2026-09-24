use std::io;
use std::net::Shutdown;
use std::os::unix::net::UnixStream;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, mpsc};
use std::time::{Duration, Instant};

use crate::wire::{self, Frame};

#[cfg(test)]
#[path = "tests/control.rs"]
mod tests;

/// What a flush's answer is handed to. It runs on whichever thread reads the
/// answer, or closes the session, and must neither block nor call back into
/// the session's Control.
type Flushed = Box<dyn FnOnce(io::Result<()>) + Send>;

/// Who is waiting for the RESULT of one of this session's requests.
enum Answer {
    Seal(mpsc::SyncSender<Result<(), i32>>),
    Flush(Flushed),
}

impl Answer {
    fn give(self, result: Result<(), i32>) {
        match self {
            Answer::Seal(sender) => {
                let _ = sender.send(result);
            }
            Answer::Flush(flushed) => flushed(result.map_err(io::Error::from_raw_os_error)),
        }
    }
}

/// The socket, and the last request ID sent on it. The two share a lock so that
/// requests reach the wire in the order of their IDs, which is what the pager
/// checks them by.
pub(crate) struct Writer {
    socket: UnixStream,
    sequence: u64,
}

pub(crate) struct Requests {
    writer: Mutex<Writer>,
    /// Every request sent and not yet answered, with who is waiting for it.
    pending: Mutex<Vec<(u64, Answer)>>,
    closed: AtomicBool,
}

/// Device-side control, separate from the thread servicing mapping commands.
///
/// It owns no mappings. Keep the Session alive until all memory users stop.
/// Sessions proceed independently. Only one seal may be pending per session; a
/// second caller receives WouldBlock. Any number of flushes may be pending, and
/// each is answered once.
#[derive(Clone)]
pub struct Control(pub(crate) Arc<Requests>);

impl Requests {
    pub(crate) fn new(socket: UnixStream) -> io::Result<Arc<Self>> {
        socket.set_write_timeout(Some(Duration::from_secs(30)))?;
        Ok(Arc::new(Self {
            writer: Mutex::new(Writer {
                socket,
                sequence: 0,
            }),
            pending: Mutex::new(Vec::new()),
            closed: AtomicBool::new(false),
        }))
    }

    pub(crate) fn send(&self, frame: Frame) -> io::Result<()> {
        if self.closed.load(Ordering::Acquire) {
            return Err(io::ErrorKind::BrokenPipe.into());
        }
        frame.write(&mut self.writer.lock().unwrap().socket)
    }

    /// Sends one request under the next ID and records who waits for its
    /// answer. A request that could not be sent is not pending and is never
    /// answered: its caller is told by the error, and the session closes.
    fn request(&self, kind: u64, answer: Answer) -> io::Result<()> {
        let mut writer = self.writer.lock().unwrap();
        // Checked under the writer: close takes it after marking the session
        // closed, so a request that gets here first is pending before close
        // looks for the last time.
        if self.closed.load(Ordering::Acquire) {
            return Err(io::ErrorKind::BrokenPipe.into());
        }
        if matches!(answer, Answer::Seal(_))
            && (self.pending.lock().unwrap().iter()).any(|(_, a)| matches!(a, Answer::Seal(_)))
        {
            return Err(io::ErrorKind::WouldBlock.into());
        }
        let id = writer
            .sequence
            .checked_add(1)
            .ok_or_else(|| io::Error::other("request IDs exhausted"))?;
        writer.sequence = id;
        self.pending.lock().unwrap().push((id, answer));
        let sent = Frame {
            kind,
            id,
            ..Frame::default()
        }
        .write(&mut writer.socket);
        if let Err(err) = sent {
            self.pending
                .lock()
                .unwrap()
                .retain(|(pending, _)| *pending != id);
            drop(writer);
            self.close();
            return Err(err);
        }
        Ok(())
    }

    pub(crate) fn complete(&self, frame: Frame) -> io::Result<()> {
        let invalid = || io::Error::new(io::ErrorKind::InvalidData, "invalid request completion");
        if frame.kind != wire::RESULT
            || frame.offset != 0
            || frame.len != 0
            || frame.backing != 0
            || frame.generation != 0
            || frame.flags > i32::MAX as u64
        {
            return Err(invalid());
        }
        let answer = {
            let mut pending = self.pending.lock().unwrap();
            let Some(at) = pending.iter().position(|(id, _)| *id == frame.id) else {
                return Err(invalid());
            };
            pending.remove(at).1
        };
        answer.give(if frame.flags == 0 {
            Ok(())
        } else {
            Err(frame.flags as i32)
        });
        Ok(())
    }

    /// Answers every pending request with EPIPE.
    fn fail_pending(&self) {
        let pending = std::mem::take(&mut *self.pending.lock().unwrap());
        for (_, answer) in pending {
            answer.give(Err(libc::EPIPE));
        }
    }

    pub(crate) fn close(&self) {
        self.closed.store(true, Ordering::Release);
        // Wake the requests before waiting on the writer; its OS timeout bounds
        // a peer that stops receiving. The Session still owns UFFD and mappings.
        self.fail_pending();
        let _ = self.writer.lock().unwrap().socket.shutdown(Shutdown::Both);
        // And again, for a request that held the writer while the first pass
        // ran: it was pending by the time the writer came free.
        self.fail_pending();
    }
}

impl Control {
    /// Ends the control service without releasing mappings. The embedder must
    /// stop memory users, join the service thread, and only then drop Session.
    pub fn disconnect(&self) {
        self.0.close();
    }

    /// Asks the host to make durable a flush the guest made of this session's
    /// region, and hands its answer to `flushed`: success once the host has
    /// made it durable, or the errno it failed with. The host may take a disk
    /// checkpoint for it first, so the answer can be seconds away; this returns
    /// as soon as the request is written, and the device completes the guest's
    /// flush when `flushed` runs.
    ///
    /// `flushed` runs once, on the session's service thread when the answer
    /// arrives, or with EPIPE on whichever thread closes the session first. It
    /// must neither block nor call back into this Control. An error here is a
    /// request that was never sent, whose `flushed` never runs.
    pub fn start_flush(
        &self,
        flushed: impl FnOnce(io::Result<()>) + Send + 'static,
    ) -> io::Result<()> {
        self.0
            .request(wire::FLUSH, Answer::Flush(Box::new(flushed)))
    }

    /// Records the host's checkpoint of this session's region without moving
    /// its bytes, so this completes in page-table time and the embedder can
    /// resume its vCPUs at once. Stores into the checkpoint copy on write.
    /// The region publishes nothing newer until the publication that uploads
    /// the checkpoint retires it.
    ///
    /// Run this on a device/control thread while Session::run continues on its
    /// own thread. Issue every session's request before waiting on the handles,
    /// so the regions seal concurrently.
    pub fn start_seal(&self) -> io::Result<PendingSeal> {
        let started = Instant::now();
        let (sender, receiver) = mpsc::sync_channel(1);
        self.0.request(wire::SEAL, Answer::Seal(sender))?;
        Ok(PendingSeal {
            requests: self.0.clone(),
            receiver,
            started,
            finished: false,
        })
    }
}

/// One bounded pending seal request. Dropping it without observing its
/// completion terminates the control session rather than leaking a request.
pub struct PendingSeal {
    requests: Arc<Requests>,
    receiver: mpsc::Receiver<Result<(), i32>>,
    started: Instant,
    finished: bool,
}

impl PendingSeal {
    /// Waits until completion, with a timeout measured from request submission.
    ///
    /// The host's own deadline is what decides a seal, and it answers one it
    /// could not finish with a failure the embedder can resume from. This
    /// timeout must therefore be far longer than the host's: it is the backstop
    /// for a host that has stopped answering, and expiring closes the session,
    /// which ends the guest.
    pub fn wait(mut self, timeout: Duration) -> io::Result<()> {
        let result = match self
            .receiver
            .recv_timeout(timeout.saturating_sub(self.started.elapsed()))
        {
            Ok(Ok(())) => Ok(()),
            Ok(Err(errno)) => Err(io::Error::from_raw_os_error(errno)),
            Err(_) => {
                self.requests.close();
                Err(io::ErrorKind::TimedOut.into())
            }
        };
        self.finished = true;
        result
    }
}

impl Drop for PendingSeal {
    fn drop(&mut self) {
        if !self.finished {
            self.requests.close();
        }
    }
}
