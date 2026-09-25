package main

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// TestListingControlRecordsFindsEveryVM is how the orchestrator discovers VMs
// without asking any host: the control records in the deployment's object
// namespace are the VMs, including the ones no host is running.
func TestListingControlRecordsFindsEveryVM(t *testing.T) {
	store := sim.New(sim.Config{}).ObjectStore()
	for _, key := range []string{
		"control/vm-a",
		"control/vm-b",
		// Not a control record: a checkpoint index of one of those VMs.
		"vm/vm-a/ckpt/1/index",
		// Not under the deployment's namespaces at all.
		"root.pb",
	} {
		put(t, store, key)
	}
	found, err := listedBy(t, store).List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(identifiers(found), []string{"vm-a", "vm-b"}) {
		t.Fatalf("listed %v, want the two VMs with control records", found)
	}
}

// countingStore reports what a listing costs: how many List calls it took and
// how many keys those calls had to return.
type countingStore struct {
	platform.ObjectStore
	calls, keys int
}

func (c *countingStore) List(ctx context.Context, request platform.ListRequest) (platform.ListResult, error) {
	result, err := c.ObjectStore.List(ctx, request)
	c.calls++
	c.keys += len(result.Objects)
	return result, err
}

// TestListingVMsScalesWithTheVMsNotTheirObjects: discovering the deployment's
// VMs is one listing of one key per VM. A VM's checkpoint objects are the bulk
// of the bucket and grow without bound while the VM runs, so a listing that
// walks them costs a request every object anyone ever wrote.
func TestListingVMsScalesWithTheVMsNotTheirObjects(t *testing.T) {
	// The setup writes hundreds of objects and this test is about what the
	// listing costs, not what a write costs, so writes are free here.
	store := sim.New(sim.Config{ObjectStore: sim.ObjectStoreConfig{PutLatency: time.Nanosecond}}).ObjectStore()
	counted := &countingStore{ObjectStore: store}
	client, err := control.NewClient(control.Config{ObjectStore: counted})
	if err != nil {
		t.Fatal(err)
	}
	vms := []string{"vm-a", "vm-b", "vm-c"}
	for _, vm := range vms {
		if _, err := client.Create(t.Context(), vm, control.Sequence(client.NewEpoch(), 1), true); err != nil {
			t.Fatal(err)
		}
		// What a running VM writes: a hundred checkpoint objects apiece, which
		// is a few minutes of one guest's intervals.
		for sequence := 1; sequence <= 5; sequence++ {
			for part := range 20 {
				put(t, store, fmt.Sprintf("vm/%s/ckpt/%d/part/%d", vm, sequence, part))
			}
		}
	}
	counted.calls, counted.keys = 0, 0
	found, err := listedBy(t, counted).List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(identifiers(found), vms) {
		t.Fatalf("listed %v, want %v", found, vms)
	}
	if counted.calls != 1 {
		t.Fatalf("listing %d VMs took %d List calls, want one", len(vms), counted.calls)
	}
	if counted.keys != len(vms) {
		t.Fatalf("listing %d VMs scanned %d keys, want one per VM", len(vms), counted.keys)
	}
}

// put writes an object whose body is its own key, which is all these listings
// care about.
func put(t *testing.T, store platform.ObjectStore, key string) {
	t.Helper()
	object, err := platform.NewObjectKey(key)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(key)
	if _, err := store.Put(t.Context(), platform.PutRequest{Key: object,
		Body: bytes.NewReader(body), Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
}

func TestAnEmptyNamespaceHasNoVMs(t *testing.T) {
	found, err := listedBy(t, sim.New(sim.Config{}).ObjectStore()).List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("listed %v", found)
	}
}

// listedBy is the bucket listing over one object store, with the control client
// it reads each record it finds with.
func listedBy(t *testing.T, store platform.ObjectStore) *bucketRecords {
	t.Helper()
	client, err := control.NewClient(control.Config{ObjectStore: store})
	if err != nil {
		t.Fatal(err)
	}
	return &bucketRecords{objects: store, control: client}
}

// identifiers is what a listing names, in the order it found them.
func identifiers(found []listing) []string {
	ids := make([]string, 0, len(found))
	for _, entry := range found {
		ids = append(ids, entry.ID)
	}
	return ids
}

// TestPodReadinessIsWhatPlacementTrusts: a pod is a candidate only while it is
// running, not being deleted, and saying it is ready. A terminating pod is
// still listed — that is what makes a drain observable — but nothing new is
// placed on it.
func TestPodReadinessIsWhatPlacementTrusts(t *testing.T) {
	ready := corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	if !podReady(ready) {
		t.Fatal("a running, ready pod was not a candidate")
	}
	terminating := ready
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	if podReady(terminating) {
		t.Fatal("a terminating pod was a candidate")
	}
	unready := corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}}}
	if podReady(unready) {
		t.Fatal("an unready pod was a candidate")
	}
	pending := corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}
	if podReady(pending) {
		t.Fatal("a pending pod was a candidate")
	}
}
