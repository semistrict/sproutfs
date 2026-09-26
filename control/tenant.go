package control

import "strings"

// A VM's identity is its name, or <tenant>/<name> for a VM that belongs to a
// tenant. The tenant is part of the identity, so it is part of every object key
// the VM has: its control record, its checkpoints and everything they hold live
// under tenants/<tenant>/ within the deployment's prefix. Deleting that prefix
// removes the tenant and nothing else, and the one bucket holds every tenant.
//
// A VM with no tenant lives where every VM lived before tenants existed, so a
// deployment that has none keeps its keys.
//
// No page crosses between tenants. A fork's child belongs to its parent's
// tenant, and a page's identity is the checkpoint that published it, which
// names the tenant, so a pager never shares a resident page between two
// tenants either.

// TenantPrefix is the namespace every tenant's objects live under.
const TenantPrefix = "tenants/"

// maximumTenant bounds a tenant's name, which is a path element of every key
// the tenant has.
const maximumTenant = 63

// SplitID reports a VM identity's tenant, empty for a VM with none, and its
// name within that tenant.
func SplitID(id string) (tenant, name string) {
	tenant, name, found := strings.Cut(id, "/")
	if !found {
		return "", id
	}
	return tenant, name
}

// TenantOf reports a VM identity's tenant, empty for a VM with none.
func TenantOf(id string) string {
	tenant, _ := SplitID(id)
	return tenant
}

// InTenant is the identity of the VM named name in tenant, or name itself for
// no tenant.
func InTenant(tenant, name string) string {
	if tenant == "" {
		return name
	}
	return tenant + "/" + name
}

// Namespace is the key prefix, within a deployment's own, that a VM's objects
// live under: tenants/<tenant>/ for a VM of a tenant, and nothing for a VM
// with none. The VM's name follows it.
func Namespace(id string) string { return TenantNamespace(TenantOf(id)) }

// TenantNamespace is the key prefix, within a deployment's own, that every
// object of one tenant's VMs lives under: tenants/<tenant>/, and nothing for
// the VMs of no tenant.
func TenantNamespace(tenant string) string {
	if tenant == "" {
		return ""
	}
	return TenantPrefix + tenant + "/"
}

// RecordName is where a VM's control record lives, relative to the
// deployment's prefix: control/<name>, under its tenant's namespace when it has
// one.
func RecordName(id string) string {
	_, name := SplitID(id)
	return Namespace(id) + RecordPrefix + name
}

// CutNamespace takes a key relative to the deployment's prefix apart into the
// tenant whose namespace it lies in, empty for none, and the rest of the key
// within that namespace. A key under the tenant namespace whose tenant is not a
// valid one reports false.
func CutNamespace(key string) (tenant, rest string, ok bool) {
	inside, isTenant := strings.CutPrefix(key, TenantPrefix)
	if !isTenant {
		return "", key, true
	}
	tenant, rest, found := strings.Cut(inside, "/")
	if !found || !ValidTenant(tenant) {
		return "", "", false
	}
	return tenant, rest, true
}

// ValidTenant reports whether a tenant's name can be a path element of every
// key the tenant has: lowercase letters, digits and dashes, starting with a
// letter or a digit, at most 63 bytes.
func ValidTenant(tenant string) bool {
	if tenant == "" || len(tenant) > maximumTenant || tenant[0] == '-' {
		return false
	}
	for index := 0; index < len(tenant); index++ {
		c := tenant[index]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// validName reports whether a VM's name, within its tenant, can be a path
// element of an object key.
func validName(name string) bool {
	if name == "" || len(name) > 256 || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsFunc(name, func(r rune) bool {
		return r == '/' || r < 0x20 || r == 0x7f
	})
}
