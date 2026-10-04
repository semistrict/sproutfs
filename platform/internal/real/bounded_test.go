package real_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/bounded"
)

// The ways front holds one request, as a store that hangs or stalls does.
const (
	// hang never answers, and leaves the store as it was.
	hang = "hang"
	// hangAfterApply has the emulator carry out the request, and never
	// answers the one that completes a write. An upload in several requests,
	// as Cloud Storage's resumable one is, is answered until the one the
	// emulator answers with the object rather than with where to go on.
	hangAfterApply = "hang-after-apply"
	// stall sends the reply's headers and the first half of its body, and
	// then nothing.
	stall = "stall"
)

// front stands in front of an emulator and holds the next request of the
// methods it is armed for, until the client gives up on it.
type front struct {
	next http.Handler

	mu      sync.Mutex
	kind    string
	methods []string
	held    int
}

func (f *front) arm(kind string, methods ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kind, f.methods = kind, methods
}

// armed is how the front is armed for a request of method.
func (f *front) armed(method string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.methods, method) {
		return ""
	}
	return f.kind
}

// holds is how many requests the front has held.
func (f *front) holds() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held
}

// hold disarms the front for the request it holds.
func (f *front) hold() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kind, f.methods = "", nil
	f.held++
}

func (f *front) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch f.armed(r.Method) {
	case hang:
		f.hold()
		// The request arrived whole, and nothing answers it. A server only
		// notices its client gave up once it has read the request's body.
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		<-r.Context().Done()
	case hangAfterApply:
		reply := httptest.NewRecorder()
		f.next.ServeHTTP(reply, r)
		if reply.Code == http.StatusOK && reply.Header().Get("Location") == "" &&
			reply.Header().Get("X-Http-Status-Code-Override") == "" {
			f.hold()
			<-r.Context().Done()
			return
		}
		forward(w, reply, reply.Body.Len())
	case stall:
		f.hold()
		reply := httptest.NewRecorder()
		f.next.ServeHTTP(reply, r)
		if forward(w, reply, reply.Body.Len()/2) {
			<-r.Context().Done()
		}
	default:
		f.next.ServeHTTP(w, r)
	}
}

// forward sends reply's headers and the first n bytes of its body, with the
// length of the whole body, and reports whether it could.
func forward(w http.ResponseWriter, reply *httptest.ResponseRecorder, n int) bool {
	body := reply.Body.Bytes()
	for name, values := range reply.Header() {
		w.Header()[name] = values
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(reply.Code)
	if _, err := w.Write(body[:n]); err != nil {
		return false
	}
	w.(http.Flusher).Flush()
	return true
}

// Each provider's adapter, under the bounds, over its emulator with a front
// that hangs and stalls: a create whose write landed and whose reply never
// came is made again and refused by its own object, a GET whose headers never
// come is made again, a GET whose body stops halfway is read on from where it
// stopped, and an unconditional PUT that hangs is given back as ErrTimedOut.
// So the client libraries give a request up when its context is cancelled,
// mid-body included, and a second request on the same client succeeds.
//
// What only a real bucket shows is whether a stuck request is a stream on a
// live connection, which a cancellation resets and a second request shares,
// or a connection that is gone, which the second request must not be sent
// on; and the tail of times to first byte the defaults are set against.
func TestEveryProvidersAdapterGivesUpAHungRequestAndResumesAStalledBody(t *testing.T) {
	bounds := bounded.Bounds{FirstByte: 300 * time.Millisecond, Stall: 300 * time.Millisecond}
	for _, provider := range []struct {
		name string
		open func(*testing.T, func(http.Handler) http.Handler) platform.ObjectStore
		// writes are the methods a provider's client uploads with.
		writes []string
	}{
		{"gcs", newConditionalFakeGCSBehind, []string{http.MethodPost, http.MethodPut}},
		{"s3", newFakeS3Behind, []string{http.MethodPut}},
	} {
		t.Run(provider.name, func(t *testing.T) {
			f := &front{}
			raw := provider.open(t, func(next http.Handler) http.Handler {
				f.next = next
				return f
			})
			store, err := bounded.New(raw, bounds, nil)
			if err != nil {
				t.Fatal(err)
			}
			key := objectKey(t, "part")
			data := make([]byte, 256<<10)
			for at := range data {
				data[at] = byte(at*7 + at>>9)
			}

			f.arm(hangAfterApply, provider.writes...)
			_, err = store.Put(t.Context(), platform.PutRequest{Key: key, Body: bytes.NewReader(data),
				Size: int64(len(data)), Conditions: platform.PutConditions{IfNoneMatch: true}})
			if !errors.Is(err, platform.ErrPrecondition) {
				t.Fatalf("a create made again over its own landed write = %v, want ErrPrecondition", err)
			}
			if got := read(t, raw, platform.GetRequest{Key: key}); !bytes.Equal(got, data) {
				t.Fatal("the store does not hold what the create wrote")
			}

			f.arm(hang, http.MethodGet)
			if got := read(t, store, platform.GetRequest{Key: key,
				Range: &platform.ByteRange{Offset: 1000, Length: 5000}}); !bytes.Equal(got, data[1000:6000]) {
				t.Fatal("the GET made again read other bytes than were asked for")
			}

			f.arm(stall, http.MethodGet)
			if got := read(t, store, platform.GetRequest{Key: key}); !bytes.Equal(got, data) {
				t.Fatal("the resumed body read other bytes than the object holds")
			}

			f.arm(hang, provider.writes...)
			scratch := objectKey(t, "scratch")
			_, err = store.Put(t.Context(), platform.PutRequest{Key: scratch, Body: bytes.NewReader(data[:100]),
				Size: 100})
			if !errors.Is(err, bounded.ErrTimedOut) {
				t.Fatalf("a hung unconditional PUT = %v, want ErrTimedOut", err)
			}

			if held := f.holds(); held != 4 {
				t.Fatalf("the front held %d requests, want 4", held)
			}
			want := bounded.Recoveries{Put: bounded.Recovery{FirstByteTimeouts: 2, Retries: 1},
				Get: bounded.Recovery{FirstByteTimeouts: 1, StallTimeouts: 1, Retries: 2}}
			if got := store.Recoveries(); got != want {
				t.Fatalf("recoveries = %+v, want %+v", got, want)
			}
		})
	}
}

// read reads one object, or a range of it, through store.
func read(t *testing.T, store platform.ObjectStore, request platform.GetRequest) []byte {
	t.Helper()
	result, err := store.Get(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(result.Body)
	if closeErr := result.Body.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	return data
}
