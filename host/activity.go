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
	// migrations, forks and receives are the handovers this host began, as
	// source or destination, by outcome. The pause of a received VM is what its
	// guest paid from the source's pause to this host's resume.
	migrations, forks, receives outcomes
	migrationPause, forkPause   latency.Histogram
	// deaths, fenced and stopped are the VMs this host gave up: their VMM
	// ended on its own, a later writer took their record, or a pager ran out
	// of a bound and had this host stop them.
	deaths, fenced, stopped atomic.Uint64
	// imports are the templates this host wrote, by outcome, and importTime
	// what each took from its digest to its pin. imageBytes is every byte of
	// guest image this host read, for a digest or an import.
	imports    outcomes
	importTime latency.Histogram
	imageBytes atomic.Uint64
}

// outcomes counts one kind of operation by how it ended.
type outcomes struct{ succeeded, failed atomic.Uint64 }

func (o *outcomes) ended(err error) {
	if err == nil {
		o.succeeded.Add(1)
	} else {
		o.failed.Add(1)
	}
}

func (o *outcomes) snapshot() Outcomes {
	return Outcomes{Succeeded: o.succeeded.Load(), Failed: o.failed.Load()}
}

// Outcomes is how many of one kind of operation succeeded and failed.
type Outcomes struct{ Succeeded, Failed uint64 }

// checkpointActivity is what the interval loop's checkpoints did.
type checkpointActivity struct {
	attempts, published, captureFailed, publishFailed, fenced atomic.Uint64
	uploadedBytes                                             atomic.Uint64
	pause, upload                                             latency.Histogram
}

// Activity is a snapshot of what this host has done since it started.
type Activity struct {
	Checkpoints CheckpointActivity
	// Migrations and Forks are the handovers this host began as a source, and
	// Receives the ones it took in as a destination. MigrationPause and
	// ForkPause are what each received guest paid, from the source's pause
	// to its resume here.
	Migrations, Forks, Receives Outcomes
	MigrationPause, ForkPause   latency.Snapshot
	// Deaths, Fenced and Stopped are the VMs this host gave up: their VMM
	// ended on its own, a later writer took their record, or a pager had this
	// host stop them for a bound.
	Deaths, Fenced, Stopped uint64
	// Imports are the templates this host wrote, ImportTime what each took,
	// and ImageBytes every byte of guest image it read.
	Imports    Outcomes
	ImportTime latency.Snapshot
	ImageBytes uint64
	// Journal is what durable flush did.
	Journal JournalActivity
}

// JournalActivity is what durable flush did on this host: whether the mode
// is on and a journal disk is served, the flushes it answered by outcome,
// what each took from its arrival to its answer and what its capture took,
// and how much of the journal's ring its live entries hold.
type JournalActivity struct {
	DurableFlush, Served bool
	Flushes              Outcomes
	Flush, Capture       latency.Snapshot
	RingBytes, LiveBytes int64
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
	a := &h.activity
	return Activity{Checkpoints: CheckpointActivity{
		Attempts: c.attempts.Load(), Published: c.published.Load(),
		CaptureFailed: c.captureFailed.Load(), PublishFailed: c.publishFailed.Load(),
		Fenced: c.fenced.Load(), UploadedBytes: c.uploadedBytes.Load(),
		Pause: c.pause.Snapshot(), Upload: c.upload.Snapshot(),
	},
		Migrations: a.migrations.snapshot(), Forks: a.forks.snapshot(), Receives: a.receives.snapshot(),
		MigrationPause: a.migrationPause.Snapshot(), ForkPause: a.forkPause.Snapshot(),
		Deaths: a.deaths.Load(), Fenced: a.fenced.Load(), Stopped: a.stopped.Load(),
		Imports: a.imports.snapshot(), ImportTime: a.importTime.Snapshot(), ImageBytes: a.imageBytes.Load(),
		Journal: h.journalActivity(),
	}
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
