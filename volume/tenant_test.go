package volume_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/volume"
)

// Everything a tenant has lives under its own prefix: its VMs' control records
// and every object their checkpoints wrote, forks included. Deleting that
// prefix removes the tenant and nothing of any other, whose VMs still open,
// still read their bytes and still pass the deployment's consistency check.
func TestATenantsObjectsLiveUnderItsPrefixAndGoWithIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())

		acme, _ := createVM(t, manager, "acme/vm-a")
		if err := acme.Volume("root").Write(t.Context(), 0, []byte("acme")); err != nil {
			t.Fatal(err)
		}
		if err := acme.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := manager.Inherit(t.Context(), acme.Status().Checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		child, err := manager.Fork(t.Context(), "acme/child", point)
		if err != nil {
			t.Fatal(err)
		}
		if err := child.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		zeta, _ := createVM(t, manager, "zeta/vm-b")
		if err := zeta.Volume("root").Write(t.Context(), 0, []byte("zeta")); err != nil {
			t.Fatal(err)
		}
		if err := zeta.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, vm := range []*volume.VM{acme, child, zeta} {
			if err := vm.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
		}

		var acmeKeys, zetaKeys int
		for _, key := range h.objectKeys(t) {
			switch {
			case strings.HasPrefix(key, "tenants/acme/control/") || strings.HasPrefix(key, "tenants/acme/vm/"):
				acmeKeys++
			case strings.HasPrefix(key, "tenants/zeta/control/") || strings.HasPrefix(key, "tenants/zeta/vm/"):
				zetaKeys++
			default:
				t.Fatalf("%q lies outside every tenant's namespace", key)
			}
		}
		if acmeKeys == 0 || zetaKeys == 0 {
			t.Fatalf("acme holds %d keys and zeta %d, want both some", acmeKeys, zetaKeys)
		}

		acmePrefix, err := platform.NewObjectPrefix(h.prefix.String() + "tenants/acme/")
		if err != nil {
			t.Fatal(err)
		}
		deleted := 0
		for {
			page, err := h.objects.List(t.Context(), platform.ListRequest{Prefix: acmePrefix})
			if err != nil {
				t.Fatal(err)
			}
			for _, object := range page.Objects {
				if err := h.objects.Delete(t.Context(), platform.DeleteRequest{Key: object.Key}); err != nil {
					t.Fatal(err)
				}
				deleted++
			}
			if page.NextContinuationToken == "" {
				break
			}
		}
		if deleted != acmeKeys {
			t.Fatalf("deleting acme's prefix removed %d objects, want its %d", deleted, acmeKeys)
		}
		if _, err := manager.Open(t.Context(), "acme/vm-a"); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("opening a VM of the deleted tenant gave %v, want ErrNotFound", err)
		}
		reopened, err := manager.Open(t.Context(), "zeta/vm-b")
		if err != nil {
			t.Fatalf("the other tenant's VM no longer opens: %v", err)
		}
		defer reopened.Close(t.Context())
		read := make([]byte, 4)
		if err := reopened.Volume("root").Read(t.Context(), 0, read); err != nil || !bytes.Equal(read, []byte("zeta")) {
			t.Fatalf("the other tenant's VM reads %q (%v), want its own bytes", read, err)
		}
		if err := volume.CheckDeployment(t.Context(), h.objects, h.prefix); err != nil {
			t.Fatalf("the deployment is inconsistent once one tenant is gone: %v", err)
		}
	})
}

// No page crosses between tenants: a fork's child belongs to its parent's
// tenant, and one named in another tenant, or in none, is refused before
// anything is written.
func TestAForkNeverCrossesTenants(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())
		parent, _ := createVM(t, manager, "acme/vm-a")
		defer parent.Close(t.Context())
		if err := parent.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := manager.Inherit(t.Context(), parent.Status().Checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		before := h.objectKeys(t)
		for _, child := range []string{"zeta/child", "child"} {
			if _, err := manager.Fork(t.Context(), child, point); !errors.Is(err, volume.ErrOtherTenant) {
				t.Fatalf("forking acme/vm-a into %q gave %v, want ErrOtherTenant", child, err)
			}
		}
		if after := added(before, h.objectKeys(t)); len(after) != 0 {
			t.Fatalf("the refused forks wrote %v", after)
		}
		if _, err := manager.InheritPublished(t.Context(), "zeta/child", parent.Status().Checkpoint); !errors.Is(err, volume.ErrOtherTenant) {
			t.Fatalf("inheriting acme's checkpoint for a zeta VM gave %v, want ErrOtherTenant", err)
		}
	})
}
