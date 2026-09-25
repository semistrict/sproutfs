// Package host is the wire protocol of the demo host binary: the JSON a
// sproutfs-host serves on its API port, and the client the orchestrator and a
// draining host reach it with.
//
// It is the deployment's control plane and carries no authority: a VM's
// authority is the epoch in its control record, and a migration's pages cross
// the hosts' own authenticated network, not this one.
package host

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/semistrict/sproutfs/api/guest"
	"github.com/semistrict/sproutfs/internal/jsonhttp"
)

// StoreCount is what one object-store operation did: every call made, the ones
// that reported an error, and the object bytes the call moved. Only Get and Put
// move bytes; the other three leave Bytes zero.
type StoreCount struct {
	Calls    int64 `json:"calls"`
	Failures int64 `json:"failures"`
	Bytes    int64 `json:"bytes"`
}

// Store is what this host's object store has served since the process started,
// per operation. Everything the host writes goes through one store — control
// records, indexes, checkpoint pages and the deletions reclamation makes — so
// this is the whole of what the deployment costs in object traffic.
type Store struct {
	Head   StoreCount `json:"head"`
	Get    StoreCount `json:"get"`
	Put    StoreCount `json:"put"`
	Delete StoreCount `json:"delete"`
	List   StoreCount `json:"list"`
}

// Handoff is what a source host gives the control plane to start one of its VMs
// somewhere else, carried unread to the destination's Receive. It is plain data
// and stays plain data: the pages it describes never cross this API, only the
// hosts' own network.
//
// A fork's handoff is the same thing from a parent that keeps running: VMID is
// the child the destination creates rather than a VM the source released, and
// Parent names the checkpoint it inherits.
type Handoff struct {
	// VMID is the VM the source released, and State the captured VMM state the
	// destination restores. The checkpoint the destination opens is whatever the
	// VM's control record selects, which is the source's last interval checkpoint;
	// the writes since it come from the source's pages.
	VMID  string
	State []byte
	// Checkpoint is the sequence the source's control record selected when it
	// gave the VM up. A destination that opens a record selecting anything else
	// refuses the handoff: another writer got in, and streaming the source's
	// pages over that writer's checkpoint would make one VM's memory out of two
	// writers' pages. It is zero for a fork, whose child has no record yet.
	Checkpoint uint64 `json:",omitempty"`
	// Parent and ParentCheckpoint make this handoff a fork: the VM the child
	// inherits, and the sequence of the last checkpoint that VM published, which
	// it pinned in its own control record before the handoff. They are empty for
	// a migration, where the VM that moves is the VM that already existed.
	Parent           string `json:",omitempty"`
	ParentCheckpoint uint64 `json:",omitempty"`
	// Source is where this VM's pages are still served from, and PageSize the
	// page they are served in.
	Source   string
	PageSize int
	// MemoryRegions is the memory layout, in ascending name order.
	MemoryRegions []HandoffMemoryRegion
	// PausedAt is when the guest stopped, which with the destination's resume
	// bounds the pause the migration cost.
	PausedAt time.Time
}

// HandoffMemoryRegion names one memory region of a handed-over VM and the size of the volume
// it maps, so the destination can bind the same layout.
type HandoffMemoryRegion struct {
	Name string
	Size uint64
	// Unpublished names the pages of this memory region that no checkpoint of the VM
	// has: the guest's writes since the source's last checkpoint. They exist
	// only in the source's pages, so the destination must fetch every one of
	// them before the source may stop serving. The guest was stopped when this
	// was taken, so it is final, and the source's dirty budget bounds it, which
	// is what makes it data the control plane can carry.
	Unpublished []HandoffPageRun
	// UnpublishedAge is how long the source had held the oldest of those pages
	// when it gave the VM up, zero where it held none. The destination dates the
	// pages it receives from it, so the VM's loss window carries across the
	// handoff instead of restarting: a VM handed from host to host would
	// otherwise never reach a bound at all.
	UnpublishedAge time.Duration `json:",omitempty"`
}

// HandoffPageRun is a run of consecutive pages the source holds.
type HandoffPageRun struct {
	First uint64
	Count int
}

// ExecRequest and ExecResult are the guest agent's own shapes. A host does not
// interpret them: it carries the request down the VM's vsock and the answer
// back, so the caller reads exactly what the guest said.
type (
	ExecRequest = guest.ExecRequest
	ExecResult  = guest.ExecResult
)

// Error is the body of every failed request, shared with the orchestrator's own
// API so that one failure reads the same wherever it is reported.
type Error = jsonhttp.Error

// VM is one VM as the host that runs it sees it.
type VM struct {
	ID string `json:"id"`
	// Template is the guest image a created VM was forked from, empty for one
	// this host opened or received.
	Template string `json:"template,omitempty"`
	// Checkpoint is the sequence of the checkpoint this VM's control record
	// selects, and Epoch the writer token this host holds for it.
	Checkpoint uint64 `json:"checkpoint"`
	Epoch      uint64 `json:"epoch"`
	// Host is the host that reported this VM.
	Host string `json:"host"`
	// DirtyBytes is an upper bound on what losing this host would cost this VM.
	DirtyBytes uint64 `json:"dirty_bytes"`
	// LossWindow is how long this VM has held a write no checkpoint covers,
	// which is what losing this host would cost it in time rather than in
	// bytes. It is zero for a VM holding nothing unpublished. Waiting reports
	// that the window has been exceeded and the pager is admitting no further
	// dirty page for this VM, so its guest is stopped at its next store until a
	// checkpoint of it lands.
	LossWindow time.Duration `json:"loss_window"`
	Waiting    bool          `json:"waiting"`
	// PrivateBytes is the host memory this VM holds that its volumes do not:
	// the pages its guest has written since its last checkpoint, resident,
	// spilled or held by a checkpoint that has not landed. It is the part of
	// this VM's memory its host could share with nothing.
	PrivateBytes uint64 `json:"private_bytes"`
}

// Sharing is how much memory sharing a host's pager is retaining for one kind
// of memory region, at the moment its status was taken. It is a gauge rather than a
// total: Pager.SharedPages counts every page ever mapped to an already resident
// identity and never falls, which says how often sharing happened rather than
// how much of it is still there.
//
// It measures resident sharing alone. Pages a fork inherited and neither VM has
// faulted in are shared in the store and on the wire and cost this host nothing,
// so none of them are here.
type Sharing struct {
	// UniqueBytes is the host memory the arena actually holds: one resident
	// page counted once, however many memory regions map it. MappedBytes is the sum
	// over memory regions of the resident pages each maps, counting every alias, so a
	// page three memory regions map counts three times. SavedBytes is the difference,
	// which is the memory this host did not have to find.
	UniqueBytes uint64 `json:"unique_bytes"`
	MappedBytes uint64 `json:"mapped_bytes"`
	SavedBytes  uint64 `json:"saved_bytes"`
}

// PagerKind is what one of a host's two pagers holds, in its own page. Nothing
// here may be added to the other pager's: the two run different pages, so a
// count of one says nothing about the other. What a host-wide reading needs is
// bytes, which Pager carries.
type PagerKind struct {
	PageBytes int `json:"page_bytes"`
	// ArenaPages is how many pages this pager's arena holds and ResidentPages
	// how many of them are taken. The arena is the whole of a guest's resident
	// memory of this kind, so it is the capacity a VM placed here has to fit
	// into; ResidentPages is not what it has to fit into, because the arena is a
	// cache — a page of a VM that has gone stays resident until something else
	// needs the page.
	ArenaPages    int `json:"arena_pages"`
	ResidentPages int `json:"resident_pages"`
	DirtyPages    int `json:"dirty_pages"`
	LogicalPages  int `json:"logical_pages"`
	// LogicalPagesFree is what this pager's per-memory-region metadata cap still has
	// left, which is what admits a VM: a create, fork or receive whose memory regions
	// of this kind need more than this is refused before anything starts its
	// VMM.
	LogicalPagesFree int `json:"logical_pages_free"`
	// Sharing is how much of the sharing this pager holds is still there.
	Sharing Sharing `json:"sharing"`
	// Faults, Evictions, Spills and SharedPages are this pager's own counts of
	// what it has done. They are events rather than memory, so they may be read
	// together with the other pager's.
	SharedPages uint64 `json:"shared_pages"`
	Faults      uint64 `json:"faults"`
	Evictions   uint64 `json:"evictions"`
	Spills      uint64 `json:"spills"`
}

// ArenaBytes is what this pager's arena holds and ResidentBytes what is taken of
// it. Bytes are what a host-wide reading adds up, because pages of the two
// pagers are different sizes.
func (p PagerKind) ArenaBytes() uint64    { return uint64(p.ArenaPages) * uint64(p.PageBytes) }
func (p PagerKind) ResidentBytes() uint64 { return uint64(p.ResidentPages) * uint64(p.PageBytes) }
func (p PagerKind) DirtyBytes() uint64    { return uint64(p.DirtyPages) * uint64(p.PageBytes) }

// Pager is what the host's pagers hold: one report per kind of memory region, and the
// byte totals across the two. SharedPages is the demo's sharing measure: pages
// mapped to an already resident identity without a read, which is what a fork of
// a running guest inherits.
type Pager struct {
	// RAM and PMEM are the two pagers, each with its own arena, spill file and
	// page. They are reported apart because they are separate things for a
	// deployment to plan for and because their page counts cannot be added.
	RAM  PagerKind `json:"ram"`
	PMEM PagerKind `json:"pmem"`
	// CommittedBytes is the guest RAM the VMs this host runs have between them,
	// resident or not: the size of each running VM's RAM volume. It is what a VM
	// costs this host and what a placement measures it by, and the RAM arena
	// minus it is what another VM has to fit into.
	CommittedBytes uint64 `json:"committed_bytes"`
}

// The byte totals across both pagers, which is the only unit the two can be
// added in.
func (p Pager) ArenaBytes() uint64    { return p.RAM.ArenaBytes() + p.PMEM.ArenaBytes() }
func (p Pager) ResidentBytes() uint64 { return p.RAM.ResidentBytes() + p.PMEM.ResidentBytes() }

// SharedPages, Faults, Evictions and Spills are counts of what the two pagers
// have done rather than memory they hold, so they read together.
func (p Pager) SharedPages() uint64 { return p.RAM.SharedPages + p.PMEM.SharedPages }
func (p Pager) Faults() uint64      { return p.RAM.Faults + p.PMEM.Faults }
func (p Pager) Evictions() uint64   { return p.RAM.Evictions + p.PMEM.Evictions }
func (p Pager) Spills() uint64      { return p.RAM.Spills + p.PMEM.Spills }

// Pages is what this host's migration page server has answered.
type Pages struct {
	Requests int64 `json:"requests"`
	Served   int64 `json:"served"`
	Absent   int64 `json:"absent"`
	Refused  int64 `json:"refused"`
}

// Resources is what a host has: the RAM allotment its pager takes pages from,
// and the page cache's own separate cap. Disk is not shared or accounted —
// each concern that writes to the node's disk has a fixed cap of its own.
type Resources struct {
	MemoryLimit int64 `json:"memory_limit"`
	MemoryUsed  int64 `json:"memory_used"`
	CacheLimit  int64 `json:"cache_limit"`
	CacheUsed   int64 `json:"cache_used"`
}

// TemplatePrefix is the reserved identity namespace of the VMs that hold
// imported guest images. A template is an ordinary VM with an ordinary control
// record — that is what lets a fork of it inherit a published checkpoint
// without copying a byte — but it is not one of the deployment's VMs: nothing
// asked for it, nothing runs it, and a listing that showed it would offer
// operations on one host's own bookkeeping. The identities the deployment
// allocates are ULIDs under "vm-", so the two namespaces cannot collide.
const TemplatePrefix = "template-"

// TemplateID is the identity of the template one guest image is imported into:
// the sha256 of the image file, hex.
//
// A template is not a VM that lives on a host. It is an imported image, and an
// image's identity is its bytes: every host configured with one image names one
// template, so they import it once between them and only if it is not there
// already, a host that restarts imports nothing, and an image that changed
// under its name is a new template rather than the same one holding other
// bytes. Nothing about the host is in it, which is what lets the host pods be a
// Deployment with names nothing depends on.
//
// It is the one identity in the deployment named by content. It names the
// template and nothing below it: pages are still shared by their identity, and
// the checkpoints under this identity are its own like any other VM's.
func TemplateID(digest [sha256.Size]byte) string {
	return TemplatePrefix + hex.EncodeToString(digest[:])
}

// IsTemplate reports an identity in the template namespace, which is what a
// listing of the deployment's VMs leaves out.
func IsTemplate(id string) bool { return strings.HasPrefix(id, TemplatePrefix) }

// Template is one guest image this host can create VMs from: the name a create
// request selects, the RAM every VM forked from it gets, and whether this host
// has imported the image into a checkpoint yet. The size is the template's
// because a VM is a fork of the template and a fork inherits what it forked.
type Template struct {
	Name        string `json:"name"`
	MemoryBytes uint64 `json:"memory_bytes"`
	Imported    bool   `json:"imported"`
}

// Status is one host's whole report.
type Status struct {
	Host string `json:"host"`
	// PageAddress is where this host serves the memory of a VM it has handed
	// over, which is what another host's handoff names as its source.
	PageAddress string `json:"page_address"`
	// Running is what this host runs and Serving what it has migrated away and
	// still holds pages for. A drain is finished when Serving is empty.
	Running []string `json:"running"`
	Serving []string `json:"serving"`
	// Outstanding is, per VM in Serving, how many pages this host still holds
	// that no checkpoint has and that its destination has not fetched. It says
	// which of two things a name in Serving is: a handover still pulling its
	// pages across, or one that has them all and is only waiting for the word
	// that releases it. A VM whose volumes could not be listed reports -1.
	Outstanding map[string]int `json:"outstanding"`
	VMs         []VM           `json:"vms"`
	// Templates are the guest images this host can create VMs from, in name
	// order, which is what says how much memory a VM created here would need.
	Templates []Template `json:"templates"`
	Pager     Pager      `json:"pager"`
	Pages     Pages      `json:"pages"`
	Resources Resources  `json:"resources"`
	Store     Store      `json:"store"`
}

// CreateRequest creates one VM from a guest image. An empty Template selects
// the host's only configured one.
type CreateRequest struct {
	ID       string `json:"id"`
	Template string `json:"template,omitempty"`
}

// CreateResult reports the VM and where its time went. Template is the time
// spent making this host's template checkpoint of the image, which is zero for
// every VM after the first. Root is the new VM's own first checkpoint, taken
// between the fork and the boot: until it is published the VM is a fork of the
// template that runs here and nowhere else, so nothing could recover it and
// nothing could fork it. Nothing has run at that point, so the root seals no
// pages and uploads none — it writes its own index and the control record's
// selection of it.
type CreateResult struct {
	VM       VM      `json:"vm"`
	Template Seconds `json:"template_seconds"`
	Fork     Seconds `json:"fork_seconds"`
	Boot     Seconds `json:"boot_seconds"`
	Root     Seconds `json:"root_seconds"`
	Total    Seconds `json:"total_seconds"`
}

// OpenRequest opens a VM on this host. An empty one is the ordinary open: the
// VM comes back exactly where it was, restored from the VMM state its selected
// checkpoint holds.
//
// Cold is the other thing an operator can ask for, and the reasons are the
// reasons one reboots any machine: a guest that is wedged, a kernel or init
// change on the disk, or simply not to pay for memory the guest does not need
// to keep. The host discards every page of the VM's memory and the VMM state
// with it, in one checkpoint, and then boots the kernel from the root volume,
// which is exactly what the last checkpoint published. The guest's filesystem
// sees that as a power cut after that checkpoint.
type OpenRequest struct {
	Cold bool `json:"cold,omitempty"`
	// Memory is the size the VM's RAM takes from here, up or down, and Disk the
	// size its root volume grows to; zero keeps the size the VM has. A cold boot
	// is the one moment a VM's shape can change, because nothing in memory
	// describes it any more, so both are refused for a warm start. A disk may
	// only grow, and the guest grows its filesystem over the new pages after the
	// boot.
	Memory uint64 `json:"memory,omitempty"`
	Disk   uint64 `json:"disk,omitempty"`
}

// OpenResult reports a VM opened from its last checkpoint, which is what a host
// loss is recovered by.
type OpenResult struct {
	VM      VM      `json:"vm"`
	Restore Seconds `json:"restore_seconds"`
	Total   Seconds `json:"total_seconds"`
	// Cold reports a VM that came back without its memory: it booted its kernel
	// rather than being restored, and the checkpoint it is at is the one that
	// discarded the memory rather than the one the stop published.
	Cold bool `json:"cold,omitempty"`
}

// ForkRequest names the children one fork creates and, when they run elsewhere,
// the page-server address of the host that will run them. A fork is a migration
// handoff from a parent that keeps running, wherever the children land: with no
// destination this host takes them in itself, over the pages the seal froze,
// and with one it serves those pages to that host until it reports it has
// them.
//
// Every child named here starts from one pause of the parent, so a fan-out
// costs the parent one pause however many are asked for.
type ForkRequest struct {
	IDs         []string `json:"ids"`
	Destination string   `json:"destination,omitempty"`
}

// ForkResult reports one fork point and the handoff of every child taken from
// it. Capture is the pause the parent paid, once: it is running again before
// any child starts, and nothing was published to take it.
//
// The handoffs are what the control plane gives each child's destination — this
// host included — and it tells this one to release each child when that child
// has every page it inherited.
type ForkResult struct {
	Handoffs []Handoff `json:"handoffs,omitempty"`
	Capture  Seconds   `json:"capture_seconds"`
	Boot     Seconds   `json:"boot_seconds"`
	Total    Seconds   `json:"total_seconds"`
}

// CaptureResult reports one explicit checkpoint: the guest's pause, and how
// long the sealed pages took to become durable behind it.
type CaptureResult struct {
	VM         string  `json:"vm"`
	Checkpoint uint64  `json:"checkpoint"`
	Pause      Seconds `json:"pause_seconds"`
	Publish    Seconds `json:"publish_seconds"`
}

// StopRequest is how a VM is stopped. A plain stop publishes the VM's disks and
// discards its memory, so a start boots it over them; Suspend publishes its
// memory and its VMM state too, so a start resumes the guest where it was.
type StopRequest struct {
	Suspend bool `json:"suspend,omitempty"`
}

// StopResult reports one VM stopped: the checkpoint its last writes were
// published under, which is the pause it comes back at, and what the whole
// stop cost. The VM's control record and its objects stay where they are, so
// any host can open it again at exactly that checkpoint.
type StopResult struct {
	VM         string  `json:"vm"`
	Checkpoint uint64  `json:"checkpoint"`
	Seconds    Seconds `json:"seconds"`
}

// MigrateRequest names where the VM's pages will be fetched from once this host
// has stopped its guest.
type MigrateRequest struct {
	// Destination is the page-server address of the host that takes the VM.
	Destination string `json:"destination"`
}

// MigrateResult is the source half of a migration: the handoff the control
// plane carries to the destination, and the pause the stop cost.
type MigrateResult struct {
	Handoff Handoff `json:"handoff"`
	Stop    Seconds `json:"stop_seconds"`
}

// ReceiveResult is the destination half: the VM is running again when this
// returns, and the stream of the source's unpublished pages has completed.
type ReceiveResult struct {
	VM VM `json:"vm"`
	// Pause is from the source stopping the guest to this host resuming it,
	// which is the whole of what the migration cost the guest.
	Pause Seconds `json:"pause_seconds"`
	// Stream is how long the source's resident pages took to arrive behind the
	// running guest.
	Stream Seconds `json:"stream_seconds"`
	// PeerPages came from the source and VolumePages from object storage;
	// Fetched of Unpublished are the pages no checkpoint had.
	PeerPages   int64 `json:"peer_pages"`
	VolumePages int64 `json:"volume_pages"`
	Fetched     int64 `json:"fetched"`
	Unpublished int64 `json:"unpublished"`
}

// Console is a window on one VM's serial console, which a host retains as the
// newest 1 MiB of output in memory. Offset is where Data begins and Next the
// offset to ask for next. A read from before what is still retained begins at
// the oldest byte the host has, and Dropped says that the output between the
// requested offset and Offset is gone.
type Console struct {
	VM      string `json:"vm"`
	Offset  int64  `json:"offset"`
	Next    int64  `json:"next"`
	Dropped bool   `json:"dropped,omitempty"`
	Data    string `json:"data"`
}

// ConsoleWrite is what a client types into a VM's console.
type ConsoleWrite struct {
	Data string `json:"data"`
}

// DrainResult reports a drain: every VM this host handed over, and the ones it
// could not. It returns only when nothing is left to hand over.
type DrainResult struct {
	Host      string   `json:"host"`
	Moved     []string `json:"moved"`
	Remaining []string `json:"remaining"`
	Seconds   Seconds  `json:"seconds"`
}

// Seconds is a duration on the wire. JSON has no duration, and a demo that
// prints pause and stream times wants one unit everywhere.
type Seconds float64

// Since is the seconds elapsed since start.
func Since(start time.Time) Seconds { return Of(time.Since(start)) }

// Of converts a duration.
func Of(d time.Duration) Seconds { return Seconds(d.Seconds()) }

// Duration converts back, which is what a client that formats a timing wants.
func (s Seconds) Duration() time.Duration { return time.Duration(float64(s) * float64(time.Second)) }
