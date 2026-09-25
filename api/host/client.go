package host

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/semistrict/sproutfs/internal/jsonhttp"
)

// Client reaches one host's API. It is plain HTTP on the pod network: the
// deployment's control plane, which carries no VM data and no authority.
type Client struct {
	base string
	http *http.Client
}

// NewClient addresses the host serving at base, an origin such as
// http://10.0.0.7:8080. token is the deployment's shared bearer token, which
// every request but the probes carries; an empty one addresses a host that
// wants none.
func NewClient(base string, client *http.Client, token string) *Client {
	client = jsonhttp.Authenticated(client, token)
	if client == nil {
		client = http.DefaultClient
	}
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	return &Client{base: base, http: client}
}

// Base is the origin this client addresses.
func (c *Client) Base() string { return c.base }

func (c *Client) path(format string, args ...any) string {
	return c.base + fmt.Sprintf(format, args...)
}

func (c *Client) Healthz(ctx context.Context) error {
	_, err := jsonhttp.Call[struct{}](ctx, c.http, http.MethodGet, c.path("/healthz"), nil)
	return err
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	return jsonhttp.Call[Status](ctx, c.http, http.MethodGet, c.path("/status"), nil)
}

func (c *Client) Create(ctx context.Context, request CreateRequest) (CreateResult, error) {
	return jsonhttp.Call[CreateResult](ctx, c.http, http.MethodPost, c.path("/vms"), request)
}

// ImportTemplate sends a guest image to be imported into the template its bytes
// name. The image is streamed as the request's body.
func (c *Client) ImportTemplate(ctx context.Context, image io.Reader, request ImportTemplateRequest) (ImportTemplateResult, error) {
	target := c.path("/templates")
	if request.Memory != 0 {
		target += "?memory=" + strconv.FormatUint(request.Memory, 10)
	}
	return jsonhttp.Upload[ImportTemplateResult](ctx, c.http, http.MethodPost, target, image)
}

func (c *Client) Open(ctx context.Context, id string, request OpenRequest) (OpenResult, error) {
	return jsonhttp.Call[OpenResult](ctx, c.http, http.MethodPost,
		c.path("/vms/%s/open", url.PathEscape(id)), request)
}

func (c *Client) Fork(ctx context.Context, parent string, request ForkRequest) (ForkResult, error) {
	return jsonhttp.Call[ForkResult](ctx, c.http, http.MethodPost, c.path("/vms/%s/fork", url.PathEscape(parent)), request)
}

func (c *Client) Capture(ctx context.Context, id string) (CaptureResult, error) {
	return jsonhttp.Call[CaptureResult](ctx, c.http, http.MethodPost, c.path("/vms/%s/capture", url.PathEscape(id)), nil)
}

func (c *Client) Console(ctx context.Context, id string, since int64) (Console, error) {
	return jsonhttp.Call[Console](ctx, c.http, http.MethodGet,
		c.path("/vms/%s/console?since=%s", url.PathEscape(id), strconv.FormatInt(since, 10)), nil)
}

func (c *Client) WriteConsole(ctx context.Context, id string, data string) error {
	_, err := jsonhttp.Call[struct{}](ctx, c.http, http.MethodPost,
		c.path("/vms/%s/console", url.PathEscape(id)), ConsoleWrite{Data: data})
	return err
}

// Exec runs one command in a VM's guest, through the host running it.
func (c *Client) Exec(ctx context.Context, id string, request ExecRequest) (ExecResult, error) {
	return jsonhttp.Call[ExecResult](ctx, c.http, http.MethodPost,
		c.path("/vms/%s/exec", url.PathEscape(id)), request)
}

func (c *Client) Migrate(ctx context.Context, id string, request MigrateRequest) (MigrateResult, error) {
	return jsonhttp.Call[MigrateResult](ctx, c.http, http.MethodPost, c.path("/vms/%s/migrate", url.PathEscape(id)), request)
}

func (c *Client) Receive(ctx context.Context, handoff Handoff) (ReceiveResult, error) {
	return jsonhttp.Call[ReceiveResult](ctx, c.http, http.MethodPost, c.path("/vms/receive"), handoff)
}

func (c *Client) Released(ctx context.Context, id string) error {
	_, err := jsonhttp.Call[struct{}](ctx, c.http, http.MethodPost, c.path("/vms/%s/released", url.PathEscape(id)), nil)
	return err
}

// Abandoned gives one handover up rather than handing it over: a fork's child
// that will never be received, or one whose destination refused it. The host
// stops serving whatever it still holds for that VM and takes its pages back,
// which for a fork's parent is the end of its seal.
//
// It is refused by nothing, which is the whole difference from Released: those
// pages are going either way, and refusing would only leave a parent sealed for
// good. Nothing else may ever ask for that VM, so this is only ever said about
// one the deployment has given up on.
func (c *Client) Abandoned(ctx context.Context, id string) error {
	_, err := jsonhttp.Call[struct{}](ctx, c.http, http.MethodPost, c.path("/vms/%s/abandoned", url.PathEscape(id)), nil)
	return err
}

func (c *Client) Drain(ctx context.Context) (DrainResult, error) {
	return jsonhttp.Call[DrainResult](ctx, c.http, http.MethodPost, c.path("/drain"), nil)
}

// Stop ends a VM this host runs and leaves it behind: its disks are published,
// with its memory and its VMM state when the request suspends it, and its
// guest, pages and handle go, so any host can open it again.
func (c *Client) Stop(ctx context.Context, id string, request StopRequest) (StopResult, error) {
	return jsonhttp.Call[StopResult](ctx, c.http, http.MethodPost,
		c.path("/vms/%s/stop", url.PathEscape(id)), request)
}

func (c *Client) Delete(ctx context.Context, id string) error {
	_, err := jsonhttp.Call[struct{}](ctx, c.http, http.MethodDelete, c.path("/vms/%s", url.PathEscape(id)), nil)
	return err
}
