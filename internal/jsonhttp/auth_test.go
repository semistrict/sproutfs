package jsonhttp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/semistrict/sproutfs/internal/jsonhttp"
)

// served records what reached the handler behind the middleware.
type served struct {
	paths []string
}

func (s *served) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.paths = append(s.paths, r.URL.Path)
	jsonhttp.Write(w, http.StatusOK, map[string]string{"status": "ok"})
}

func TestAuthorizeAdmitsTheTokenAndNothingElse(t *testing.T) {
	behind := &served{}
	handler := jsonhttp.Authorize("the-token", []string{"/healthz"}, behind)
	send := func(path, header string) int {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(""))
		if header != "" {
			request.Header.Set("Authorization", header)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Code
	}
	if status := send("/vms", ""); status != http.StatusUnauthorized {
		t.Fatalf("a request with no token answered %d, want 401", status)
	}
	if status := send("/vms", "Bearer the-toke"); status != http.StatusUnauthorized {
		t.Fatalf("a request with a prefix of the token answered %d, want 401", status)
	}
	if status := send("/vms", "the-token"); status != http.StatusUnauthorized {
		t.Fatalf("a request with the token and no scheme answered %d, want 401", status)
	}
	if len(behind.paths) != 0 {
		t.Fatalf("refused requests reached the handler: %v", behind.paths)
	}
	if status := send("/healthz", ""); status != http.StatusOK {
		t.Fatalf("the open probe answered %d, want 200", status)
	}
	if status := send("/vms", "Bearer the-token"); status != http.StatusOK {
		t.Fatalf("an authenticated request answered %d, want 200", status)
	}
	if want := []string{"/healthz", "/vms"}; strings.Join(behind.paths, " ") != strings.Join(want, " ") {
		t.Fatalf("the handler served %v, want %v", behind.paths, want)
	}
}

// A deployment that configures no token admits everyone, which is what a
// process run by hand outside a cluster is.
func TestAuthorizeWithoutATokenAdmitsEveryone(t *testing.T) {
	behind := &served{}
	handler := jsonhttp.Authorize("", nil, behind)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/vms", nil))
	if recorder.Code != http.StatusOK || len(behind.paths) != 1 {
		t.Fatalf("status %d, handler saw %v", recorder.Code, behind.paths)
	}
}

// The client is the other half: every request it makes carries the token, and
// the refusal it gets without one reads as the shared error shape.
func TestAuthenticatedClientCarriesTheToken(t *testing.T) {
	behind := &served{}
	server := httptest.NewServer(jsonhttp.Authorize("the-token", nil, behind))
	defer server.Close()

	if _, err := jsonhttp.Call[map[string]string](context.Background(), server.Client(),
		http.MethodPost, server.URL+"/vms", nil); err == nil {
		t.Fatal("an unauthenticated call succeeded")
	} else if failure := (jsonhttp.Error{}); !asError(err, &failure) || failure.Op != "authorize" {
		t.Fatalf("an unauthenticated call failed with %v", err)
	}
	client := jsonhttp.Authenticated(server.Client(), "the-token")
	result, err := jsonhttp.Call[map[string]string](context.Background(), client,
		http.MethodPost, server.URL+"/vms", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "ok" {
		t.Fatalf("result %v", result)
	}
}

// asError reports whether err is the shared error shape, which is how a client
// reads what the far side said rather than its status line.
func asError(err error, into *jsonhttp.Error) bool {
	failure, ok := err.(jsonhttp.Error)
	if ok {
		*into = failure
	}
	return ok
}

// The refusal body is the shared shape, so a client reports it like any other
// failure.
func TestAuthorizeRefusalIsTheSharedErrorShape(t *testing.T) {
	handler := jsonhttp.Authorize("the-token", nil, &served{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/vms", nil))
	var failure jsonhttp.Error
	if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Op != "authorize" || !strings.Contains(failure.Message, jsonhttp.TokenEnv) {
		t.Fatalf("failure %+v", failure)
	}
}
