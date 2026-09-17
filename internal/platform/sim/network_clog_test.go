package sim_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
)

// A clog blocks a link the way a partition does and then ends by itself, which
// is what makes the caller's own timeout the thing under test.
func TestClogBlocksALinkUntilTheSimulatedInstant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		listener, client, server := connectedPair(t, runtime)
		defer listener.Close()
		defer client.Close()
		defer server.Close()

		runtime.Network().Clog("client", "server", time.Now().Add(30*time.Second))
		err := client.Send(t.Context(), platform.Frame{Header: []byte("first")})
		if !errors.Is(err, platform.ErrUnavailable) {
			t.Fatalf("send over a clogged link = %v, want ErrUnavailable", err)
		}
		// The reverse link was never clogged: a clog is directional, so a reply
		// still reaches a sender that can no longer be reached.
		if err := server.Send(t.Context(), platform.Frame{Header: []byte("reply")}); err != nil {
			t.Fatalf("send over the healthy reverse link: %v", err)
		}
		if header, _ := receiveFrame(t, client); !bytes.Equal(header, []byte("reply")) {
			t.Fatalf("reverse link delivered %q", header)
		}

		time.Sleep(29 * time.Second)
		if err := client.Send(t.Context(), platform.Frame{Header: []byte("early")}); !errors.Is(err, platform.ErrUnavailable) {
			t.Fatalf("send before the clog expired = %v, want ErrUnavailable", err)
		}
		time.Sleep(2 * time.Second)
		if err := client.Send(t.Context(), platform.Frame{Header: []byte("late")}); err != nil {
			t.Fatalf("send after the clog expired: %v", err)
		}
		if header, _ := receiveFrame(t, server); !bytes.Equal(header, []byte("late")) {
			t.Fatalf("healed link delivered %q", header)
		}
	})
}

// A clog refuses a dial as well as a send: a caller that reconnects must not be
// able to route around it.
func TestClogRefusesDials(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		listener, err := runtime.Network().Listen("server")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		runtime.Network().Clog("client", "server", time.Now().Add(time.Minute))
		if _, err := runtime.Network().Dial(t.Context(), "client", "server"); !errors.Is(err, platform.ErrUnavailable) {
			t.Fatalf("dial over a clogged link = %v, want ErrUnavailable", err)
		}
		time.Sleep(time.Minute)
		conn, err := runtime.Network().Dial(t.Context(), "client", "server")
		if err != nil {
			t.Fatalf("dial after the clog expired: %v", err)
		}
		defer conn.Close()
	})
}

// Healing a link ends its clog: a caller that has decided a link works again
// should not have to know what was blocking it.
func TestHealEndsAClog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		listener, client, server := connectedPair(t, runtime)
		defer listener.Close()
		defer client.Close()
		defer server.Close()
		runtime.Network().Clog("client", "server", time.Now().Add(time.Hour))
		runtime.Network().Heal("client", "server")
		if err := client.Send(t.Context(), platform.Frame{Header: []byte("healed")}); err != nil {
			t.Fatal(err)
		}
		if header, _ := receiveFrame(t, server); !bytes.Equal(header, []byte("healed")) {
			t.Fatalf("healed link delivered %q", header)
		}
	})
}

// A swizzle separates every pair for its own interval inside the window, and
// the order the links come back in is not the order they went away in. That
// difference is the whole point: it is what leaves two peers believing
// different things at the same instant.
func TestSwizzleBlocksAndHealsLinksInDifferentOrders(t *testing.T) {
	addrs := []platform.Address{"host-0", "host-1", "host-2", "store"}
	reordered := 0
	overlapping := 0
	synctest.Test(t, func(t *testing.T) {
		for seed := uint64(1); seed <= 32; seed++ {
			runtime := sim.New(sim.Config{Seed: seed})
			network := runtime.Network()
			base := time.Now()
			network.Swizzle(addrs, time.Minute, runtime.Random("swizzle"))

			type link struct{ from, to platform.Address }
			var links []link
			for _, from := range addrs {
				for _, to := range addrs {
					if from != to {
						links = append(links, link{from, to})
					}
				}
			}
			blockedAt := map[link]time.Duration{}
			healedAt := map[link]time.Duration{}
			// Walk the window a second at a time and record when each link
			// first blocks and when it last unblocks.
			for step := time.Duration(0); step <= time.Minute; step += time.Second {
				time.Sleep(base.Add(step).Sub(time.Now()))
				for _, l := range links {
					clogged := network.Clogged(l.from, l.to)
					if _, seen := blockedAt[l]; clogged && !seen {
						blockedAt[l] = step
					}
					if _, seen := blockedAt[l]; seen && !clogged {
						if _, done := healedAt[l]; !done {
							healedAt[l] = step
						}
					}
				}
			}
			if len(blockedAt) != len(links) {
				t.Fatalf("seed %d left %d of %d links carrying traffic throughout",
					seed, len(links)-len(blockedAt), len(links))
			}
			if len(healedAt) != len(links) {
				t.Fatalf("seed %d left %d links clogged past the window", seed, len(links)-len(healedAt))
			}
			for _, l := range links {
				for _, other := range links {
					if blockedAt[l] < blockedAt[other] && healedAt[l] > healedAt[other] {
						reordered++
					}
					if l != other && blockedAt[l] < healedAt[other] && blockedAt[other] < healedAt[l] {
						overlapping++
					}
				}
			}
			// Nothing is blocked once the window has passed.
			for _, l := range links {
				if network.Clogged(l.from, l.to) {
					t.Fatalf("seed %d left %s->%s clogged after the window", seed, l.from, l.to)
				}
			}
		}
	})
	if reordered == 0 {
		t.Fatal("every swizzle healed links in the order it blocked them")
	}
	if overlapping == 0 {
		t.Fatal("no two links were ever blocked at the same time")
	}
}

// A swizzle is a schedule drawn from the seed, so replaying a seed has to
// produce the same schedule.
func TestSwizzleIsSeedStable(t *testing.T) {
	addrs := []platform.Address{"a", "b", "c"}
	sample := func(seed uint64) string {
		var result string
		synctest.Test(t, func(t *testing.T) {
			runtime := sim.New(sim.Config{Seed: seed})
			base := time.Now()
			runtime.Network().Swizzle(addrs, 10*time.Second, runtime.Random("swizzle"))
			for step := time.Duration(0); step <= 10*time.Second; step += 100 * time.Millisecond {
				time.Sleep(base.Add(step).Sub(time.Now()))
				for _, from := range addrs {
					for _, to := range addrs {
						if from != to && runtime.Network().Clogged(from, to) {
							result += fmt.Sprintf("%v:%s->%s;", step, from, to)
						}
					}
				}
			}
		})
		return result
	}
	if first, second := sample(7), sample(7); first != second {
		t.Fatal("one seed produced two swizzle schedules")
	}
	if sample(7) == sample(8) {
		t.Fatal("two seeds produced the same swizzle schedule")
	}
}
