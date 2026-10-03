package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// controller is what the driver asks of a node: a node itself in a test, and
// a node's control API on a host.
type controller interface {
	identity(context.Context, struct{}) (identityReply, error)
	follow(context.Context, listRequest) (struct{}, error)
	publish(context.Context, guestRequest) (publishReply, error)
	read(context.Context, readRequest) (readReply, error)
	lose(context.Context, struct{}) (struct{}, error)
	back(context.Context, struct{}) (struct{}, error)
	drop(context.Context, struct{}) (struct{}, error)
	settle(context.Context, struct{}) (struct{}, error)
	stats(context.Context, struct{}) (statsReply, error)
	calibrate(context.Context, calibrateRequest) (calibration, error)
}

// handler is a node's control API: each of its controller's calls, as a POST
// of its request whose reply is the call's.
func (n *node) handler() http.Handler {
	mux := http.NewServeMux()
	route(mux, "identity", n.identity)
	route(mux, "follow", n.follow)
	route(mux, "publish", n.publish)
	route(mux, "read", n.read)
	route(mux, "lose", n.lose)
	route(mux, "back", n.back)
	route(mux, "drop", n.drop)
	route(mux, "settle", n.settle)
	route(mux, "stats", n.stats)
	route(mux, "calibrate", n.calibrate)
	return mux
}

func route[Request, Reply any](mux *http.ServeMux, name string, call func(context.Context, Request) (Reply, error)) {
	mux.HandleFunc("POST /"+name, func(w http.ResponseWriter, r *http.Request) {
		var request Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		got, err := call(r.Context(), request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(got); err != nil {
			slog.WarnContext(r.Context(), "node: a reply did not go", "call", name, "error", err)
		}
	})
}

// remote is a node reached over its control API.
type remote struct {
	client  *http.Client
	address string
}

func (r remote) identity(ctx context.Context, request struct{}) (identityReply, error) {
	return call[identityReply](ctx, r, "identity", request)
}

func (r remote) follow(ctx context.Context, request listRequest) (struct{}, error) {
	return call[struct{}](ctx, r, "follow", request)
}

func (r remote) publish(ctx context.Context, request guestRequest) (publishReply, error) {
	return call[publishReply](ctx, r, "publish", request)
}

func (r remote) read(ctx context.Context, request readRequest) (readReply, error) {
	return call[readReply](ctx, r, "read", request)
}

func (r remote) lose(ctx context.Context, request struct{}) (struct{}, error) {
	return call[struct{}](ctx, r, "lose", request)
}

func (r remote) back(ctx context.Context, request struct{}) (struct{}, error) {
	return call[struct{}](ctx, r, "back", request)
}

func (r remote) drop(ctx context.Context, request struct{}) (struct{}, error) {
	return call[struct{}](ctx, r, "drop", request)
}

func (r remote) settle(ctx context.Context, request struct{}) (struct{}, error) {
	return call[struct{}](ctx, r, "settle", request)
}

func (r remote) stats(ctx context.Context, request struct{}) (statsReply, error) {
	return call[statsReply](ctx, r, "stats", request)
}

func (r remote) calibrate(ctx context.Context, request calibrateRequest) (calibration, error) {
	return call[calibration](ctx, r, "calibrate", request)
}

// call makes one call of a node's control API.
func call[Reply any](ctx context.Context, r remote, name string, request any) (Reply, error) {
	var reply Reply
	encoded, err := json.Marshal(request)
	if err != nil {
		return reply, err
	}
	post, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+r.address+"/"+name,
		bytes.NewReader(encoded))
	if err != nil {
		return reply, err
	}
	response, err := r.client.Do(post)
	if err != nil {
		return reply, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		text, err := io.ReadAll(response.Body)
		if err != nil {
			return reply, fmt.Errorf("%s on %s: %s, and its body did not read: %w", name, r.address, response.Status, err)
		}
		return reply, fmt.Errorf("%s on %s: %s: %s", name, r.address, response.Status, strings.TrimSpace(string(text)))
	}
	return reply, json.NewDecoder(response.Body).Decode(&reply)
}
