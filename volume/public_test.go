package volume_test

import (
	"bytes"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/volume"
)

// A template of no tenant is public: a VM of every tenant forks it, reads its
// image and writes under its own tenant. The template's objects lie outside
// every tenant's namespace, so no tenant is billed for them and deleting a
// tenant leaves them for the rest.
func TestEveryTenantForksAPublicTemplate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())

		template, _ := createVM(t, manager, "template-image")
		if err := template.Volume("root").Write(t.Context(), 0, []byte("base")); err != nil {
			t.Fatal(err)
		}
		if err := template.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		published := template.Status().Checkpoint
		if err := template.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		templateKeys := h.objectKeys(t)

		for _, id := range []string{"acme/vm-a", "zeta/vm-b"} {
			point, err := manager.InheritPublished(t.Context(), id, published)
			if err != nil {
				t.Fatalf("%s inheriting the public template: %v", id, err)
			}
			child, err := manager.Fork(t.Context(), id, point)
			if err != nil {
				t.Fatalf("forking the public template into %s: %v", id, err)
			}
			read := make([]byte, 4)
			if err := child.Volume("root").Read(t.Context(), 0, read); err != nil || !bytes.Equal(read, []byte("base")) {
				t.Fatalf("%s reads %q (%v), want the template's image", id, read, err)
			}
			if err := child.Volume("root").Write(t.Context(), 4096, []byte(id)); err != nil {
				t.Fatal(err)
			}
			if err := child.Checkpoint(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := child.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		for _, key := range added(templateKeys, h.objectKeys(t)) {
			if !strings.HasPrefix(key, "tenants/acme/") && !strings.HasPrefix(key, "tenants/zeta/") {
				t.Fatalf("a child wrote %q, outside its tenant's namespace", key)
			}
		}
		for _, key := range templateKeys {
			if strings.HasPrefix(key, "tenants/") {
				t.Fatalf("the public template holds %q, inside a tenant's namespace", key)
			}
		}

		acme, err := volume.StoredBytes(t.Context(), h.objects, h.prefix, "acme")
		if err != nil {
			t.Fatal(err)
		}
		public, err := volume.StoredBytes(t.Context(), h.objects, h.prefix, "")
		if err != nil {
			t.Fatal(err)
		}
		if vms := slices.Sorted(maps.Keys(acme)); !slices.Equal(vms, []string{"acme/vm-a"}) || public["template-image"] == 0 {
			t.Fatalf("acme is billed for %v and the deployment for the template %d bytes, want acme/vm-a alone and some",
				vms, public["template-image"])
		}

		acmePrefix, err := platform.NewObjectPrefix(h.prefix.String() + "tenants/acme/")
		if err != nil {
			t.Fatal(err)
		}
		if err := platform.ListAll(t.Context(), h.objects, acmePrefix, func(object platform.ObjectMetadata) error {
			return h.objects.Delete(t.Context(), platform.DeleteRequest{Key: object.Key})
		}); err != nil {
			t.Fatal(err)
		}
		for _, key := range templateKeys {
			if !slices.Contains(h.objectKeys(t), key) {
				t.Fatalf("deleting acme took the public template's %q", key)
			}
		}
		zeta, err := manager.Open(t.Context(), "zeta/vm-b")
		if err != nil {
			t.Fatal(err)
		}
		defer zeta.Close(t.Context())
		read := make([]byte, 4)
		if err := zeta.Volume("root").Read(t.Context(), 0, read); err != nil || !bytes.Equal(read, []byte("base")) {
			t.Fatalf("zeta reads %q (%v) once acme is gone, want the template's image", read, err)
		}
		if err := volume.CheckDeployment(t.Context(), h.objects, h.prefix); err != nil {
			t.Fatalf("the deployment is inconsistent once one tenant is gone: %v", err)
		}
	})
}

// Only a template of no tenant is public. A VM of no tenant that is not a
// template, and a tenant's own template, stay that tenant's. And no fork is
// ever named in the template namespace, which only an import writes.
func TestOnlyATemplateOfNoTenantIsPublic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager := h.manager(t, h.config())
		defer manager.Close(t.Context())

		points := map[string]*volume.ForkPoint{}
		for _, id := range []string{"vm-plain", "acme/template-image", "template-image"} {
			vm, _ := createVM(t, manager, id)
			if err := vm.Checkpoint(t.Context()); err != nil {
				t.Fatal(err)
			}
			point, err := manager.Inherit(t.Context(), vm.Status().Checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			points[id] = point
			if err := vm.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		before := h.objectKeys(t)
		for _, refused := range []struct{ child, parent string }{
			{"acme/child", "vm-plain"},
			{"zeta/child", "acme/template-image"},
		} {
			if _, err := manager.Fork(t.Context(), refused.child, points[refused.parent]); !errors.Is(err, volume.ErrOtherTenant) {
				t.Fatalf("forking %s into %s gave %v, want ErrOtherTenant", refused.parent, refused.child, err)
			}
		}
		for _, child := range []string{"template-copy", "acme/template-copy"} {
			if _, err := manager.Fork(t.Context(), child, points["template-image"]); !errors.Is(err, volume.ErrInvalidConfig) {
				t.Fatalf("forking into %s gave %v, want ErrInvalidConfig", child, err)
			}
		}
		if after := added(before, h.objectKeys(t)); len(after) != 0 {
			t.Fatalf("the refused forks wrote %v", after)
		}
	})
}
