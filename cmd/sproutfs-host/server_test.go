package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/semistrict/sproutfs/internal/api/guest"
	hostapi "github.com/semistrict/sproutfs/internal/api/host"
	"github.com/semistrict/sproutfs/internal/host"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/volume"
)

// fakeHost is a host that runs no VM: it records what the HTTP layer asked it
// for and answers with whatever the test put in it.
type fakeHost struct {
	calls []string
	err   error
	// notReady is why this host cannot take work, which is what a host still
	// importing its guest images reports, and notLive why the process itself can
	// no longer serve at all.
	notReady error
	notLive  error

	status   hostapi.Status
	created  hostapi.CreateResult
	opened   hostapi.OpenResult
	forked   hostapi.ForkResult
	captured hostapi.CaptureResult
	console  hostapi.Console
	migrated hostapi.MigrateResult
	received hostapi.ReceiveResult
	drained  hostapi.DrainResult
	stopped  hostapi.StopResult
	executed hostapi.ExecResult
}

func (f *fakeHost) record(format string, args ...any) error {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
	return f.err
}

// Ready is what the readiness probe asks: this fake is a host whose images are
// imported unless a test says otherwise.
func (f *fakeHost) Ready(context.Context) error { return f.notReady }

// Live is what the liveness probe asks: this fake is a process that can still
// serve unless a test says otherwise.
func (f *fakeHost) Live(context.Context) error { return f.notLive }

func (f *fakeHost) Status(context.Context) (hostapi.Status, error) {
	return f.status, f.record("status")
}

func (f *fakeHost) Create(_ context.Context, id, template string) (hostapi.CreateResult, error) {
	return f.created, f.record("create %s %s", id, template)
}

func (f *fakeHost) Open(_ context.Context, id string, request hostapi.OpenRequest) (hostapi.OpenResult, error) {
	if request.Cold {
		return f.opened, f.record("open %s cold memory=%d disk=%d", id, request.Memory, request.Disk)
	}
	return f.opened, f.record("open %s", id)
}

func (f *fakeHost) Fork(_ context.Context, parent string, request hostapi.ForkRequest) (hostapi.ForkResult, error) {
	ids := strings.Join(request.IDs, ",")
	if request.Destination != "" {
		return f.forked, f.record("fork %s %s to %s", parent, ids, request.Destination)
	}
	return f.forked, f.record("fork %s %s", parent, ids)
}

func (f *fakeHost) Capture(_ context.Context, id string) (hostapi.CaptureResult, error) {
	return f.captured, f.record("capture %s", id)
}

func (f *fakeHost) Console(_ context.Context, id string, since int64) (hostapi.Console, error) {
	return f.console, f.record("console %s %d", id, since)
}

func (f *fakeHost) WriteConsole(_ context.Context, id string, data string) error {
	return f.record("write %s %q", id, data)
}

// Exec is the guest channel: the fake answers with whatever the test put in it.
func (f *fakeHost) Exec(_ context.Context, id string, request hostapi.ExecRequest) (hostapi.ExecResult, error) {
	return f.executed, f.record("exec %s %q", id, request.Cmd)
}

func (f *fakeHost) Migrate(_ context.Context, id string, destination platform.Address) (hostapi.MigrateResult, error) {
	return f.migrated, f.record("migrate %s %s", id, destination)
}

func (f *fakeHost) Receive(_ context.Context, handoff hostapi.Handoff) (hostapi.ReceiveResult, error) {
	return f.received, f.record("receive %s %s", handoff.VMID, handoff.Source)
}

func (f *fakeHost) Released(_ context.Context, id string) error { return f.record("released %s", id) }

func (f *fakeHost) Abandoned(_ context.Context, id string) error {
	return f.record("abandoned %s", id)
}

func (f *fakeHost) Drain(context.Context) (hostapi.DrainResult, error) {
	return f.drained, f.record("drain")
}

func (f *fakeHost) Stop(_ context.Context, id string) (hostapi.StopResult, error) {
	return f.stopped, f.record("stop %s", id)
}

func (f *fakeHost) Delete(_ context.Context, id string) error { return f.record("delete %s", id) }

func (f *fakeHost) Close(context.Context) error { return f.record("close") }

// call runs one request against the host API and returns the status and body.
func call(t *testing.T, fake host.VMs, method, target, body string) (int, string) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	recorder := httptest.NewRecorder()
	newServer(fake, "").ServeHTTP(recorder, request)
	return recorder.Code, strings.TrimSpace(recorder.Body.String())
}

// TestTheHostAPIRequiresTheDeploymentToken: this API creates, deletes, drains
// and migrates every VM on the host, on a pod network anything in the cluster
// can reach. Nothing admitted the caller: whoever reached the port was the
// control plane. The kubelet's probe is the exception, because it reaches a pod
// before anything has given it a token and says nothing about the VMs.
func TestTheHostAPIRequiresTheDeploymentToken(t *testing.T) {
	const token = "the-deployment-token"
	fake := &fakeHost{}
	server := newServer(fake, token)
	send := func(method, target, bearer string) int {
		request := httptest.NewRequest(method, target, strings.NewReader(""))
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		return recorder.Code
	}
	if status := send(http.MethodDelete, "/vms/vm-1", ""); status != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated delete answered %d, want 401", status)
	}
	if status := send(http.MethodDelete, "/vms/vm-1", "guessed"); status != http.StatusUnauthorized {
		t.Fatalf("a delete with the wrong token answered %d, want 401", status)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("an unauthenticated request reached the host: %v", fake.calls)
	}
	if status := send(http.MethodGet, "/healthz", ""); status != http.StatusOK {
		t.Fatalf("the kubelet's probe answered %d, want 200", status)
	}
	if status := send(http.MethodDelete, "/vms/vm-1", token); status != http.StatusOK {
		t.Fatalf("an authenticated delete answered %d, want 200", status)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "delete vm-1" {
		t.Fatalf("the host was asked for %v", fake.calls)
	}
}

// TestDrainingIsNotAGet: a GET is what a caching proxy, a link checker or a
// curious operator does to every URL it is given, and this one migrates every
// VM off the host. It is a POST, and the preStop hook runs a command rather
// than fetching a URL.
func TestDrainingIsNotAGet(t *testing.T) {
	fake := &fakeHost{}
	if status, body := call(t, fake, http.MethodGet, "/drain", ""); status != http.StatusMethodNotAllowed {
		t.Fatalf("GET /drain answered %d: %s", status, body)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("a GET drained the host: %v", fake.calls)
	}
	if status, body := call(t, fake, http.MethodPost, "/drain", ""); status != http.StatusOK {
		t.Fatalf("POST /drain answered %d: %s", status, body)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "drain" {
		t.Fatalf("the host was asked for %v", fake.calls)
	}
}

func TestHealthzReportsReady(t *testing.T) {
	status, body := call(t, &fakeHost{}, http.MethodGet, "/healthz", "")
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200", status)
	}
	if body != `{"status":"ok"}` {
		t.Fatalf("body %s", body)
	}
}

func TestCreateCarriesIdentityAndTemplate(t *testing.T) {
	fake := &fakeHost{created: hostapi.CreateResult{VM: hostapi.VM{ID: "vm-1", Template: "alpine"}, Total: 1.5}}
	status, body := call(t, fake, http.MethodPost, "/vms", `{"id":"vm-1","template":"alpine"}`)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "create vm-1 alpine" {
		t.Fatalf("the host was asked for %v", fake.calls)
	}
	var result hostapi.CreateResult
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	if result.VM.ID != "vm-1" || result.Total != 1.5 {
		t.Fatalf("result %+v", result)
	}
}

func TestCreateWithoutIdentityIsRefused(t *testing.T) {
	fake := &fakeHost{}
	status, body := call(t, fake, http.MethodPost, "/vms", `{"template":"alpine"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status %d: %s", status, body)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("a refused request reached the host: %v", fake.calls)
	}
	var failure hostapi.Error
	if err := json.Unmarshal([]byte(body), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Op != "create" || failure.Message != "invalid request: a created VM needs an identity" {
		t.Fatalf("failure %+v", failure)
	}
}

func TestForkNeedsTheChildIdentity(t *testing.T) {
	fake := &fakeHost{}
	status, _ := call(t, fake, http.MethodPost, "/vms/vm-1/fork", `{}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status %d", status)
	}
	fake.forked = hostapi.ForkResult{Handoffs: []hostapi.Handoff{{VMID: "vm-2", Parent: "vm-1"}}}
	status, body := call(t, fake, http.MethodPost, "/vms/vm-1/fork", `{"ids":["vm-2"]}`)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "fork vm-1 vm-2" {
		t.Fatalf("the host was asked for %v", fake.calls)
	}
}

func TestConsoleReadsFromAnOffsetAndRefusesAnythingElse(t *testing.T) {
	fake := &fakeHost{console: hostapi.Console{VM: "vm-1", Offset: 64, Next: 70, Data: "hello\n"}}
	status, body := call(t, fake, http.MethodGet, "/vms/vm-1/console?since=64", "")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "console vm-1 64" {
		t.Fatalf("the host was asked for %v", fake.calls)
	}
	var window hostapi.Console
	if err := json.Unmarshal([]byte(body), &window); err != nil {
		t.Fatal(err)
	}
	if window.Data != "hello\n" || window.Next != 70 {
		t.Fatalf("window %+v", window)
	}
	status, body = call(t, fake, http.MethodGet, "/vms/vm-1/console?since=later", "")
	if status != http.StatusBadRequest {
		t.Fatalf("status %d: %s", status, body)
	}
}

func TestConsoleWriteCarriesTheBytes(t *testing.T) {
	fake := &fakeHost{}
	status, body := call(t, fake, http.MethodPost, "/vms/vm-1/console", `{"data":"uname -a\n"}`)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	if body != `{"status":"ok"}` {
		t.Fatalf("body %s", body)
	}
	if len(fake.calls) != 1 || fake.calls[0] != `write vm-1 "uname -a\n"` {
		t.Fatalf("the host was asked for %v", fake.calls)
	}
}

func TestMigrateNeedsADestinationAndReturnsTheHandoff(t *testing.T) {
	fake := &fakeHost{}
	status, _ := call(t, fake, http.MethodPost, "/vms/vm-1/migrate", `{}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status %d", status)
	}
	fake.migrated = hostapi.MigrateResult{Handoff: hostapi.Handoff{VMID: "vm-1",
		Source: "10.0.0.1:8081", PageSize: 2 << 20}, Stop: 0.25}
	status, body := call(t, fake, http.MethodPost, "/vms/vm-1/migrate", `{"destination":"10.0.0.2:8081"}`)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "migrate vm-1 10.0.0.2:8081" {
		t.Fatalf("the host was asked for %v", fake.calls)
	}
	var result hostapi.MigrateResult
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	if result.Handoff.VMID != "vm-1" || result.Handoff.Source != "10.0.0.1:8081" || result.Stop != 0.25 {
		t.Fatalf("result %+v", result)
	}
}

func TestReceiveRefusesAHandoffThatNamesNoSource(t *testing.T) {
	fake := &fakeHost{}
	status, _ := call(t, fake, http.MethodPost, "/vms/receive", `{"VMID":"vm-1"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status %d", status)
	}
	fake.received = hostapi.ReceiveResult{VM: hostapi.VM{ID: "vm-1"}, Pause: 0.08, PeerPages: 12}
	status, body := call(t, fake, http.MethodPost, "/vms/receive",
		`{"VMID":"vm-1","Source":"10.0.0.1:8081","PageSize":2097152}`)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "receive vm-1 10.0.0.1:8081" {
		t.Fatalf("the host was asked for %v", fake.calls)
	}
}

// TestReceiveTakesAForkOfThisHostsOwnInstant: a fork is a handoff wherever the
// child lands, and a child taken in on its parent's own host is served nothing
// — it maps the frames the seal froze, so no page of it ever reaches the wire
// and the handoff names no address to fetch from. Refusing that handoff for
// having no source is refusing every same-host fork: the one path the cluster
// takes by default never worked through this API, though Host.Receive has
// always taken it.
func TestReceiveTakesAForkOfThisHostsOwnInstant(t *testing.T) {
	fake := &fakeHost{received: hostapi.ReceiveResult{VM: hostapi.VM{ID: "child"}}}
	status, body := call(t, fake, http.MethodPost, "/vms/receive",
		`{"VMID":"child","Parent":"vm-1","ParentCheckpoint":7}`)
	if status != http.StatusOK {
		t.Fatalf("a fork of this host's own instant answered %d: %s", status, body)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "receive child " {
		t.Fatalf("the host was asked for %v", fake.calls)
	}
}

func TestDrainAndReleaseAreTheHandoverContract(t *testing.T) {
	fake := &fakeHost{drained: hostapi.DrainResult{Host: "host-0", Moved: []string{"vm-1"},
		Remaining: []string{}, Seconds: 2}}
	status, body := call(t, fake, http.MethodPost, "/drain", "")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	var result hostapi.DrainResult
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	if result.Host != "host-0" || len(result.Moved) != 1 || result.Moved[0] != "vm-1" || len(result.Remaining) != 0 {
		t.Fatalf("result %+v", result)
	}
	status, body = call(t, fake, http.MethodPost, "/vms/vm-1/released", "")
	if status != http.StatusOK || body != `{"status":"ok"}` {
		t.Fatalf("status %d: %s", status, body)
	}
	// And the other end of it: a handover the control plane has given up on,
	// which the host stops holding whatever is outstanding on it.
	status, body = call(t, fake, http.MethodPost, "/vms/vm-2/abandoned", "")
	if status != http.StatusOK || body != `{"status":"ok"}` {
		t.Fatalf("status %d: %s", status, body)
	}
	if want := []string{"drain", "released vm-1", "abandoned vm-2"}; !slices.Equal(fake.calls, want) {
		t.Fatalf("the host was asked for %v, want %v", fake.calls, want)
	}
}

func TestAVMThisHostDoesNotRunIsNotFound(t *testing.T) {
	fake := &fakeHost{err: fmt.Errorf("%w: vm-9", host.ErrNotRunning)}
	status, body := call(t, fake, http.MethodPost, "/vms/vm-9/capture", "")
	if status != http.StatusNotFound {
		t.Fatalf("status %d: %s", status, body)
	}
	var failure hostapi.Error
	if err := json.Unmarshal([]byte(body), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Op != "capture" || failure.Message != "this host does not run that VM: vm-9" {
		t.Fatalf("failure %+v", failure)
	}
}

func TestAVMThisHostAlreadyRunsIsAConflict(t *testing.T) {
	fake := &fakeHost{err: fmt.Errorf("%w: vm-1", host.ErrRunning)}
	status, body := call(t, fake, http.MethodPost, "/vms/vm-1/open", "")
	if status != http.StatusConflict {
		t.Fatalf("status %d: %s", status, body)
	}
}

func TestDeleteRemovesTheVM(t *testing.T) {
	fake := &fakeHost{}
	status, body := call(t, fake, http.MethodDelete, "/vms/vm-1", "")
	if status != http.StatusOK || body != `{"status":"ok"}` {
		t.Fatalf("status %d: %s", status, body)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "delete vm-1" {
		t.Fatalf("the host was asked for %v", fake.calls)
	}
}

// TestStopIsAPostAndNamesTheVM: a stop closes a running guest after publishing
// what it holds, so it is a POST on the VM like a capture or a migration, not a
// GET anything might make.
func TestStopClosesTheVMOnItsHost(t *testing.T) {
	fake := &fakeHost{}
	if status, body := call(t, fake, http.MethodGet, "/vms/vm-1/stop", ""); status != http.StatusMethodNotAllowed {
		t.Fatalf("GET /vms/vm-1/stop answered %d: %s", status, body)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("a GET stopped a VM: %v", fake.calls)
	}
	fake.stopped = hostapi.StopResult{VM: "vm-1", Checkpoint: 18, Seconds: 0.5}
	status, body := call(t, fake, http.MethodPost, "/vms/vm-1/stop", "")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	var result hostapi.StopResult
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	if result.VM != "vm-1" || result.Checkpoint != 18 {
		t.Fatalf("result %+v, want the checkpoint the stop published", result)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "stop vm-1" {
		t.Fatalf("the host was asked for %v", fake.calls)
	}
}

// TestStoppingAVMAForkInstantHoldsIsAConflict: the frames a stop would release
// are the ones a child is still reading, so the request is refused rather than
// acted on, and the caller is told it may ask again.
func TestStoppingAVMAForkInstantHoldsIsAConflict(t *testing.T) {
	fake := &fakeHost{err: fmt.Errorf("%w: vm-1", volume.ErrSealed)}
	status, body := call(t, fake, http.MethodPost, "/vms/vm-1/stop", "")
	if status != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", status, body)
	}
}

func TestStatusIsTheWholeHostReport(t *testing.T) {
	fake := &fakeHost{status: hostapi.Status{Host: "host-0", PageAddress: "10.0.0.1:8081",
		Running: []string{"vm-1"}, Serving: []string{"vm-2"},
		Pager: hostapi.Pager{PageBytes: 2 << 20, ResidentPages: 8, SharedPages: 5}}}
	status, body := call(t, fake, http.MethodGet, "/status", "")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	var report hostapi.Status
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatal(err)
	}
	if report.Host != "host-0" || report.PageAddress != "10.0.0.1:8081" ||
		len(report.Running) != 1 || report.Running[0] != "vm-1" ||
		len(report.Serving) != 1 || report.Serving[0] != "vm-2" ||
		report.Pager.SharedPages != 5 {
		t.Fatalf("report %+v", report)
	}
}

// TestExecCarriesTheCommandAndTheGuestsAnswer: the host reads nothing in an
// exec but the VM's name, and what comes back is the guest's own report.
func TestExecCarriesTheCommandAndTheGuestsAnswer(t *testing.T) {
	fake := &fakeHost{executed: hostapi.ExecResult{Exit: 3, Stdout: "out\n", Stderr: "err\n", Seconds: 0.25}}
	status, body := call(t, fake, http.MethodPost, "/vms/vm-1/exec", `{"cmd":"echo out","timeout":5}`)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	if len(fake.calls) != 1 || fake.calls[0] != `exec vm-1 "echo out"` {
		t.Fatalf("the host was asked for %v", fake.calls)
	}
	var result hostapi.ExecResult
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	if result.Exit != 3 || result.Stdout != "out\n" || result.Stderr != "err\n" {
		t.Fatalf("result %+v", result)
	}
}

// TestExecNeedsACommand is the one exec this host refuses to carry.
func TestExecNeedsACommand(t *testing.T) {
	fake := &fakeHost{}
	status, body := call(t, fake, http.MethodPost, "/vms/vm-1/exec", `{"cmd":""}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status %d: %s", status, body)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("a refused request reached the host: %v", fake.calls)
	}
	var failure hostapi.Error
	if err := json.Unmarshal([]byte(body), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Op != "exec" || failure.Message != "invalid request: an exec needs a command" {
		t.Fatalf("failure %+v", failure)
	}
}

// TestExecReportsAGuestThatIsNotAnsweringAsUnavailable, which is a VM that is
// still booting rather than a request anybody should change.
func TestExecReportsAGuestThatIsNotAnsweringAsUnavailable(t *testing.T) {
	fake := &fakeHost{err: fmt.Errorf("%w: vm-1 has no agent yet", guest.ErrNoGuest)}
	status, body := call(t, fake, http.MethodPost, "/vms/vm-1/exec", `{"cmd":"true"}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", status, body)
	}
}

// TestReadinessWaitsForTheGuestImages: a host that has not imported its guest
// images cannot create a VM without reading a whole image inside the request,
// so it is not an endpoint of the deployment's Service until it has. Liveness
// is the other question — the process is alive and doing honest work — and
// answers while readiness does not, so nothing restarts a host that is
// importing.
func TestReadinessWaitsForTheGuestImages(t *testing.T) {
	fake := &fakeHost{notReady: errors.New("the guest images have not been imported yet")}
	status, body := call(t, fake, http.MethodGet, "/healthz", "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("an importing host answered readiness %d, want 503: %s", status, body)
	}
	if !strings.Contains(body, "have not been imported") {
		t.Fatalf("the refusal reads %s, want it to say why", body)
	}
	if status, body := call(t, fake, http.MethodGet, "/livez", ""); status != http.StatusOK {
		t.Fatalf("an importing host answered liveness %d, want 200: %s", status, body)
	}
	fake.notReady = nil
	if status, body := call(t, fake, http.MethodGet, "/healthz", ""); status != http.StatusOK {
		t.Fatalf("an imported host answered readiness %d, want 200: %s", status, body)
	}
}

// TestVersionSaysWhichBuildThisIs: a demo cluster is rolled by replacing one
// image, so asking the pod is the only way to tell what it is running.
func TestVersionSaysWhichBuildThisIs(t *testing.T) {
	status, body := call(t, &fakeHost{}, http.MethodGet, "/version", "")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	if body != `{"version":"`+version+`"}` {
		t.Fatalf("body %s, want the linked version %q", body, version)
	}
}

// TestMetricsExposeThePagerAndTheStore in the text format a scraper reads.
func TestMetricsExposeThePagerAndTheStore(t *testing.T) {
	fake := &fakeHost{status: hostapi.Status{
		Running: []string{"vm-1", "vm-2"},
		Pager: hostapi.Pager{PageBytes: 2 << 20, ArenaPages: 1024, ResidentPages: 96,
			Faults: 7, Spills: 3},
		Store: hostapi.Store{Put: hostapi.StoreCount{Calls: 12, Failures: 1, Bytes: 4096}},
	}}
	status, body := call(t, fake, http.MethodGet, "/metrics", "")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	for _, want := range []string{
		"# TYPE sproutfs_pager_resident_pages gauge",
		"sproutfs_pager_resident_pages 96",
		"sproutfs_pager_arena_pages 1024",
		"sproutfs_pager_faults_total 7",
		"sproutfs_vms_running 2",
		`sproutfs_store_calls_total{operation="put"} 12`,
		`sproutfs_store_bytes_total{operation="put"} 4096`,
		`sproutfs_store_failures_total{operation="get"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the exposition has no %q in it:\n%s", want, body)
		}
	}
}
