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

// ErrTooLarge reports a body longer than the bound it was read under. The
// bytes past the bound are never read, so a peer that sends without end costs
// the reader the bound and no more.
var ErrTooLarge = errors.New("the body is larger than its bound")

// quotedBytes is how much of a body that is not the shared error shape a
// failure quotes. It is enough to say what the far side was; the rest of
// what it sent is not worth carrying in an error.
const quotedBytes = 512

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
	raw, err := readBounded(r.Body, MaxBody)
	if err != nil {
		return fmt.Errorf("reading the request body: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return json.Unmarshal(raw, value)
}

// readBounded reads all of body, refusing it with ErrTooLarge once it is past
// limit bytes. It reads one byte past the limit to tell the two apart, and
// nothing after that.
func readBounded(body io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%w of %d bytes", ErrTooLarge, limit)
	}
	return raw, nil
}

// Call sends body, when it is not nil, as JSON and decodes the response into R.
// A response that is not 2xx is returned as an Error, so a caller can report
// what the far side said rather than its status line.
func Call[R any](ctx context.Context, client *http.Client, method, url string, body any) (R, error) {
	return CallWithin[R](ctx, client, method, url, body, MaxBody)
}

// CallWithin is Call for a peer whose answer has a bound of its own, smaller
// than MaxBody. A response past limit bytes fails with ErrTooLarge, whatever
// its status, and is not decoded.
func CallWithin[R any](ctx context.Context, client *http.Client, method, url string, body any, limit int64) (R, error) {
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
	raw, err := readBounded(response.Body, limit)
	if err != nil {
		return result, fmt.Errorf("%s %s: %s: reading the response: %w", method, url, response.Status, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure Error
		if json.Unmarshal(raw, &failure) == nil && failure.Message != "" {
			failure.Status = response.StatusCode
			return result, failure
		}
		return result, fmt.Errorf("%s %s: %s: %s", method, url, response.Status, quote(raw))
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return result, nil
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, errors.Join(fmt.Errorf("%s %s: decoding the response failed", method, url), err)
	}
	return result, nil
}

// quote is what a failure says of a body that is not the shared error shape:
// its first quotedBytes, and how much more there was.
func quote(raw []byte) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) <= quotedBytes {
		return string(raw)
	}
	return fmt.Sprintf("%s... (%d more bytes)", raw[:quotedBytes], len(raw)-quotedBytes)
}
