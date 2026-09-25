package sim

import (
	"context"
	"io"
	"sync"

	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/platform"
)

type FailureMode uint8

const (
	CrashProcess FailureMode = iota
	PowerLoss
	DestroyMachine
	FailDisk
)

type ProcessConfig struct {
	ID      string
	Machine string
	Zone    string
	Disk    *Disk
}

// Process owns the lifetime of a simulated process and every goroutine or
// closer registered with it. Cancellation is cooperative: functions passed to
// Start and Go must observe their context.
type Process struct {
	runtime *Runtime
	config  ProcessConfig

	mu         *ctxsync.Mutex
	running    bool
	stopping   bool
	generation uint64
	ctx        context.Context
	cancel     context.CancelCauseFunc
	group      *sync.WaitGroup
	closers    []io.Closer
	stopped    chan struct{}
}

func (r *Runtime) NewProcess(config ProcessConfig) *Process {
	if config.ID == "" {
		panic("sim: process id must not be empty")
	}
	return &Process{runtime: r, config: config, mu: ctxsync.NewMutex()}
}

func (p *Process) Start(parent context.Context, run func(context.Context)) error {
	if run == nil {
		panic("sim: process function must not be nil")
	}
	if err := p.mu.Lock(parent); err != nil {
		return err
	}
	defer p.mu.Unlock()
	if p.running || p.stopping {
		return platform.ErrAlreadyExists
	}
	p.ctx, p.cancel = context.WithCancelCause(parent)
	p.group = new(sync.WaitGroup)
	p.closers = nil
	p.running = true
	p.stopping = false
	p.generation++
	generation := p.generation
	p.group.Go(func() { run(p.ctx) })
	p.runtime.trace.record(Event{Kind: "process", Resource: p.config.ID, Operation: "start", Outcome: "ok", LocalID: generation})
	return nil
}

// Go starts a process-owned goroutine. It returns ErrProcessStopped if the
// process has not been started or has already stopped.
func (p *Process) Go(ctx context.Context, run func(context.Context)) error {
	if run == nil {
		panic("sim: process function must not be nil")
	}
	if err := p.mu.Lock(ctx); err != nil {
		return err
	}
	defer p.mu.Unlock()
	if !p.running {
		return platform.ErrProcessStopped
	}
	processCtx := p.ctx
	group := p.group
	group.Go(func() { run(processCtx) })
	return nil
}

// Own registers a listener, connection, or other resource that must be closed
// when the process crashes.
func (p *Process) Own(ctx context.Context, closer io.Closer) error {
	if closer == nil {
		panic("sim: owned closer must not be nil")
	}
	if err := p.mu.Lock(ctx); err != nil {
		return err
	}
	defer p.mu.Unlock()
	if !p.running {
		return platform.ErrProcessStopped
	}
	p.closers = append(p.closers, closer)
	return nil
}

// Stop ends the process the orderly way: its context is cancelled and its
// goroutines and closers are waited for, and its disk is left exactly as it is.
// It is what a host that drained and closed does, and it is traced as a stop
// rather than a crash, so a campaign that asks the trace which hosts it lost is
// told about the kills and not about the shutdowns.
func (p *Process) Stop(ctx context.Context) error {
	return p.end(ctx, CrashProcess, "stop", false)
}

// Crash ends the process the way a machine does: nothing is given the chance to
// finish, and the disk is left in the state the mode names.
func (p *Process) Crash(ctx context.Context, mode FailureMode) error {
	return p.end(ctx, mode, "crash", true)
}

func (p *Process) end(ctx context.Context, mode FailureMode, operation string, faultDisk bool) error {
	if err := p.mu.Lock(ctx); err != nil {
		return err
	}
	if p.stopping {
		stopped := p.stopped
		p.mu.Unlock()
		return waitForProcessStop(ctx, stopped)
	}
	if !p.running {
		p.mu.Unlock()
		return platform.ErrProcessStopped
	}
	cancel := p.cancel
	group := p.group
	closers := append([]io.Closer(nil), p.closers...)
	generation := p.generation
	stopped := make(chan struct{})
	p.running = false
	p.stopping = true
	p.stopped = stopped
	p.mu.Unlock()

	cancel(platform.ErrProcessStopped)
	go p.finishCrash(mode, operation, faultDisk, generation, group, closers, stopped)
	return waitForProcessStop(ctx, stopped)
}

func (p *Process) Generation(ctx context.Context) (uint64, error) {
	if err := p.mu.Lock(ctx); err != nil {
		return 0, err
	}
	defer p.mu.Unlock()
	return p.generation, nil
}

func (p *Process) finishCrash(mode FailureMode, operation string, faultDisk bool, generation uint64, group *sync.WaitGroup, closers []io.Closer, stopped chan struct{}) {
	for _, closer := range closers {
		_ = closer.Close()
	}
	group.Wait()
	if faultDisk && p.config.Disk != nil {
		switch mode {
		case PowerLoss:
			_ = p.config.Disk.PowerLoss(context.Background())
		case DestroyMachine:
			_ = p.config.Disk.Destroy(context.Background())
		case FailDisk:
			p.config.Disk.Fail()
		}
	}
	_ = p.mu.Lock(context.Background())
	p.stopping = false
	p.ctx = nil
	p.cancel = nil
	p.group = nil
	p.closers = nil
	p.stopped = nil
	p.mu.Unlock()
	outcome := failureModeName(mode)
	if !faultDisk {
		outcome = "ok"
	}
	p.runtime.trace.record(Event{Kind: "process", Resource: p.config.ID, Operation: operation, Outcome: outcome, LocalID: generation})
	close(stopped)
}

func waitForProcessStop(ctx context.Context, stopped <-chan struct{}) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-stopped:
		return nil
	}
}

func failureModeName(mode FailureMode) string {
	switch mode {
	case CrashProcess:
		return "process"
	case PowerLoss:
		return "power_loss"
	case DestroyMachine:
		return "destroy_machine"
	case FailDisk:
		return "disk_failure"
	default:
		return "unknown"
	}
}
