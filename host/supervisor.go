package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/vmmachine"
	"github.com/semistrict/sproutfs/vmmemory"
)

// DefaultRAMPageSize and PMEMPageSize are the pages the two pagers run. RAM's
// is a deployment's choice: 2 MiB on the HugeTLB pool by default, or 4 KiB on
// an arena of ordinary memory, where a guest's store copies, owns and
// publishes 4 KiB. 2 MiB is faster at every timing measured on 2026-09-23 —
// boot 0.75 s against 4.4, a third more updates a second over a heap nothing
// faults in — and costs about the same memory once forks do real work; 4 KiB
// holds a tenth of the memory only for forks that write little and scattered,
// such as a seeded database updated at random. PMEM's is
// always 2 MiB, on the pool, which is also the alignment Firecracker requires
// of a PMEM device. They are here rather than beside either pager so that the
// byte budgets a deployment divides are checked against the pages they will
// actually be counted in, and so that the statements cannot drift apart.
const (
	DefaultRAMPageSize = checkpoint.PageSize2MiB
	PMEMPageSize       = checkpoint.PageSize2MiB
)

// RAMPage is the RAM pager's page a configuration names: DefaultRAMPageSize
// where it names none, and an error for anything but 4 KiB and 2 MiB, which are
// the two arenas there are.
func RAMPage(size uint64) (uint64, error) {
	switch size {
	case 0:
		return DefaultRAMPageSize, nil
	case checkpoint.PageSize4KiB, checkpoint.PageSize2MiB:
		return size, nil
	}
	return 0, fmt.Errorf("a RAM page of %d bytes: want %d or %d", size, checkpoint.PageSize4KiB, checkpoint.PageSize2MiB)
}

// VMs is every operation this host's API offers, and nothing about how a VM is
// run. The supervisor implements it over Firecracker, and so does a test's
// fake, which is what lets the HTTP layer be served and tested anywhere.
type VMs interface {
	// Ready reports whether this host can take work: nil once every guest image
	// it is configured with has been imported into a template, and the reason
	// it cannot until then. A host that is not ready is not an endpoint of the
	// deployment's Service, so nothing places a VM on it — a create that landed
	// on a host still reading a guest image would wait out that import inside
	// the request, and a rollout would send every create to the pod that has
	// done the least work.
	Ready(ctx context.Context) error
	// Live reports whether this process can still do the work it exists for: nil
	// while it can, and why it cannot otherwise. It is the other question a
	// probe asks, and it has to be able to fail — a supervisor that has closed,
	// or a host whose own context has been cancelled, has released its pager and
	// its VMM processes and can serve nothing, and a pod like that answering a
	// probe out of the mux is one the kubelet leaves running for ever. A host
	// still importing its guest images is live and not ready, which is the whole
	// reason these are two questions.
	Live(ctx context.Context) error
	Status(ctx context.Context) (hostapi.Status, error)
	Create(ctx context.Context, request hostapi.CreateRequest) (hostapi.CreateResult, error)
	// ImportTemplate imports a guest image into the template its bytes name,
	// which any host can then create VMs from by that identity. An image that
	// is already imported costs one control record read.
	ImportTemplate(ctx context.Context, image io.Reader, request hostapi.ImportTemplateRequest) (hostapi.ImportTemplateResult, error)
	// Open opens a VM no host runs and starts its guest. An empty request
	// restores the VM exactly where it was, from the VMM state its selected
	// checkpoint holds; a cold one discards every page of its memory and that
	// state in one checkpoint and boots its kernel from the root volume
	// instead, at whatever shape the request asks for. A cold start this host
	// could not boot cold — it has no kernel configured — is refused before
	// anything is discarded.
	Open(ctx context.Context, id string, request hostapi.OpenRequest) (hostapi.OpenResult, error)
	Fork(ctx context.Context, parent string, request hostapi.ForkRequest) (hostapi.ForkResult, error)
	// Capture takes a checkpoint of a VM this host runs now. A request with
	// Into captures it into a new VM instead, which publishes its root and
	// never boots; the source keeps running.
	Capture(ctx context.Context, id string, request hostapi.CaptureRequest) (hostapi.CaptureResult, error)
	Console(ctx context.Context, id string, since int64) (hostapi.Console, error)
	WriteConsole(ctx context.Context, id string, data string) error
	Exec(ctx context.Context, id string, request hostapi.ExecRequest) (hostapi.ExecResult, error)
	Migrate(ctx context.Context, id string, destination platform.Address) (hostapi.MigrateResult, error)
	Receive(ctx context.Context, handoff hostapi.Handoff) (hostapi.ReceiveResult, error)
	Released(ctx context.Context, id string) error
	// Abandoned gives one handover up rather than handing it over: a fork's
	// child that will never be received, one whose destination refused it, one
	// whose fan-out failed. Whatever this host still holds for that VM goes, and
	// a fork's parent takes its sealed pages back and is checkpointed again.
	//
	// It refuses nothing, which is what parts it from Released: those pages are
	// going either way — the VM they belong to is one nothing will ever ask for
	// again — and a refusal would only leave a parent sealed for good.
	Abandoned(ctx context.Context, id string) error
	Drain(ctx context.Context) (hostapi.DrainResult, error)
	// Stop ends a VM this host runs and leaves it behind: a last checkpoint of
	// its disks — of its memory and VMM state too, when the request suspends
	// it — and then the VMM process, the pages and the handle go. Its control record and its objects stay, so any host
	// can open it again at the bytes the stop published — which is what makes
	// a stop different from losing the host, where the writes since the last
	// checkpoint go with it. Refused for a VM a fork point holds sealed, as
	// a delete is. It reports the checkpoint it published, which is the pause
	// the VM comes back at.
	Stop(ctx context.Context, id string, request hostapi.StopRequest) (hostapi.StopResult, error)
	Delete(ctx context.Context, id string) error
}

// Service is what one host process serves: the VMs, and the orderly shutdown
// that makes them durable.
type Service interface {
	VMs
	// Close stops every VM this host runs, publishing a final checkpoint of
	// each, and then releases the pager, the arena and the spill file in that
	// order.
	Close(ctx context.Context) error
}

var (
	// ErrRequest reports a request this host will not act on, whatever its VMs
	// are doing.
	ErrRequest = errors.New("invalid request")
	// ErrRunning and ErrNotRunning report a VM this host is not in a position
	// to act on: one it already runs, and one it does not.
	ErrRunning    = errors.New("the VM is already running on this host")
	ErrNotRunning = errors.New("this host does not run that VM")
)

// SupervisorConfig is the whole of a host process's configuration, which is its
// environment. A host owns no durable local state, so nothing here is
// remembered between process lifetimes: the object store holds every VM's
// authority and its data.
type SupervisorConfig struct {
	// ObjectStore is the deployment's object namespace: every VM's authority
	// and all of its data. Network carries migration pages between hosts, Disk
	// is the node disk the pager spills to, and Disks opens the local storage
	// of each VMM's private staging under the scratch directory. Which adapter
	// each of them is, is the command's choice and nothing else's.
	ObjectStore platform.ObjectStore
	Network     platform.Network
	Disk        platform.Disk
	Disks       platform.Disks
	// Clock is the passage of time every deadline this host keeps is measured
	// against, and Entropy the source the checkpoint interval's jitter is drawn
	// from. Nil is the operating system's, which is what a deployment runs on.
	Clock   platform.Clock
	Entropy platform.Entropy
	// PagePort is the port this host serves migration pages on.
	PagePort int
	// PodIP is the address this host advertises its API and its pages at, and PodName
	// its name to the orchestrator.
	PodIP, PodName, Namespace string
	// Orchestrator is where a drain asks for somewhere to put its VMs and
	// reports what it is doing with each of them, and APIToken the deployment's
	// shared bearer token, which this host's own API requires and every request
	// it makes to the orchestrator carries. An empty token is a deployment that
	// admits anyone, which only a host run by hand is.
	Orchestrator, APIToken string
	// HugepageDir is the pod's hugetlbfs mount. The PMEM arena is a MFD_HUGETLB
	// memfd rather than a file in it, but the mount is what the kubelet grants
	// the pod its HugeTLB allotment through, so its absence means there are no
	// huge pages to allocate and is worth failing on at startup. The RAM arena
	// is an ordinary memfd and does not touch the pool unless its page is 2 MiB.
	HugepageDir string
	// RAMPageSize is the RAM pager's page, as RAMPage reads it: zero is the
	// default 4 KiB. At 2 MiB the RAM arena's share comes out of the pod's
	// HugeTLB allotment as PMEM's does.
	RAMPageSize uint64
	// ScratchDir is the node-disk directory holding the pager's spill file and
	// the VMM scratch. A starting host wipes it: a restart is a host loss, so
	// nothing under it is authority for anything.
	ScratchDir string
	// ArenaBytes is the resident page store of each pager. The PMEM share comes
	// out of the pod's HugeTLB allotment and the RAM share out of the pod's
	// ordinary memory, so a node provisions the two separately. The two arenas
	// are separate memfds and their capacities sum to what the deployment gave
	// this host; whoever fills this in has already divided it, so nothing below
	// has a share to decide.
	ArenaBytes KindBytes
	// Arena is how both pagers divide their resident pages between the files
	// of their arenas. The zero value is vmmemory.ArenaShared.
	Arena vmmemory.ArenaMode
	// MemoryBytes is the host-wide RAM allotment both pagers take their pages
	// from. It is one budget because it is one machine's memory, and because
	// bytes are the only unit the two pagers' pages can be added in.
	MemoryBytes int64
	// CacheBytes caps the page cache and SpillBytes each pager's own spill file.
	// Disk is capped per concern rather than shared: neither can take what the
	// other needs, so there is no ledger between them.
	CacheBytes int64
	SpillBytes KindBytes
	// LogicalPages bounds per-memory-region metadata and DirtyPages the volatile
	// private state on RAM and spill together, each in the pages of the pager it
	// belongs to. DirtyPages is what fills a spill file, so SpillBytes is its
	// bound, per pager.
	LogicalPages, DirtyPages KindPages
	// Starter runs every VMM process this host takes over: this host prepares
	// a VM's memory and drives the process once it runs, and the Starter owns
	// everything else about it — the binary, a jailer, the kernel, the devices
	// and the vsock the host reaches the guest's agent through.
	Starter vmmachine.Starter
	// Templates are the guest images a VM can be created from, by the name a
	// request selects.
	Templates Templates
	// VMMemoryBytes is the RAM of a VM whose template names no size of its own.
	VMMemoryBytes uint64
	// CheckpointInterval is how often every VM this host runs is checkpointed,
	// which bounds what losing this host rewinds a guest by.
	CheckpointInterval time.Duration
	// LossWindow is how long a VM may hold a write no checkpoint covers before
	// the pager stops admitting dirty pages for it, which bounds that rewind in
	// time rather than only in bytes. Zero selects host.DefaultLossWindow and a
	// negative value disables it. It reaches both the pager, which holds the
	// guest back, and the host, which reports the window and hurries its retries
	// while one is exceeded.
	LossWindow time.Duration
	// FlushBound is how stale a VM's disks may be for a guest's flush to
	// complete at once; past it the flush waits for a checkpoint of them.
	// Zero selects twice the checkpoint interval and a negative value completes every
	// flush at once.
	FlushBound time.Duration
}

// Template is one guest image a VM can be created from: the image on this
// host's disk, and the RAM every VM created from it gets. The size belongs to
// the template because it is the template's own volume that fixes it: a VM is a
// fork of the template, and a fork inherits the size of what it forked.
type Template struct {
	Path        string
	MemoryBytes uint64
}

// Templates are the guest images a VM can be created from, by the name a create
// request selects.
type Templates map[string]Template

// Resolve reports the guest image a create request selects. An empty name takes
// the only configured template, which is what a single-image deployment makes
// every request mean.
func (t Templates) Resolve(name string) (string, Template, error) {
	if name == "" {
		if len(t) != 1 {
			return "", Template{}, fmt.Errorf("no template named, and this host has %d", len(t))
		}
		for only, entry := range t {
			return only, entry, nil
		}
	}
	entry, found := t[name]
	if !found {
		return "", Template{}, fmt.Errorf("no template named %q", name)
	}
	return name, entry, nil
}
