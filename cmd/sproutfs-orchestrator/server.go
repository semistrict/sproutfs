package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/semistrict/sproutfs/api/orch"
	"github.com/semistrict/sproutfs/internal/jsonhttp"
)

// newServer routes the orchestrator API. It is the whole interface the demo
// drives: the CLI speaks it, and so does a host that is draining.
//
// token is the deployment's shared bearer token, which every request but the
// kubelet's probes must carry. An empty one serves the API to whoever reaches
// it, which is what an orchestrator run by hand outside a cluster does.
func newServer(o *orchestrator, token string) http.Handler {
	mux := http.NewServeMux()
	// The orchestrator holds nothing of its own, so it is ready as soon as it
	// serves; liveness and readiness are the same answer and are separate
	// endpoints only so that a probe of either reads the same on both services.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		jsonhttp.Write(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) {
		jsonhttp.Write(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		jsonhttp.Write(w, http.StatusOK, map[string]string{"version": version})
	})
	mux.HandleFunc("GET /hosts", func(w http.ResponseWriter, r *http.Request) {
		hosts, err := o.Hosts(r.Context())
		reply(w, r, "hosts", hosts, err)
	})
	mux.HandleFunc("GET /vms", func(w http.ResponseWriter, r *http.Request) {
		vms, err := o.VMs(r.Context())
		reply(w, r, "vms", vms, err)
	})
	mux.HandleFunc("POST /vms", func(w http.ResponseWriter, r *http.Request) {
		var request orch.CreateRequest
		if err := jsonhttp.Read(r, &request); err != nil {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "create", err)
			return
		}
		created, err := o.Create(r.Context(), request)
		reply(w, r, "create", created, err)
	})
	mux.HandleFunc("POST /vms/{id}/fork", func(w http.ResponseWriter, r *http.Request) {
		var request orch.ForkRequest
		if err := jsonhttp.Read(r, &request); err != nil {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "fork", err)
			return
		}
		forked, err := o.Fork(r.Context(), r.PathValue("id"), request.Count, request.To)
		reply(w, r, "fork", forked, err)
	})
	mux.HandleFunc("POST /vms/{id}/capture", func(w http.ResponseWriter, r *http.Request) {
		captured, err := o.Capture(r.Context(), r.PathValue("id"))
		reply(w, r, "capture", captured, err)
	})
	mux.HandleFunc("POST /vms/{id}/migrate", func(w http.ResponseWriter, r *http.Request) {
		var request orch.MigrateRequest
		if err := jsonhttp.Read(r, &request); err != nil {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "migrate", err)
			return
		}
		migrated, err := o.Migrate(r.Context(), r.PathValue("id"), request.To)
		reply(w, r, "migrate", migrated, err)
	})
	mux.HandleFunc("POST /vms/{id}/recover", func(w http.ResponseWriter, r *http.Request) {
		var request orch.RecoverRequest
		if err := jsonhttp.Read(r, &request); err != nil {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "recover", err)
			return
		}
		recovered, err := o.Recover(r.Context(), r.PathValue("id"), request.Force)
		reply(w, r, "recover", recovered, err)
	})
	mux.HandleFunc("POST /vms/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		var request orch.StopRequest
		if err := jsonhttp.Read(r, &request); err != nil {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "stop", err)
			return
		}
		stopped, err := o.Stop(r.Context(), r.PathValue("id"), request)
		reply(w, r, "stop", stopped, err)
	})
	mux.HandleFunc("POST /vms/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		var request orch.StartRequest
		if err := jsonhttp.Read(r, &request); err != nil {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "start", err)
			return
		}
		started, err := o.Start(r.Context(), r.PathValue("id"), request)
		reply(w, r, "start", started, err)
	})
	// The check is a GET because it reads: it lists the whole object namespace
	// and changes nothing. A deployment that disagrees with itself comes back as
	// a body of violations with a 200, because the request succeeded — what is
	// wrong is the answer.
	mux.HandleFunc("GET /check", func(w http.ResponseWriter, r *http.Request) {
		checked, err := o.Check(r.Context())
		reply(w, r, "check", checked, err)
	})
	mux.HandleFunc("POST /hosts/{name}/kill", func(w http.ResponseWriter, r *http.Request) {
		killed, err := o.Kill(r.Context(), r.PathValue("name"))
		reply(w, r, "kill", killed, err)
	})
	mux.HandleFunc("DELETE /vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		act(w, r, "delete", o.Delete(r.Context(), r.PathValue("id")))
	})
	mux.HandleFunc("POST /vms/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		var request orch.ExecRequest
		if err := jsonhttp.Read(r, &request); err != nil {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "exec", err)
			return
		}
		if request.Cmd == "" {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "exec",
				fmt.Errorf("%w: an exec needs a command", errRequest))
			return
		}
		result, err := o.Exec(r.Context(), r.PathValue("id"), request)
		reply(w, r, "exec", result, err)
	})
	mux.HandleFunc("POST /drains", func(w http.ResponseWriter, r *http.Request) {
		var report orch.DrainReport
		if err := jsonhttp.Read(r, &report); err != nil {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "drain", err)
			return
		}
		act(w, r, "drain", o.Drained(r.Context(), report))
	})
	mux.HandleFunc("GET /vms/{id}/console", func(w http.ResponseWriter, r *http.Request) {
		since := int64(0)
		if raw := r.URL.Query().Get("since"); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || parsed < 0 {
				jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "console",
					fmt.Errorf("%w: since is %q, want a byte offset", errRequest, raw))
				return
			}
			since = parsed
		}
		console, err := o.Console(r.Context(), r.PathValue("id"), since)
		reply(w, r, "console", console, err)
	})
	mux.HandleFunc("POST /vms/{id}/console", func(w http.ResponseWriter, r *http.Request) {
		var request orch.ConsoleWrite
		if err := jsonhttp.Read(r, &request); err != nil {
			jsonhttp.Fail(r.Context(), w, http.StatusBadRequest, "console", err)
			return
		}
		act(w, r, "console", o.WriteConsole(r.Context(), r.PathValue("id"), request.Data))
	})
	return jsonhttp.Authorize(token, openPaths, mux)
}

// openPaths are served without the deployment's token: the kubelet reaches a
// pod's probes before anything has given it one.
var openPaths = []string{"/healthz", "/livez"}

func reply[R any](w http.ResponseWriter, r *http.Request, op string, result R, err error) {
	if err != nil {
		jsonhttp.Fail(r.Context(), w, statusOf(err), op, err)
		return
	}
	jsonhttp.Write(w, http.StatusOK, result)
}

func act(w http.ResponseWriter, r *http.Request, op string, err error) {
	if err != nil {
		jsonhttp.Fail(r.Context(), w, statusOf(err), op, err)
		return
	}
	jsonhttp.Write(w, http.StatusOK, map[string]string{"status": "ok"})
}

// statusOf says whose problem a failure is: the request's, the deployment's, or
// a host's.
func statusOf(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, errRequest):
		return http.StatusBadRequest
	case errors.Is(err, errNotFound):
		return http.StatusNotFound
	case errors.Is(err, errRunning), errors.Is(err, errContested):
		return http.StatusConflict
	case errors.Is(err, errNoHost):
		return http.StatusServiceUnavailable
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable
	}
	// Nothing here refused it, so a host did, and that host is the only thing
	// that knows why: a VM a fork point holds sealed, a VM it does not run,
	// an identity it already runs. Its own status line is relayed rather than
	// reported as an internal failure of this process, which would send an
	// operator to read the wrong logs for a refusal they can wait out. A host
	// that broke rather than refused is the deployment's own failure and is
	// reported as one.
	var host jsonhttp.Error
	if errors.As(err, &host) && host.Status >= 400 && host.Status < 500 {
		return host.Status
	}
	return http.StatusInternalServerError
}
