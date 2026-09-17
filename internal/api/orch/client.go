package orch

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/semistrict/sproutfs/internal/jsonhttp"
)

// Client reaches the orchestrator: the CLI's whole interface to the deployment,
// and how a draining host asks for somewhere to put its VMs.
type Client struct {
	base string
	http *http.Client
}

// NewClient addresses the orchestrator serving at base. token is the
// deployment's shared bearer token, which every request but the probes
// carries; an empty one addresses an orchestrator that wants none.
func NewClient(base string, client *http.Client, token string) *Client {
	client = jsonhttp.Authenticated(client, token)
	if client == nil {
		client = http.DefaultClient
	}
	return &Client{base: strings.TrimRight(base, "/"), http: client}
}

func (c *Client) path(format string, args ...any) string {
	return c.base + fmt.Sprintf(format, args...)
}

func (c *Client) Healthz(ctx context.Context) error {
	_, err := jsonhttp.Call[struct{}](ctx, c.http, http.MethodGet, c.path("/healthz"), nil)
	return err
}

func (c *Client) Hosts(ctx context.Context) ([]Host, error) {
	return jsonhttp.Call[[]Host](ctx, c.http, http.MethodGet, c.path("/hosts"), nil)
}

// VMs reports the deployment's VMs, which are the control records in the
// deployment's bucket: a record exists exactly while its VM does.
func (c *Client) VMs(ctx context.Context) ([]VM, error) {
	return jsonhttp.Call[[]VM](ctx, c.http, http.MethodGet, c.path("/vms"), nil)
}

func (c *Client) Create(ctx context.Context, template string) (CreateResult, error) {
	return jsonhttp.Call[CreateResult](ctx, c.http, http.MethodPost, c.path("/vms"),
		CreateRequest{Template: template})
}

func (c *Client) Fork(ctx context.Context, id string, count int, to string) (ForkResult, error) {
	return jsonhttp.Call[ForkResult](ctx, c.http, http.MethodPost,
		c.path("/vms/%s/fork", url.PathEscape(id)), ForkRequest{Count: count, To: to})
}

// Migrate moves one VM. An empty destination lets the orchestrator pick a host
// other than the one running it, which is what a drain asks for.
func (c *Client) Migrate(ctx context.Context, id, to string) (MigrateResult, error) {
	return jsonhttp.Call[MigrateResult](ctx, c.http, http.MethodPost,
		c.path("/vms/%s/migrate", url.PathEscape(id)), MigrateRequest{To: to})
}

func (c *Client) Capture(ctx context.Context, id string) (CaptureResult, error) {
	return jsonhttp.Call[CaptureResult](ctx, c.http, http.MethodPost,
		c.path("/vms/%s/capture", url.PathEscape(id)), nil)
}

// Recover reopens a VM whose host is gone. force carries the operator's own
// evidence of that loss, for a pod the Kubernetes API still lists whose host
// does not answer.
func (c *Client) Recover(ctx context.Context, id string, force bool) (RecoverResult, error) {
	return jsonhttp.Call[RecoverResult](ctx, c.http, http.MethodPost,
		c.path("/vms/%s/recover", url.PathEscape(id)), RecoverRequest{Force: force})
}

// Stop ends a running VM and leaves it behind: the host running it publishes
// what its guest holds and closes it, and the VM is then only its control
// record and its objects until something starts it again.
func (c *Client) Stop(ctx context.Context, id string) (StopResult, error) {
	return jsonhttp.Call[StopResult](ctx, c.http, http.MethodPost,
		c.path("/vms/%s/stop", url.PathEscape(id)), nil)
}

// Start opens a stopped VM on a host again. An empty destination places it on
// the ready host whose guests have promised the least of its arena; a cold
// request brings the VM back without its memory, at whatever shape it asks for.
func (c *Client) Start(ctx context.Context, id string, request StartRequest) (StartResult, error) {
	return jsonhttp.Call[StartResult](ctx, c.http, http.MethodPost,
		c.path("/vms/%s/start", url.PathEscape(id)), request)
}

// Check runs the deployment check over the bucket and reports every violation
// it found, which is what a run asks for once it has deleted every VM.
func (c *Client) Check(ctx context.Context) (CheckResult, error) {
	return jsonhttp.Call[CheckResult](ctx, c.http, http.MethodGet, c.path("/check"), nil)
}

func (c *Client) Kill(ctx context.Context, name string) (KillResult, error) {
	return jsonhttp.Call[KillResult](ctx, c.http, http.MethodPost,
		c.path("/hosts/%s/kill", url.PathEscape(name)), nil)
}

func (c *Client) Delete(ctx context.Context, id string) error {
	_, err := jsonhttp.Call[struct{}](ctx, c.http, http.MethodDelete,
		c.path("/vms/%s", url.PathEscape(id)), nil)
	return err
}

// Exec runs one command in a VM's guest, through whichever host runs it.
func (c *Client) Exec(ctx context.Context, id string, request ExecRequest) (ExecOutcome, error) {
	return jsonhttp.Call[ExecOutcome](ctx, c.http, http.MethodPost,
		c.path("/vms/%s/exec", url.PathEscape(id)), request)
}

// ReportDrain tells the orchestrator what a draining host is doing with one of
// its VMs. It is how the table knows a drain is under way rather than only that
// a migration happened.
func (c *Client) ReportDrain(ctx context.Context, report DrainReport) error {
	_, err := jsonhttp.Call[struct{}](ctx, c.http, http.MethodPost, c.path("/drains"), report)
	return err
}

func (c *Client) Console(ctx context.Context, id string, since int64) (Console, error) {
	return jsonhttp.Call[Console](ctx, c.http, http.MethodGet,
		c.path("/vms/%s/console?since=%s", url.PathEscape(id), strconv.FormatInt(since, 10)), nil)
}

func (c *Client) WriteConsole(ctx context.Context, id, data string) error {
	_, err := jsonhttp.Call[struct{}](ctx, c.http, http.MethodPost,
		c.path("/vms/%s/console", url.PathEscape(id)), ConsoleWrite{Data: data})
	return err
}
