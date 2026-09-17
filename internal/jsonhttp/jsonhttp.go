// Package jsonhttp is the JSON transport the demo control plane is made of: one
// request shape, one error shape, and one client call. It is shared by the host
// API, the orchestrator API and the CLI so that a failure reads the same
// wherever it is reported.
package jsonhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

// MaxBody bounds a request or response body. Every message here is a handful of
// fields plus, at most, one VMM state blob.
const MaxBody = 64 << 20

// Error is the body of every failed request. Op names the operation, which is
// what a client fanning out over hosts reports back.
type Error struct {
	Op      string `json:"op,omitempty"`
	Message string `json:"error"`
	// Status is the status line the response carried. It is not part of the
	// body — a server sends it once, on the response — and Call fills it in
	// from there, so that a control plane relaying one host's refusal can
	// report it as the refusal that host said it was rather than as an internal
	// failure of its own. Zero is an error that never crossed a connection.
	Status int `json:"-"`
}

func (e Error) Error() string {
	if e.Op == "" {
		return e.Message
	}
	return e.Op + ": " + e.Message
}

// Write sends one value as the response body.
func Write(w http.ResponseWriter, status int, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		slog.Error("jsonhttp: encoding a response failed", "error", err)
		http.Error(w, `{"error":"encoding the response failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(raw); err != nil {
		slog.Warn("jsonhttp: writing a response failed", "error", err)
	}
}

// Fail reports one failed operation as the shared error body and logs it. The
// caller supplies the status, because only it knows which of its own errors are
// the client's fault.
func Fail(ctx context.Context, w http.ResponseWriter, status int, op string, err error) {
	slog.ErrorContext(ctx, "jsonhttp: request failed", "op", op, "status", status, "error", err)
	Write(w, status, Error{Op: op, Message: err.Error()})
}

// Read decodes a request body into value. An empty body leaves value alone,
// which is what an operation whose every field is optional wants.
func Read(r *http.Request, value any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil {
		return err
	}
	if len(raw) > MaxBody {
		return fmt.Errorf("request body is larger than %d bytes", MaxBody)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return json.Unmarshal(raw, value)
}

// Call sends body, when it is not nil, as JSON and decodes the response into R.
// A response that is not 2xx is returned as an Error, so a caller can report
// what the far side said rather than its status line.
func Call[R any](ctx context.Context, client *http.Client, method, url string, body any) (R, error) {
	var result R
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return result, err
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return result, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxBody+1))
	if err != nil {
		return result, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure Error
		if json.Unmarshal(raw, &failure) == nil && failure.Message != "" {
			failure.Status = response.StatusCode
			return result, failure
		}
		return result, fmt.Errorf("%s %s: %s: %s", method, url, response.Status, bytes.TrimSpace(raw))
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return result, nil
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, errors.Join(fmt.Errorf("%s %s: decoding the response failed", method, url), err)
	}
	return result, nil
}
