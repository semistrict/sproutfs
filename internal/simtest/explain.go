package simtest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
)

// Snapshot is the whole deployment at one moment, as the real code reports it,
// for the interactive explainer (TASK-65): which hosts are up, which VMs each
// runs, every page of every volume of those VMs, what each host holds for a
// handover, and what the store's control records select.
type Snapshot struct {
	Hosts   []HostView   `json:"hosts"`
	Records []RecordView `json:"records"`
}

// HostView is one host: whether its process runs, the handovers it holds pages
// for (Status().Serving) and the receives in flight on it, and the VMs it runs.
type HostView struct {
	Name      string   `json:"name"`
	Up        bool     `json:"up"`
	Holding   []string `json:"holding"`
	Receiving []string `json:"receiving"`
	VMs       []VMView `json:"vms"`
}

// VMView is one VM a host runs: the checkpoint its handle selects, whether a
// fork point holds its pages sealed, whether it is a fork whose own first
// checkpoint has not landed, and its volumes page by page.
type VMView struct {
	ID          string       `json:"id"`
	Parent      string       `json:"parent,omitempty"`
	Selected    uint64       `json:"selected,string"`
	Sealed      bool         `json:"sealed"`
	RootPending bool         `json:"root_pending"`
	Volumes     []VolumeView `json:"volumes"`
}

// VolumeView is one volume of a VM, page by page.
type VolumeView struct {
	Name      string     `json:"name"`
	PageBytes uint64     `json:"page_bytes"`
	Pages     []PageView `json:"pages"`
}

// PageView is one page: the checkpoint whose bytes it reads (VM and sequence,
// or none for a page no checkpoint holds), whether it is resident in this
// host's pager, and whether it holds bytes no checkpoint has — what a host's
// loss would cost.
type PageView struct {
	VM          string `json:"vm,omitempty"`
	Checkpoint  uint64 `json:"checkpoint,omitempty,string"`
	Resident    bool   `json:"resident"`
	Unpublished bool   `json:"unpublished"`
}

// RecordView is one VM's control record in the store: the checkpoint it
// selects, whether that checkpoint has landed, and the checkpoints forks
// pinned. Sequences are strings on the wire: they are 64-bit, and a
// JavaScript number holds 53 bits.
type RecordView struct {
	VM       string   `json:"vm"`
	Selected uint64   `json:"selected,string"`
	Created  bool     `json:"created"`
	Pinned   []string `json:"pinned,omitempty"`
}

// Snapshot reads the deployment as it is now. A VM whose volumes or memory
// cannot be read at this moment is an error: the explainer shows what the code
// reports, or nothing.
func (w *World) Snapshot(ctx context.Context) (Snapshot, error) {
	var snapshot Snapshot
	for index, h := range w.hosts {
		view := HostView{Name: h.name, Holding: []string{}, Receiving: []string{}, VMs: []VMView{}}
		if running := w.up(index); running != nil {
			view.Up = true
			status := running.Status()
			view.Holding = append(view.Holding, status.Serving...)
			view.Receiving = append(view.Receiving, status.Receiving...)
		}
		snapshot.Hosts = append(snapshot.Hosts, view)
	}
	records, err := w.records()
	if err != nil {
		return Snapshot{}, err
	}
	w.mu.Lock()
	ids := slices.Sorted(func(yield func(string) bool) {
		for id := range w.instances {
			if !yield(id) {
				return
			}
		}
	})
	w.mu.Unlock()
	for _, id := range ids {
		record, err := records.Read(ctx, id)
		switch {
		case errors.Is(err, platform.ErrNotFound):
		case err != nil:
			return Snapshot{}, fmt.Errorf("%s: reading the record: %w", id, err)
		default:
			snapshot.Records = append(snapshot.Records, RecordView{VM: id, Selected: record.Selected,
				Created: record.Created, Pinned: sequenceStrings(record.Pinned)})
		}
		in, g := w.runningVM(id)
		if in == nil {
			continue
		}
		view, err := w.vmView(ctx, in, g)
		if err != nil {
			return Snapshot{}, fmt.Errorf("%s: %w", id, err)
		}
		snapshot.Hosts[in.host].VMs = append(snapshot.Hosts[in.host].VMs, view)
	}
	return snapshot, nil
}

// sequenceStrings spells checkpoint sequences as decimal strings.
func sequenceStrings(sequences []uint64) []string {
	spelled := make([]string, 0, len(sequences))
	for _, sequence := range sequences {
		spelled = append(spelled, strconv.FormatUint(sequence, 10))
	}
	return spelled
}

// vmView reads one running VM page by page: each page's identity from its
// volume, and whether it is resident and unpublished from its memory region.
func (w *World) vmView(ctx context.Context, in *instance, g *guest) (VMView, error) {
	vm := w.vm(in)
	if vm == nil {
		return VMView{}, errors.New("no handle on a running VM")
	}
	status := vm.Status()
	view := VMView{ID: in.spec.ID, Parent: in.spec.Parent, Selected: status.Checkpoint.Sequence,
		Sealed: status.Sealed, RootPending: status.Root}
	// Memory first, then the disks, which is how the explainer tells it.
	names := slices.Clone(g.names)
	slices.SortStableFunc(names, func(a, b string) int {
		return boolOrder(a != MemoryVolume) - boolOrder(b != MemoryVolume)
	})
	for _, name := range names {
		volume := vm.Volume(name)
		region := g.memoryRegions[name]
		if volume == nil || region == nil {
			continue
		}
		pageBytes := volume.PageSize()
		pages := make([]PageView, volume.Size()/pageBytes)
		extents, err := volume.Locate(ctx, 0, volume.Size())
		if err != nil {
			return VMView{}, fmt.Errorf("locating %s: %w", name, err)
		}
		for _, extent := range extents {
			if extent.Identity == control.ZeroIdentity {
				continue
			}
			for offset := extent.Offset; offset < extent.Offset+extent.Length; offset += pageBytes {
				pages[offset/pageBytes].VM = extent.Identity.Ref.VM
				pages[offset/pageBytes].Checkpoint = extent.Identity.Ref.Sequence
			}
		}
		resident, err := region.Resident()
		if err != nil {
			return VMView{}, fmt.Errorf("listing what %s holds resident: %w", name, err)
		}
		for _, page := range resident {
			pages[page].Resident = true
		}
		unpublished, err := region.Unpublished()
		if err != nil {
			return VMView{}, fmt.Errorf("listing what %s holds unpublished: %w", name, err)
		}
		for _, page := range unpublished {
			pages[page].Unpublished = true
		}
		view.Volumes = append(view.Volumes, VolumeView{Name: name, PageBytes: pageBytes, Pages: pages})
	}
	return view, nil
}

// boolOrder sorts false before true.
func boolOrder(b bool) int {
	if b {
		return 1
	}
	return 0
}
