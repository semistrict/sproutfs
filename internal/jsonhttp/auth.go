package jsonhttp

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// TokenEnv is the environment variable every process of the deployment reads
// its shared bearer token from. One token admits the whole control plane: the
// orchestrator to the hosts, a draining host to the orchestrator, and the CLI
// to either. It is not authority over any VM — a VM's authority is the epoch in
// its control record — and it carries no identity: it says only that the caller
// is part of this deployment.
const TokenEnv = "SPROUTFS_API_TOKEN"

// Token is the deployment's token as a process should hold it. The value comes
// out of a Kubernetes Secret through an environment variable, and a Secret
// written with a heredoc or edited by hand carries a trailing newline: one
// process that trimmed its copy and one that did not refused every request
// between them with a 401 that said nothing about whitespace. Both sides of
// every hop go through here, so a token that reads the same is the same, and a
// value that is only whitespace is what it looks like — no token at all.
func Token(value string) string { return strings.TrimSpace(value) }

// Authorize admits a request carrying the bearer token and refuses every other
// one, in the shared error shape so that a refusal reads like any other
// failure. open names the paths served without it: the kubelet's probes reach a
// pod before anything has given it a token, and they say nothing about the VMs
// this process runs.
//
// An empty token turns authentication off, which is what a single-process test
// or a host started by hand wants; a process that serves an API without one
// says so at startup rather than leaving it to be discovered.
func Authorize(token string, open []string, handler http.Handler) http.Handler {
	token = Token(token)
	if token == "" {
		return handler
	}
	want := []byte(token)
	exempt := make(map[string]bool, len(open))
	for _, path := range open {
		exempt[path] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if exempt[r.URL.Path] {
			handler.ServeHTTP(w, r)
			return
		}
		got, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !found || subtle.ConstantTimeCompare([]byte(Token(got)), want) != 1 {
			slog.WarnContext(r.Context(), "jsonhttp: refused an unauthenticated request",
				"method", r.Method, "path", r.URL.Path, "peer", r.RemoteAddr)
			Write(w, http.StatusUnauthorized, Error{Op: "authorize",
				Message: fmt.Sprintf("%s requires the deployment's bearer token", TokenEnv)})
			return
		}
		handler.ServeHTTP(w, r)
	})
}

// Authenticated is client over which every request carries the deployment's
// bearer token. An empty token returns client unchanged, so a process
// configured without one talks to a deployment that wants none.
func Authenticated(client *http.Client, token string) *http.Client {
	token = Token(token)
	if token == "" {
		return client
	}
	if client == nil {
		client = http.DefaultClient
	}
	copied := *client
	copied.Transport = bearer{token: token, next: client.Transport}
	return &copied
}

// bearer adds the deployment's token to every request. The header is set on a
// copy: a RoundTripper must not change the request it is given.
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(request *http.Request) (*http.Response, error) {
	next := b.next
	if next == nil {
		next = http.DefaultTransport
	}
	carried := request.Clone(request.Context())
	carried.Header.Set("Authorization", "Bearer "+b.token)
	return next.RoundTrip(carried)
}
