package vmmemory

import "testing"

// A dirty budget is a count, not an allocation: a pager admitting a trillion
// dirty pages holds state only for the reservations it has handed out, lowest
// first, and a reservation given back is the next one handed out.
func TestReservationsCostWhatIsTakenNotWhatIsAdmitted(t *testing.T) {
	r := newReservations(1 << 40)
	for want := range 3 {
		if got, ok := r.take(); !ok || got != want {
			t.Fatalf("take %d gave %d %v", want, got, ok)
		}
	}
	r.record(1, 7, true)
	if sum, held := r.digest(1); !held || sum != 7 {
		t.Fatalf("reservation 1 holds %v with checksum %d, want written with 7", held, sum)
	}
	r.put(1)
	if _, held := r.digest(1); held {
		t.Fatal("a reservation given back still holds its bytes")
	}
	if got, ok := r.take(); !ok || got != 1 {
		t.Fatalf("the reservation given back was not the next taken: %d %v", got, ok)
	}
	if got := r.available(); got != 1<<40-3 {
		t.Fatalf("%d reservations available, want %d", got, 1<<40-3)
	}
	if got := len(r.held); got > 3 {
		t.Fatalf("three reservations cost state for %d", got)
	}
}

// A budget runs out.
func TestReservationsRunOut(t *testing.T) {
	r := newReservations(2)
	r.take()
	r.take()
	if got, ok := r.take(); ok {
		t.Fatalf("a budget of two handed out a third reservation, %d", got)
	}
}
