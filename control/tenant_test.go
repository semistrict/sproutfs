package control_test

import (
	"testing"

	"github.com/semistrict/sproutfs/control"
)

// An identity is a name, or a tenant and a name. Everything the tenant has
// lives under tenants/<tenant>/, and a VM with none lives where every VM lived
// before tenants existed.
func TestAnIdentityNamesItsTenantAndItsNamespace(t *testing.T) {
	for _, test := range []struct {
		id, tenant, name, namespace, record string
	}{
		{"vm-01", "", "vm-01", "", "control/vm-01"},
		{"acme/vm-01", "acme", "vm-01", "tenants/acme/", "tenants/acme/control/vm-01"},
		{"acme/template-ab", "acme", "template-ab", "tenants/acme/", "tenants/acme/control/template-ab"},
	} {
		tenant, name := control.SplitID(test.id)
		if tenant != test.tenant || name != test.name {
			t.Fatalf("%q splits into %q and %q, want %q and %q", test.id, tenant, name, test.tenant, test.name)
		}
		if got := control.Namespace(test.id); got != test.namespace {
			t.Fatalf("%q lives under %q, want %q", test.id, got, test.namespace)
		}
		if got := control.RecordName(test.id); got != test.record {
			t.Fatalf("%q's record is at %q, want %q", test.id, got, test.record)
		}
		if got := control.InTenant(test.tenant, test.name); got != test.id {
			t.Fatalf("%q in %q is %q, want %q", test.name, test.tenant, got, test.id)
		}
		if !control.ValidID(test.id) {
			t.Fatalf("%q is refused", test.id)
		}
	}
	for _, refused := range []string{"", "acme/", "/vm", "a/b/c", "Acme/vm", "-acme/vm", "acme corp/vm",
		"acme/..", "acme/.", "tenant-name-that-is-longer-than-sixty-three-bytes-which-is-too-long/vm"} {
		if control.ValidID(refused) {
			t.Fatalf("%q is accepted", refused)
		}
	}
	for _, key := range []string{"control/vm-01", "vm/vm-01/ckpt/1/index"} {
		tenant, rest, ok := control.CutNamespace(key)
		if !ok || tenant != "" || rest != key {
			t.Fatalf("%q is cut into %q %q %v, want no tenant", key, tenant, rest, ok)
		}
	}
	tenant, rest, ok := control.CutNamespace("tenants/acme/vm/vm-01/ckpt/1/index")
	if !ok || tenant != "acme" || rest != "vm/vm-01/ckpt/1/index" {
		t.Fatalf("a tenant's key is cut into %q %q %v", tenant, rest, ok)
	}
	if _, _, ok := control.CutNamespace("tenants/Bad/control/vm"); ok {
		t.Fatal("a key under a tenant that is not valid is accepted")
	}
}
