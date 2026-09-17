package host_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/semistrict/sproutfs/internal/vmmigrate"
)

// TestReceiveRefusesAHandoffWhoseRecordHasMovedOn: a migration publishes
// nothing, so between the source's release and the destination's open the VM's
// record is openable by anybody — a recovery that took the source for gone, or
// an operator. That writer takes the epoch, writes and publishes a checkpoint of
// its own. The destination then opened a record selecting that writer's
// checkpoint and post-copied the source's frames over it, so one VM's memory
// ended up made of two writers' pages, with no error anywhere. The handoff
// carries the checkpoint the source's record selected, and a destination that
// opens a different one refuses the handoff rather than streaming over it.
func TestReceiveRefusesAHandoffWhoseRecordHasMovedOn(t *testing.T) {
	h, pagers, arenas := startMigrationHosts(t)
	var received *machine
	h.configs[1].Migration.StartVM = starter(t, pagers[1], arenas[1], &received)
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "raced", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	source, err := newMachine(t, pagers[0], arenas[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	source.store("ram0", 0, 7)
	if err := source.checkpoint(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	source.store("ram0", 1, 9)
	if err := h.hosts[0].AddMachine("raced", source); err != nil {
		t.Fatal(err)
	}
	handoff, err := h.hosts[0].Migrate(t.Context(), "raced", h.pages[1])
	if err != nil {
		t.Fatal(err)
	}
	// Another writer gets in and publishes over the record the handoff named.
	writer, err := h.hosts[2].Volumes().Open(t.Context(), "raced")
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Volume("ram0").Write(t.Context(), 0, bytes.Repeat([]byte{11}, migrationPageSize)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Checkpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	taken, err := h.hosts[1].Receive(t.Context(), handoff)
	if err == nil {
		taken.Close()
		t.Fatal("the destination post-copied the source's frames over another writer's checkpoint")
	}
	if !errors.Is(err, vmmigrate.ErrStale) {
		t.Fatalf("receiving a handoff the record has moved past = %v, want ErrStale", err)
	}
	if machines := h.hosts[1].Machines(); len(machines) != 0 {
		t.Fatalf("a refused receive left the destination running %v", machines)
	}
	if vms := h.hosts[1].Volumes().VMs(); len(vms) != 0 {
		t.Fatalf("a refused receive left the destination holding %d handles", len(vms))
	}
	if err := writer.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}
