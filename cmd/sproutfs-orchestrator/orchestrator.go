package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/api/orch"
	"github.com/semistrict/sproutfs/volume"
)

// pod is one host pod as the Kubernetes API reports it. It is the whole of what
// placement needs: a name to address it by, an address to reach it at, and
// whether it says it is ready.
type pod struct {
	Name  string
	IP    string
	Ready bool
}

// pods is where the hosts are. The orchestrator holds no roster of its own: it
// lists the pods carrying the deployment's host label every time it is asked.
type pods interface {
	List(ctx context.Context) ([]pod, error)
	Delete(ctx context.Context, name string) error
}

// hostClient is one host's API, which the orchestrator is only ever a client
// of. *host.Client is one; a test supplies a fake.
type hostClient interface {
	Status(ctx context.Context) (host.Status, error)
	Create(ctx context.Context, request host.CreateRequest) (host.CreateResult, error)
	ImportTemplate(ctx context.Context, image io.Reader, request host.ImportTemplateRequest) (host.ImportTemplateResult, error)
	Open(ctx context.Context, id string, request host.OpenRequest) (host.OpenResult, error)
	Fork(ctx context.Context, parent string, request host.ForkRequest) (host.ForkResult, error)
	Capture(ctx context.Context, id string, request host.CaptureRequest) (host.CaptureResult, error)
	Console(ctx context.Context, id string, since int64) (host.Console, error)
	WriteConsole(ctx context.Context, id string, data string) error
	Exec(ctx context.Context, id string, request host.ExecRequest) (host.ExecResult, error)
	Migrate(ctx context.Context, id string, request host.MigrateRequest) (host.MigrateResult, error)
	Receive(ctx context.Context, handoff host.Handoff) (host.ReceiveResult, error)
	Released(ctx context.Context, id string) error
	Abandoned(ctx context.Context, id string) error
	Stop(ctx context.Context, id string, request host.StopRequest) (host.StopResult, error)
	Delete(ctx context.Context, id string) error
}

// records is where the VMs are: the control records of the deployment's object
// namespace, which exist whether or not any host is running the VM.
type records interface {
	List(ctx context.Context) ([]listing, error)
}

// listing is one control record as the bucket found it, which is the identity
// it belongs to: a record exists exactly while its VM does.
type listing struct {
	ID string
}

// live is the identities of the VMs that exist.
func live(found []listing) []string {
	ids := make([]string, 0, len(found))
	for _, entry := range found {
		ids = append(ids, entry.ID)
	}
	return ids
}

var (
	// errRequest reports a request the orchestrator will not act on.
	errRequest = errors.New("invalid request")
	// errNoHost reports that no host can take the work.
	errNoHost = errors.New("no host is available")
	// errNotFound reports a VM or host nothing knows about.
	errNotFound = errors.New("not found")
	// errRunning reports a recovery with no positive evidence that the VM's host
	// is gone: a live host still reports running it, or a host that could be
	// running it did not answer. Taking the epoch either way would fence a guest
	// that is perfectly fine.
	errRunning = errors.New("nothing shows that the VM's host is gone")
	// errContested reports two hosts claiming one VM. One of them is running a
	// guest whose writes can never be published, and nothing here can say which,
	// so no request that must name a VM's host is acted on until it is one.
	errContested = errors.New("two hosts claim the VM")
	// errLostSource reports the host that was holding a migrated VM's pages
	// having gone while its destination was still fetching them. Those pages
	// are gone with it, so the migration is ended rather than waited on.
	errLostSource = errors.New("the host holding the VM's pages is gone")
)

// orchestrator places VMs on hosts and carries handoffs between them. It is
// stateless: every answer is assembled from the Kubernetes API, the bucket and
// the hosts themselves.
type orchestrator struct {
	pods    pods
	records records
	// dial reaches one host's API, and identify allocates a VM identity.
	dial     func(pod) hostClient
	identify func() string
	// apiPort is the port a host serves its API on and pagePort the one it
	// serves migration pages on, which is what a handoff's destination is
	// named by.
	apiPort, pagePort int
	// audit runs the deployment check over the bucket, which is the one thing
	// this orchestrator reads out of the object namespace that is not a control
	// record. It is a function rather than a store and a prefix because that is
	// the whole of what /check needs, and it keeps volume.CheckDeployment's
	// allowances in one place: the command that knows what a live deployment
	// leaves behind. Nil is an orchestrator that cannot answer the question.
	audit func(ctx context.Context) error
	// table is where this orchestrator writes down which host every VM is on
	// and what it last asked to be done with it. It is not authority: every row
	// in it can be rebuilt by surveying the hosts and listing the bucket, which
	// is why a failure to write it is logged rather than failing the operation
	// the write was part of.
	table *table

	// sourceWatch is how often a migration in flight asks whether the host
	// holding its pages is still there. Zero selects SourceWatchInterval.
	sourceWatch time.Duration

	// recentHosts is the last survey and recentAt when it finished, which is
	// what spares a console being polled at a few hertz a fan-out per frame.
	surveyMu    sync.Mutex
	recentHosts []liveHost
	recentAt    time.Time
}

const (
	// hostStatusTimeout bounds one host's Status within a survey. A host's
	// status is an in-memory report, so this is generous for a pod network;
	// what it really bounds is a host that has stopped answering without
	// closing the connection, which would otherwise cost every request the
	// whole of the client's own timeout. Missing it costs that host nothing
	// beyond this survey: it is reported with its failure and asked again.
	hostStatusTimeout = 2 * time.Second
	// surveyInterval is how long one survey answers the deployment's read
	// requests for. Anything that moves a VM surveys afresh and drops what it
	// finds, so the only staleness this admits is a host that changed on its
	// own within the last second.
	surveyInterval = time.Second
)

// note records one VM's state. Nothing is authority here, so a table that could
// not be written is a diagnostic, not a failed operation: the next survey
// rebuilds what was missed.
func (o *orchestrator) note(ctx context.Context, row vmRecord) {
	if o.table == nil {
		return
	}
	if err := o.table.Record(ctx, row); err != nil {
		slog.ErrorContext(ctx, "sproutfs-orchestrator: recording a VM failed",
			"vm", row.ID, "state", row.State, "error", err)
	}
}

// forget removes a deleted VM from the table.
func (o *orchestrator) forget(ctx context.Context, id string) {
	if o.table == nil {
		return
	}
	if err := o.table.Forget(ctx, id); err != nil {
		slog.ErrorContext(ctx, "sproutfs-orchestrator: forgetting a VM failed", "vm", id, "error", err)
	}
}

// host is one live host and the client that reaches it.
type liveHost struct {
	report orch.Host
	client hostClient
	// vms is what the host said about each VM it holds, which is where a
	// listing's checkpoint and epoch come from.
	vms []host.VM
	// templates are the guest images this host can create VMs from, which is
	// what says how much memory a VM created here would need.
	templates []host.Template
}

// free is the memory a VM placed on this host has to fit into: its arena, less
// the guest RAM the VMs it already runs have between them. The arena is the
// whole of a guest's resident memory, so it is the capacity; what a VM costs
// that capacity is the RAM its guest was promised, which is why this counts
// that rather than VMs — a host running one 2 GiB guest would otherwise look
// emptier than one running three small ones.
//
// Arena residency is not the measure. The arena is a cache: a page of a VM that
// has been migrated away or deleted stays resident until something else needs
// the page, so occupancy only goes up and a host that has done work looks full
// whatever it is running. Measuring that way refuses every destination on a warm
// host, which is a drain with nowhere to go.
//
// A host that reports no arena at all is measured as empty, which is what an
// older host or a test that says nothing about memory is.
func (h liveHost) free() uint64 {
	// What a VM costs a host's memory is its RAM, so what is left for another is
	// the RAM arena rather than both: the PMEM arena holds the roots, which are
	// one image the whole fleet shares.
	pager := h.report.Pager
	arena := pager.RAM.ArenaBytes()
	if arena == 0 || pager.CommittedBytes >= arena {
		return 0
	}
	return arena - pager.CommittedBytes
}

// memoryOf reports what one VM of a template needs resident. An empty name is
// what a create that names no template sends, which every host resolves to its
// only one; a name no host reports is one the create itself will refuse, so it
// is admitted here and fails where it can say why.
func memoryOf(hosts []liveHost, template string) uint64 {
	var need uint64
	for _, h := range hosts {
		for _, known := range h.templates {
			if known.Name == template || (template == "" && len(h.templates) == 1) {
				need = max(need, known.MemoryBytes)
			}
		}
	}
	return need
}

// memoryFor reports the guest RAM one VM has, which is what a placement admits
// it against. The table is where that lives: a host reports the template of a
// VM it created and stops doing so the moment the VM moves, and a cold start
// can have given the VM memory its template never had.
//
// A VM the table wrote no memory down for is measured against its template, as
// it was before a VM's memory was its own; one the table has never heard of at
// all is admitted against nothing, wherever it is later forked, migrated or
// started.
func (o *orchestrator) memoryFor(ctx context.Context, hosts []liveHost, id string) uint64 {
	row := o.rowOf(ctx, id)
	if row.Memory != 0 {
		return row.Memory
	}
	return memoryOf(hosts, row.Template)
}

// rowOf reads one VM's table row, reporting an empty one for a VM the table has
// never heard of and for a table that could not be read: neither is evidence of
// anything, and a placement measured against nothing is the placement this had
// before it measured memory at all.
func (o *orchestrator) rowOf(ctx context.Context, id string) vmRecord {
	if o.table == nil {
		return vmRecord{}
	}
	row, found, err := o.table.VM(ctx, id)
	if err != nil {
		slog.WarnContext(ctx, "sproutfs-orchestrator: reading a VM's row failed",
			"vm", id, "error", err)
		return vmRecord{}
	}
	if !found {
		return vmRecord{}
	}
	return row
}

// recent reports what every host is running, from the last survey while that is
// still current and otherwise from a new one. This is the read path — a
// listing, a console being polled at a few hertz, a command routed to the host
// that holds a VM — where a second-old answer is the same answer.
func (o *orchestrator) recent(ctx context.Context) ([]liveHost, error) {
	o.surveyMu.Lock()
	if o.recentHosts != nil && time.Since(o.recentAt) < surveyInterval {
		hosts := slices.Clone(o.recentHosts)
		o.surveyMu.Unlock()
		return hosts, nil
	}
	o.surveyMu.Unlock()
	return o.fanOut(ctx, true)
}

// survey asks every host pod what it is running, now. Everything that creates,
// moves or removes a VM goes through here, and what it finds answers only that
// operation: the remembered survey is dropped, because the operation is about
// to make it wrong.
//
// This is also where the table is written, and the only place a request writes
// it: an operation that moves a VM is the one that makes the table wrong, so it
// is the one that corrects it. A read — a listing, a console, a command routed
// to a host — writes nothing.
func (o *orchestrator) survey(ctx context.Context) ([]liveHost, error) {
	hosts, err := o.fanOut(ctx, false)
	if err != nil {
		return nil, err
	}
	o.observe(ctx, hosts)
	o.release(ctx, hosts)
	return hosts, nil
}

// observe writes one survey's account of where the VMs are.
func (o *orchestrator) observe(ctx context.Context, hosts []liveHost) {
	o.write(ctx, hosts, func(found surveyed) error {
		return o.table.Observe(ctx, found)
	})
}

// fanOut asks every host for its status at once, and writes nothing down: it is
// the read path as much as the write one, and a listing that rewrote the table
// would put the deployment's only SQLite writer behind every console poll. A
// host that does not answer
// within hostStatusTimeout is reported with its failure rather than dropped:
// the demo kills one on purpose, and the difference between a host that is gone
// and one that is merely quiet is what a recovery depends on. One host is one
// deadline, so a survey costs the slowest answer and never more.
func (o *orchestrator) fanOut(ctx context.Context, remember bool) ([]liveHost, error) {
	found, err := o.pods.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing host pods: %w", err)
	}
	slices.SortFunc(found, func(a, b pod) int { return strings.Compare(a.Name, b.Name) })
	hosts := make([]liveHost, len(found))
	var wg sync.WaitGroup
	for index, p := range found {
		report := orch.Host{Name: p.Name, Ready: p.Ready, Running: []string{}, Serving: []string{}}
		if p.IP != "" {
			report.API = "http://" + net.JoinHostPort(p.IP, strconv.Itoa(o.apiPort))
			report.Page = net.JoinHostPort(p.IP, strconv.Itoa(o.pagePort))
		}
		hosts[index] = liveHost{report: report}
		if p.IP == "" {
			hosts[index].report.Error = "the pod has no address yet"
			continue
		}
		client := o.dial(p)
		hosts[index].client = client
		wg.Go(func() {
			asked, cancel := context.WithTimeout(ctx, hostStatusTimeout)
			defer cancel()
			status, err := client.Status(asked)
			if err != nil {
				hosts[index].report.Error = err.Error()
				return
			}
			hosts[index].report.Running = status.Running
			hosts[index].report.Serving = status.Serving
			hosts[index].report.Pager = status.Pager
			hosts[index].report.Pages = status.Pages
			hosts[index].report.Store = status.Store
			hosts[index].vms = status.VMs
			hosts[index].templates = status.Templates
			if status.PageAddress != "" {
				hosts[index].report.Page = status.PageAddress
			}
		})
	}
	wg.Wait()
	o.surveyMu.Lock()
	if remember {
		o.recentHosts, o.recentAt = hosts, time.Now()
	} else {
		o.recentHosts, o.recentAt = nil, time.Time{}
	}
	o.surveyMu.Unlock()
	return slices.Clone(hosts), nil
}

// release gives a host back the pages it still serves for a VM nothing is
// doing anything with. A host serves a VM's pages from the moment it hands it
// over until the orchestrator tells it the destination has them, and that word
// is the only release there is: an orchestrator that restarted, or whose call
// failed, would otherwise leave the source serving for good — and a fork's
// parent sealed with it, never checkpointed, never fenced and never migratable.
//
// Every survey reconciles Serving against the table, which is what the table is
// for: a VM with an operation in flight is left alone, because the destination
// does not have its pages yet and releasing them would lose every write since
// the source's last checkpoint. Everything else is released, which is idempotent
// and which a restarted orchestrator therefore does on its first survey.
//
// A handover of a VM no host runs and the table has never heard of is the
// exception, and it is given up rather than released: it is the child of a
// fan-out that failed, so nothing holds the pages the source kept for it and
// nothing ever will. Asking to release those is asking for something the source
// can only refuse — they are the only copy — so a survey that kept asking kept
// a parent sealed until the host's own deadline retired the hold.
func (o *orchestrator) release(ctx context.Context, hosts []liveHost) {
	if o.table == nil {
		return
	}
	for _, h := range hosts {
		if h.client == nil || h.report.Error != "" {
			continue
		}
		for _, id := range h.report.Serving {
			row, found, err := o.table.VM(ctx, id)
			if err != nil {
				// A table that cannot be read is no evidence of anything.
				slog.ErrorContext(ctx, "sproutfs-orchestrator: reading a served VM's row failed",
					"vm", id, "host", h.report.Name, "error", err)
				continue
			}
			if found && stillInFlight(row) {
				continue
			}
			if !found && !runningSomewhere(hosts, id) {
				// Nothing knows this VM: no row names it and no host runs it, so
				// nothing will ever fetch what this host holds for it.
				o.giveUp(ctx, h, id)
				continue
			}
			if err := h.client.Released(ctx, id); err != nil {
				slog.ErrorContext(ctx, "sproutfs-orchestrator: releasing a stale handover failed",
					"vm", id, "host", h.report.Name, "error", err)
				continue
			}
			slog.InfoContext(ctx, "sproutfs-orchestrator: released a handover nothing was waiting on",
				"vm", id, "host", h.report.Name, "state", row.State)
		}
	}
}

// ReconcileInterval is how often the orchestrator rebuilds its table from the
// deployment itself. Nothing in the table is authority, so this is the only
// thing that has to happen on a clock: a VM another orchestrator deleted, or one
// whose host is gone, is learned here rather than from a request that happened
// to ask.
const ReconcileInterval = 30 * time.Second

// Reconciling rebuilds the table on its own timer for as long as ctx lives. It
// is what replaced doing the same work inside a listing: reconciling reads
// every control record in the bucket and writes the whole table, which is not
// what a GET should cost, and a console polled at a few hertz made the
// deployment's only SQLite writer the busiest thing in it.
func (o *orchestrator) Reconciling(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = ReconcileInterval
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := o.Reconcile(ctx); err != nil {
			slog.WarnContext(ctx, "sproutfs-orchestrator: reconciling the table failed", "error", err)
		}
	}
}

// Reconcile surveys the deployment and rebuilds the table from it and the
// bucket's own list of VMs, which is the only thing that tells a VM that was
// deleted from one whose host is gone. It is also what releases a handover
// nothing is waiting on, so an orchestrator that restarted mid-migration frees
// the source's pages here.
func (o *orchestrator) Reconcile(ctx context.Context) error {
	identities, err := o.identities(ctx)
	if err != nil {
		return err
	}
	hosts, err := o.survey(ctx)
	if err != nil {
		return err
	}
	o.write(ctx, hosts, func(found surveyed) error {
		return o.table.Reconcile(ctx, found, live(identities))
	})
	return nil
}

// write turns a survey into table rows and applies them. A table that could not
// be written is a diagnostic: the next survey writes what this one missed.
func (o *orchestrator) write(ctx context.Context, hosts []liveHost, apply func(surveyed) error) {
	if o.table == nil {
		return
	}
	found := surveyed{listed: make([]string, 0, len(hosts)),
		answered: make([]string, 0, len(hosts)), running: map[string]string{}}
	for _, h := range hosts {
		found.listed = append(found.listed, h.report.Name)
		if h.report.Error == "" {
			found.answered = append(found.answered, h.report.Name)
		}
		for _, id := range h.report.Running {
			found.running[id] = h.report.Name
		}
	}
	if err := apply(found); err != nil {
		slog.ErrorContext(ctx, "sproutfs-orchestrator: reconciling the table failed", "error", err)
	}
}

// Hosts reports every host pod of the deployment.
func (o *orchestrator) Hosts(ctx context.Context) ([]orch.Host, error) {
	hosts, err := o.recent(ctx)
	if err != nil {
		return nil, err
	}
	reports := make([]orch.Host, 0, len(hosts))
	for _, h := range hosts {
		reports = append(reports, h.report)
	}
	return reports, nil
}

// VMs reports every VM of the deployment: the control records in the bucket,
// each attributed to the host that says it runs it. A VM no host reports is one
// whose host is gone, which is what recovery is for.
func (o *orchestrator) VMs(ctx context.Context) ([]orch.VM, error) {
	identities, err := o.identities(ctx)
	if err != nil {
		return nil, err
	}
	hosts, err := o.recent(ctx)
	if err != nil {
		return nil, err
	}
	states := map[string]vmRecord{}
	if o.table != nil {
		rows, err := o.table.VMs(ctx)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			states[row.ID] = row
		}
	}
	// What a host says about a VM it holds is the only current account of that
	// VM's checkpoint and epoch: the control record in the bucket is a moment
	// behind whatever its running host has published since.
	held := map[string]host.VM{}
	for _, h := range hosts {
		for _, vm := range h.vms {
			held[vm.ID] = vm
		}
	}
	// A host reporting a VM is what running means, and no host reporting one is
	// what stopped means. The table can say otherwise — it holds what the
	// orchestrator last did, which is how a VM in flight is visible while no
	// host reports it — and it does so below.
	running := map[string]orch.VM{}
	for _, h := range hosts {
		for _, id := range h.report.Running {
			vm := orch.VM{ID: id, Host: h.report.Name, State: stateRunning}
			if record, found := held[id]; found {
				vm.Checkpoint, vm.Epoch, vm.Template = record.Checkpoint, record.Epoch, record.Template
				// Only the host running a VM knows what it holds unpublished, so
				// the window comes from that report and nowhere else.
				vm.LossWindow, vm.Waiting = record.LossWindow, record.Waiting
				// What a VM holds privately is its host's too: the pages are in
				// that host's pager and nowhere else.
				vm.PrivateBytes = record.PrivateBytes
			}
			running[id] = vm
		}
	}
	vms := make([]orch.VM, 0, len(identities))
	for _, entry := range identities {
		vm := orch.VM{ID: entry.ID, State: stateStopped}
		if known, found := running[entry.ID]; found {
			vm = known
		}
		vms = append(vms, vm)
	}
	// A VM whose first checkpoint has not landed has no control record to list
	// yet, and its host is the only place it exists. Report those too.
	listed := live(identities)
	for id, vm := range running {
		if !slices.Contains(listed, id) {
			vms = append(vms, vm)
		}
	}
	for index, vm := range vms {
		row, found := states[vm.ID]
		if !found {
			continue
		}
		vms[index].State, vms[index].From, vms[index].To = row.State, row.From, row.To
		vms[index].Parent = row.Parent
		// A VM in flight has no host reporting it, and the table is the only
		// account of where it is going.
		if vms[index].Host == "" {
			vms[index].Host = row.Host
		}
	}
	slices.SortFunc(vms, func(a, b orch.VM) int { return strings.Compare(a.ID, b.ID) })
	return vms, nil
}

// identities is every VM of the deployment, which is every control record in
// the bucket that is not a template's. A template is an ordinary VM holding an
// imported guest image, so that a fork of it inherits a published checkpoint;
// it is one host's own bookkeeping, nothing asked for it and nothing runs it,
// and listing it would offer operations on it.
func (o *orchestrator) identities(ctx context.Context) ([]listing, error) {
	records, err := o.records.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing control records: %w", err)
	}
	// The filter is over a copy: the listing belongs to whatever produced it.
	return slices.DeleteFunc(slices.Clone(records),
		func(entry listing) bool { return host.IsTemplate(entry.ID) }), nil
}

// place picks the host with the most memory free for a VM of need bytes,
// skipping any host that did not answer and, when one is named, the host a VM
// is being moved off. A VM that fits nowhere is refused here rather than
// started somewhere it cannot run: a host admitted past its arena runs guests
// that fault against the spill file, or a VM whose memory will not attach at
// all, and either is discovered as a VM that does not start.
//
// A need of zero is a VM whose template nothing here knows — one the table has
// never heard of, or a host too old to report its templates — and every host
// that answered admits it, which is the placement this had before it measured
// memory at all. A host that reports no arena measures as nothing free, so it
// takes such a VM and nothing else.
func place(hosts []liveHost, exclude string, need uint64) (liveHost, error) {
	var best liveHost
	found, answered := false, false
	for _, h := range hosts {
		if h.client == nil || h.report.Error != "" || !h.report.Ready || h.report.Name == exclude {
			continue
		}
		answered = true
		if h.free() < need {
			continue
		}
		if !found || h.free() > best.free() {
			best, found = h, true
		}
	}
	if !found && answered {
		return liveHost{}, fmt.Errorf("%w: no host has %d bytes of memory free", errNoHost, need)
	}
	if !found {
		return liveHost{}, fmt.Errorf("%w: no ready host answered", errNoHost)
	}
	return best, nil
}

// admits reports whether one named host can hold a VM of need bytes, which is
// the same question place asks of every host at once.
func admits(target liveHost, need uint64) error {
	if target.free() < need {
		return fmt.Errorf("%w: %s has %d bytes of memory free, and this VM needs %d",
			errNoHost, target.report.Name, target.free(), need)
	}
	return nil
}

// runner finds the host that runs one VM, and refuses when two of them claim
// it. A VM has one writer, so two claims are the deployment disagreeing with
// itself — a recovery that raced a host which was not gone, or a handoff whose
// ends both kept the VM — and either claimant may be the one whose writes can
// never be published. Acting on the first of them is the worst answer
// available: a migration off a superseded host hands a third host a stale
// writer's pages. The operator is told which hosts disagree instead, and a
// recovery, once one of them is really gone, is what settles it.
func runner(hosts []liveHost, id string) (liveHost, error) {
	var found liveHost
	var claimants []string
	for _, h := range hosts {
		if !slices.Contains(h.report.Running, id) {
			continue
		}
		if len(claimants) == 0 {
			found = h
		}
		claimants = append(claimants, h.report.Name)
	}
	switch len(claimants) {
	case 0:
		return liveHost{}, fmt.Errorf("%w: no host runs %s", errNotFound, id)
	case 1:
		return found, nil
	default:
		return liveHost{}, fmt.Errorf("%w: %s is claimed by %s", errContested, id,
			strings.Join(claimants, ", "))
	}
}

// runningSomewhere reports whether any host the survey reached says it runs
// this VM. A host that did not answer says nothing either way, so this is
// evidence that a VM exists and never evidence that it does not.
func runningSomewhere(hosts []liveHost, id string) bool {
	for _, h := range hosts {
		if h.report.Error != "" {
			// A host that is not answering is not evidence of anything.
			return true
		}
		if slices.Contains(h.report.Running, id) {
			return true
		}
	}
	return false
}

// named finds one host by its pod name.
func named(hosts []liveHost, name string) (liveHost, error) {
	for _, h := range hosts {
		if h.report.Name == name {
			return h, nil
		}
	}
	return liveHost{}, fmt.Errorf("%w: no host named %s", errNotFound, name)
}

// Create allocates an identity and creates the VM on the least loaded host.
func (o *orchestrator) Create(ctx context.Context, request orch.CreateRequest) (orch.CreateResult, error) {
	if request.From != nil && (request.From.VM == "" || request.Template != "") {
		return orch.CreateResult{}, fmt.Errorf("%w: a create from a checkpoint names the VM it is of, and no template",
			errRequest)
	}
	template, parent := request.Template, ""
	hosts, err := o.survey(ctx)
	if err != nil {
		return orch.CreateResult{}, err
	}
	// The memory a VM has is what its create asks for, or else its template's
	// or the VM it starts from, and it is written down here, so that
	// everything after — a fork of it, a migration, a start — measures the VM
	// rather than looking a template up again for it.
	need := request.Memory
	if request.From != nil {
		parent = request.From.VM
		template = o.rowOf(ctx, parent).Template
		if need == 0 {
			need = o.memoryFor(ctx, hosts, parent)
		}
	}
	if need == 0 {
		need = memoryOf(hosts, template)
	}
	target, err := place(hosts, "", need)
	if err != nil {
		return orch.CreateResult{}, err
	}
	id := o.identify()
	o.note(ctx, vmRecord{ID: id, Host: target.report.Name, State: stateCreating,
		Template: template, Parent: parent, Memory: need})
	result, err := target.client.Create(ctx, host.CreateRequest{ID: id, Template: request.Template,
		From: request.From, Memory: request.Memory, Disk: request.Disk, VCPUs: request.VCPUs})
	if err != nil {
		o.forget(ctx, id)
		return orch.CreateResult{}, fmt.Errorf("creating %s on %s: %w", id, target.report.Name, err)
	}
	o.note(ctx, vmRecord{ID: id, Host: target.report.Name, State: stateRunning,
		Template: template, Parent: parent, Memory: need})
	slog.InfoContext(ctx, "sproutfs-orchestrator: created a VM", "vm", id, "host", target.report.Name,
		"template", template, "seconds", float64(result.Total))
	return orch.CreateResult{Host: target.report.Name, Result: result}, nil
}

// ImportTemplate hands a guest image to one ready host, which imports it into
// the template its bytes name. The template is the deployment's from then on:
// any host creates from it by the identity this reports. The host is the ready
// one with the most memory free, which says nothing about the template and only
// spreads the work of reading images.
func (o *orchestrator) ImportTemplate(ctx context.Context, image io.Reader,
	request host.ImportTemplateRequest) (host.ImportTemplateResult, error) {
	hosts, err := o.survey(ctx)
	if err != nil {
		return host.ImportTemplateResult{}, err
	}
	target, err := place(hosts, "", 0)
	if err != nil {
		return host.ImportTemplateResult{}, err
	}
	result, err := target.client.ImportTemplate(ctx, image, request)
	if err != nil {
		return host.ImportTemplateResult{}, fmt.Errorf("importing a template on %s: %w", target.report.Name, err)
	}
	slog.InfoContext(ctx, "sproutfs-orchestrator: imported a template", "template", result.Template.ID,
		"host", target.report.Name, "checkpoint", result.Checkpoint)
	return result, nil
}

// Fork forks a running VM. Every fork goes through here: the orchestrator
// allocates each child's identity and writes down where it is going before the
// parent is paused, so a child that never finishes starting is still an
// identity someone knows about rather than an anonymous control record.
//
// Every child of one request starts from one pause of the parent, so a
// fan-out costs the parent one pause. A fork is a migration handoff from a
// parent that keeps running, so the children may be placed anywhere and the
// handshake is the same wherever they land: the default is the parent's own
// host, which takes its children in over the pages the seal froze so that
// nothing crosses the network, and naming another host makes each child pull
// the pages no checkpoint holds out of the parent's page server, exactly as a
// migration's destination does. The parent holds the point until every child
// has them all.
func (o *orchestrator) Fork(ctx context.Context, id string, count int, to string) (orch.ForkResult, error) {
	began := time.Now()
	if count <= 0 {
		count = 1
	}
	hosts, err := o.survey(ctx)
	if err != nil {
		return orch.ForkResult{}, err
	}
	source, err := runner(hosts, id)
	if err != nil {
		return orch.ForkResult{}, err
	}
	target := source
	if to != "" && to != source.report.Name {
		if target, err = named(hosts, to); err != nil {
			return orch.ForkResult{}, err
		}
		if target.client == nil || target.report.Error != "" {
			return orch.ForkResult{}, fmt.Errorf("%w: %s is not answering", errNoHost, target.report.Name)
		}
	}
	// A child is a guest of the same template as its parent and has the memory
	// its parent has: that is what a fork is, and a parent that was cold started
	// into a different shape is what its children inherit rather than whatever
	// the template holds now.
	parent := o.rowOf(ctx, id)
	need := parent.Memory
	if need == 0 {
		need = memoryOf(hosts, parent.Template)
	}
	// Every child is a guest of its own with RAM of its own, wherever it runs:
	// what children of one parent share is the pages they have not diverged
	// from, not the promise that they may. A fan-out that does not fit is
	// refused before the parent is paused for it, on the parent's own host as
	// much as on another.
	if err := admits(target, uint64(count)*need); err != nil {
		return orch.ForkResult{}, err
	}
	children := make([]string, 0, count)
	for range count {
		children = append(children, o.identify())
	}
	// The children are in the table before their parent is paused, with the
	// host they are going to and the parent they come from.
	for _, child := range children {
		o.note(ctx, vmRecord{ID: child, Host: target.report.Name, State: stateCreating,
			Parent: id, Template: parent.Template, Memory: need})
	}
	result := orch.ForkResult{Host: source.report.Name, To: target.report.Name, Children: children}
	forked, err := o.fork(ctx, source, target, id, children)
	if err != nil {
		for _, child := range children {
			o.forget(ctx, child)
		}
		return orch.ForkResult{}, err
	}
	for _, child := range children {
		o.note(ctx, vmRecord{ID: child, Host: target.report.Name, State: stateRunning,
			Parent: id, Template: parent.Template, Memory: need})
	}
	result.Capture, result.Start = forked.Capture, forked.Boot
	result.Total = host.Since(began)
	slog.InfoContext(ctx, "sproutfs-orchestrator: forked a VM", "vm", id, "children", len(children),
		"from", source.report.Name, "to", target.report.Name, "pause_seconds", float64(forked.Capture))
	return result, nil
}

// fork carries one fork through: the migration handshake, wherever the children
// land. The parent's host builds a handoff per child and holds the point for
// each of them, every destination creates its child and binds the pages no
// checkpoint holds — off the parent's page server on another host, off the
// pages themselves on the parent's own — and only when the last child has them
// does the parent take its pages back.
func (o *orchestrator) fork(ctx context.Context, source, target liveHost, parent string,
	children []string) (host.ForkResult, error) {
	// A child of the parent's own host is handed over without an address: its
	// inherited pages never reach the wire.
	destination := target.report.Page
	if target.report.Name == source.report.Name {
		destination = ""
	}
	handed, err := source.client.Fork(ctx, parent, host.ForkRequest{IDs: children,
		Destination: destination})
	if err != nil {
		// One request forked one parent into a set of children, and the set did
		// not happen. Whichever of them exist are identities only this request
		// ever knew, so they are taken back the way a destination's refusal
		// takes its own back; deleting an identity that was never created
		// removes nothing.
		o.discardChildren(ctx, target, children)
		return host.ForkResult{}, fmt.Errorf("forking %s: %w", parent, err)
	}
	if len(handed.Handoffs) != len(children) {
		return host.ForkResult{}, fmt.Errorf("%w: %s returned %d handoffs for %d children",
			errRequest, source.report.Name, len(handed.Handoffs), len(children))
	}
	started := time.Now()
	var failure error
	var running []string
	for _, handoff := range handed.Handoffs {
		if failure != nil {
			// The fan-out is over, so this child is never offered anywhere: its
			// hold is given up rather than released.
			o.giveUp(ctx, source, handoff.VMID)
			continue
		}
		if _, err := target.client.Receive(ctx, handoff); err != nil {
			failure = fmt.Errorf("starting %s on %s: %w", handoff.VMID, target.report.Name, err)
			// A child no destination took has none of the pages its hold keeps
			// and never will, so the source can only refuse to release them:
			// they exist nowhere else. It gives them up instead, which is what
			// takes the seal off the parent.
			o.giveUp(ctx, source, handoff.VMID)
			continue
		}
		running = append(running, handoff.VMID)
		// Released is the child's word that it holds every page it inherited,
		// which is what gives the parent its pages back; the parent itself
		// never stopped.
		if err := source.client.Released(ctx, handoff.VMID); err != nil {
			slog.ErrorContext(ctx, "sproutfs-orchestrator: releasing a fork's parent failed",
				"vm", handoff.VMID, "host", source.report.Name, "error", err)
		}
	}
	if failure != nil {
		// One request forked one parent into a set of children, and the set did
		// not happen. The children that did start are guests nobody asked for,
		// holding a host's memory under identities the caller was never told;
		// they are deleted rather than left for an operator to find.
		o.discardChildren(ctx, target, running)
		return host.ForkResult{}, failure
	}
	handed.Boot = host.Since(started)
	return handed, nil
}

// giveUp tells a host that one handover it holds will never be received, so
// that it stops holding what it kept for it — for a fork that is the parent's
// sealed pages, which is what lets the parent be checkpointed again.
//
// It is the give-up rather than the release because nothing fetched those pages
// and nothing ever will: a release of them is a request the host can only
// refuse, for ever. A failure to say it is logged rather than reported — the
// operation's own failure is what the caller needs, and the host's deadline
// retires the hold in the end either way.
func (o *orchestrator) giveUp(ctx context.Context, held liveHost, id string) {
	if held.client == nil {
		return
	}
	if err := held.client.Abandoned(ctx, id); err != nil {
		slog.ErrorContext(ctx, "sproutfs-orchestrator: giving up a handover failed",
			"vm", id, "host", held.report.Name, "error", err)
		return
	}
	slog.InfoContext(ctx, "sproutfs-orchestrator: gave up a handover nothing will ever receive",
		"vm", id, "host", held.report.Name)
}

// discardChildren takes back the children of a fan-out that did not happen. They
// are guests nobody asked for, under identities only the failed request ever
// knew, so nothing else would ever name them again. A failure to delete one is
// logged: the fork's own failure is what the caller needs, and a child left
// behind is a diagnostic rather than a second error to report.
func (o *orchestrator) discardChildren(ctx context.Context, target liveHost, children []string) {
	for _, child := range children {
		if err := target.client.Delete(ctx, child); err != nil {
			slog.ErrorContext(ctx, "sproutfs-orchestrator: deleting a partial fork's child failed",
				"vm", child, "host", target.report.Name, "error", err)
		}
	}
}

// Capture takes one explicit checkpoint on the host running the VM.
func (o *orchestrator) Capture(ctx context.Context, id string, request orch.CaptureRequest) (orch.CaptureResult, error) {
	hosts, err := o.recent(ctx)
	if err != nil {
		return orch.CaptureResult{}, err
	}
	source, err := runner(hosts, id)
	if err != nil {
		return orch.CaptureResult{}, err
	}
	if !request.New {
		result, err := source.client.Capture(ctx, id, host.CaptureRequest{})
		if err != nil {
			return orch.CaptureResult{}, fmt.Errorf("capturing %s on %s: %w", id, source.report.Name, err)
		}
		return orch.CaptureResult{Host: source.report.Name, Result: result}, nil
	}
	// The new VM is a copy of the source at one pause, so it is of the
	// source's template and has the source's memory. It is written down before
	// the host creates it, as every identity is, and it runs nowhere after.
	parent := o.rowOf(ctx, id)
	into := vmRecord{ID: o.identify(), State: stateCreating, Template: parent.Template, Parent: id,
		Memory: o.memoryFor(ctx, hosts, id)}
	o.note(ctx, into)
	result, err := source.client.Capture(ctx, id, host.CaptureRequest{Into: into.ID})
	if err != nil {
		o.forget(ctx, into.ID)
		return orch.CaptureResult{}, fmt.Errorf("capturing %s into %s on %s: %w", id, into.ID,
			source.report.Name, err)
	}
	into.State = stateStopped
	o.note(ctx, into)
	slog.InfoContext(ctx, "sproutfs-orchestrator: captured a VM into a new one", "vm", id,
		"into", into.ID, "host", source.report.Name, "checkpoint", result.Checkpoint)
	return orch.CaptureResult{Host: source.report.Name, Result: result}, nil
}

// Migrate carries one VM between hosts: the source stops the guest and hands
// the VM over, the destination opens it and resumes it from the captured state,
// and only once the destination has every page no checkpoint holds may the
// source release the pages it is still serving.
//
// An empty destination picks the least loaded host other than the source, which
// is what a draining host asks for.
func (o *orchestrator) Migrate(ctx context.Context, id, to string) (orch.MigrateResult, error) {
	began := time.Now()
	hosts, err := o.survey(ctx)
	if err != nil {
		return orch.MigrateResult{}, err
	}
	source, err := runner(hosts, id)
	if err != nil {
		return orch.MigrateResult{}, err
	}
	need := o.memoryFor(ctx, hosts, id)
	var target liveHost
	if to == "" {
		target, err = place(hosts, source.report.Name, need)
	} else {
		target, err = named(hosts, to)
	}
	if err != nil {
		return orch.MigrateResult{}, err
	}
	if target.report.Name == source.report.Name {
		return orch.MigrateResult{}, fmt.Errorf("%w: %s already runs %s", errRequest, to, id)
	}
	if target.client == nil || target.report.Error != "" {
		return orch.MigrateResult{}, fmt.Errorf("%w: %s is not answering", errNoHost, target.report.Name)
	}
	// A destination without room for the guest would take it and then fail to
	// hold it, and the VM is already stopped by then.
	if err := admits(target, need); err != nil {
		return orch.MigrateResult{}, err
	}
	o.note(ctx, vmRecord{ID: id, Host: source.report.Name, State: stateMigrating,
		From: source.report.Name, To: target.report.Name})
	handoff, err := source.client.Migrate(ctx, id, host.MigrateRequest{Destination: target.report.Page})
	if err != nil {
		o.note(ctx, vmRecord{ID: id, Host: source.report.Name, State: stateRunning})
		return orch.MigrateResult{}, fmt.Errorf("stopping %s on %s: %w", id, source.report.Name, err)
	}
	received, err := o.receive(ctx, source, target, id, handoff.Handoff)
	if err != nil {
		// The guest is stopped and the source still holds its pages. Nothing
		// here can resume it: its memory regions have given their volumes up, so the
		// VM is reopened from its last checkpoint instead.
		o.note(ctx, vmRecord{ID: id, State: stateStopped})
		err = fmt.Errorf("receiving %s on %s: %w", id, target.report.Name, err)
		if errors.Is(err, errLostSource) {
			// The pages the destination had not fetched were only on that host
			// and are gone with it, so there is nothing left to wait for and
			// nothing to publish: what a recovery opens is the checkpoint the
			// control record still selects.
			return orch.MigrateResult{}, errors.Join(err, o.recoverLost(ctx, id))
		}
		return orch.MigrateResult{}, err
	}
	o.note(ctx, vmRecord{ID: id, Host: target.report.Name, State: stateRunning})
	if err := source.client.Released(ctx, id); err != nil {
		// The destination has every page, so this costs the source only the
		// pages it goes on holding until it exits.
		slog.ErrorContext(ctx, "sproutfs-orchestrator: releasing a migrated VM failed",
			"vm", id, "host", source.report.Name, "error", err)
	}
	slog.InfoContext(ctx, "sproutfs-orchestrator: migrated a VM", "vm", id,
		"from", source.report.Name, "to", target.report.Name, "pause_seconds", float64(received.Pause))
	return orch.MigrateResult{VM: id, From: source.report.Name, To: target.report.Name,
		Pause: received.Pause, Stream: received.Stream, PeerPages: received.PeerPages,
		VolumePages: received.VolumePages, Unpublished: received.Unpublished,
		Total: host.Since(began)}, nil
}

// SourceWatchInterval is how often a migration in flight asks whether the host
// still holding the VM's pages is there. It is the deployment's only answer to
// a destination that is waiting for pages no checkpoint holds: those pages
// exist on that host alone, so the destination asks for them until they arrive
// and nothing in it may ever decide they will not.
const SourceWatchInterval = 5 * time.Second

func (o *orchestrator) watchInterval() time.Duration {
	if o.sourceWatch > 0 {
		return o.sourceWatch
	}
	return SourceWatchInterval
}

// receive carries the destination's half of a handoff while watching the host
// that still holds the VM's pages. The destination returns when it has every
// page no checkpoint has, and waits for as long as that takes; losing the host
// holding them is what ends the wait, and this is the only thing that sees it.
func (o *orchestrator) receive(ctx context.Context, source, target liveHost, id string,
	handoff host.Handoff) (host.ReceiveResult, error) {
	receiving, lose := context.WithCancelCause(ctx)
	defer lose(nil)
	watched := make(chan struct{})
	defer close(watched)
	go o.watchSource(receiving, source.report.Name, id, watched, lose)
	result, err := target.client.Receive(receiving, handoff)
	if err != nil && ctx.Err() == nil && receiving.Err() != nil {
		// The migration was ended here rather than by the caller or the
		// destination, so what ended it is what this reports.
		return host.ReceiveResult{}, context.Cause(receiving)
	}
	return result, err
}

// watchSource ends a receive whose source host is gone. It surveys on its own
// interval until the receive is over, and acts only on positive evidence of the
// loss, because the destination's guest is torn down by it: a pod the
// Kubernetes API no longer lists, or a host that answers and neither runs the
// VM nor serves its pages any more, which is a host that came back without the
// pages it was holding. A host that is merely quiet is a host whose guest may
// be perfectly well.
func (o *orchestrator) watchSource(ctx context.Context, from, id string,
	until <-chan struct{}, lose context.CancelCauseFunc) {
	ticker := time.NewTicker(o.watchInterval())
	defer ticker.Stop()
	for {
		select {
		case <-until:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		hosts, err := o.survey(ctx)
		if err != nil {
			// A survey that failed is no evidence of anything.
			slog.WarnContext(ctx, "sproutfs-orchestrator: surveying the source of a migration failed",
				"vm", id, "host", from, "error", err)
			continue
		}
		if !lostSource(hosts, from, id) {
			continue
		}
		slog.WarnContext(ctx, "sproutfs-orchestrator: the host holding a migrated VM's pages is gone",
			"vm", id, "host", from)
		lose(fmt.Errorf("%w: %s was holding the pages of %s that no checkpoint has", errLostSource, from, id))
		return
	}
}

// lostSource reports the host that handed a VM over being gone, which is what
// makes the pages it was still serving gone too.
func lostSource(hosts []liveHost, from, id string) bool {
	for _, h := range hosts {
		if h.report.Name != from {
			continue
		}
		if h.report.Error != "" {
			// Quiet says nothing: this host may be serving those pages to the
			// destination right now behind one dropped status request.
			return false
		}
		return !slices.Contains(h.report.Serving, id) && !slices.Contains(h.report.Running, id)
	}
	// The Kubernetes API no longer lists the pod at all.
	return true
}

// recoverLost reopens a VM whose migration ended with the host holding its
// pages. It is the ordinary recovery: the destination gave the half-received
// guest up, nothing runs the VM, and what an open finds is the checkpoint its
// control record selects, which is the source's last interval checkpoint.
func (o *orchestrator) recoverLost(ctx context.Context, id string) error {
	recovered, err := o.Recover(ctx, id, false)
	if err != nil {
		return fmt.Errorf("recovering %s after losing the host holding its pages: %w", id, err)
	}
	slog.InfoContext(ctx, "sproutfs-orchestrator: recovered a VM whose migration lost its source",
		"vm", id, "host", recovered.Host, "checkpoint", recovered.Result.VM.Checkpoint)
	return nil
}

// Recover reopens a VM whose host is gone. Opening takes the control record's
// epoch over, which fences whatever held it before, so it acts only on positive
// evidence that the loss is real: every host pod the Kubernetes API lists
// answered this survey, and none of them runs the VM. A host that answers and
// says it runs the VM is fine and is never fenced; a host that did not answer is
// not a host that is gone — its guest may be running perfectly well behind one
// dropped request — and taking the epoch from it would start a second writer of
// one VM.
//
// force is the operator's own evidence, for the case a survey cannot settle: a
// pod the API still lists whose process is gone. Killing the host is the other
// way to produce that evidence, and the one the demo uses, because a deleted pod
// stops being listed.
func (o *orchestrator) Recover(ctx context.Context, id string, force bool) (orch.RecoverResult, error) {
	return o.reopen(ctx, id, reopening{state: stateRecovering, what: "recovered",
		// A host that did not answer is not a host that is gone. Force is the
		// operator's own evidence that it is.
		requireAnswers: !force,
		quietAdvice:    "kill that host, or recover by force"})
}

// Stop ends a VM the deployment is finished with for now. The host running it
// publishes everything its guest holds and closes it, so the VM is then exactly
// its last checkpoint and nothing is running anywhere; a start opens it again
// at those bytes.
//
// It is the host's work and the orchestrator's only part is finding the host
// and writing down that the VM is nowhere. A VM no host runs has nothing to
// stop, and a VM two hosts claim is refused here as everywhere else: one of
// them is a writer whose stores can never be published, and nothing here can
// say which.
func (o *orchestrator) Stop(ctx context.Context, id string, request orch.StopRequest) (orch.StopResult, error) {
	began := time.Now()
	hosts, err := o.survey(ctx)
	if err != nil {
		return orch.StopResult{}, err
	}
	source, err := runner(hosts, id)
	if err != nil {
		return orch.StopResult{}, err
	}
	stopped, err := source.client.Stop(ctx, id, host.StopRequest{Suspend: request.Suspend})
	if err != nil {
		return orch.StopResult{}, fmt.Errorf("stopping %s on %s: %w", id, source.report.Name, err)
	}
	o.note(ctx, vmRecord{ID: id, State: stateStopped})
	slog.InfoContext(ctx, "sproutfs-orchestrator: stopped a VM", "vm", id, "host", source.report.Name,
		"checkpoint", stopped.Checkpoint, "suspended", request.Suspend)
	return orch.StopResult{VM: id, Host: source.report.Name, Checkpoint: stopped.Checkpoint,
		Total: host.Since(began)}, nil
}

// Start opens a stopped VM on a host again, on the one named or on the ready
// host whose guests have promised the least of its arena.
//
// It is Recover without the evidence of a loss. A recovery has to prove the VM's
// host is gone, because opening takes the control record's epoch and would fence
// a guest that is running perfectly well; a stopped VM has no such host — the
// last one to run it published its state and closed it — so a host that did not
// answer this survey is not a reason to refuse. Everything that says the VM is
// between hosts rather than stopped still refuses: a host that runs it, two
// hosts that claim it, a host still serving the pages no checkpoint has, and an
// operation of this orchestrator's own still in flight.
func (o *orchestrator) Start(ctx context.Context, id string, request orch.StartRequest) (orch.StartResult, error) {
	// A VM that comes back where it was comes back at the shape its memory
	// describes, so there is nothing to resize; asking is a mistake rather than
	// a request that quietly does nothing.
	if !request.Cold && (request.Memory != 0 || request.Disk != 0 || request.VCPUs != 0) {
		return orch.StartResult{}, fmt.Errorf(
			"%w: a VM's memory, disk and processors can change only at a cold start, "+
				"which is the one moment nothing in memory describes its shape", errRequest)
	}
	return o.reopen(ctx, id, reopening{to: request.To, state: stateStarting, what: "started",
		open: host.OpenRequest{Cold: request.Cold, Memory: request.Memory, Disk: request.Disk,
			VCPUs: request.VCPUs}})
}

// reopening is the terms one reopen runs under: where the VM is to go, the
// state the table carries while it is going there, and whether a host that did
// not answer refuses it.
type reopening struct {
	// to is the host named for it, empty to place it on the emptiest ready one.
	to string
	// open is what the host is asked for. A recovery's is always the ordinary
	// open — a VM whose host is gone comes back where it was — and a start's
	// carries the cold flag and the shape.
	open host.OpenRequest
	// state is what the table says while the open is in flight and what says
	// what this was, and quietAdvice what a refusal tells the operator to do
	// about a host that did not answer.
	state, what, quietAdvice string
	// requireAnswers refuses while any listed pod is quiet, which is what a
	// recovery's evidence of a loss is and what a stopped VM does not need.
	requireAnswers bool
}

// reopen opens a VM nothing is running on a host that can take it, which is
// what both a recovery and a start are. Everything before the open is the same
// question for both: is this VM really running nowhere, or is it between two
// hosts and about to be somewhere?
func (o *orchestrator) reopen(ctx context.Context, id string, terms reopening) (orch.RecoverResult, error) {
	hosts, err := o.survey(ctx)
	if err != nil {
		return orch.RecoverResult{}, err
	}
	current, err := runner(hosts, id)
	if err == nil {
		return orch.RecoverResult{}, fmt.Errorf("%w: %s still runs %s",
			errRunning, current.report.Name, id)
	}
	if errors.Is(err, errContested) {
		// Two hosts claim it, so it is certainly not a VM whose host is gone.
		// Which of them to stop is the operator's call, and killing that host is
		// how it is made.
		return orch.RecoverResult{}, err
	}
	// No host running a VM is not the same thing as no host holding it. A source
	// that has handed the VM over goes on serving the pages no checkpoint of it
	// has until the destination reports having them, and an operation this
	// orchestrator started is in flight for as long as its row says so: in both
	// of them the VM is between two hosts rather than lost, and opening it would
	// take the epoch out from under the host that is about to run it. Force says
	// a host's process is gone and says nothing about either, so it does not get
	// past them; a row whose operation really did die ages out of flight and
	// stops refusing on its own.
	if holder := holding(hosts, id); holder != "" {
		return orch.RecoverResult{}, fmt.Errorf(
			"%w: %s still serves the pages of %s that no checkpoint has", errRunning, holder, id)
	}
	if o.table != nil {
		row, found, err := o.table.VM(ctx, id)
		if err != nil {
			return orch.RecoverResult{}, fmt.Errorf("reading the table row of %s: %w", id, err)
		}
		if found && stillInFlight(row) {
			return orch.RecoverResult{}, fmt.Errorf("%w: %s is %s, on %s",
				errRunning, id, row.State, row.Host)
		}
	}
	if quiet := unanswered(hosts); len(quiet) > 0 && terms.requireAnswers {
		return orch.RecoverResult{}, fmt.Errorf(
			"%w: %s did not answer, so %s may still be running there; %s",
			errRunning, strings.Join(quiet, ", "), id, terms.quietAdvice)
	}
	// A cold start that resizes the memory is admitted against the size it is
	// asking for, because that is what the guest will hold once it is running
	// there, and it is what the VM is from then on.
	need := o.memoryFor(ctx, hosts, id)
	if terms.open.Memory != 0 {
		need = terms.open.Memory
	}
	var target liveHost
	if terms.to == "" {
		target, err = place(hosts, "", need)
	} else {
		target, err = named(hosts, terms.to)
	}
	if err != nil {
		return orch.RecoverResult{}, err
	}
	if target.client == nil || target.report.Error != "" {
		return orch.RecoverResult{}, fmt.Errorf("%w: %s is not answering", errNoHost, target.report.Name)
	}
	// A host admitted past its arena runs a guest that faults against the spill
	// file, or one whose memory will not attach at all, and either is discovered
	// as a VM that does not start.
	if err := admits(target, need); err != nil {
		return orch.RecoverResult{}, err
	}
	// The memory the VM has is written down with the row: a cold start that
	// resized it is the one thing that changes it, and from then on every
	// placement measures the VM rather than its template.
	o.note(ctx, vmRecord{ID: id, Host: target.report.Name, State: terms.state,
		Memory: terms.open.Memory})
	result, err := target.client.Open(ctx, id, terms.open)
	if err != nil {
		o.note(ctx, vmRecord{ID: id, State: stateStopped})
		return orch.RecoverResult{}, fmt.Errorf("opening %s on %s: %w", id, target.report.Name, err)
	}
	o.note(ctx, vmRecord{ID: id, Host: target.report.Name, State: stateRunning,
		Memory: terms.open.Memory})
	slog.InfoContext(ctx, "sproutfs-orchestrator: "+terms.what+" a VM", "vm", id,
		"host", target.report.Name, "checkpoint", result.VM.Checkpoint)
	return orch.RecoverResult{Host: target.report.Name, Result: result}, nil
}

// Check runs the deployment check over the bucket and reports every violation
// it found. A deployment that disagrees with itself is not a failed request:
// the request worked, and what is wrong is the answer, so it comes back as a
// body an operator can read rather than as a status line. A store this could
// not read is a failure, because nothing was checked and reporting a pass would
// be the worst answer available.
func (o *orchestrator) Check(ctx context.Context) (orch.CheckResult, error) {
	if o.audit == nil {
		return orch.CheckResult{}, fmt.Errorf(
			"%w: this orchestrator has no bucket to check", errRequest)
	}
	err := o.audit(ctx)
	if err == nil {
		return orch.CheckResult{OK: true}, nil
	}
	var inconsistent *volume.InconsistentError
	if !errors.As(err, &inconsistent) {
		return orch.CheckResult{}, fmt.Errorf("checking the deployment: %w", err)
	}
	result := orch.CheckResult{Violations: make([]orch.Violation, 0, len(inconsistent.Violations))}
	for _, violation := range inconsistent.Violations {
		message := ""
		if violation.Err != nil {
			message = violation.Err.Error()
		}
		result.Violations = append(result.Violations, orch.Violation{Key: violation.Key,
			Class: violation.Class.String(), Message: message})
	}
	slog.WarnContext(ctx, "sproutfs-orchestrator: the deployment disagrees with itself",
		"violations", len(result.Violations))
	return result, nil
}

// holding reports the host that still serves one VM's pages, empty for a VM no
// host holds pages of. A host serves a VM from the moment it hands it over
// until the orchestrator says the destination has its pages, so a VM that is
// served is one a migration or a fork is still carrying.
func holding(hosts []liveHost, id string) string {
	for _, h := range hosts {
		if slices.Contains(h.report.Serving, id) {
			return h.report.Name
		}
	}
	return ""
}

// unanswered reports the host pods this survey learned nothing from, in name
// order. Any of them could be running any VM of the deployment, which is what
// makes a recovery taken while one of them is quiet a guess rather than a
// repair.
func unanswered(hosts []liveHost) []string {
	var quiet []string
	for _, h := range hosts {
		if h.report.Error != "" {
			quiet = append(quiet, h.report.Name)
		}
	}
	return quiet
}

// Kill deletes one host pod, which is the demo's host loss: its VMs' writes
// since their last interval checkpoint go with it, and the rest is in the
// bucket.
func (o *orchestrator) Kill(ctx context.Context, name string) (orch.KillResult, error) {
	if name == "" {
		return orch.KillResult{}, fmt.Errorf("%w: a host to kill needs a name", errRequest)
	}
	if err := o.pods.Delete(ctx, name); err != nil {
		return orch.KillResult{}, fmt.Errorf("deleting pod %s: %w", name, err)
	}
	slog.WarnContext(ctx, "sproutfs-orchestrator: killed a host", "host", name)
	return orch.KillResult{Host: name}, nil
}

// Delete closes and deletes a VM on the host that runs it, and on any ready
// host when no host runs it.
//
// A VM's authority is its control record and its data the objects that record
// selects, both of them in the bucket, so removing them is work any host can do;
// what only the running host can do is close the guest. Routing every delete to
// a running host therefore left a VM whose host was gone undeletable — there was
// no host to route to, and its record and objects stayed in the bucket for good
// — which is exactly the VM an operator most wants to be rid of.
func (o *orchestrator) Delete(ctx context.Context, id string) error {
	hosts, err := o.survey(ctx)
	if err != nil {
		return err
	}
	source, err := runner(hosts, id)
	if errors.Is(err, errNotFound) {
		// Nothing needs room for this: no guest starts.
		source, err = place(hosts, "", 0)
	}
	if err != nil {
		return err
	}
	if err := source.client.Delete(ctx, id); err != nil {
		return err
	}
	o.forget(ctx, id)
	return nil
}

// Exec runs one command in a VM's guest. The table says which host to ask; the
// survey confirms it, because a table row is what the orchestrator last did and
// the host is what is true now.
func (o *orchestrator) Exec(ctx context.Context, id string, request host.ExecRequest) (orch.ExecOutcome, error) {
	hosts, err := o.recent(ctx)
	if err != nil {
		return orch.ExecOutcome{}, err
	}
	source, err := o.routeTo(ctx, hosts, id)
	if err != nil {
		return orch.ExecOutcome{}, err
	}
	result, err := source.client.Exec(ctx, id, request)
	if err != nil {
		return orch.ExecOutcome{}, fmt.Errorf("running a command in %s on %s: %w",
			id, source.report.Name, err)
	}
	return orch.ExecOutcome{Host: source.report.Name, VM: id, Result: result}, nil
}

// routeTo is the host to send a VM's request to: the one the table names, when
// that host answered this survey and still says it runs the VM, and otherwise
// whichever host does say so. The table breaks no tie between two hosts that
// both claim the VM — it says what this orchestrator last did, not which of two
// writers still holds the epoch — so a contested claim is refused here as
// everywhere else.
func (o *orchestrator) routeTo(ctx context.Context, hosts []liveHost, id string) (liveHost, error) {
	claimant, err := runner(hosts, id)
	if err != nil {
		return liveHost{}, err
	}
	if o.table != nil {
		if row, found, err := o.table.VM(ctx, id); err == nil && found && row.Host != "" {
			if named, err := named(hosts, row.Host); err == nil && named.client != nil &&
				named.report.Error == "" && slices.Contains(named.report.Running, id) {
				return named, nil
			}
		} else if err != nil {
			slog.WarnContext(ctx, "sproutfs-orchestrator: reading the VM table failed",
				"vm", id, "error", err)
		}
	}
	return claimant, nil
}

// Drained records what a draining host is doing with one of its VMs. The
// orchestrator drives the migration itself, so this changes nothing it does:
// it makes the moment visible, which is what a table read during a drain is
// for.
func (o *orchestrator) Drained(ctx context.Context, report orch.DrainReport) error {
	if report.Host == "" || report.VM == "" {
		return fmt.Errorf("%w: a drain report names a host and a VM", errRequest)
	}
	switch report.Phase {
	case orch.DrainStarted:
		o.note(ctx, vmRecord{ID: report.VM, Host: report.Host, State: stateMigrating,
			From: report.Host})
		slog.InfoContext(ctx, "sproutfs-orchestrator: a drain began handing a VM over",
			"vm", report.VM, "host", report.Host)
	case orch.DrainFinished:
		if report.Error != "" {
			// The handover was this orchestrator's own migration, and what it
			// recorded is what happened: running where it was when the source
			// refused to stop the guest, stopped when the destination could
			// not take it in — the guest is stopped by then and its volumes
			// given up, and a row saying running would describe a guest that
			// is not. Only a row this drain left at "migrating" is one the
			// migration never got to write, which is a request that never
			// reached here: that guest was never stopped, and a row saying
			// stopped would offer a recovery that fences the host still
			// running it.
			if row := o.rowOf(ctx, report.VM); row.State == stateMigrating && row.From == report.Host {
				o.note(ctx, vmRecord{ID: report.VM, Host: report.Host, State: stateRunning})
			}
			slog.ErrorContext(ctx, "sproutfs-orchestrator: a drain could not hand a VM over",
				"vm", report.VM, "host", report.Host, "error", report.Error)
			return nil
		}
		slog.InfoContext(ctx, "sproutfs-orchestrator: a drain handed a VM over",
			"vm", report.VM, "host", report.Host)
	default:
		return fmt.Errorf("%w: a drain phase is %q or %q, not %q",
			errRequest, orch.DrainStarted, orch.DrainFinished, report.Phase)
	}
	return nil
}

// Console reads one VM's serial console from the host running it.
func (o *orchestrator) Console(ctx context.Context, id string, since int64) (orch.Console, error) {
	hosts, err := o.recent(ctx)
	if err != nil {
		return orch.Console{}, err
	}
	source, err := runner(hosts, id)
	if err != nil {
		return orch.Console{}, err
	}
	return source.client.Console(ctx, id, since)
}

// WriteConsole types into one VM's serial console.
func (o *orchestrator) WriteConsole(ctx context.Context, id, data string) error {
	hosts, err := o.recent(ctx)
	if err != nil {
		return err
	}
	source, err := runner(hosts, id)
	if err != nil {
		return err
	}
	return source.client.WriteConsole(ctx, id, data)
}
