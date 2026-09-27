package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/semistrict/sproutfs/api/guest"
	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/internal/jsonhttp"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/vmmigrate"
	"github.com/semistrict/sproutfs/volume"
)

// newServer routes the host API. Every handler reports the shared JSON error
// shape, so a client never has to read a status line to learn what failed.
//
// token is the deployment's shared bearer token, which every request but the
// kubelet's probes must carry. An empty one serves the API to whoever reaches
// it, which is what a host started by hand outside a cluster does.
func newServer(h host.VMs, token string) http.Handler {
	mux := http.NewServeMux()
	// Readiness is whether this host can take a VM, which is not the same
	// question as whether the process is alive: a host still importing its
	// guest images answers /livez and not /healthz, so it is restarted by
	// neither the kubelet nor an operator while it does honest work.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := h.Ready(r.Context()); err != nil {
			jsonhttp.Fail(r.Context(), w, http.StatusServiceUnavailable, "healthz", err)
			return
		}
		jsonhttp.Write(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Liveness is whether this process can still do the work it exists for. It
	// has to be able to fail: a supervisor that has closed has released its
	// pager and its VMM processes and can serve nothing, and a pod answering the
	// probe out of the mux is one the kubelet leaves running for ever.
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) {
		if err := h.Live(r.Context()); err != nil {
			jsonhttp.Fail(r.Context(), w, http.StatusServiceUnavailable, "livez", err)
			return
		}
		jsonhttp.Write(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		jsonhttp.Write(w, http.StatusOK, map[string]string{"version": version})
	})
	// The exposition is the status this host already keeps, in the text format
	// a scraper reads. It carries the deployment's token like every other
	// endpoint: what a host holds is what its VMs are doing.
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		status, err := h.Status(r.Context())
		if err != nil {
			jsonhttp.Fail(r.Context(), w, statusOf(err), "metrics", err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if _, err := io.WriteString(w, metrics(status)); err != nil {
			slog.WarnContext(r.Context(), "sproutfs-host: writing the metrics failed", "error", err)
		}
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		status, err := h.Status(r.Context())
		reply(w, r, "status", status, err)
	})
	// What one tenant's VMs hold in the object store, for an embedder's
	// billing. It lists the store rather than reading this host's state, so it
	// answers for VMs no host runs and VMs deleted with their pins standing.
	mux.HandleFunc("GET /stored", func(w http.ResponseWriter, r *http.Request) {
		stored, err := h.Stored(r.Context(), r.URL.Query().Get("tenant"))
		reply(w, r, "stored", stored, err)
	})
	// A VM's kept checkpoints are its control record's, so any host answers for
	// any VM, and releases one whether or not anything runs it.
	mux.HandleFunc("GET /vms/{id}/kept", func(w http.ResponseWriter, r *http.Request) {
		kept, err := h.Kept(r.Context(), r.PathValue("id"))
		reply(w, r, "kept", kept, err)
	})
	mux.HandleFunc("POST /vms/{id}/kept/{checkpoint}/release", func(w http.ResponseWriter, r *http.Request) {
		checkpoint, err := strconv.ParseUint(r.PathValue("checkpoint"), 10, 64)
		if err != nil || checkpoint == 0 {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "release",
				fmt.Errorf("%w: checkpoint is %q, want a checkpoint sequence", host.ErrRequest,
					r.PathValue("checkpoint")))
			return
		}
		act(w, r, "release", h.Release(r.Context(), r.PathValue("id"), checkpoint))
	})
	mux.HandleFunc("POST /vms", func(w http.ResponseWriter, r *http.Request) {
		var request hostapi.CreateRequest
		if err := jsonhttp.Read(r, &request); err != nil {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "create", err)
			return
		}
		if request.ID == "" {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "create",
				fmt.Errorf("%w: a created VM needs an identity", host.ErrRequest))
			return
		}
		if request.From != nil && (request.From.VM == "" || request.Template != "") {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "create",
				fmt.Errorf("%w: a create from a checkpoint names the VM it is of, and no template",
					host.ErrRequest))
			return
		}
		created, err := h.Create(r.Context(), request)
		reply(w, r, "create", created, err)
	})
	// A guest image is the request's body, streamed rather than decoded: a
	// builder's image is gigabytes. The RAM a VM of it starts with is a query
	// parameter, because the body is the image.
	mux.HandleFunc("POST /templates", func(w http.ResponseWriter, r *http.Request) {
		request := hostapi.ImportTemplateRequest{Tenant: r.URL.Query().Get("tenant")}
		if text := r.URL.Query().Get("memory"); text != "" {
			memory, err := strconv.ParseUint(text, 10, 64)
			if err != nil || memory == 0 {
				jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "import template",
					fmt.Errorf("%w: memory is %q, want a positive number of bytes", host.ErrRequest, text))
				return
			}
			request.Memory = memory
		}
		imported, err := h.ImportTemplate(r.Context(), r.Body, request)
		reply(w, r, "import template", imported, err)
	})
	mux.HandleFunc("POST /vms/{id}/open", func(w http.ResponseWriter, r *http.Request) {
		// An open with no body at all is the ordinary one: the VM comes back
		// exactly where it was.
		var request hostapi.OpenRequest
		if err := jsonhttp.Read(r, &request); err != nil {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "open", err)
			return
		}
		opened, err := h.Open(r.Context(), r.PathValue("id"), request)
		reply(w, r, "open", opened, err)
	})
	mux.HandleFunc("POST /vms/{id}/fork", func(w http.ResponseWriter, r *http.Request) {
		var request hostapi.ForkRequest
		if err := jsonhttp.Read(r, &request); err != nil {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "fork", err)
			return
		}
		if len(request.IDs) == 0 {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "fork",
				fmt.Errorf("%w: a fork needs an identity for every child", host.ErrRequest))
			return
		}
		forked, err := h.Fork(r.Context(), r.PathValue("id"), request)
		reply(w, r, "fork", forked, err)
	})
	mux.HandleFunc("POST /vms/{id}/capture", func(w http.ResponseWriter, r *http.Request) {
		// A capture with no body at all is the ordinary one: a checkpoint of
		// the VM itself.
		var request hostapi.CaptureRequest
		if err := jsonhttp.Read(r, &request); err != nil {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "capture", err)
			return
		}
		if request.Into == r.PathValue("id") {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "capture",
				fmt.Errorf("%w: a VM is captured into a new VM, not into itself", host.ErrRequest))
			return
		}
		checkpoint, err := h.Capture(r.Context(), r.PathValue("id"), request)
		reply(w, r, "capture", checkpoint, err)
	})
	mux.HandleFunc("GET /vms/{id}/console", func(w http.ResponseWriter, r *http.Request) {
		since := int64(0)
		if raw := r.URL.Query().Get("since"); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || parsed < 0 {
				jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "console",
					fmt.Errorf("%w: since is %q, want a byte offset", host.ErrRequest, raw))
				return
			}
			since = parsed
		}
		console, err := h.Console(r.Context(), r.PathValue("id"), since)
		reply(w, r, "console", console, err)
	})
	mux.HandleFunc("POST /vms/{id}/console", func(w http.ResponseWriter, r *http.Request) {
		var request hostapi.ConsoleWrite
		if err := jsonhttp.Read(r, &request); err != nil {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "console", err)
			return
		}
		act(w, r, "console", h.WriteConsole(r.Context(), r.PathValue("id"), request.Data))
	})
	mux.HandleFunc("POST /vms/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		var request hostapi.ExecRequest
		if err := jsonhttp.Read(r, &request); err != nil {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "exec", err)
			return
		}
		if request.Cmd == "" {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "exec",
				fmt.Errorf("%w: an exec needs a command", host.ErrRequest))
			return
		}
		result, err := h.Exec(r.Context(), r.PathValue("id"), request)
		reply(w, r, "exec", result, err)
	})
	mux.HandleFunc("POST /vms/{id}/migrate", func(w http.ResponseWriter, r *http.Request) {
		var request hostapi.MigrateRequest
		if err := jsonhttp.Read(r, &request); err != nil {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "migrate", err)
			return
		}
		if request.Destination == "" {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "migrate",
				fmt.Errorf("%w: a migration needs the destination's page address", host.ErrRequest))
			return
		}
		migrated, err := h.Migrate(r.Context(), r.PathValue("id"), platform.Address(request.Destination))
		reply(w, r, "migrate", migrated, err)
	})
	mux.HandleFunc("POST /vms/receive", func(w http.ResponseWriter, r *http.Request) {
		var handoff hostapi.Handoff
		if err := jsonhttp.Read(r, &handoff); err != nil {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "receive", err)
			return
		}
		// A fork of this host's own fork point is the one handoff with nothing to
		// fetch from: the child maps the pages the seal froze, so no page of it
		// ever reaches the wire and the source names no address. Every other
		// handoff — a migration, or a child whose parent runs elsewhere — has to
		// say where the pages no checkpoint holds are still served.
		if handoff.VMID == "" || (handoff.Source == "" && handoff.Parent == "") {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "receive",
				fmt.Errorf("%w: a handoff names a VM, and the host still serving its pages "+
					"unless it is a fork this host holds the point of", host.ErrRequest))
			return
		}
		received, err := h.Receive(r.Context(), handoff)
		reply(w, r, "receive", received, err)
	})
	mux.HandleFunc("POST /vms/{id}/released", func(w http.ResponseWriter, r *http.Request) {
		act(w, r, "released", h.Released(r.Context(), r.PathValue("id")))
	})
	// Giving a handover up is the other end of releasing it: the control plane
	// says this VM will never be received, so whatever is still held for it goes
	// and a fork's parent takes its sealed pages back. It refuses nothing,
	// because a refusal would only leave that parent sealed for good.
	mux.HandleFunc("POST /vms/{id}/abandoned", func(w http.ResponseWriter, r *http.Request) {
		act(w, r, "abandoned", h.Abandoned(r.Context(), r.PathValue("id")))
	})
	mux.HandleFunc("POST /drain", func(w http.ResponseWriter, r *http.Request) {
		drained, err := h.Drain(r.Context())
		reply(w, r, "drain", drained, err)
	})
	mux.HandleFunc("POST /vms/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		var request hostapi.StopRequest
		if err := jsonhttp.Read(r, &request); err != nil {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "stop", err)
			return
		}
		stopped, err := h.Stop(r.Context(), r.PathValue("id"), request)
		reply(w, r, "stop", stopped, err)
	})
	mux.HandleFunc("DELETE /vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		act(w, r, "delete", h.Delete(r.Context(), r.PathValue("id")))
	})
	return jsonhttp.Authorize(token, openPaths, mux)
}

// openPaths are served without the deployment's token: the kubelet reaches a
// pod's probes before anything has given it one, and they say nothing about the
// VMs this host runs. /metrics is not among them — what a host holds is what
// its VMs are doing — so a scraper carries the token like any other client.
var openPaths = []string{"/healthz", "/livez"}

// reply writes one operation's result or its failure.
func reply[R any](w http.ResponseWriter, r *http.Request, op string, result R, err error) {
	if err != nil {
		jsonhttp.Fail(r.Context(), w, statusOf(err), op, err)
		return
	}
	jsonhttp.Write(w, http.StatusOK, result)
}

// act writes the outcome of an operation with nothing to report but success.
func act(w http.ResponseWriter, r *http.Request, op string, err error) {
	if err != nil {
		jsonhttp.Fail(r.Context(), w, statusOf(err), op, err)
		return
	}
	jsonhttp.Write(w, http.StatusOK, map[string]string{"status": "ok"})
}

// statusOf maps a failure to what the client should make of it: whether to fix
// the request, to ask another host, or to try again later.
func statusOf(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, host.ErrRequest), errors.Is(err, host.ErrInvalidConfig),
		errors.Is(err, volume.ErrInvalidConfig), errors.Is(err, control.ErrInvalidConfig),
		errors.Is(err, platform.ErrInvalidObjectKey),
		// A fork or a create across tenants is a request no host would act on.
		errors.Is(err, volume.ErrOtherTenant):
		return http.StatusBadRequest
	case errors.Is(err, platform.ErrNotFound), errors.Is(err, host.ErrNotRunning):
		return http.StatusNotFound
	case errors.Is(err, volume.ErrExists), errors.Is(err, control.ErrExists), errors.Is(err, host.ErrRunning),
		// A VM a fork point holds sealed is one this host will not stop or
		// delete until the child reading those pages has them.
		errors.Is(err, volume.ErrSealed):
		return http.StatusConflict
	case errors.Is(err, host.ErrNotMigratable), errors.Is(err, volume.ErrHandedOff),
		errors.Is(err, volume.ErrForkPending), errors.Is(err, control.ErrFenced),
		// A checkpoint that is not published, or that its VM's writer may be
		// reclaiming, is one to create from once its VM has published again.
		errors.Is(err, control.ErrNotPublished),
		// A release of a checkpoint nothing keeps, or of one a VM was created
		// from, which nothing releases.
		errors.Is(err, control.ErrNotKept), errors.Is(err, control.ErrForked),
		// A release refused because the destination has not fetched every page
		// this host holds for it is a request that is merely early: the pages
		// exist nowhere else, and the caller asks again once they are there.
		errors.Is(err, vmmigrate.ErrOutstanding):
		return http.StatusConflict
	case errors.Is(err, vmmemory.ErrCapacity):
		// The pager could not map this VM's memory regions. Another host can.
		return http.StatusConflict
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, host.ErrClosed), errors.Is(err, volume.ErrClosed),
		// A guest that is not answering yet is a VM that is still booting, not
		// a request anybody should change.
		errors.Is(err, guest.ErrNoGuest):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
