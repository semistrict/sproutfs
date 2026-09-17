use std::io;
use std::net::Shutdown;
use std::os::unix::net::UnixStream;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, mpsc};
use std::time::{Duration, Instant};

use crate::wire::{self, Frame};

#[cfg(test)]
#[path = "tests/control.rs"]
mod tests;

type Completion = (u64, mpsc::SyncSender<Result<(), i32>>);

pub(crate) struct Requests {
    writer: Mutex<UnixStream>,
    pending: Mutex<Option<Completion>>,
    sequence: AtomicU64,
    closed: AtomicBool,
}

/// Device-side control, separate from the thread servicing mapping commands.
///
/// It owns no mappings. Keep the Session alive until all memory users stop.
/// Sessions proceed independently. Only one seal may be pending per session; a
/// second caller receives WouldBlock.
#[derive(Clone)]
pub struct Control(pub(crate) Arc<Requests>);

impl Requests {
    pub(crate) fn new(socket: UnixStream) -> io::Result<Arc<Self>> {
        socket.set_write_timeout(Some(Duration::from_secs(30)))?;
        Ok(Arc::new(Self {
            writer: Mutex::new(socket),
            pending: Mutex::new(None),
            sequence: AtomicU64::new(0),
            closed: AtomicBool::new(false),
        }))
    }

    pub(crate) fn send(&self, frame: Frame) -> io::Result<()> {
        if self.closed.load(Ordering::Acquire) {
            return Err(io::ErrorKind::BrokenPipe.into());
        }
        frame.write(&mut self.writer.lock().unwrap())
    }

    pub(crate) fn complete(&self, frame: Frame) -> io::Result<()> {
        let invalid = || io::Error::new(io::ErrorKind::InvalidData, "invalid seal completion");
        if frame.kind != wire::RESULT
            || frame.offset != 0
            || frame.len != 0
            || frame.backing != 0
            || frame.generation != 0
            || frame.flags > i32::MAX as u64
        {
            return Err(invalid());
        }
        let mut pending = self.pending.lock().unwrap();
        let Some((id, _)) = pending.as_ref() else {
            return Err(invalid());
        };
        if *id != frame.id {
            return Err(invalid());
        }
        let (_, sender) = pending.take().unwrap();
        let result = if frame.flags == 0 {
            Ok(())
        } else {
            Err(frame.flags as i32)
        };
        let _ = sender.send(result);
        Ok(())
    }

    pub(crate) fn close(&self) {
        self.closed.store(true, Ordering::Release);
        // Wake a request before waiting on the writer; its OS timeout bounds a
        // peer that stops receiving. The Session still owns UFFD and mappings.
        if let Some((_, sender)) = self.pending.lock().unwrap().take() {
            let _ = sender.send(Err(libc::EPIPE));
        }
        let _ = self.writer.lock().unwrap().shutdown(Shutdown::Both);
    }
}

impl Control {
    /// Ends the control service without releasing mappings. The embedder must
    /// stop memory users, join the service thread, and only then drop Session.
    pub fn disconnect(&self) {
        self.0.close();
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
        let pending = &self.0.pending;
        let id = self
            .0
            .sequence
            .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |id| id.checked_add(1))
            .map_err(|_| io::Error::other("seal IDs exhausted"))?
            + 1;
        let (sender, receiver) = mpsc::sync_channel(1);
        {
            let mut pending = pending.lock().unwrap();
            if pending.is_some() {
                return Err(io::ErrorKind::WouldBlock.into());
            }
            *pending = Some((id, sender));
        }
        if let Err(err) = self.0.send(Frame {
            kind: wire::SEAL,
            id,
            ..Frame::default()
        }) {
            self.0.close();
            return Err(err);
        }
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
