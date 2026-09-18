package volume_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/volume"
)

// A fork's root publication is the fork's own, and a failure of it costs the
// fork nothing it held: it keeps reading and writing here, its parent keeps
// its sealed pages, and a retry publishes the root the first attempt could
// not.
func TestForkSurvivesAFailedRootPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		fault := &faultStore{ObjectStore: h.objects}
		config := h.config()
		config.Store = h.imageStore(t, fault)
		manager := h.manager(t, config)
		defer manager.Close(t.Context())
		parent, want := createVM(t, manager, "parent")
		defer parent.Close(t.Context())
		if err := parent.Volume("root").Write(t.Context(), 0, []byte("parent bytes")); err != nil {
			t.Fatal(err)
		}
		copy(want["root"], []byte("parent bytes"))
		if err := parent.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := parent.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		fork, err := manager.Fork(t.Context(), "fork", point)
		if err != nil {
			t.Fatal(err)
		}
		defer fork.Close(t.Context())
		want.check(t, fork, "the fork before its root was published")

		root := fmt.Sprintf("vm/fork/ckpt/%d/index", counted(fork, 1))
		fault.block(func(key platform.ObjectKey) bool {
			return strings.HasSuffix(key.String(), root)
		})
		if err := fork.Volume("root").Write(t.Context(), 4096, []byte("fork bytes")); err != nil {
			t.Fatal(err)
		}
		forked := clone(want)
		copy(forked["root"][4096:], []byte("fork bytes"))
		if err := fork.Checkpoint(t.Context()); !errors.Is(err, platform.ErrInjectedFault) {
			t.Fatalf("the fork's root publication = %v, want the injected fault", err)
		}
		// Nothing was lost and nothing was released: the fork still reads every
		// byte, and the parent is still sealed for it.
		forked.check(t, fork, "the fork after its root publication failed")
		if status := fork.Status(); !status.Root {
			t.Fatalf("a fork whose root did not land reports %+v", status)
		}
		if status := parent.Status(); !status.Sealed {
			t.Fatalf("the parent gave its pages back to a fork that never published: %+v", status)
		}
		if _, err := manager.Open(t.Context(), "fork"); !errors.Is(err, volume.ErrForkPending) {
			t.Fatalf("opened a fork whose root never published: %v", err)
		}

		fault.block(nil)
		if err := fork.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		forked.check(t, fork, "the fork after the retry published its root")
		// The failed attempt burnt the sequence the record was created
		// selecting, so the root landed under the next one and the selection
		// moved the record to it.
		if status := fork.Status(); status.Root ||
			status.Checkpoint.Sequence != counted(fork, 2) {
			t.Fatalf("the fork after its root was retried = %+v", status)
		}
		if status := parent.Status(); status.Sealed {
			t.Fatalf("the parent is still sealed after its child published: %+v", status)
		}
		want.check(t, parent, "the parent after the fork published")
	})
}
