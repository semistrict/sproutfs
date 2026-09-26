package host_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/volume"
)

// templateVolumes are a template's two volumes: the RAM a VM forked from it
// gets, which reads as the zeroes a cold boot starts from, and the root the
// guest image is written into.
var templateVolumes = []volume.VolumeSpec{{Name: "ram0", Size: 8192, PageSize: migrationPageSize}, {Name: "root", Size: 8192, PageSize: migrationPageSize}}

// guestImage is a guest image's bytes: enough of them to be a page nobody could
// mistake for the zeroes an untouched volume reads as, so a checkpoint of it
// publishes a part.
func guestImage(value byte) []byte { return bytes.Repeat([]byte{value}, 4096) }

// templateImport is one host's import of one guest image. The image's bytes are
// the whole of what names the template, so the name the request carries says
// only which of this host's configured images it is.
func templateImport(image string, value byte) host.TemplateImport {
	return host.TemplateImport{Image: image, Volumes: templateVolumes, Root: "root",
		Source: bytes.NewReader(guestImage(value))}
}

// templateIDOf is the identity a guest image's bytes name, computed the way a
// reader of the deployment would: the sha256 of the image, hex, under the
// template prefix.
func templateIDOf(image []byte) string {
	sum := sha256.Sum256(image)
	return hostapi.TemplateID(sum)
}

// templateObjects is every object key of one identity's checkpoint namespace, in
// ascending order and without the deployment's own prefix.
func templateObjects(t *testing.T, h *hostHarness, id string) []string {
	t.Helper()
	base := h.prefix.String()
	prefix, err := platform.NewObjectPrefix(base + "vm/" + id + "/")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	token := ""
	for {
		page, err := h.runtime.ObjectStore().List(context.Background(),
			platform.ListRequest{Prefix: prefix, ContinuationToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range page.Objects {
			keys = append(keys, strings.TrimPrefix(entry.Key.String(), base))
		}
		if page.NextContinuationToken == "" {
			slices.Sort(keys)
			return keys
		}
		token = page.NextContinuationToken
	}
}

// templateRecord is one template's control record, and reports whether the
// deployment holds one at all.
func templateRecord(t *testing.T, h *hostHarness, index int, id string) (control.Record, bool) {
	t.Helper()
	record, err := h.hosts[index].Control().Read(context.Background(), id)
	if errors.Is(err, platform.ErrNotFound) {
		return control.Record{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return record, true
}

// templateOn asks one host for the template of a guest image and fails the test
// if it cannot have it, which is what a host that could not import stays
// unready for.
func templateOn(t *testing.T, h *hostHarness, index int, request host.TemplateImport) *host.ImportedTemplate {
	t.Helper()
	imported, err := h.hosts[index].TemplateOf(t.Context(), request)
	if err != nil {
		t.Fatalf("the template of %s: %v", request.Image, err)
	}
	return imported
}

// forkReads is what a VM created from a template reads at the front of its root
// volume, which is the image the template holds.
func forkReads(t *testing.T, h *hostHarness, index int, id string, template *host.ImportedTemplate) []byte {
	t.Helper()
	vm, err := h.hosts[index].Volumes().Fork(t.Context(), id, template.Point)
	if err != nil {
		t.Fatalf("creating %s from %s: %v", id, template.ID(), err)
	}
	defer vm.Close(t.Context())
	if err := vm.Checkpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	page := make([]byte, 4096)
	if err := vm.Volume("root").Read(t.Context(), 0, page); err != nil {
		t.Fatal(err)
	}
	return page
}

// TestTwoHostsImportOneImageOnce: a template is the image imported, and an
// image's identity is its bytes. Two hosts configured with the same image
// therefore name one template between them: the first to reach it imports it,
// the second finds it published and opens nothing at all, and both fork from
// the one checkpoint it holds.
func TestTwoHostsImportOneImageOnce(t *testing.T) {
	h := newSizedHostHarness(t, 2)
	h.start(t)
	image := guestImage(0xa5)
	want := templateIDOf(image)

	first := templateOn(t, h, 0, templateImport("alpine", 0xa5))
	if first.ID() != want {
		t.Fatalf("the first host imported at %s, want %s: a template is named by its image", first.ID(), want)
	}
	record, found := templateRecord(t, h, 0, want)
	if !found {
		t.Fatalf("the import left no control record at %s", want)
	}
	epoch, selected := record.Epoch, record.Selected

	second := templateOn(t, h, 1, templateImport("alpine", 0xa5))
	if second.ID() != want {
		t.Fatalf("the second host's template is %s, want %s", second.ID(), want)
	}
	// It imported nothing: it took no epoch, so the record is exactly what the
	// first host left, and it published no checkpoint over it.
	again, _ := templateRecord(t, h, 1, want)
	if again.Epoch != epoch {
		t.Fatalf("the second host took the template's epoch: it was %d and is %d", epoch, again.Epoch)
	}
	if again.Selected != selected {
		t.Fatalf("the second host published over the template: it selected %d and now selects %d",
			selected, again.Selected)
	}

	// And both create VMs that read the image, out of the one template.
	for index, template := range []*host.ImportedTemplate{first, second} {
		if got := forkReads(t, h, index, fmt.Sprintf("vm-%d", index), template); !bytes.Equal(got, image) {
			t.Fatalf("a VM created on host %d reads %#x..., want the image it forked", index, got[:8])
		}
	}
}

// TestAHostRestartedAfterImportingImportsNothing: the bytes behind a name have
// not changed because a process ended, so a host that comes back finds the
// template of its image published and imports nothing. That is the whole of
// what makes the host pods a Deployment again: nothing about a template's
// identity is the pod's.
func TestAHostRestartedAfterImportingImportsNothing(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	h.start(t)

	first := templateOn(t, h, 0, templateImport("alpine", 0xa5))
	before, found := templateRecord(t, h, 0, first.ID())
	if !found {
		t.Fatalf("the import left no control record at %s", first.ID())
	}
	objects := templateObjects(t, h, first.ID())

	// The pod is replaced: this process ends and another starts, under a name
	// of its own that nothing here ever knew.
	h.stop(t, 0)
	h.launch(t, 0)

	second := templateOn(t, h, 0, templateImport("alpine", 0xa5))
	if second.ID() != first.ID() {
		t.Fatalf("the replacement's template is %s, want the %s its predecessor imported",
			second.ID(), first.ID())
	}
	after, _ := templateRecord(t, h, 0, first.ID())
	if after.Epoch != before.Epoch || after.Selected != before.Selected {
		t.Fatalf("the replacement imported again: the record was epoch %d selecting %d and is epoch %d selecting %d",
			before.Epoch, before.Selected, after.Epoch, after.Selected)
	}
	if got := templateObjects(t, h, first.ID()); !slices.Equal(got, objects) {
		t.Fatalf("the replacement published %d objects under the template, which held %d",
			len(got)-len(objects), len(objects))
	}
	if got := forkReads(t, h, 0, "vm-a", second); !bytes.Equal(got, guestImage(0xa5)) {
		t.Fatalf("a VM created after the restart reads %#x..., want the image it forked", got[:8])
	}
}

// TestAChangedImageIsANewTemplate: a redeploy is the bytes behind a name
// changing, and a template is the bytes. The new image is a template of its
// own, and the old one is left exactly as it is — its record, its checkpoint
// and the pin on it — because another host may still be forking from it.
func TestAChangedImageIsANewTemplate(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	h.start(t)

	old := templateOn(t, h, 0, templateImport("alpine", 0xa5))
	before, found := templateRecord(t, h, 0, old.ID())
	if !found {
		t.Fatalf("the import left no control record at %s", old.ID())
	}
	if !before.IsPinned(before.Selected) {
		t.Fatalf("the import left %s selecting %d unpinned: a template pins what its VMs fork",
			old.ID(), before.Selected)
	}
	objects := templateObjects(t, h, old.ID())

	// The image on the disk is replaced and the host starts again.
	h.stop(t, 0)
	h.launch(t, 0)
	fresh := templateOn(t, h, 0, templateImport("alpine", 0x5a))
	if fresh.ID() == old.ID() {
		t.Fatalf("a changed image imported at %s again: a template is named by its bytes", fresh.ID())
	}
	if want := templateIDOf(guestImage(0x5a)); fresh.ID() != want {
		t.Fatalf("the changed image imported at %s, want %s", fresh.ID(), want)
	}
	after, still := templateRecord(t, h, 0, old.ID())
	if !still {
		t.Fatalf("the new import removed the old template's record at %s", old.ID())
	}
	if after.Epoch != before.Epoch || after.Selected != before.Selected ||
		!slices.Equal(after.Pinned, before.Pinned) {
		t.Fatalf("the new import changed the old template's record: epoch %d selecting %d pinning %v,"+
			" want epoch %d selecting %d pinning %v",
			after.Epoch, after.Selected, after.Pinned, before.Epoch, before.Selected, before.Pinned)
	}
	if got := templateObjects(t, h, old.ID()); !slices.Equal(got, objects) {
		t.Fatalf("the new import touched the old template's objects: %v, want %v", got, objects)
	}
	if got := forkReads(t, h, 0, "vm-old", old); !bytes.Equal(got, guestImage(0xa5)) {
		t.Fatalf("a VM created from the old template reads %#x..., want the image it forked", got[:8])
	}
	if got := forkReads(t, h, 0, "vm-new", fresh); !bytes.Equal(got, guestImage(0x5a)) {
		t.Fatalf("a VM created from the new template reads %#x..., want the image it forked", got[:8])
	}
}

// TestTwoHostsRacingToImportOneImageImportItOnce: the two hosts read the
// template's record at the same moment and both find it absent, so both go on
// to create it. The record's create-if-absent picks one of them; the other is
// refused it, reads the winner's record and waits for the checkpoint its import
// publishes rather than opening the record and fencing an import in flight.
//
// Either of them may be the winner, so what is asserted is what both of them
// leave: one identity, one epoch, and both hosts forking the image out of it.
func TestTwoHostsRacingToImportOneImageImportItOnce(t *testing.T) {
	h := newSizedHostHarness(t, 2)
	h.start(t)
	image := guestImage(0xa5)
	want := templateIDOf(image)

	var wg sync.WaitGroup
	templates := make([]*host.ImportedTemplate, 2)
	failures := make([]error, 2)
	start := make(chan struct{})
	for index := range templates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			templates[index], failures[index] = h.hosts[index].TemplateOf(t.Context(),
				templateImport("alpine", 0xa5))
		}()
	}
	close(start)
	wg.Wait()
	for index, err := range failures {
		if err != nil {
			t.Fatalf("host %d raced for the template and lost it: %v", index, err)
		}
		if templates[index].ID() != want {
			t.Fatalf("host %d's template is %s, want %s", index, templates[index].ID(), want)
		}
	}
	// One import: the record was written once and never taken over, so its
	// epoch is the first epoch an importing host drew.
	record, found := templateRecord(t, h, 0, want)
	if !found {
		t.Fatalf("the race left no control record at %s", want)
	}
	if !record.IsPinned(record.Selected) {
		t.Fatalf("the race left %s selecting %d unpinned", want, record.Selected)
	}
	if got := len(record.Pinned); got != 1 {
		t.Fatalf("the race left %d pins on %s, want the one pause its VMs fork", got, want)
	}
	for index, template := range templates {
		if got := forkReads(t, h, index, fmt.Sprintf("vm-%d", index), template); !bytes.Equal(got, image) {
			t.Fatalf("a VM created on host %d reads %#x..., want the image it forked", index, got[:8])
		}
	}
}

// TestAHalfImportedTemplateIsRecoveredRatherThanWaitedOnForEver: an import that
// wrote the record and died leaves a template whose selected checkpoint is not
// the image and is pinned by nothing. A host that finds it waits for the import
// to publish, because an import in flight is the ordinary reason a record has
// no pin yet, and then recovers it as any VM is recovered: it takes the epoch,
// which fences a writer that is not dead after all, and imports again under it.
//
// The alternative is every host of the deployment waiting for ever on one pod
// that died, which is a half-imported template wedging the deployment.
func TestAHalfImportedTemplateIsRecoveredRatherThanWaitedOnForEver(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	h.start(t)
	image := guestImage(0xa5)
	id := templateIDOf(image)

	// An import that got as far as the record and no further: the identity is
	// taken, and nothing of the image is published under it.
	started, err := h.hosts[0].Volumes().Create(t.Context(), id, templateVolumes)
	if err != nil {
		t.Fatal(err)
	}
	record, found := templateRecord(t, h, 0, id)
	if !found {
		t.Fatalf("the fixture wanted a record at %s", id)
	}
	if len(record.Pinned) != 0 {
		t.Fatalf("the fixture wanted a template nothing had pinned, and %s pins %v", id, record.Pinned)
	}
	if err := started.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	request := templateImport("alpine", 0xa5)
	// The wait is the whole of what separates an import in flight from one
	// whose host is gone, and this one's host is gone.
	request.Wait = -1
	recovered := templateOn(t, h, 0, request)
	if recovered.ID() != id {
		t.Fatalf("the recovery imported at %s, want %s: a template is named by its image", recovered.ID(), id)
	}
	after, _ := templateRecord(t, h, 0, id)
	if after.Epoch <= record.Epoch {
		t.Fatalf("the recovery did not take the epoch: it was %d and is %d", record.Epoch, after.Epoch)
	}
	if !after.IsPinned(after.Selected) {
		t.Fatalf("the recovery left %s selecting %d unpinned", id, after.Selected)
	}
	if got := forkReads(t, h, 0, "vm-a", recovered); !bytes.Equal(got, image) {
		t.Fatalf("a VM created from the recovered template reads %#x..., want the image it forked", got[:8])
	}
}

// TestAnImportInFlightIsWaitedForRatherThanTakenOver: the other side of the
// wait. A host that finds a record with no pin while the import that wrote it
// is still running must not open it — an open takes the epoch and fences the
// import — so it reads the record until the pin is there.
func TestAnImportInFlightIsWaitedForRatherThanTakenOver(t *testing.T) {
	h := newSizedHostHarness(t, 2)
	h.start(t)
	image := guestImage(0xa5)
	id := templateIDOf(image)

	// Host zero's import, held open between its record and its pin, which is
	// exactly the window a second host must wait out.
	started, err := h.hosts[0].Volumes().Create(t.Context(), id, templateVolumes)
	if err != nil {
		t.Fatal(err)
	}
	if err := started.Volume("root").Write(t.Context(), 0, image); err != nil {
		t.Fatal(err)
	}

	request := templateImport("alpine", 0xa5)
	request.Wait = 10 * time.Second
	waiting := make(chan *host.ImportedTemplate, 1)
	failed := make(chan error, 1)
	go func() {
		imported, err := h.hosts[1].TemplateOf(t.Context(), request)
		if err != nil {
			failed <- err
			return
		}
		waiting <- imported
	}()

	// Nothing fences host zero while it finishes: it checkpoints the image and
	// pins the point its VMs are forked at, under the epoch it drew.
	if err := started.Checkpoint(t.Context()); err != nil {
		t.Fatalf("the import in flight was fenced: %v", err)
	}
	point, err := started.ForkPoint(t.Context(), volume.Prepared(nil, nil))
	if err != nil {
		t.Fatalf("pinning the import in flight: %v", err)
	}
	if err := point.Retire(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := started.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	record, _ := templateRecord(t, h, 0, id)

	select {
	case err := <-failed:
		t.Fatalf("the host waiting on the import gave up: %v", err)
	case imported := <-waiting:
		if imported.ID() != id {
			t.Fatalf("the waiting host's template is %s, want %s", imported.ID(), id)
		}
		after, _ := templateRecord(t, h, 0, id)
		if after.Epoch != record.Epoch {
			t.Fatalf("the waiting host took the epoch: it was %d and is %d", record.Epoch, after.Epoch)
		}
		if got := forkReads(t, h, 1, "vm-a", imported); !bytes.Equal(got, image) {
			t.Fatalf("a VM created from the waited-for template reads %#x..., want the image", got[:8])
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the host waiting on the import never saw it published")
	}
}

// TestAnotherHostOpensATemplateByItsIdentity: a template imported on request on
// one host is the deployment's, not that host's. Any other host opens it by its
// identity alone — one read of its control record, no image and no import —
// and a VM created there reads the image. An identity nothing imported is
// refused, and so is one whose import has not published yet.
func TestAnotherHostOpensATemplateByItsIdentity(t *testing.T) {
	h := newHostHarness(t)
	h.start(t)
	imported := templateOn(t, h, 0, templateImport("builder-output", 0x42))

	opened, err := h.hosts[1].Template(t.Context(), imported.ID())
	if err != nil {
		t.Fatalf("host-1 opening %s by its identity: %v", imported.ID(), err)
	}
	if opened.ID() != imported.ID() || opened.Point.Parent() != imported.Point.Parent() {
		t.Fatalf("host-1 opened %s at %v, want %s at %v", opened.ID(), opened.Point.Parent(),
			imported.ID(), imported.Point.Parent())
	}
	if got := forkReads(t, h, 1, "vm-1", opened); !bytes.Equal(got, guestImage(0x42)) {
		t.Fatalf("a VM created on host-1 reads %x..., want the imported image", got[:4])
	}

	missing := templateIDOf(guestImage(0x43))
	if _, err := h.hosts[1].Template(t.Context(), missing); !errors.Is(err, host.ErrUnknownTemplate) {
		t.Fatalf("opening a template nothing imported gave %v, want ErrUnknownTemplate", err)
	}
	pending := templateIDOf(guestImage(0x44))
	vm, err := h.hosts[0].Volumes().Create(t.Context(), pending, templateVolumes)
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close(t.Context())
	if _, err := h.hosts[1].Template(t.Context(), pending); !errors.Is(err, host.ErrTemplatePending) {
		t.Fatalf("opening a template whose import has not published gave %v, want ErrTemplatePending", err)
	}
	if _, err := h.hosts[1].Template(t.Context(), "vm-not-a-template"); !errors.Is(err, host.ErrRequest) {
		t.Fatalf("opening an identity outside the template namespace gave %v, want ErrRequest", err)
	}
}

// TestATenantsTemplateIsItsOwn: a template imported for a tenant lives in that
// tenant's namespace, and only that tenant's VMs fork it. Another tenant that
// wants the same image imports it for itself.
func TestATenantsTemplateIsItsOwn(t *testing.T) {
	h := newHostHarness(t)
	h.start(t)
	request := templateImport("builder-output", 0x51)
	request.Tenant = "acme"
	imported := templateOn(t, h, 0, request)
	if want := "acme/" + templateIDOf(guestImage(0x51)); imported.ID() != want {
		t.Fatalf("the tenant's template is %s, want %s", imported.ID(), want)
	}
	opened, err := h.hosts[1].Template(t.Context(), imported.ID())
	if err != nil {
		t.Fatal(err)
	}
	if got := forkReads(t, h, 1, "acme/vm-1", opened); !bytes.Equal(got, guestImage(0x51)) {
		t.Fatalf("a VM of the tenant reads %x..., want the image", got[:4])
	}
	if _, err := h.hosts[1].Volumes().Fork(t.Context(), "zeta/vm-1", opened.Point); !errors.Is(err, volume.ErrOtherTenant) {
		t.Fatalf("another tenant forking the template gave %v, want ErrOtherTenant", err)
	}
	request = templateImport("builder-output", 0x51)
	request.Tenant = "Not A Tenant"
	if _, err := h.hosts[0].TemplateOf(t.Context(), request); !errors.Is(err, host.ErrRequest) {
		t.Fatalf("importing for an invalid tenant gave %v, want ErrRequest", err)
	}
}
