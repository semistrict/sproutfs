package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/api/orch"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/handover"
	"github.com/semistrict/sproutfs/internal/jsonhttp"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// faultChance is how often an activated fault site fails one call. A seed
// activates a quarter of the sites, so a run explores a few of the boundary's
// faults often rather than all of them rarely.
const faultChance = 0.2

// errRefused is a host's API answering with an error status: the host did
// nothing the request asked.
var errRefused = jsonhttp.Error{Op: "simulated", Message: "the host refused the request",
	Status: http.StatusServiceUnavailable}

// errReplyLost is a request the far side carried out whose answer never came
// back: the connection broke, or the caller's deadline passed, after it acted.
var errReplyLost = errors.New("the connection was reset before the reply")

// errUnavailable is the Kubernetes API or the bucket failing a request it did
// not carry out.
var errUnavailable = errors.New("the service is unavailable")

// refusal is a host answering that it will not do what it was asked, and did
// nothing.
func refusal(message string) error {
	return jsonhttp.Error{Message: message, Status: http.StatusConflict}
}

// fakePods is the Kubernetes API: the host pods the orchestrator finds, and the
// one it deletes. It answers from two goroutines at once, because a migration
// watches the host holding its pages while its destination is receiving.
type fakePods struct {
	mu      *sync.Mutex
	pods    []pod
	deleted []string
	err     error
}

func (f *fakePods) List(ctx context.Context) ([]pod, error) {
	if sim.Buggify(ctx, "orchestrator/pods-unavailable/List", faultChance) {
		return nil, errUnavailable
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return slices.Clone(f.pods), nil
}

// Delete stops listing the pod, which is what the Kubernetes API does and what
// a recovery takes as evidence that the host is gone.
func (f *fakePods) Delete(ctx context.Context, name string) error {
	if sim.Buggify(ctx, "orchestrator/pods-unavailable/Delete", faultChance) {
		return errUnavailable
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.deleted = append(f.deleted, name)
	f.pods = slices.DeleteFunc(f.pods, func(p pod) bool { return p.Name == name })
	if sim.Buggify(ctx, "orchestrator/pods-reply-lost/Delete", faultChance) {
		return errReplyLost
	}
	return nil
}

// fakeRecords is the bucket: every VM that has a control record, which is every
// VM that exists. The hosts write it as they create, take in and delete VMs,
// and a test may stage it itself.
type fakeRecords struct {
	ids []string
	err error
	// pending is the records whose first checkpoint has not landed.
	pending map[string]bool
	// epochs is each record's epoch, which every host's open advances, and mu
	// the lock the hosts take around it. afterEpoch runs once a caller has
	// read an epoch, which is where a test opens the VM behind its back.
	epochs     map[string]uint64
	mu         *sync.Mutex
	afterEpoch func()
	// journals is the journals each record names.
	journals map[string][]control.Journal
}

// unavailable is the bucket failing one request, which method names.
func (f *fakeRecords) unavailable(ctx context.Context, method string) bool {
	return sim.Buggify(ctx, "orchestrator/records-unavailable/"+method, faultChance)
}

func (f *fakeRecords) Pending(ctx context.Context, id string) (bool, error) {
	if f.unavailable(ctx, "Pending") {
		return false, errUnavailable
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending[id], f.err
}

func (f *fakeRecords) Journals(ctx context.Context, id string) ([]control.Journal, error) {
	if f.unavailable(ctx, "Journals") {
		return nil, errUnavailable
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.journals[id], f.err
}

func (f *fakeRecords) Epoch(ctx context.Context, id string) (uint64, error) {
	if f.unavailable(ctx, "Epoch") {
		return 0, errUnavailable
	}
	f.mu.Lock()
	epoch := f.epochs[id]
	after := f.afterEpoch
	f.mu.Unlock()
	if after != nil {
		after()
	}
	return epoch, f.err
}

func (f *fakeRecords) List(ctx context.Context) ([]listing, error) {
	if f.unavailable(ctx, "List") {
		return nil, errUnavailable
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	found := make([]listing, 0, len(f.ids))
	for _, id := range f.ids {
		found = append(found, listing{ID: id})
	}
	return found, f.err
}

// write gives a VM a control record, at its first epoch when it has none. The
// caller holds mu, as every host of the deployment does while it acts.
func (f *fakeRecords) write(id string) {
	if !slices.Contains(f.ids, id) {
		f.ids = append(f.ids, id)
	}
	f.epochs[id] = max(f.epochs[id], 1)
}

// remove deletes a VM's control record. The caller holds mu.
func (f *fakeRecords) remove(id string) {
	f.ids = slices.DeleteFunc(f.ids, func(value string) bool { return value == id })
}

// exists reports whether a VM has a control record. The caller holds mu.
func (f *fakeRecords) exists(id string) bool { return slices.Contains(f.ids, id) }

// fakeHostClient is one host of the deployment. It records what the
// orchestrator asked it, which is how the order of a migration is asserted.
type fakeHostClient struct {
	// mu is the whole fake deployment's, because what one host is asked and what
	// the log says are read by the goroutine watching a migration's source while
	// another goroutine is inside a receive.
	mu      *sync.Mutex
	name    string
	running []string
	serving []string
	page    string
	// down makes this host unreachable, which is what a killed pod looks like
	// until the Kubernetes API stops listing it.
	down bool
	// wedged makes this host accept the connection and never answer, which is
	// what a host whose supervisor is stuck looks like from outside.
	wedged bool
	// checkpoint is what this host reports every VM it holds is checkpointed at,
	// and shared what its pager reports it has shared.
	checkpoint uint64
	shared     uint64
	// arenaPages is how many pages this host's arena holds and residentPages
	// how many are taken; committed is the guest RAM the VMs it runs have
	// between them, which is what a placement measures a host by. templates are
	// the guest images it can create VMs from.
	arenaPages, residentPages int
	committed                 uint64
	templates                 []host.Template
	// journal is what this host reports of durable flush.
	journal host.Journal
	// refuse is what this host answers a stop, a delete or a fork with, which
	// is how a test stages a host's own refusal — a fork point that holds the
	// VM sealed — with the status line that host would have sent.
	refuse error
	// receives, when positive, is how many handoffs this host takes before it
	// refuses the rest, which is what a destination that dies partway through a
	// fan-out of forks looks like; received is what it took. forks is the same
	// for a fan-out on the parent's own host: how many children start before the
	// request fails with the rest of them never started.
	receives int
	received []string
	forks    int
	// refusesEveryReceive is a destination that takes no child at all, which is
	// what a host that is full, fenced or being deleted looks like to a fan-out.
	// refusedReceives is how many of the next receives it refuses before it
	// takes one, which is a destination that is briefly unable to.
	refusesEveryReceive bool
	refusedReceives     int
	// loseAnswer names the method whose next request this host carries out and
	// then reports failed, which is a request whose answer was lost on its way
	// back.
	loseAnswer string
	// quietAfterRefusal is how many surveys this host does not answer after it
	// refuses a receive, which is a destination that failed because it went
	// away for a while.
	quietAfterRefusal, quiet int
	// onRefusal runs as this host refuses a receive, which is where a test
	// changes the deployment around a receive that failed.
	onRefusal func()
	// outlives, when positive, is how many surveys the next receive goes on
	// for after its caller was told it failed: a receive whose caller hung up
	// while this host went on with it. The host reports it in Receiving until
	// the last of those surveys, and then takes the VM in, or gives it up when
	// outlivedFails says so. receiving is what it reports, and lingering how
	// many surveys are left.
	outlives, lingering int
	outlivedFails       bool
	receiving           []string
	// discardLingers is how many surveys a held receive the orchestrator ended
	// goes on being torn down for, reported in Receiving and holding
	// receiveCommit of guest RAM, as a host's does between the caller hanging
	// up and the discard. The RAM is committed from the receive's start.
	discardLingers int
	receiveCommit  uint64
	// hold is what this host's migrations report it holds a handover for.
	hold host.Seconds
	// letGoLate is how long after its hold this host lets a handover's pages
	// go: a host's timer can fire a moment after the orchestrator's clock says
	// the hold is over.
	letGoLate time.Duration
	// pulling is the VMs this host was asked to run marked to pull their whole
	// memory, which it reports with each of them and carries in their handoffs.
	pulling map[string]bool
	// nested is the VMs this host runs that are nested VMs, which it reports
	// with each of them.
	nested map[string]bool
	// rootPending is the forks this host runs whose root has not landed.
	rootPending map[string]bool
	// epochs is the deployment's record epochs, shared with fakeRecords, and
	// unseen the VMs this host runs that a survey asked it about too early to
	// see.
	epochs map[string]uint64
	unseen map[string]bool
	// handed is the handoff of each VM this host migrated away, which it hands
	// out again while it serves that VM's pages.
	handed map[string]host.MigrateResult
	// outstanding names the VMs this host still holds pages for that no
	// destination has fetched — every child of a fork point it took, until
	// that child is received somewhere — and fetched, shared by every host of
	// the fake deployment, the ones a destination has taken in. A release of a
	// handover that is outstanding and unfetched is refused, exactly as the page
	// server refuses one: those pages exist nowhere else.
	outstanding map[string]bool
	fetched     map[string]bool
	// retired, shared like fetched, names the fork children whose hold was
	// given up or ran out. Such a child has nothing left to be taken in over,
	// so a receive of it is refused.
	retired map[string]bool
	log     *[]string
	// holdReceive makes this host's receive wait until release, which is a
	// post-copy whose remaining pages are on a host that is not answering;
	// onReceive runs as it begins, which is where a test takes that host away.
	holdReceive bool
	onReceive   func()
	// onOpen runs as an open begins, before the host runs the VM, which is
	// where a test puts what the deployment does while a start is under way.
	onOpen func()
	// member is what this host reports to the membership, nil for a host that
	// keeps no cache disk.
	member   *host.Member
	held     chan struct{}
	heldOnce sync.Once
	// bucket is the deployment's control records, which this host writes as
	// it creates, takes in and deletes VMs.
	bucket *fakeRecords
	// killed is a host whose process ended with its pod, and lost, shared by
	// every host of the fake deployment, the page addresses of such hosts: a
	// receive whose pages are on one of them can never get them.
	killed bool
	lost   map[string]bool
	// partitioned is when the pod network stops cutting this host off: until
	// then nothing reaches it, though it goes on running its guests.
	// onReplyLost runs, with mu held, as an answer of this host's is lost,
	// which is where a campaign cuts the host off for a while: the answer was
	// lost because the network broke.
	partitioned time.Time
	onReplyLost func(*fakeHostClient)
}

// release lets a held receive finish, which is the source's pages arriving
// after all.
func (f *fakeHostClient) release() { f.heldOnce.Do(func() { close(f.held) }) }

// cutOff makes this host stop answering while an operation is under way,
// which is down set from inside one: a survey may be reading it at that moment.
func (f *fakeHostClient) cutOff() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = true
}

// arena sets how full this host's page store is, and promises the same amount
// of guest RAM, which is the ordinary case: a host whose arena is taken by the
// guests it is running.
func (f *fakeHostClient) arena(pages, resident int) {
	f.arenaPages, f.residentPages = pages, resident
	f.committed = uint64(resident) * (2 << 20)
}

// commit sets the guest RAM this host's VMs have between them, apart from
// whatever its arena happens to be holding.
func (f *fakeHostClient) commit(bytes uint64) { f.committed = bytes }

func (f *fakeHostClient) record(format string, args ...any) {
	*f.log = append(*f.log, f.name+" "+fmt.Sprintf(format, args...))
}

var errDown = errors.New("connection refused")

// errOutstanding is what a host answers a release of a handover whose pages no
// destination has fetched: the bytes exist nowhere else, so the release is
// refused and the host goes on holding them.
var errOutstanding = refusal("unpublished pages are still outstanding")

// wedgeGuard bounds how long a wedged host's Status blocks when nothing else
// bounds it, so a survey with no deadline of its own fails this test rather
// than hanging it.
const wedgeGuard = 6 * time.Second

// unanswered is a request this host never acted on: one that did not reach it,
// as a connection refused or a pod network that dropped it, or one it answered
// with an error status. A host that was killed or is cut off answers nothing.
// method names the call, so each fault of each method is a site of its own.
func (f *fakeHostClient) unanswered(ctx context.Context, method string) error {
	f.mu.Lock()
	cut := f.killed || time.Now().Before(f.partitioned)
	f.mu.Unlock()
	if cut {
		return errDown
	}
	if sim.Buggify(ctx, "orchestrator/host-unreachable/"+method, faultChance) {
		return errDown
	}
	if sim.Buggify(ctx, "orchestrator/host-refuses/"+method, faultChance) {
		return errRefused
	}
	return nil
}

// replyLost reports a request this host carried out whose answer is lost on
// the way back. The caller holds mu.
func (f *fakeHostClient) replyLost(ctx context.Context, method string) bool {
	lost := f.loseAnswer == method || sim.Buggify(ctx, "orchestrator/host-reply-lost/"+method, faultChance)
	if f.loseAnswer == method {
		f.loseAnswer = ""
	}
	if lost && f.onReplyLost != nil {
		f.onReplyLost(f)
	}
	return lost
}

// die is this host's process ending with its pod: every guest it ran, every
// page it held for a handover and every receive it had under way go with it,
// and it answers nothing again.
func (f *fakeHostClient) die() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = true
	f.running, f.serving, f.receiving = nil, nil, nil
	f.lost[f.page] = true
}

// runs refuses a request about a VM this host does not run, as a host does.
// The caller holds mu.
func (f *fakeHostClient) runs(id string) error {
	if !slices.Contains(f.running, id) {
		return jsonhttp.Error{Message: id + " is not running here", Status: http.StatusNotFound}
	}
	return nil
}

func (f *fakeHostClient) Status(ctx context.Context) (host.Status, error) {
	if err := f.unanswered(ctx, "Status"); err != nil {
		return host.Status{}, err
	}
	f.mu.Lock()
	wedged, down := f.wedged, f.down
	if f.quiet > 0 {
		f.quiet--
		down = true
		if f.quiet == 0 {
			f.record("answers again")
		}
	}
	f.mu.Unlock()
	if wedged {
		select {
		case <-ctx.Done():
			return host.Status{}, ctx.Err()
		case <-time.After(wedgeGuard):
			return host.Status{}, errDown
		}
	}
	if down {
		return host.Status{}, errDown
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.linger()
	running := slices.DeleteFunc(slices.Clone(f.running), func(id string) bool { return f.unseen[id] })
	records := make([]host.VM, 0, len(running))
	for _, id := range running {
		record := host.VM{ID: id, Host: f.name, Checkpoint: f.checkpoint}
		if f.pulling[id] {
			record.Pull = &host.Pull{}
		}
		record.Nested = f.nested[id]
		record.RootPending = f.rootPending[id]
		records = append(records, record)
	}
	return host.Status{Host: f.name, PageAddress: f.page, Member: f.member,
		Running: running, Serving: slices.Clone(f.serving),
		Receiving: slices.Clone(f.receiving), VMs: records, Templates: slices.Clone(f.templates),
		Journal: f.journal,
		// A placement measures a host by the RAM arena against the guest RAM it
		// has committed, so that is the pager this fake fills in.
		Pager: host.Pager{
			RAM: host.PagerKind{SharedPages: f.shared, PageBytes: 2 << 20,
				ArenaPages: f.arenaPages, ResidentPages: f.residentPages},
			CommittedBytes: f.committed}}, nil
}

// linger moves a receive that outlived its caller on by one survey, and ends
// it at the last one: the VM taken in, or given up.
func (f *fakeHostClient) linger() {
	if f.lingering == 0 {
		return
	}
	f.lingering--
	if f.lingering > 0 {
		return
	}
	for _, id := range f.receiving {
		// A fork's child claims its hold before it runs, and one whose hold
		// was given up meanwhile is discarded.
		if f.outlivedFails || f.retired[id] {
			f.record("gave %s up", id)
			f.committed -= f.receiveCommit
			continue
		}
		f.record("took %s in", id)
		f.running = append(f.running, id)
		f.fetched[id] = true
	}
	f.receiving = nil
}

func (f *fakeHostClient) Create(ctx context.Context, request host.CreateRequest) (host.CreateResult, error) {
	if err := f.unanswered(ctx, "Create"); err != nil {
		return host.CreateResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case request.Pull:
		f.record("create %s %s pull", request.ID, request.Template)
	case request.Nested:
		f.record("create %s %s nested", request.ID, request.Template)
	case request.From != nil:
		f.record("create %s from %s@%d memory=%d", request.ID, request.From.VM, request.From.Checkpoint,
			request.Memory)
	case request.Ephemeral != 0:
		f.record("create %s %s ephemeral=%d", request.ID, request.Template, request.Ephemeral)
	case request.Memory != 0 || request.Disk != 0 || request.VCPUs != 0:
		f.record("create %s %s memory=%d disk=%d vcpus=%d", request.ID, request.Template,
			request.Memory, request.Disk, request.VCPUs)
	default:
		f.record("create %s %s", request.ID, request.Template)
	}
	f.running = append(f.running, request.ID)
	f.pulling[request.ID] = request.Pull
	f.nested[request.ID] = request.Nested
	f.bucket.write(request.ID)
	if f.replyLost(ctx, "Create") {
		return host.CreateResult{}, errReplyLost
	}
	return host.CreateResult{VM: host.VM{ID: request.ID, Template: request.Template, Host: f.name}}, nil
}

func (f *fakeHostClient) ImportTemplate(ctx context.Context, image io.Reader,
	request host.ImportTemplateRequest) (host.ImportTemplateResult, error) {
	if err := f.unanswered(ctx, "ImportTemplate"); err != nil {
		return host.ImportTemplateResult{}, err
	}
	read, err := io.ReadAll(image)
	if err != nil {
		return host.ImportTemplateResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("import template %q memory=%d", read, request.Memory)
	if f.replyLost(ctx, "ImportTemplate") {
		return host.ImportTemplateResult{}, errReplyLost
	}
	return host.ImportTemplateResult{Template: host.Template{Name: "template-ab", ID: "template-ab",
		MemoryBytes: request.Memory, Imported: true}, Checkpoint: 3}, nil
}

func (f *fakeHostClient) Open(ctx context.Context, id string, request host.OpenRequest) (host.OpenResult, error) {
	if err := f.unanswered(ctx, "Open"); err != nil {
		return host.OpenResult{}, err
	}
	if f.onOpen != nil {
		f.onOpen()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.bucket.exists(id) {
		f.record("open %s refused with no record", id)
		return host.OpenResult{}, jsonhttp.Error{Message: id + " has no control record", Status: http.StatusNotFound}
	}
	if request.Epoch != 0 && request.Epoch != f.epochs[id] {
		// Something opened the VM since the caller read its epoch, and the
		// open is refused, as control.Client.OpenAfter refuses it.
		f.record("open %s refused at epoch %d", id, f.epochs[id])
		return host.OpenResult{}, jsonhttp.Error{Message: "the record moved past the epoch the caller read",
			Status: http.StatusConflict}
	}
	f.epochs[id]++
	switch {
	case request.Pull:
		f.record("open %s pull", id)
	case request.Cold && (request.Memory != 0 || request.Disk != 0):
		f.record("open %s cold memory=%d disk=%d", id, request.Memory, request.Disk)
	case request.Cold:
		f.record("open %s cold", id)
	default:
		f.record("open %s", id)
	}
	f.running = append(f.running, id)
	f.pulling[id] = request.Pull
	if f.replyLost(ctx, "Open") {
		return host.OpenResult{}, errReplyLost
	}
	return host.OpenResult{VM: host.VM{ID: id, Host: f.name, Checkpoint: 7}, Cold: request.Cold}, nil
}

func (f *fakeHostClient) Fork(ctx context.Context, parent string, request host.ForkRequest) (host.ForkResult, error) {
	if err := f.unanswered(ctx, "Fork"); err != nil {
		return host.ForkResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("fork %s %s", parent, strings.Join(request.IDs, ","))
	if err := f.runs(parent); err != nil {
		return host.ForkResult{}, err
	}
	result := host.ForkResult{Capture: 0.01, Hold: f.hold}
	for index, id := range request.IDs {
		if f.forks > 0 && index >= f.forks {
			// Some of them were handed over and the rest never will be, which is
			// what a host that died partway through a fan-out leaves behind.
			return host.ForkResult{}, jsonhttp.Error{Message: "the host could not hand it over", Status: http.StatusInternalServerError}
		}
		source := f.page
		if request.Destination == "" {
			// A child this host takes in itself is served nothing.
			source = ""
		}
		// The hold is this host's from here, wherever the child lands: it holds
		// the parent sealed until the child has every page it inherited, and
		// this host reports it as the handover it is.
		f.serving = append(f.serving, id)
		f.outstanding[id] = true
		if f.hold > 0 {
			// Every hold ends on its own at the end of the hold this host
			// reports, whether or not anything can reach it.
			time.AfterFunc(f.hold.Duration(), func() {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.retire(id)
			})
		}
		result.Handoffs = append(result.Handoffs, host.Handoff{VMID: id, Parent: parent, Source: source,
			Pull: request.Pull})
	}
	if f.replyLost(ctx, "Fork") {
		return host.ForkResult{}, errReplyLost
	}
	return result, nil
}

func (f *fakeHostClient) Capture(ctx context.Context, id string, request host.CaptureRequest) (host.CaptureResult, error) {
	if err := f.unanswered(ctx, "Capture"); err != nil {
		return host.CaptureResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	result := host.CaptureResult{VM: id, Checkpoint: 11}
	switch {
	case request.Into != "":
		f.record("capture %s into %s", id, request.Into)
		result = host.CaptureResult{VM: request.Into, Checkpoint: 12}
	case request.Keep:
		f.record("capture %s kept", id)
	default:
		f.record("capture %s", id)
	}
	if err := f.runs(id); err != nil {
		return host.CaptureResult{}, err
	}
	if request.Into != "" {
		// The copy is a VM of its own from here, stopped at the checkpoint.
		f.bucket.write(request.Into)
	}
	if f.replyLost(ctx, "Capture") {
		return host.CaptureResult{}, errReplyLost
	}
	return result, nil
}

func (f *fakeHostClient) Console(ctx context.Context, id string, since int64) (host.Console, error) {
	if err := f.unanswered(ctx, "Console"); err != nil {
		return host.Console{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("console %s %d", id, since)
	return host.Console{VM: id, Offset: since, Next: since + 3, Data: "hi\n"}, nil
}

func (f *fakeHostClient) WriteConsole(ctx context.Context, id string, data string) error {
	if err := f.unanswered(ctx, "WriteConsole"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("write %s %q", id, data)
	if f.replyLost(ctx, "WriteConsole") {
		return errReplyLost
	}
	return nil
}

func (f *fakeHostClient) Exec(ctx context.Context, id string, request host.ExecRequest) (host.ExecResult, error) {
	if err := f.unanswered(ctx, "Exec"); err != nil {
		return host.ExecResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("exec %s %q", id, request.Cmd)
	if f.down {
		return host.ExecResult{}, errDown
	}
	if f.replyLost(ctx, "Exec") {
		return host.ExecResult{}, errReplyLost
	}
	return host.ExecResult{Exit: 0, Stdout: f.name + " ran " + request.Cmd + "\n"}, nil
}

// Migrate stops the guest and hands the VM over. The handoff's checkpoint is
// the record's epoch, which in this fake moves exactly when a checkpoint would:
// a receive refuses a handoff the record has moved past.
func (f *fakeHostClient) Migrate(ctx context.Context, id string, request host.MigrateRequest) (host.MigrateResult, error) {
	if err := f.unanswered(ctx, "Migrate"); err != nil {
		return host.MigrateResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("migrate %s %s", id, request.Destination)
	if err := f.runs(id); err != nil {
		return host.MigrateResult{}, err
	}
	f.running = slices.DeleteFunc(f.running, func(value string) bool { return value == id })
	f.serving = append(f.serving, id)
	if f.hold > 0 {
		// A host gives the pages up on its own at the end of the hold it
		// reports, whether or not anything can reach it.
		time.AfterFunc(f.hold.Duration()+f.letGoLate, func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.serving = slices.DeleteFunc(f.serving, func(value string) bool { return value == id })
		})
	}
	result := host.MigrateResult{Handoff: host.Handoff{VMID: id, Checkpoint: f.epochs[id],
		Source: f.page, PageSize: 2 << 20, Pull: f.pulling[id]}, Hold: f.hold}
	if f.handed == nil {
		f.handed = make(map[string]host.MigrateResult)
	}
	f.handed[id] = result
	if f.replyLost(ctx, "Migrate") {
		return host.MigrateResult{}, errReplyLost
	}
	return result, nil
}

// Handed hands out again the handoff of a VM this host migrated away, for as
// long as it still serves that VM's pages.
func (f *fakeHostClient) Handed(ctx context.Context, id string) (host.MigrateResult, bool, error) {
	if err := f.unanswered(ctx, "Handed"); err != nil {
		return host.MigrateResult{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.serving, id) {
		return host.MigrateResult{}, false, nil
	}
	handed, found := f.handed[id]
	return handed, found, nil
}

func (f *fakeHostClient) Receive(ctx context.Context, handoff host.Handoff) (host.ReceiveResult, error) {
	if err := f.unanswered(ctx, "Receive"); err != nil {
		return host.ReceiveResult{}, err
	}
	f.mu.Lock()
	if handoff.Pull {
		f.record("receive %s %s pull", handoff.VMID, handoff.Source)
	} else {
		f.record("receive %s %s", handoff.VMID, handoff.Source)
	}
	if handoff.Parent != "" && f.retired[handoff.VMID] {
		f.mu.Unlock()
		return host.ReceiveResult{}, refusal("the fork point the child inherits is retired")
	}
	if handoff.Parent != "" && f.fetched[handoff.VMID] {
		// The child was taken in once, and its hold claimed.
		f.mu.Unlock()
		return host.ReceiveResult{}, refusal("the child's hold is already claimed")
	}
	if handoff.Parent == "" && handoff.Checkpoint != 0 && handoff.Checkpoint != f.epochs[handoff.VMID] {
		// Something opened the VM since it was handed over, so the record no
		// longer selects what the handoff carries.
		f.mu.Unlock()
		return host.ReceiveResult{}, refusal("the record moved past the handoff")
	}
	if handoff.Source != "" && f.lost[handoff.Source] {
		// The pages no checkpoint has were on a host that is gone.
		f.mu.Unlock()
		return host.ReceiveResult{}, refusal("the source of the pages is gone")
	}
	if slices.Contains(f.receiving, handoff.VMID) {
		// A host admits one receive of a VM at a time.
		f.mu.Unlock()
		return host.ReceiveResult{}, refusal("that VM is already being received here")
	}
	if f.outlives > 0 {
		f.receiving = append(f.receiving, handoff.VMID)
		f.lingering, f.outlives = f.outlives, 0
		f.mu.Unlock()
		return host.ReceiveResult{}, errors.New("the connection was reset")
	}
	if f.refusesEveryReceive || f.refusedReceives > 0 || (f.receives > 0 && len(f.received) >= f.receives) {
		f.refusedReceives = max(f.refusedReceives-1, 0)
		f.quiet = f.quietAfterRefusal
		refused := f.onRefusal
		f.mu.Unlock()
		if refused != nil {
			refused()
		}
		return host.ReceiveResult{}, refusal("the destination could not start it")
	}
	hold, began := f.holdReceive, f.onReceive
	if f.discardLingers > 0 {
		f.committed += f.receiveCommit
	}
	f.mu.Unlock()
	if hold {
		if began != nil {
			began()
		}
		select {
		case <-f.held:
		case <-ctx.Done():
			// The orchestrator ended the migration, so the guest half received
			// here is given up: its memory is part this host's and part on a
			// host that is gone, and nothing can publish that.
			f.mu.Lock()
			f.record("receive-discarded %s", handoff.VMID)
			if f.discardLingers > 0 {
				f.receiving = append(f.receiving, handoff.VMID)
				f.lingering, f.outlivedFails = f.discardLingers, true
			}
			f.mu.Unlock()
			return host.ReceiveResult{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.received = append(f.received, handoff.VMID)
	f.running = append(f.running, handoff.VMID)
	// A fork's child is published as its receive ends, and a migrated VM's
	// record moves to its new writer.
	f.bucket.write(handoff.VMID)
	f.epochs[handoff.VMID]++
	f.pulling[handoff.VMID] = handoff.Pull
	// The child holds every page it inherited, which is what lets the source
	// release the hold it kept for it.
	f.fetched[handoff.VMID] = true
	if f.replyLost(ctx, "Receive") {
		return host.ReceiveResult{}, errReplyLost
	}
	return host.ReceiveResult{VM: host.VM{ID: handoff.VMID, Host: f.name},
		Pause: 0.09, Stream: 1.5, PeerPages: 24, Unpublished: 6}, nil
}

func (f *fakeHostClient) Released(ctx context.Context, id string) error {
	if err := f.unanswered(ctx, "Released"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("released %s", id)
	if f.outstanding[id] && !f.fetched[id] {
		// The destination has not fetched what this host holds for it, and the
		// pages exist nowhere else: the release is refused and the host goes on
		// holding them, which for a fork's parent means it stays sealed.
		return errOutstanding
	}
	f.serving = slices.DeleteFunc(f.serving, func(value string) bool { return value == id })
	if f.replyLost(ctx, "Released") {
		return errReplyLost
	}
	return nil
}

// Abandoned gives a handover up whatever is still outstanding on it: the VM it
// belongs to is one nothing will ever ask for again. A child a destination took
// in has claimed its hold, which the answer says.
func (f *fakeHostClient) Abandoned(ctx context.Context, id string) (host.AbandonedResult, error) {
	// A request on a context that has ended never leaves, as a real client's
	// does not.
	if err := ctx.Err(); err != nil {
		return host.AbandonedResult{}, err
	}
	if err := f.unanswered(ctx, "Abandoned"); err != nil {
		return host.AbandonedResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	claimed := f.fetched[id] && !f.retired[id]
	if claimed {
		f.record("abandoned %s claimed", id)
	} else {
		f.record("abandoned %s", id)
	}
	f.retire(id)
	if f.replyLost(ctx, "Abandoned") {
		return host.AbandonedResult{}, errReplyLost
	}
	return host.AbandonedResult{Claimed: claimed}, nil
}

// retire ends a hold this host keeps: it serves nothing for the VM any more,
// and a fork's child can no longer be taken in over it.
func (f *fakeHostClient) retire(id string) {
	delete(f.outstanding, id)
	f.retired[id] = true
	f.serving = slices.DeleteFunc(f.serving, func(value string) bool { return value == id })
}

// Stop closes the guest and leaves the VM: this host stops running it, and
// nothing else about it changes.
func (f *fakeHostClient) Stop(ctx context.Context, id string, request host.StopRequest) (host.StopResult, error) {
	if err := f.unanswered(ctx, "Stop"); err != nil {
		return host.StopResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case request.Suspend && request.Keep:
		f.record("suspend %s kept", id)
	case request.Suspend:
		f.record("suspend %s", id)
	case request.Keep:
		f.record("stop %s kept", id)
	default:
		f.record("stop %s", id)
	}
	if f.refuse != nil {
		return host.StopResult{}, f.refuse
	}
	if err := f.runs(id); err != nil {
		return host.StopResult{}, err
	}
	f.running = slices.DeleteFunc(f.running, func(value string) bool { return value == id })
	if f.replyLost(ctx, "Stop") {
		return host.StopResult{}, errReplyLost
	}
	return host.StopResult{VM: id, Checkpoint: 13}, nil
}

// Delete closes the VM if this host runs it, and removes its control record,
// which any host can do.
func (f *fakeHostClient) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.unanswered(ctx, "Delete"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("delete %s", id)
	if f.refuse != nil {
		return f.refuse
	}
	f.running = slices.DeleteFunc(f.running, func(value string) bool { return value == id })
	f.bucket.remove(id)
	if f.replyLost(ctx, "Delete") {
		return errReplyLost
	}
	return nil
}

// Kept answers from the one kept checkpoint every VM of this fake has.
func (f *fakeHostClient) Kept(ctx context.Context, id string) (host.KeptResult, error) {
	if err := f.unanswered(ctx, "Kept"); err != nil {
		return host.KeptResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("kept %s", id)
	return host.KeptResult{VM: id, Kept: []host.Kept{{Checkpoint: 7, State: true}}}, nil
}

func (f *fakeHostClient) Release(ctx context.Context, id string, checkpoint uint64) error {
	if err := f.unanswered(ctx, "Release"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("release %s@%d", id, checkpoint)
	if f.refuse != nil {
		return f.refuse
	}
	if f.replyLost(ctx, "Release") {
		return errReplyLost
	}
	return nil
}

// deployment is an orchestrator over fakes: the pods, the bucket and the hosts.
type deployment struct {
	orchestrator *orchestrator
	pods         *fakePods
	records      *fakeRecords
	hosts        map[string]*fakeHostClient
	log          []string
	next         int
	// mu is the whole fake deployment's lock, and fetched, retired and lost
	// what its hosts share: what one host holds for a handover is released by
	// what another host fetched, a child whose hold one host gave up cannot be
	// taken in by another, and a host that died took the pages it served with
	// it.
	mu                     *sync.Mutex
	fetched, retired, lost map[string]bool
}

// newDeployment builds one orchestrator over the named hosts, each running the
// VMs it is given.
func newDeployment(t *testing.T, running map[string][]string) *deployment {
	t.Helper()
	mu := new(sync.Mutex)
	d := &deployment{pods: &fakePods{mu: mu}, records: &fakeRecords{epochs: map[string]uint64{}, mu: mu},
		hosts: map[string]*fakeHostClient{}, mu: mu,
		fetched: map[string]bool{}, retired: map[string]bool{}, lost: map[string]bool{}}
	for _, name := range slices.Sorted(maps.Keys(running)) {
		d.addHost(name, running[name])
	}
	d.orchestrator = &orchestrator{pods: d.pods, records: d.records,
		dial: func(p pod) hostClient { return d.hosts[p.Name] },
		identify: func() string {
			d.next++
			return "vm-new-" + strconv.Itoa(d.next)
		},
		apiPort: 8080, pagePort: 8081, table: testTable(t),
		// The code a deployment that sets none runs under, as loadConfig gives.
		code: rank.DefaultCode,
		// A migration watches the host that holds its pages, and retries a
		// receive after a wait; a test waits seconds for neither.
		sourceWatch: 10 * time.Millisecond,
		handover:    handover.Policy{Pause: time.Millisecond, MaxPause: 4 * time.Millisecond, PerDestination: 2}}
	return d
}

// addHost lists one more ready host pod, running the VMs it is given, each of
// which has a control record. The caller holds no lock: a survey may be
// listing the pods.
func (d *deployment) addHost(name string, running []string) *fakeHostClient {
	d.mu.Lock()
	defer d.mu.Unlock()
	address := "10.0.0." + strconv.Itoa(len(d.hosts)+1)
	d.pods.pods = append(d.pods.pods, pod{Name: name, IP: address, Ready: true})
	h := &fakeHostClient{mu: d.mu, name: name, running: slices.Clone(running),
		serving: []string{}, page: address + ":8081", log: &d.log, held: make(chan struct{}),
		outstanding: map[string]bool{}, fetched: d.fetched, retired: d.retired, epochs: d.records.epochs,
		unseen:  map[string]bool{},
		pulling: map[string]bool{}, nested: map[string]bool{}, bucket: d.records, lost: d.lost}
	d.hosts[name] = h
	d.records.ids = append(d.records.ids, running...)
	return h
}

// testTable is a VM table in a file of this test's own, which is the same
// SQLite the deployment runs on rather than a stand-in for it.
func testTable(t *testing.T) *table {
	t.Helper()
	catalog, err := openTable(t.Context(), filepath.Join(t.TempDir(), "orchestrator.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := catalog.Close(); err != nil {
			t.Error(err)
		}
	})
	return catalog
}

// TestAWedgedHostDoesNotBlockTheDeployment: every request the orchestrator
// serves begins by asking every host what it is running, so one host that
// accepts the connection and never answers is one host that stops the whole
// deployment. The survey bounds what it waits for, and the rest of the
// deployment goes on being usable: the wedged host is reported with its
// failure and skipped.
func TestAWedgedHostDoesNotBlockTheDeployment(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {"vm-a"}, "host-2": {}})
	d.hosts["host-0"].wedged = true
	// Generous next to the seconds a survey may wait, and far below the
	// minutes a request costs when nothing bounds it.
	const patience = 4 * time.Second
	within := func(what string, run func() error) {
		t.Helper()
		began := time.Now()
		err := run()
		elapsed := time.Since(began)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if elapsed > patience {
			t.Fatalf("%s took %s with one host wedged, want under %s", what, elapsed, patience)
		}
	}
	within("listing the VMs", func() error {
		_, err := d.orchestrator.VMs(t.Context())
		return err
	})
	within("running a command on another host", func() error {
		_, err := d.orchestrator.Exec(t.Context(), "vm-a", host.ExecRequest{Cmd: "true"})
		return err
	})
	within("migrating between two other hosts", func() error {
		_, err := d.orchestrator.Migrate(t.Context(), "vm-a", "host-2")
		return err
	})
}

// A create goes to the host with the most memory free, which is what a VM
// costs a host: two guests on host-0 have taken most of its arena, and the one
// on host-1 has not.
func TestCreatePlacesOnTheHostWithTheMostMemoryFree(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a", "vm-b"}, "host-1": {"vm-c"}})
	d.hosts["host-0"].arena(1024, 900)
	d.hosts["host-1"].arena(1024, 300)
	result, err := d.orchestrator.Create(t.Context(), orch.CreateRequest{Template: "alpine"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Host != "host-1" {
		t.Fatalf("the VM went to %s, want the host with the most memory free", result.Host)
	}
	if result.Result.VM.ID != "vm-new-1" {
		t.Fatalf("identity %s", result.Result.VM.ID)
	}
	if len(d.log) != 1 || d.log[0] != "host-1 create vm-new-1 alpine" {
		t.Fatalf("the deployment did %v", d.log)
	}
}

func TestCreateSkipsAHostThatDoesNotAnswer(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a", "vm-b"}, "host-1": {}})
	d.hosts["host-1"].down = true
	result, err := d.orchestrator.Create(t.Context(), orch.CreateRequest{Template: "alpine"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Host != "host-0" {
		t.Fatalf("the VM went to %s, want the only host that answered", result.Host)
	}
}

func TestCreateWithoutAnyLiveHostIsRefused(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}})
	d.hosts["host-0"].down = true
	_, err := d.orchestrator.Create(t.Context(), orch.CreateRequest{Template: "alpine"})
	if !errors.Is(err, errNoHost) {
		t.Fatalf("error %v, want no host available", err)
	}
}

func TestForkRunsOnTheVMsOwnHost(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	result, err := d.orchestrator.Fork(t.Context(), "vm-a", orch.ForkRequest{Count: 3})
	if err != nil {
		t.Fatal(err)
	}
	if result.Host != "host-0" || result.To != "host-0" || len(result.Children) != 3 {
		t.Fatalf("result %+v", result)
	}
	// One request and one pause of the parent, however many children it starts,
	// and the same handshake a fork onto another host has: the parent's host
	// hands each child over, takes it in itself — over the pages, so with no
	// address to fetch from — and is told to release it.
	want := []string{
		"host-0 fork vm-a vm-new-1,vm-new-2,vm-new-3",
		"host-0 receive vm-new-1 ", "host-0 released vm-new-1",
		"host-0 receive vm-new-2 ", "host-0 released vm-new-2",
		"host-0 receive vm-new-3 ", "host-0 released vm-new-3",
	}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
}

// A fork placed on another host is the migration handshake: the parent's host
// builds the handoffs and serves the pages, the destination starts each child,
// and the parent takes its pages back only once every child has them.
func TestForkOnAnotherHostCarriesTheHandoff(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	result, err := d.orchestrator.Fork(t.Context(), "vm-a", orch.ForkRequest{Count: 2, To: "host-1"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Host != "host-0" || result.To != "host-1" || len(result.Children) != 2 {
		t.Fatalf("result %+v", result)
	}
	want := []string{
		"host-0 fork vm-a vm-new-1,vm-new-2",
		"host-1 receive vm-new-1 " + d.hosts["host-0"].page, "host-0 released vm-new-1",
		"host-1 receive vm-new-2 " + d.hosts["host-0"].page, "host-0 released vm-new-2",
	}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
}

func TestForkOfAVMNoHostRunsIsNotFound(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}})
	_, err := d.orchestrator.Fork(t.Context(), "vm-z", orch.ForkRequest{Count: 1})
	if !errors.Is(err, errNotFound) {
		t.Fatalf("error %v, want not found", err)
	}
}

func TestMigrateStopsReceivesAndOnlyThenReleases(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	result, err := d.orchestrator.Migrate(t.Context(), "vm-a", "host-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.From != "host-0" || result.To != "host-1" || result.Pause != 0.09 || result.Stream != 1.5 {
		t.Fatalf("result %+v", result)
	}
	if result.PeerPages != 24 || result.Unpublished != 6 {
		t.Fatalf("result %+v", result)
	}
	want := []string{
		"host-0 migrate vm-a 10.0.0.2:8081",
		"host-1 receive vm-a 10.0.0.1:8081",
		"host-0 released vm-a",
	}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
}

func TestMigrateWithoutADestinationPicksAnotherHost(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	result, err := d.orchestrator.Migrate(t.Context(), "vm-a", "")
	if err != nil {
		t.Fatal(err)
	}
	if result.To != "host-1" {
		t.Fatalf("the VM went to %s, want the other host", result.To)
	}
}

func TestMigrateToTheHostAlreadyRunningItIsRefused(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	_, err := d.orchestrator.Migrate(t.Context(), "vm-a", "host-0")
	if !errors.Is(err, errRequest) {
		t.Fatalf("error %v, want an invalid request", err)
	}
	if len(d.log) != 0 {
		t.Fatalf("a refused migration did %v", d.log)
	}
}

// TestTwoHostsClaimingOneVMStopsTheDeploymentActingOnIt: one VM has one writer,
// and two hosts reporting it is the deployment saying otherwise — a recovery
// raced a host that was not really gone, or a handoff left both ends claiming
// it. Picking one of them is the worst answer available: a migration off the
// wrong one hands a third host a stale writer's pages, and a fork of it takes
// its fork point from a writer nothing selects. Every request that must name the
// host a VM runs on refuses instead, and the operator is told which hosts
// disagree.
func TestTwoHostsClaimingOneVMStopsTheDeploymentActingOnIt(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {"vm-a"}, "host-2": {}})
	refused := map[string]func() error{
		"migrating": func() error {
			_, err := d.orchestrator.Migrate(t.Context(), "vm-a", "host-2")
			return err
		},
		"forking": func() error {
			_, err := d.orchestrator.Fork(t.Context(), "vm-a", orch.ForkRequest{Count: 1})
			return err
		},
		"capturing": func() error {
			_, err := d.orchestrator.Capture(t.Context(), "vm-a", orch.CaptureRequest{})
			return err
		},
		"running a command in": func() error {
			_, err := d.orchestrator.Exec(t.Context(), "vm-a", host.ExecRequest{Cmd: "true"})
			return err
		},
		"deleting": func() error { return d.orchestrator.Delete(t.Context(), "vm-a") },
		"recovering": func() error {
			_, err := d.orchestrator.Recover(t.Context(), "vm-a", false)
			return err
		},
	}
	for _, what := range slices.Sorted(maps.Keys(refused)) {
		if err := refused[what](); !errors.Is(err, errContested) {
			t.Fatalf("%s a VM two hosts claim reported %v, want the contested claim", what, err)
		}
	}
	if len(d.log) != 0 {
		t.Fatalf("the deployment did %v to a VM two hosts claim, want nothing", d.log)
	}
	if got := statusOf(fmt.Errorf("%w: host-0 and host-1", errContested)); got != 409 {
		t.Fatalf("a contested claim answers %d, want 409", got)
	}
}

// A VM the table names is still routed to the host the table names — but only
// while that host is the only one claiming it.
func TestTheTableDoesNotResolveAContestedClaim(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {"vm-a"}})
	if err := d.orchestrator.table.Record(t.Context(), vmRecord{ID: "vm-a", Host: "host-0",
		State: stateRunning}); err != nil {
		t.Fatal(err)
	}
	_, err := d.orchestrator.Exec(t.Context(), "vm-a", host.ExecRequest{Cmd: "true"})
	if !errors.Is(err, errContested) {
		t.Fatalf("an exec routed by the table reported %v, want the contested claim", err)
	}
}

func TestDrainingAHostHasNowhereToGoOnItsOwn(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}})
	_, err := d.orchestrator.Migrate(t.Context(), "vm-a", "")
	if !errors.Is(err, errNoHost) {
		t.Fatalf("error %v, want no host available", err)
	}
}

func TestRecoverOpensTheVMOnALiveHost(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	// The host that ran vm-a is gone: its pod no longer exists.
	d.pods.pods = d.pods.pods[1:]
	delete(d.hosts, "host-0")
	result, err := d.orchestrator.Recover(t.Context(), "vm-a", false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Host != "host-1" || result.Result.VM.Checkpoint != 7 {
		t.Fatalf("result %+v", result)
	}
	if len(d.log) != 1 || d.log[0] != "host-1 open vm-a" {
		t.Fatalf("the deployment did %v", d.log)
	}
}

func TestRecoverIsRefusedWhileALiveHostRunsTheVM(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	_, err := d.orchestrator.Recover(t.Context(), "vm-a", false)
	if !errors.Is(err, errRunning) {
		t.Fatalf("error %v, want the running conflict", err)
	}
	if len(d.log) != 0 {
		t.Fatalf("a refused recovery did %v", d.log)
	}
}

// TestRecoverIsRefusedWhileAHostDoesNotAnswer: a host that does not answer one
// Status call is not a host that is gone. It may be a host whose guest is
// running perfectly well behind a slow or dropped request, and taking the epoch
// from it would start a second writer of the same VM. Recovery needs positive
// evidence of the loss, which one failed call is not.
func TestRecoverIsRefusedWhileAHostDoesNotAnswer(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	d.hosts["host-0"].down = true
	_, err := d.orchestrator.Recover(t.Context(), "vm-a", false)
	if !errors.Is(err, errRunning) {
		t.Fatalf("error %v, want a refusal: nothing proves the VM's host is gone", err)
	}
	if len(d.log) != 0 {
		t.Fatalf("a refused recovery did %v", d.log)
	}
}

// TestRecoverByForceTakesTheOperatorsWord: an operator who knows the host is
// gone — a pod the API is slow to stop listing, a node that went with it — says
// so, and the recovery proceeds. Nothing else about it changes: a host that
// answers and says it runs the VM is still never fenced.
func TestRecoverByForceTakesTheOperatorsWord(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	d.hosts["host-0"].down = true
	result, err := d.orchestrator.Recover(t.Context(), "vm-a", true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Host != "host-1" {
		t.Fatalf("the VM was reopened on %s", result.Host)
	}
}

// A fork's child exists only on its host until its first checkpoint lands, so
// a host lost before then loses it: a recovery finds nothing to open, frees the
// identity through a host that answered, forgets the row, and reports the VM
// lost rather than leaving a VM no start could ever open.
func TestRecoverFreesAForkLostBeforeItsFirstCheckpointLanded(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	d.records.pending = map[string]bool{"vm-a": true}
	d.pods.pods = d.pods.pods[1:]
	delete(d.hosts, "host-0")
	_, err := d.orchestrator.Recover(t.Context(), "vm-a", false)
	if !errors.Is(err, errLost) {
		t.Fatalf("recovering a fork lost before its root landed = %v, want errLost", err)
	}
	if status := statusOf(err); status != http.StatusGone {
		t.Fatalf("a lost fork is reported as %d, want %d", status, http.StatusGone)
	}
	if want := []string{"host-1 delete vm-a"}; !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
}

// A start is as sure as a recovery before it frees a fork's identity: a host
// that did not answer may be running the child still, publishing its root.
func TestAForkWhoseHostIsQuietIsNotFreed(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	d.records.pending = map[string]bool{"vm-a": true}
	d.hosts["host-0"].down = true
	_, err := d.orchestrator.Start(t.Context(), "vm-a", orch.StartRequest{})
	if !errors.Is(err, errRunning) {
		t.Fatalf("starting a fork whose host is quiet = %v, want errRunning", err)
	}
	if len(d.log) != 0 {
		t.Fatalf("a refused start did %v", d.log)
	}
	if _, err := d.orchestrator.Recover(t.Context(), "vm-a", true); !errors.Is(err, errLost) {
		t.Fatalf("recovering it by force = %v, want errLost", err)
	}
	if want := []string{"host-1 delete vm-a"}; !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
}

// TestRecoverByForceStillRefusesALiveHostsVM: force is evidence about a host
// that says nothing, never permission to fence one that is answering.
func TestRecoverByForceStillRefusesALiveHostsVM(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	_, err := d.orchestrator.Recover(t.Context(), "vm-a", true)
	if !errors.Is(err, errRunning) {
		t.Fatalf("error %v, want a refusal: host-0 answered and runs vm-a", err)
	}
}

// TestRecoverProceedsAfterTheHostWasKilled: deleting the pod is the evidence.
// Once the Kubernetes API no longer lists it, nothing can be running that VM
// and its epoch is free to take.
func TestRecoverProceedsAfterTheHostWasKilled(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	if _, err := d.orchestrator.Kill(t.Context(), "host-0"); err != nil {
		t.Fatal(err)
	}
	result, err := d.orchestrator.Recover(t.Context(), "vm-a", false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Host != "host-1" {
		t.Fatalf("the VM was reopened on %s", result.Host)
	}
}

func TestVMsMergesTheBucketWithWhatTheHostsRun(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	// vm-b has a control record and no host: its host is gone.
	d.records.ids = append(d.records.ids, "vm-b")
	vms, err := d.orchestrator.VMs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 2 {
		t.Fatalf("vms %+v", vms)
	}
	if vms[0].ID != "vm-a" || vms[0].Host != "host-0" {
		t.Fatalf("vm-a is %+v", vms[0])
	}
	if vms[1].ID != "vm-b" || vms[1].Host != "" {
		t.Fatalf("vm-b is %+v", vms[1])
	}
}

// TestVMsReportsWhatTheRunningHostSaysOfEachVM: a listing's checkpoint is the
// running host's account of it, not the bucket's, which is a checkpoint behind.
func TestVMsReportsWhatTheRunningHostSaysOfEachVM(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}})
	d.hosts["host-0"].checkpoint = 42
	vms, err := d.orchestrator.VMs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 1 || vms[0].Checkpoint != 42 {
		t.Fatalf("vms %+v", vms)
	}
}

// A fork returns before its child's root lands, and a listing says which
// children are not durable yet: what the host running one reports.
func TestVMsReportsAForkWhoseRootHasNotLanded(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a", "vm-b"}})
	d.hosts["host-0"].rootPending = map[string]bool{"vm-b": true}
	vms, err := d.orchestrator.VMs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 2 || vms[0].RootPending || !vms[1].RootPending {
		t.Fatalf("vms %+v, want only vm-b's root pending", vms)
	}
}

// TestHostsReportsThePagersSharing: forking is visible in the shared page
// count, so a host report carries it.
func TestHostsReportsThePagersSharing(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}})
	d.hosts["host-0"].shared = 1234
	hosts, err := d.orchestrator.Hosts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Pager.SharedPages() != 1234 {
		t.Fatalf("hosts %+v", hosts)
	}
}

func TestVMsIncludesAForkWhoseFirstCheckpointHasNotLanded(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}})
	d.records.ids = []string{}
	vms, err := d.orchestrator.VMs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 1 || vms[0].ID != "vm-a" || vms[0].Host != "host-0" {
		t.Fatalf("vms %+v", vms)
	}
}

func TestHostsReportsWhyAHostDidNotAnswer(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	d.hosts["host-1"].down = true
	hosts, err := d.orchestrator.Hosts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 {
		t.Fatalf("hosts %+v", hosts)
	}
	if hosts[0].Name != "host-0" || hosts[0].Error != "" || len(hosts[0].Running) != 1 {
		t.Fatalf("host-0 is %+v", hosts[0])
	}
	if hosts[1].Name != "host-1" || !strings.Contains(hosts[1].Error, "connection refused") {
		t.Fatalf("host-1 is %+v", hosts[1])
	}
	if hosts[1].API != "http://10.0.0.2:8080" {
		t.Fatalf("host-1 api %s", hosts[1].API)
	}
}

func TestKillDeletesThePod(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}})
	result, err := d.orchestrator.Kill(t.Context(), "host-0")
	if err != nil {
		t.Fatal(err)
	}
	if result.Host != "host-0" || len(d.pods.deleted) != 1 || d.pods.deleted[0] != "host-0" {
		t.Fatalf("result %+v, deleted %v", result, d.pods.deleted)
	}
}

func TestKillWithoutANameIsRefused(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}})
	_, err := d.orchestrator.Kill(t.Context(), "")
	if !errors.Is(err, errRequest) {
		t.Fatalf("error %v, want an invalid request", err)
	}
}

func TestConsoleIsProxiedToTheHostRunningTheVM(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {"vm-a"}})
	window, err := d.orchestrator.Console(t.Context(), "vm-a", 12)
	if err != nil {
		t.Fatal(err)
	}
	if window.Data != "hi\n" || window.Next != 15 {
		t.Fatalf("window %+v", window)
	}
	if err := d.orchestrator.WriteConsole(t.Context(), "vm-a", "ls\n"); err != nil {
		t.Fatal(err)
	}
	want := []string{"host-1 console vm-a 12", `host-1 write vm-a "ls\n"`}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
}

// TestExecRunsOnTheHostTheTableNames, which is what the table is for: a VM is
// addressed without anything having to ask every host which one holds it.
func TestExecRunsOnTheHostTheTableNames(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {"vm-b"}})
	// The table is written by what moves a VM and by the reconciler, never by a
	// read, so this is what has the rows a routed command is supposed to use.
	if err := d.orchestrator.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	d.log = nil
	outcome, err := d.orchestrator.Exec(t.Context(), "vm-b", host.ExecRequest{Cmd: "echo hello"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Host != "host-1" || outcome.VM != "vm-b" {
		t.Fatalf("outcome %+v, want vm-b on host-1", outcome)
	}
	if outcome.Result.Stdout != "host-1 ran echo hello\n" {
		t.Fatalf("the guest said %q", outcome.Result.Stdout)
	}
	want := []string{`host-1 exec vm-b "echo hello"`}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
	row, found, err := d.orchestrator.table.VM(t.Context(), "vm-b")
	if err != nil || !found {
		t.Fatalf("the table found vm-b %t: %v", found, err)
	}
	if row.Host != "host-1" || row.State != stateRunning {
		t.Fatalf("row %+v, want vm-b running on host-1", row)
	}
}

// TestExecFollowsAVMThatMoved: the table is what the orchestrator last did, the
// survey is what is true now, and a stale row must not send a command to the
// host the VM has left.
func TestExecFollowsAVMThatMoved(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	if err := d.orchestrator.table.Record(t.Context(),
		vmRecord{ID: "vm-a", Host: "host-1", State: stateRunning}); err != nil {
		t.Fatal(err)
	}
	outcome, err := d.orchestrator.Exec(t.Context(), "vm-a", host.ExecRequest{Cmd: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Host != "host-0" {
		t.Fatalf("the command went to %s, want the host that says it runs vm-a", outcome.Host)
	}
}

// TestExecReportsAVMNoHostRuns rather than guessing at one.
func TestExecReportsAVMNoHostRuns(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}})
	_, err := d.orchestrator.Exec(t.Context(), "vm-gone", host.ExecRequest{Cmd: "true"})
	if !errors.Is(err, errNotFound) {
		t.Fatalf("error %v, want not found", err)
	}
}

// TestCreateRecordsTheVMBeforeAndAfterItStarts, so that a table read during a
// slow boot shows a VM being created rather than nothing at all.
func TestCreateRecordsTheVMBeforeAndAfterItStarts(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}})
	result, err := d.orchestrator.Create(t.Context(), orch.CreateRequest{Template: "alpine"})
	if err != nil {
		t.Fatal(err)
	}
	row, found, err := d.orchestrator.table.VM(t.Context(), result.Result.VM.ID)
	if err != nil || !found {
		t.Fatalf("the table found the new VM %t: %v", found, err)
	}
	if row.Host != "host-0" || row.State != stateRunning || row.Template != "alpine" {
		t.Fatalf("row %+v, want it running on host-0 from alpine", row)
	}
}

// TestMigrateRecordsTheMoveAndThenTheDestination.
func TestMigrateRecordsTheMoveAndThenTheDestination(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	if _, err := d.orchestrator.Migrate(t.Context(), "vm-a", "host-1"); err != nil {
		t.Fatal(err)
	}
	row, _, err := d.orchestrator.table.VM(t.Context(), "vm-a")
	if err != nil {
		t.Fatal(err)
	}
	if row.Host != "host-1" || row.State != stateRunning || row.From != "" || row.To != "" {
		t.Fatalf("row %+v, want vm-a settled on host-1", row)
	}
}

// TestDeleteForgetsTheVM.
func TestDeleteForgetsTheVM(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}})
	if err := d.orchestrator.Delete(t.Context(), "vm-a"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := d.orchestrator.table.VM(t.Context(), "vm-a"); err != nil || found {
		t.Fatalf("vm-a is still in the table: found %t, %v", found, err)
	}
}

// TestDrainReportsMakeTheHandoverVisibleWhileItHappens, which is the whole
// point of a host reporting a drain the orchestrator is driving anyway.
func TestDrainReportsMakeTheHandoverVisibleWhileItHappens(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	started := orch.DrainReport{Host: "host-0", VM: "vm-a", Phase: orch.DrainStarted}
	if err := d.orchestrator.Drained(t.Context(), started); err != nil {
		t.Fatal(err)
	}
	row, _, err := d.orchestrator.table.VM(t.Context(), "vm-a")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != stateMigrating || row.From != "host-0" {
		t.Fatalf("row %+v, want vm-a migrating off host-0", row)
	}
	failed := orch.DrainReport{Host: "host-0", VM: "vm-a", Phase: orch.DrainFinished,
		Error: "no host is available"}
	if err := d.orchestrator.Drained(t.Context(), failed); err != nil {
		t.Fatal(err)
	}
	row, _, err = d.orchestrator.table.VM(t.Context(), "vm-a")
	if err != nil {
		t.Fatal(err)
	}
	// A VM the drain could not hand over is still running where it was: it kept
	// its guest and its checkpoint loop, and a row saying stopped would offer a
	// recovery that fences the host still running it.
	if row.State != stateRunning || row.Host != "host-0" {
		t.Fatalf("row %+v, want vm-a still running on host-0", row)
	}
}

// TestADrainReportDoesNotResurrectAVMWhoseReceiveFailed: a drain's handover is
// the orchestrator's own migration, and when the destination could not take
// the VM in, the source has already stopped the guest and given its volumes up.
// The orchestrator recorded that as it happened. The host's report that the
// drain could not hand the VM over comes after, and a report that overwrote the
// row with "running" would describe a guest that is stopped and offer nothing
// that recovers it.
func TestADrainReportDoesNotResurrectAVMWhoseReceiveFailed(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	d.hosts["host-1"].refusesEveryReceive = true
	started := orch.DrainReport{Host: "host-0", VM: "vm-a", Phase: orch.DrainStarted}
	if err := d.orchestrator.Drained(t.Context(), started); err != nil {
		t.Fatal(err)
	}
	if _, err := d.orchestrator.Migrate(t.Context(), "vm-a", "host-1"); err == nil {
		t.Fatal("the migration to a destination that refuses every receive succeeded")
	}
	row, _, err := d.orchestrator.table.VM(t.Context(), "vm-a")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != stateStopped {
		t.Fatalf("row %+v after the receive failed, want vm-a stopped", row)
	}
	failed := orch.DrainReport{Host: "host-0", VM: "vm-a", Phase: orch.DrainFinished,
		Error: "receiving vm-a on host-1: the destination could not start it"}
	if err := d.orchestrator.Drained(t.Context(), failed); err != nil {
		t.Fatal(err)
	}
	row, _, err = d.orchestrator.table.VM(t.Context(), "vm-a")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != stateStopped {
		t.Fatalf("row %+v after the drain reported its failure, want vm-a still stopped", row)
	}
}

// TestDrainReportRefusesAPhaseItDoesNotKnow.
func TestDrainReportRefusesAPhaseItDoesNotKnow(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}})
	err := d.orchestrator.Drained(t.Context(), orch.DrainReport{Host: "host-0", VM: "vm-a", Phase: "halfway"})
	if !errors.Is(err, errRequest) {
		t.Fatalf("error %v, want an invalid request", err)
	}
	err = d.orchestrator.Drained(t.Context(), orch.DrainReport{Phase: orch.DrainStarted})
	if !errors.Is(err, errRequest) {
		t.Fatalf("error %v, want an invalid request", err)
	}
}

// TestListingReportsTheStateTheTableHolds, which is how a VM in flight is
// visible at all: no host reports it while it is moving.
func TestListingReportsTheStateTheTableHolds(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	if err := d.orchestrator.table.Record(t.Context(), vmRecord{ID: "vm-b", Host: "host-0",
		State: stateMigrating, From: "host-0", To: "host-1"}); err != nil {
		t.Fatal(err)
	}
	d.records.ids = append(d.records.ids, "vm-b")
	vms, err := d.orchestrator.VMs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]orch.VM{}
	for _, vm := range vms {
		byID[vm.ID] = vm
	}
	if byID["vm-a"].State != stateRunning || byID["vm-a"].Host != "host-0" {
		t.Fatalf("vm-a reads %+v, want it running on host-0", byID["vm-a"])
	}
	moving := byID["vm-b"]
	if moving.State != stateMigrating || moving.From != "host-0" || moving.To != "host-1" {
		t.Fatalf("vm-b reads %+v, want a migration in flight", moving)
	}
}

// TestReconcileDropsAVMThatWasDeletedElsewhere: with nothing in the bucket and
// no host reporting it, a row is a VM somebody else deleted, not a stopped one.
// It is the reconciler that finds this and not a listing: reading every control
// record in the bucket and rewriting the whole table is not what a GET costs.
func TestReconcileDropsAVMThatWasDeletedElsewhere(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}})
	if err := d.orchestrator.table.Record(t.Context(),
		vmRecord{ID: "vm-old", Host: "host-0", State: stateRunning}); err != nil {
		t.Fatal(err)
	}
	if err := d.orchestrator.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, found, err := d.orchestrator.table.VM(t.Context(), "vm-old"); err != nil || found {
		t.Fatalf("vm-old is still in the table: found %t, %v", found, err)
	}
}

// A host goes on serving the pages of every VM it handed over until the
// orchestrator tells it the destination has them. That release is the only one
// there is, so an orchestrator that restarted, or whose call failed, leaves the
// source serving for good — and a fork's parent sealed with it. Every survey
// reconciles Serving against the table instead: a VM with nothing in flight is
// released.
func TestSurveyReleasesServingWithNothingInFlight(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {"vm-b"}})
	// host-0 handed vm-b over and forked fork-c onto host-1; both arrived, and
	// neither release was ever made.
	d.hosts["host-0"].serving = []string{"vm-b", "fork-c"}
	d.hosts["host-1"].running = append(d.hosts["host-1"].running, "fork-c")
	if _, err := d.orchestrator.survey(t.Context()); err != nil {
		t.Fatal(err)
	}
	if serving := d.hosts["host-0"].serving; len(serving) != 0 {
		t.Fatalf("host-0 still serves %v after a survey found nothing in flight", serving)
	}
	if !slices.Contains(d.log, "host-0 released vm-b") || !slices.Contains(d.log, "host-0 released fork-c") {
		t.Fatalf("the survey released %v", d.log)
	}
}

// A survey releases nothing a host serves for an operation still in flight: the
// destination does not have those pages yet, and releasing them would lose
// every write since the source's last checkpoint.
func TestSurveyLeavesServingForAnOperationInFlight(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	d.hosts["host-0"].serving = []string{"vm-b"}
	if err := d.orchestrator.table.Record(t.Context(), vmRecord{ID: "vm-b", Host: "host-0",
		State: stateMigrating, From: "host-0", To: "host-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.orchestrator.survey(t.Context()); err != nil {
		t.Fatal(err)
	}
	if serving := d.hosts["host-0"].serving; !slices.Equal(serving, []string{"vm-b"}) {
		t.Fatalf("host-0 serves %v, want the migration in flight left alone", serving)
	}
}

// TestPlacementFollowsFreeMemoryAndRefusesWhatDoesNotFit: what a VM costs a
// host is its memory, and the arena is the whole of the memory a guest is
// resident in. Counting VMs instead puts the next guest on the host with the
// fewest of them however full its arena is — a host running one 2 GiB workload
// guest looks emptier than one running three 512 MiB guests — and admits a
// create that no host can hold at all, which is discovered as a VM that will
// not start.
func TestPlacementFollowsFreeMemoryAndRefusesWhatDoesNotFit(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {"vm-a", "vm-b"}})
	// host-0 runs nothing and has almost nothing free; host-1 runs two small
	// guests and has room for another.
	d.hosts["host-0"].arena(1024, 1020)
	d.hosts["host-1"].arena(1024, 200)
	for _, h := range d.hosts {
		h.templates = []host.Template{{Name: "workload", MemoryBytes: 512 << 20, Imported: true}}
	}
	created, err := d.orchestrator.Create(t.Context(), orch.CreateRequest{Template: "workload"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Host != "host-1" {
		t.Fatalf("the VM was placed on %s, which has no room for it", created.Host)
	}

	// And a template no host can hold is refused, rather than started on
	// whichever host is running the fewest VMs and lost when it will not start.
	d.hosts["host-1"].arena(1024, 1020)
	if _, err := d.orchestrator.Create(t.Context(), orch.CreateRequest{Template: "workload"}); !errors.Is(err, errNoHost) {
		t.Fatalf("a create that fits nowhere = %v, want errNoHost", err)
	}
	for _, line := range d.log {
		if strings.HasPrefix(line, "host-0 create") {
			t.Fatalf("the refused create reached a host anyway: %v", d.log)
		}
	}
}

// TestForkOntoAHostWithNoRoomIsRefused: a fork onto another host is a child
// with its own resident memory, so it is admitted against that host's arena
// exactly as a create is.
func TestForkOntoAHostWithNoRoomIsRefused(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	d.hosts["host-0"].arena(1024, 200)
	d.hosts["host-1"].arena(1024, 1020)
	for _, h := range d.hosts {
		h.templates = []host.Template{{Name: "workload", MemoryBytes: 512 << 20, Imported: true}}
	}
	d.orchestrator.note(t.Context(), vmRecord{ID: "vm-a", Host: "host-0", State: stateRunning,
		Template: "workload"})
	if _, err := d.orchestrator.Fork(t.Context(), "vm-a", orch.ForkRequest{Count: 1, To: "host-1"}); !errors.Is(err, errNoHost) {
		t.Fatalf("a fork onto a host with no room = %v, want errNoHost", err)
	}
	for _, line := range d.log {
		if strings.HasPrefix(line, "host-0 fork") {
			t.Fatalf("the refused fork paused the parent anyway: %v", d.log)
		}
	}
}

// TestAPartialCrossHostForkTakesBackTheChildrenItStarted: one request forks one
// parent into a set of children, and a set that did not happen leaves nothing
// behind. The children that did start are guests nobody asked for, holding a
// host's memory under identities the caller was never told, and only this
// request ever knew their names.
func TestAPartialCrossHostForkTakesBackTheChildrenItStarted(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	// The destination takes the first child and refuses the second.
	d.hosts["host-1"].receives = 1
	if _, err := d.orchestrator.Fork(t.Context(), "vm-a", orch.ForkRequest{Count: 2, To: "host-1"}); err == nil {
		t.Fatal("a fork whose destination refused a child reported success")
	}
	started, deleted := "", ""
	for _, line := range d.log {
		if after, found := strings.CutPrefix(line, "host-1 receive "); found && started == "" {
			started = strings.Fields(after)[0]
		}
		if after, found := strings.CutPrefix(line, "host-1 delete "); found {
			deleted = strings.Fields(after)[0]
		}
	}
	if started == "" {
		t.Fatalf("no child was started at all: %v", d.log)
	}
	if deleted != started {
		t.Fatalf("the fork started %s and deleted %q: %v", started, deleted, d.log)
	}
	if running := d.hosts["host-1"].running; len(running) != 0 {
		t.Fatalf("the destination still runs %v", running)
	}
}

// TestTemplatesAreNotVMsOfTheDeployment: a template is an ordinary VM holding
// an imported guest image, which is what lets a fork of it inherit a published
// checkpoint. It is the hosts' own bookkeeping — nothing asked for it, nothing
// runs it, and there is one per guest image the deployment has ever been
// configured with — so listing it offered the operator VMs to delete, migrate
// and recover that the deployment never made.
func TestTemplatesAreNotVMsOfTheDeployment(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}})
	d.records.ids = append(d.records.ids,
		host.TemplateID(sha256.Sum256([]byte("alpine")), []uint64{2 << 20, 2 << 20}),
		host.TemplateID(sha256.Sum256([]byte("workload")), []uint64{2 << 20, 4 << 10}))
	vms, err := d.orchestrator.VMs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 1 || vms[0].ID != "vm-a" {
		t.Fatalf("the listing reports %+v, want only vm-a", vms)
	}
	// And the table never learns about them either: a template with a row would
	// be reconciled, released and recovered like any other VM.
	if err := d.orchestrator.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err := d.orchestrator.table.VMs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "vm-a" {
		t.Fatalf("the table holds %+v, want only vm-a", rows)
	}
}
