package host

import (
	"errors"
	"testing"
)

// No guest runs as a template, of a tenant or of none: a guest running as a
// public one would publish pages every tenant reads.
func TestNoGuestRunsAsATemplate(t *testing.T) {
	for _, id := range []string{"template-image", "acme/template-image"} {
		if err := guestless(id); !errors.Is(err, ErrRequest) {
			t.Fatalf("a guest named %s = %v, want ErrRequest", id, err)
		}
	}
	for _, id := range []string{"vm-1", "acme/vm-1", "acme/templates"} {
		if err := guestless(id); err != nil {
			t.Fatalf("a guest named %s = %v, want none", id, err)
		}
	}
}
