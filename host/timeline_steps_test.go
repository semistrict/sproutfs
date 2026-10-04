package host_test

import (
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/platform/sim"
)

// A fork's source times its confirm, its pause inside the seal, the pin and
// the handoff, and the child's destination times its open, its registration,
// its post-copy and, on another host, the claim. The timelines run on a clock
// that never moves, so the steps come out in the order they ended.
func TestAForkAndItsReceiveTimeTheirSteps(t *testing.T) {
	for _, remote := range []bool{false, true} {
		name := "on the parent's host"
		if remote {
			name = "on another host"
		}
		t.Run(name, func(t *testing.T) {
			h, pagers := startMigrationHosts(t)
			destination := 0
			if remote {
				destination = 1
			}
			var child *machine
			h.configs[destination].Migration.StartVM = starter(t, pagers[destination], &child)
			h.start(t)

			vm, err := h.hosts[0].Volumes().Create(t.Context(), "parent", migrationVolumes)
			if err != nil {
				t.Fatal(err)
			}
			guest, err := newMachine(t, pagers[0], vm, nil)
			if err != nil {
				t.Fatal(err)
			}
			guest.store("ram0", 1, 7)
			if err := h.hosts[0].AddMachine("parent", guest); err != nil {
				t.Fatal(err)
			}
			defer h.hosts[0].RemoveMachine("parent")

			clock := sim.New(sim.Config{Seed: 1}).NewClock("timeline")
			address := h.pages[0]
			if remote {
				address = h.pages[1]
			}
			forking := host.WithTimeline(t.Context(), clock, "parent", "fork")
			handoffs, err := h.hosts[0].Fork(forking, "parent", []string{"child"}, address)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := host.TimelineSteps(forking), []string{"confirm", "pause", "seal", "pin", "handoff"}; !slices.Equal(got, want) {
				t.Fatalf("the fork timed %v, want %v", got, want)
			}

			receiving := host.WithTimeline(t.Context(), clock, "child", "receive")
			received, err := h.hosts[destination].Receive(receiving, handoffs[0])
			if err != nil {
				t.Fatal(err)
			}
			defer received.Close()
			awaitRooted(t, received.VM())
			want := []string{"open", "register", "post-copy"}
			if remote {
				want = append(want, "claim")
			}
			if got := host.TimelineSteps(receiving); !slices.Equal(got, want) {
				t.Fatalf("the receive timed %v, want %v", got, want)
			}
			if err := h.hosts[0].ReleaseMigrated("child"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
