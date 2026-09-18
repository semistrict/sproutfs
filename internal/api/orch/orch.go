// Package orch is the wire protocol of the demo orchestrator: what the CLI
// asks for, what a draining host asks for, and the client both reach it with.
//
// The orchestrator is stateless. Everything it reports it learned from the
// Kubernetes API, which is where the hosts are, or from the bucket and the
// hosts themselves, which is where the VMs are.
package orch

import (
	"time"

	"github.com/semistrict/sproutfs/internal/api/host"
	"github.com/semistrict/sproutfs/internal/jsonhttp"
)

// Error is the shared failure body of the whole demo control plane.
type Error = jsonhttp.Error

// Seconds is a duration on the wire, the same one the host API reports.
type Seconds = host.Seconds

// Host is one host pod as the orchestrator found it.
type Host struct {
	// Name is the pod's name, which is what the CLI names a host by.
	Name string `json:"name"`
	// API is the origin of the host's HTTP API and Page the address its
	// migration page server is reached at.
	API  string `json:"api"`
	Page string `json:"page"`
	// Ready is the pod's own readiness, and Running what the host answered when
	// asked what it runs. Error is why it did not answer, if it did not.
	Ready   bool     `json:"ready"`
	Running []string `json:"running"`
	Serving []string `json:"serving"`
	Error   string   `json:"error,omitempty"`
	// Pager is what the host's shared frame store holds, as it reported it. Its
	// SharedPages is the demo's sharing measure, which is what forking a running
	// guest is visible in.
	Pager host.Pager `json:"pager"`
	// Pages is what this host's page server has answered: the pages a
	// destination of a migration, or a fork placed on another host, pulled out
	// of its frames.
	Pages host.Pages `json:"pages"`
	// Store is what that host's object store has served since it started, per
	// operation. It is what a checkpoint model costs in object traffic.
	Store host.Store `json:"store"`
}

// VM is one VM as the orchestrator assembled it: a control record in the
// bucket, and the host that reports running it.
type VM struct {
	ID string `json:"id"`
	// Host is the host that runs it, empty for a VM no live host reports, which
	// is what a VM whose host was killed looks like until it is recovered.
	Host string `json:"host,omitempty"`
	// Checkpoint is the sequence the VM's control record selects and Epoch the
	// writer token it holds.
	Checkpoint uint64 `json:"checkpoint"`
	Epoch      uint64 `json:"epoch"`
	// Template is the guest image it was created from, when the host that runs
	// it still remembers, and Parent the VM it was forked from.
	Template string `json:"template,omitempty"`
	Parent   string `json:"parent,omitempty"`
	// State is what the orchestrator's table last recorded this VM doing:
	// creating, running, migrating, stopped or recovering. From and To are the
	// hosts of a migration that is in flight, and empty otherwise.
	State string `json:"state,omitempty"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	// LossWindow is how long the host running this VM has held a write no
	// checkpoint of it covers, which is what losing that host would cost it in
	// time. Waiting reports a VM past its host's window, whose stores the pager
	// is holding back until a checkpoint of it lands. Both are zero for a VM no
	// live host reports, which has nothing unpublished anywhere.
	LossWindow time.Duration `json:"loss_window,omitempty"`
	Waiting    bool          `json:"waiting,omitempty"`
}

// ExecRequest and ExecResult are the guest agent's own shapes, carried down to
// the VM's host and into its guest unchanged.
type (
	ExecRequest = host.ExecRequest
	ExecResult  = host.ExecResult
)

// ExecOutcome is one command run in a guest, with the host it went through.
type ExecOutcome struct {
	Host   string     `json:"host"`
	VM     string     `json:"vm"`
	Result ExecResult `json:"result"`
}

// Drain phases. A draining host reports each of its VMs twice: once when it
// asks for the VM to be moved, and once when that has finished or failed.
const (
	DrainStarted  = "start"
	DrainFinished = "done"
)

// DrainReport is what a draining host tells the orchestrator about one VM. The
// orchestrator drives the migration itself, so this adds nothing it could not
// work out — except the moment: a table read during a drain shows which VMs the
// host has already asked about and which it has not reached.
type DrainReport struct {
	Host  string `json:"host"`
	VM    string `json:"vm"`
	Phase string `json:"phase"`
	// Error is why the handover failed, for a finished phase that did not.
	Error string `json:"error,omitempty"`
}

// CreateRequest creates one VM. The orchestrator allocates its identity and
// places it on the host running the fewest.
type CreateRequest struct {
	Template string `json:"template,omitempty"`
}

// CreateResult is where the VM went and what its creation cost.
type CreateResult struct {
	Host   string            `json:"host"`
	Result host.CreateResult `json:"result"`
}

// ForkRequest asks for Count forks of one running VM. Zero means one. To is the
// host the children run on, by pod name; empty is the parent's own host, where
// a child shares its parent's frames rather than pulling them over the network.
type ForkRequest struct {
	Count int    `json:"count,omitempty"`
	To    string `json:"to,omitempty"`
}

// ForkResult reports one fork: the children it created, in the order they were
// taken, and what it cost. Host is the parent's host and To the host the
// children run on, which is the same host unless the fork was placed elsewhere.
//
// Capture is the pause the parent paid, once: every child of one fork starts
// from one instant of it. Start is how long the children took to be running.
type ForkResult struct {
	Host     string   `json:"host"`
	To       string   `json:"to"`
	Children []string `json:"children"`
	Capture  Seconds  `json:"capture_seconds"`
	Start    Seconds  `json:"start_seconds"`
	Total    Seconds  `json:"total_seconds"`
}

// MigrateRequest names the destination host by pod name. An empty To lets the
// orchestrator choose, which is what a draining host asks for.
type MigrateRequest struct {
	To string `json:"to,omitempty"`
}

// MigrateResult reports one whole migration: the source's stop, the
// destination's resume and the stream behind it.
type MigrateResult struct {
	VM   string `json:"vm"`
	From string `json:"from"`
	To   string `json:"to"`
	// Pause is from the source stopping the guest to the destination resuming
	// it, and Stream how long the source's pages took to follow.
	Pause  Seconds `json:"pause_seconds"`
	Stream Seconds `json:"stream_seconds"`
	// PeerPages came from the source host and VolumePages from object storage.
	PeerPages   int64   `json:"peer_pages"`
	VolumePages int64   `json:"volume_pages"`
	Unpublished int64   `json:"unpublished"`
	Total       Seconds `json:"total_seconds"`
}

// CaptureResult reports one explicit checkpoint taken on the host running the
// VM.
type CaptureResult struct {
	Host   string             `json:"host"`
	Result host.CaptureResult `json:"result"`
}

// RecoverRequest reopens a VM whose host is gone. Force is the operator's own
// evidence of that loss, which a recovery needs when the Kubernetes API still
// lists a pod that did not answer: without it such a recovery is refused rather
// than risk fencing a host whose guest is running perfectly well.
type RecoverRequest struct {
	Force bool `json:"force,omitempty"`
}

// RecoverResult reports a VM reopened from its last checkpoint on a host that
// was not running it, which is what a host loss is repaired by.
type RecoverResult struct {
	Host   string          `json:"host"`
	Result host.OpenResult `json:"result"`
}

// StopResult reports one VM stopped: the host that published its last writes
// and closed it. The VM is still there — its control record and its objects are
// where they were — so a start opens it again at exactly those bytes.
type StopResult struct {
	VM   string `json:"vm"`
	Host string `json:"host"`
	// Checkpoint is the sequence the stop published, which is the instant a
	// start brings the VM back at.
	Checkpoint uint64  `json:"checkpoint"`
	Total      Seconds `json:"total_seconds"`
}

// StartRequest opens a stopped VM again. An empty To places it on the ready
// host whose guests have promised the least of its arena; a named one puts it
// there, and is refused if that host has no room for the guest.
type StartRequest struct {
	To string `json:"to,omitempty"`
	// Cold brings the VM back without its memory: the host discards every page
	// of it and the VMM state with it in one checkpoint, and boots the kernel
	// from the root volume, which is exactly what the last checkpoint published.
	// The guest's filesystem sees that as a power cut after that checkpoint.
	Cold bool `json:"cold,omitempty"`
	// Memory is the size the VM's RAM takes from here and Disk the size its root
	// volume grows to; zero keeps the size it has. A cold boot is the one moment
	// a VM's shape can change, because nothing in memory describes it any more,
	// so both are refused without Cold. A disk may only grow, and the guest
	// grows its filesystem over the new pages after the boot.
	Memory uint64 `json:"memory,omitempty"`
	Disk   uint64 `json:"disk,omitempty"`
}

// StartResult reports a stopped VM running again: where it went and the
// checkpoint it came back at. It is a recovery's own report, because a start is
// a recovery without the evidence of a loss — the VM was stopped deliberately
// and the host that ran it closed it.
type StartResult = RecoverResult

// CheckResult is one run of the deployment check over the bucket: whether the
// durable state agrees with itself, and every way it does not.
//
// Violations are not a failed request. The request worked; the answer is what
// is wrong with the deployment, so it is a body rather than a status line, and
// a caller that wants an exit code reads OK.
type CheckResult struct {
	OK         bool        `json:"ok"`
	Violations []Violation `json:"violations,omitempty"`
}

// Violation is one thing wrong with the deployment's durable objects: the
// object key it is about, what is wrong, and the class of host loss that could
// excuse it — "violation" for one nothing excuses.
type Violation struct {
	Key     string `json:"key,omitempty"`
	Class   string `json:"class"`
	Message string `json:"error"`
}

// KillResult reports a host pod deleted, which is the demo's host loss.
type KillResult struct {
	Host string `json:"host"`
}

// Console is one window on a VM's serial console, proxied from its host.
type Console = host.Console

// ConsoleWrite is what a client types into a VM's console.
type ConsoleWrite = host.ConsoleWrite
