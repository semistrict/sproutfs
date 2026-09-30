package host_test

import (
	"testing"
	"time"

	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/platform"
)

// awaitCheckpoints steps the clock until the interval checkpoints have ended
// as done says, a second at a time, because each loop arms its timer and
// publishes on goroutines of its own.
func awaitCheckpoints(t *testing.T, h *hostHarness, clock interface{ Advance(time.Duration) int },
	done func(host.CheckpointActivity) bool) host.CheckpointActivity {
	t.Helper()
	for step := 0; ; step++ {
		got := h.hosts[0].Activity().Checkpoints
		if done(got) {
			return got
		}
		if step == 600 {
			t.Fatalf("the interval checkpoints came to %+v in ten minutes", got)
		}
		clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}
}

// A host counts its interval checkpoints and how each ended: a publication
// the store refused is a publish failure, and one it took is published, with
// its pause, its upload and the bytes it uploaded.
func TestAHostCountsItsIntervalCheckpoints(t *testing.T) {
	// A checkpoint's share of the store's traffic is counted by a metered
	// store, as a supervisor's is.
	h, clock, pagers := termsHost(t, func(config *host.Config) {
		metered, err := platform.NewMeteredObjectStore(config.ObjectStore, config.Clock)
		if err != nil {
			t.Fatal(err)
		}
		config.ObjectStore = metered
	})
	_, guest, _ := termsVM(t, h, pagers, "vm-1", host.MachineTerms{CheckpointInterval: time.Minute})
	guest.store("disk", 0, 7)
	h.runtime.ObjectStore().Fail()
	failed := awaitCheckpoints(t, h, clock, func(c host.CheckpointActivity) bool { return c.PublishFailed > 0 })
	if failed.Published != 0 || failed.Attempts == 0 {
		t.Fatalf("with the store down the checkpoints came to %+v, want attempts and no publication", failed)
	}
	h.runtime.ObjectStore().Recover()
	published := awaitCheckpoints(t, h, clock, func(c host.CheckpointActivity) bool { return c.Published > 0 })
	if published.UploadedBytes == 0 || published.Pause.Count == 0 || published.Upload.Count != published.Published {
		t.Fatalf("a published checkpoint was counted as %+v, want its bytes, its pause and its upload", published)
	}
	if ended := published.Published + published.CaptureFailed + published.PublishFailed + published.Fenced; ended > published.Attempts {
		t.Fatalf("%d checkpoints ended of %d attempted", ended, published.Attempts)
	}
}

// A migration is counted where it began and where it landed: the source counts
// a migration and the destination a receive and the pause its guest paid.
func TestAHostCountsItsMigrationsAndReceives(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	var received *machine
	h.configs[1].Migration.StartVM = starter(t, pagers[1], &received)
	h.start(t)
	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	source, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[0].AddMachine("vm-1", source); err != nil {
		t.Fatal(err)
	}
	handoff, err := h.hosts[0].Migrate(t.Context(), "vm-1", h.pages[1])
	if err != nil {
		t.Fatal(err)
	}
	taken, err := h.hosts[1].Receive(t.Context(), handoff)
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	if err := h.hosts[0].ReleaseMigrated("vm-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.hosts[0].Migrate(t.Context(), "vm-absent", h.pages[1]); err == nil {
		t.Fatal("a migration of a VM the host does not run succeeded")
	}
	sourceActivity, destination := h.hosts[0].Activity(), h.hosts[1].Activity()
	if sourceActivity.Migrations != (host.Outcomes{Succeeded: 1, Failed: 1}) {
		t.Fatalf("the source counts migrations %+v, want one succeeded and one failed", sourceActivity.Migrations)
	}
	if destination.Receives != (host.Outcomes{Succeeded: 1}) || destination.MigrationPause.Count != 1 ||
		destination.ForkPause.Count != 0 {
		t.Fatalf("the destination counts receives %+v and %d migration pauses, want one of each",
			destination.Receives, destination.MigrationPause.Count)
	}
}
