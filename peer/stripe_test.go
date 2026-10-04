package peer_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/rank"
)

// A stripe read has connections of its own. A guest fault's page that the
// source is slow to build holds every reply behind it on its connection, and a
// stripe read made meanwhile is answered at once all the same: it never waits
// behind a page.
func TestAStripeReadNeverWaitsBehindAPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := newMemoryCache(1)
		cache.stripes[stripeKey{window, 0, 0}] = []byte("a stripe")
		s := newServing(t, peer.ServerConfig{Cache: cache, Membership: cache.source, Member: cache.member})
		destination := s.table(t, "destination", peer.TableConfig{Connections: peer.Connections{Fault: 1, BulkRead: 1,
			BulkWrite: 1, Stripe: 1}})
		slow := make(chan error, 1)
		go func() {
			_, err := disk(t.Context(), destination, 0, 1)
			slow <- err
		}()
		<-s.gate.entered
		read := make(chan error, 1)
		go func() {
			reply, err := destination.ReadStripes(t.Context(), cache.route,
				peer.StripeRead{Window: window, Code: rank.Code{K: 1, M: 1}, MaxBytes: 1 << 10})
			if err == nil {
				if len(reply.Items) != 1 || string(reply.Payload) != "a stripe" {
					err = errors.New("the read came back without its stripe")
				}
				reply.Release()
			}
			read <- err
		}()
		// A second of simulated time is a thousand round trips.
		time.Sleep(time.Second)
		select {
		case err := <-read:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("a stripe read waited behind a page the source had not built")
		}
		if status := destination.Status(); status.Connections != (peer.Connections{Fault: 1, Stripe: 1}) {
			t.Fatalf("the destination holds %+v connections, want one fault and one stripe connection", status.Connections)
		}
		close(s.gate.open)
		if err := <-slow; err != nil {
			t.Fatal(err)
		}
	})
}

// A presence check is a fault unless its context names another class. A
// pull asks it as bulk work: it goes over a bulk connection and opens no
// fault connection, where a guest's fault would wait behind it.
func TestAPresenceCheckGoesOverTheClassItsContextNames(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := newMemoryCache(1)
		s := newServing(t, peer.ServerConfig{Cache: cache, Membership: cache.source, Member: cache.member})
		for _, c := range []struct {
			host  platform.Address
			ctx   context.Context
			wants peer.Connections
		}{
			{"faulting", t.Context(), peer.Connections{Fault: 1}},
			{"pulling", peer.WithClass(t.Context(), peer.BulkRead), peer.Connections{BulkRead: 1}},
		} {
			asker := s.table(t, c.host, peer.TableConfig{})
			held, err := asker.Presence(c.ctx, cache.route,
				peer.Presence{Windows: []rank.Window{window}, Code: rank.Code{K: 1, M: 1}})
			if err != nil {
				t.Fatal(err)
			}
			if len(held) != 1 || len(held[0]) != 2 {
				t.Fatalf("%s: presence %v, want two indices of one window", c.host, held)
			}
			if status := asker.Status(); status.Connections != c.wants {
				t.Fatalf("%s holds %+v connections, want %+v", c.host, status.Connections, c.wants)
			}
		}
	})
}

// A host serves stripes within its serving bandwidth: a read that finds it
// spent is answered BUSY, nothing is read for it, and its reader is free to
// ask another holder. Once a tenth of a second has refilled it, the next read
// is served. A read of no bytes, which asks only for a fill right, never
// counts against it.
func TestAServerAnswersBusyPastItsServingBandwidth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := newMemoryCache(1)
		stripe := make([]byte, 64<<10)
		cache.stripes[stripeKey{window, 0, 0}] = stripe
		// A megabyte a second bursts a tenth of a second, about 102 KiB: the
		// first read leaves 38 KiB, the second leaves a debt of 26 KiB.
		s := newServing(t, peer.ServerConfig{Cache: cache, Membership: cache.source, Member: cache.member, StripeBytesPerSecond: 1 << 20})
		destination := s.table(t, "destination", peer.TableConfig{})
		read := func(maxBytes int64) error {
			reply, err := destination.ReadStripes(t.Context(), cache.route,
				peer.StripeRead{Window: window, Code: rank.Code{K: 1, M: 1}, MaxBytes: maxBytes})
			if err == nil {
				reply.Release()
			}
			return err
		}
		for at := range 2 {
			if err := read(128 << 10); err != nil {
				t.Fatalf("read %d within the burst: %v", at, err)
			}
		}
		if err := read(128 << 10); !errors.Is(err, peer.ErrBusy) {
			t.Fatalf("a read past the serving bandwidth came back with %v, want busy", err)
		}
		if err := read(0); err != nil {
			t.Fatalf("a read of no bytes past the serving bandwidth: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
		if err := read(128 << 10); err != nil {
			t.Fatalf("a read once the bandwidth refilled: %v", err)
		}
		stats := s.server.Stats()
		if stats.StripeReads != 3 || stats.Stripes != 3 || stats.StripeBytes != 3*int64(len(stripe)) ||
			stats.StripesBusy != 1 {
			t.Fatalf("the server reports %+v, want three reads of one stripe served and one busy", stats)
		}
	})
}
