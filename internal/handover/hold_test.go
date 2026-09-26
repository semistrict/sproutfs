package handover

import (
	"errors"
	"testing"
	"time"
)

// A source that is listed and does not answer is no evidence of anything
// while it holds the pages: it may be serving them right now. Its hold ending
// is, because it promised the pages for that long and no longer.
func TestAQuietSourceIsGoneOnlyWhenItsHoldIsOver(t *testing.T) {
	hold := Held(start, time.Minute)
	quiet := Look{Listed: true}
	if err := hold.Gone(t.Context(), quiet, start.Add(time.Minute-time.Nanosecond)); err != nil {
		t.Fatalf("a quiet source inside its hold is gone: %v", err)
	}
	if err := hold.Gone(t.Context(), quiet, start.Add(time.Minute)); !errors.Is(err, ErrHoldOver) {
		t.Fatalf("a quiet source at the end of its hold = %v, want ErrHoldOver", err)
	}
}

// A source that promised nothing is never over, however long it is quiet.
func TestASourceThatPromisedNothingIsNeverOver(t *testing.T) {
	hold := Held(start, 0)
	if err := hold.Gone(t.Context(), Look{Listed: true}, start.Add(24*time.Hour)); err != nil {
		t.Fatalf("a quiet source that promised no hold is gone: %v", err)
	}
}

// A source the deployment no longer lists, or one that answers and holds
// nothing of the VM, has no pages left to give, inside its hold or not.
func TestAnUnlistedOrEmptySourceIsGoneInsideItsHold(t *testing.T) {
	hold := Held(start, time.Minute)
	if err := hold.Gone(t.Context(), Look{}, start); !errors.Is(err, ErrUnlisted) {
		t.Fatalf("an unlisted source = %v, want ErrUnlisted", err)
	}
	if err := hold.Gone(t.Context(), Look{Listed: true, Answered: true}, start); !errors.Is(err, ErrLetGo) {
		t.Fatalf("a source holding nothing = %v, want ErrLetGo", err)
	}
	if err := hold.Gone(t.Context(), Look{Listed: true, Answered: true, Holding: true}, start); err != nil {
		t.Fatalf("a source that answers and holds the pages is gone: %v", err)
	}
}
