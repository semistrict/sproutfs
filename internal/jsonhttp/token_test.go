package jsonhttp_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/semistrict/sproutfs/internal/jsonhttp"
)

// TestTheTokenIsTrimmedOnEverySide: the deployment's token comes out of a
// Kubernetes Secret through an environment variable, and a Secret written with a
// heredoc or edited by hand carries a trailing newline. One process trimmed its
// copy and another did not, so every request between them was refused with a 401
// that said nothing about whitespace — the hardest possible way to learn that
// two identical-looking tokens differ. Both sides trim, so a token that reads
// the same is the same.
func TestTheTokenIsTrimmedOnEverySide(t *testing.T) {
	admitted := false
	server := httptest.NewServer(jsonhttp.Authorize(" secret\n", nil,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			admitted = true
			jsonhttp.Write(w, http.StatusOK, map[string]string{"status": "ok"})
		})))
	defer server.Close()

	client := jsonhttp.Authenticated(server.Client(), "secret\n")
	response, err := client.Get(server.URL + "/anything")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("a token that differs only in whitespace was refused with %d", response.StatusCode)
	}
	if !admitted {
		t.Fatal("the request never reached the handler")
	}
}

// TestAWhitespaceOnlyTokenIsNoToken: a Secret whose value is a newline is a
// deployment that was never given a token, and it must not turn into one that
// admits exactly the caller sending a newline.
func TestAWhitespaceOnlyTokenIsNoToken(t *testing.T) {
	if got := jsonhttp.Token(" \n\t "); got != "" {
		t.Fatalf("a whitespace-only token reads %q, want no token at all", got)
	}
	if got := jsonhttp.Token("secret\n"); got != "secret" {
		t.Fatalf("a token with a trailing newline reads %q", got)
	}
}
