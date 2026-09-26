package volume

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
)

// StoredBytes reports what one tenant's VMs hold in the object store: for every
// VM identity anything is stored under, the bytes of its control record and of
// every object of every checkpoint it published. The empty tenant is the VMs
// that belong to none. It is what an embedder bills each VM for.
//
// The number is exact, because it is what the store lists and not a count kept
// beside it: whatever a crash, a fence or an interrupted sweep left behind is
// in it, and whatever reclamation deleted is not. Every object is stored under
// the VM that published it, and every page is too:
//
//   - A fork reads its parent's pages where the parent published them. They
//     stay the parent's bytes, and a pin keeps them after the parent is
//     deleted. So a deleted VM that was ever forked goes on appearing here,
//     with no control record, until a collector frees what it pinned.
//   - Compaction rewrites a page only into a later checkpoint of the VM that
//     published it. The page is billed to that VM twice until the sweep behind
//     the next checkpoint deletes the old copy, and never to any other VM.
//
// It costs one listing of the tenant's control records and one of its
// checkpoint objects, and no GET or HEAD: a LIST request per page of keys,
// which is a thousand keys on the stores a deployment runs on. A VM has one
// record, and a checkpoint is its index object and a part per 64 MiB it wrote,
// so a tenant of a thousand VMs holding ten checkpoints each costs about
// twenty-one requests.
func StoredBytes(ctx context.Context, store platform.ObjectStore, prefix platform.ObjectPrefix, tenant string) (map[string]uint64, error) {
	if store == nil || tenant != "" && !control.ValidTenant(tenant) {
		return nil, ErrInvalidConfig
	}
	base := deploymentBase(prefix)
	stored := make(map[string]uint64)
	for _, within := range []string{control.RecordPrefix, checkpointNamespace} {
		listed, err := platform.NewObjectPrefix(base + control.TenantNamespace(tenant) + within)
		if err != nil {
			return nil, err
		}
		err = platform.ListAll(ctx, store, listed, func(object platform.ObjectMetadata) error {
			found, err := ownerOf(strings.TrimPrefix(object.Key.String(), base))
			if err != nil {
				return fmt.Errorf("%w: %s: %v", ErrCorrupt, object.Key, err)
			}
			stored[found.vm] += uint64(object.Size)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return stored, nil
}

// deploymentBase is the key prefix every object of one deployment starts with.
func deploymentBase(prefix platform.ObjectPrefix) string {
	base := prefix.String()
	if base != "" && !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base
}

// checkpointNamespace is where, within a tenant's namespace, every VM's
// checkpoint objects are: vm/<name>/ckpt/<sequence>/, which is every key the
// checkpoint store writes.
const checkpointNamespace = "vm/"

// indexObject is the name the checkpoint store gives the object holding a
// checkpoint's segments and its root, which is what says a checkpoint was
// published at all.
const indexObject = "index"

// owner is the VM one key of a deployment is stored under, which is the VM the
// bytes behind it are billed to.
type owner struct {
	vm string
	// record says the key is the VM's control record. below is the rest of any
	// other key, after vm/<name>/.
	record bool
	below  string
}

// ownerOf takes a key relative to the deployment's prefix apart into the VM it
// is stored under: control/<name> is a VM's record and vm/<name>/ holds
// everything it published, under tenants/<tenant>/ for a VM of a tenant. A
// key that is under no VM is the error that says why.
func ownerOf(key string) (owner, error) {
	tenant, rest, ok := control.CutNamespace(key)
	if !ok {
		return owner{}, errors.New("the tenant namespace holds an object of no valid tenant")
	}
	if name, isRecord := strings.CutPrefix(rest, control.RecordPrefix); isRecord {
		id := control.InTenant(tenant, name)
		if strings.Contains(name, "/") || !control.ValidID(id) {
			return owner{}, errors.New("the control namespace holds an object that is not a record")
		}
		return owner{vm: id, record: true}, nil
	}
	inside, isCheckpoint := strings.CutPrefix(rest, checkpointNamespace)
	name, below, found := strings.Cut(inside, "/")
	id := control.InTenant(tenant, name)
	if !isCheckpoint || !found || !control.ValidID(id) {
		return owner{}, errors.New("the key names no record and no checkpoint object")
	}
	return owner{vm: id, below: below}, nil
}

// checkpointKey takes apart what follows vm/<name>/ in a checkpoint object's
// key: "ckpt/<sequence>/index" or "ckpt/<sequence>/part/<n>". index says the
// key is the checkpoint's index object.
func checkpointKey(below string) (sequence uint64, index, ok bool) {
	tail, inside := strings.CutPrefix(below, "ckpt/")
	if !inside {
		return 0, false, false
	}
	number, tail, found := strings.Cut(tail, "/")
	if !found {
		return 0, false, false
	}
	sequence, err := strconv.ParseUint(number, 10, 64)
	if err != nil || sequence == 0 || strconv.FormatUint(sequence, 10) != number {
		return 0, false, false
	}
	if tail == indexObject {
		return sequence, true, true
	}
	part, inside := strings.CutPrefix(tail, "part/")
	if !inside {
		return 0, false, false
	}
	value, err := strconv.ParseUint(part, 10, 32)
	if err != nil || strconv.FormatUint(value, 10) != part {
		return 0, false, false
	}
	return sequence, false, true
}
