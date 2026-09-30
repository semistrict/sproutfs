package host

import (
	"sync/atomic"
	"time"

	"github.com/semistrict/sproutfs/internal/latency"
)

// activity is what this host has done since it started, counted as it
// happens. It is what the status and the metrics report, and it lives only as
// long as the process: a host that exits takes its counts with it, and nothing
// here is read by anything that decides what the host does.
type activity struct {
	checkpoints checkpointActivity
}

// checkpointActivity is what the interval loop's checkpoints did.
type checkpointActivity struct {
	attempts, published, captureFailed, publishFailed, fenced atomic.Uint64
	uploadedBytes                                             atomic.Uint64
	pause, upload                                             latency.Histogram
}

// Activity is a snapshot of what this host has done since it started.
type Activity struct {
	Checkpoints CheckpointActivity
}

// CheckpointActivity is what the interval checkpoints did: every attempt, and
// how each ended. Published and the three failures add up to the attempts
// that have ended. Pause is what the guest paid for each, and Upload what
// publishing it took behind the running guest.
type CheckpointActivity struct {
	Attempts, Published                  uint64
	CaptureFailed, PublishFailed, Fenced uint64
	UploadedBytes                        uint64
	Pause, Upload                        latency.Snapshot
}

// Activity reports what this host has done since it started.
func (h *Host) Activity() Activity {
	c := &h.activity.checkpoints
	return Activity{Checkpoints: CheckpointActivity{
		Attempts: c.attempts.Load(), Published: c.published.Load(),
		CaptureFailed: c.captureFailed.Load(), PublishFailed: c.publishFailed.Load(),
		Fenced: c.fenced.Load(), UploadedBytes: c.uploadedBytes.Load(),
		Pause: c.pause.Snapshot(), Upload: c.upload.Snapshot(),
	}}
}

// captured records an interval checkpoint whose pause has ended, well or not.
func (c *checkpointActivity) captured(pause time.Duration, err error, fenced bool) {
	c.attempts.Add(1)
	switch {
	case err == nil:
		c.pause.Observe(pause)
	case fenced:
		c.fenced.Add(1)
	default:
		c.captureFailed.Add(1)
	}
}

// ended records how the publication of a captured checkpoint ended.
func (c *checkpointActivity) ended(upload time.Duration, bytes int64, err error, fenced bool) {
	switch {
	case err == nil:
		c.published.Add(1)
		c.upload.Observe(upload)
		c.uploadedBytes.Add(uint64(max(bytes, 0)))
	case fenced:
		c.fenced.Add(1)
	default:
		c.publishFailed.Add(1)
	}
}
