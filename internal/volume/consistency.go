package volume

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
)

// Allowance names one class of leftover object or dangling reference a check
// tolerates. Every one of them is something a host lost at a particular
// instant leaves behind and no writer ever comes back for: they are a
// collector's to reconcile, not a writer's, so a test that kills hosts says so
// by naming the class rather than by ignoring the check.
//
// Nothing else is excusable. A violation with no allowance is durable state
// disagreeing with itself, which no crash can produce.
type Allowance int

const (
	// AllowSupersededEpoch admits the checkpoints of a writer epoch below the
	// record's current one. Opening a VM advances the epoch and the new handle
	// reclaims only what it published itself: the checkpoint it opened on, and
	// everything a fenced predecessor left unreferenced, stay behind. Every
	// takeover — a restart, a migration, a fence — leaves one.
	AllowSupersededEpoch Allowance = iota + 1
	// AllowUnpublishedIndex admits a checkpoint whose parts are there and whose
	// index object is not: a publication interrupted before its commit. Nothing
	// names those parts and nothing ever will.
	AllowUnpublishedIndex
	// AllowUnreferencedCheckpoint admits a whole published checkpoint of the
	// record's own epoch that nothing selects and nothing pins. A VM's first
	// checkpoint is one: the handle that created it publishes that checkpoint
	// before it owns anything, so it has nothing to reclaim.
	AllowUnreferencedCheckpoint
	// AllowUnrecordedVM admits objects under a VM with no control record at
	// all: a create interrupted between its first checkpoint and the record, a
	// delete interrupted between the record and the objects, and — with no host
	// lost at all — the pinned lineage a finished delete leaves, which is what a
	// deleted VM that was ever forked always leaves behind.
	AllowUnrecordedVM
)

var allowanceNames = map[Allowance]string{
	AllowSupersededEpoch:        "superseded-epoch",
	AllowUnpublishedIndex:       "unpublished-index",
	AllowUnreferencedCheckpoint: "unreferenced-checkpoint",
	AllowUnrecordedVM:           "unrecorded-vm",
}

func (a Allowance) String() string {
	if name, known := allowanceNames[a]; known {
		return name
	}
	return "violation"
}

// Violation is one thing wrong with a deployment's durable objects: what is
// wrong, the object key it is about, and the class of host loss that could
// excuse it. A zero Class is a violation nothing excuses.
type Violation struct {
	Key   string
	Class Allowance
	Err   error
}

func (v Violation) Error() string {
	if v.Key == "" {
		return fmt.Sprintf("[%s] %v", v.Class, v.Err)
	}
	return fmt.Sprintf("[%s] %s: %v", v.Class, v.Key, v.Err)
}

func (v Violation) Unwrap() error { return v.Err }

// InconsistentError is what a deployment that disagrees with itself reports:
// every violation the check found, in object-key order.
type InconsistentError struct {
	Violations []Violation
}

func (e *InconsistentError) Error() string {
	lines := make([]string, 0, len(e.Violations)+1)
	lines = append(lines, fmt.Sprintf("volume: the deployment is inconsistent (%d violations)", len(e.Violations)))
	for _, violation := range e.Violations {
		lines = append(lines, "  "+violation.Error())
	}
	return strings.Join(lines, "\n")
}

// CheckDeployment lists one deployment's whole object namespace and reports
// every way its durable state disagrees with itself. It is the invariant a
// scenario asserts once it has quiesced: whatever faults it injected, what is
// left in the store must still be a deployment.
//
// It checks that every control record and every part parses at the format
// version this build writes; that every pinned sequence is a published
// checkpoint of the VM whose record pins it, since a pin is written on a
// checkpoint that is already selected and nothing ever deletes one; that every
// checkpoint a selected or pinned root names exists with the part count and the
// member bytes that root recorded; and that every object under a VM's
// checkpoint namespace is reached by some record's selected checkpoint, by a
// pinned one, or by the one checkpoint of grace a compaction's emptied
// checkpoint is spared for.
//
// A pin has nothing else to agree with. It names no holder and no descendant's
// record names it, because nothing releases one: what the check can say is that
// the lineage it protects — the pinned checkpoint and every checkpoint its root
// names — is whole, which is exactly what a grandchild reading through it
// needs.
//
// Each allow names a class of leftover a host lost at a particular instant
// leaves and no writer returns for; violations of that class are reported
// nowhere. Everything else is returned as an *InconsistentError.
//
// Every VM must be closed first. A publication in flight has written parts
// nothing names yet, which reads here as an interrupted one.
func CheckDeployment(ctx context.Context, store platform.ObjectStore, prefix platform.ObjectPrefix, allow ...Allowance) error {
	if store == nil {
		return ErrInvalidConfig
	}
	records, err := control.NewClient(control.Config{ObjectStore: store, ObjectPrefix: prefix})
	if err != nil {
		return err
	}
	checkpoints, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: store, ObjectPrefix: prefix})
	if err != nil {
		return err
	}
	base := prefix.String()
	if base != "" && !strings.HasSuffix(base, "/") {
		base += "/"
	}
	audit := &audit{store: store, records: records, checkpoints: checkpoints, base: base,
		allowed: make(map[Allowance]bool, len(allow)), reached: make(map[string]bool)}
	for _, class := range allow {
		audit.allowed[class] = true
	}
	if err := audit.run(ctx); err != nil {
		return err
	}
	if len(audit.violations) == 0 {
		return nil
	}
	return &InconsistentError{Violations: audit.violations}
}

// audit holds one run of the check: what the store listed, what the records
// said, and what the roots reached.
type audit struct {
	store       platform.ObjectStore
	records     *control.Client
	checkpoints *checkpoint.Store
	base        string
	allowed     map[Allowance]bool
	// reached is every object key some record's selected checkpoint, a pinned
	// one or a compaction's grace names.
	reached    map[string]bool
	violations []Violation
}

// object is one key the listing returned, taken apart into what it names.
type object struct {
	key string
	// vm and sequence name the checkpoint a checkpoint object belongs to, and
	// index says the key is that checkpoint's index object, which is the one
	// that says the publication finished.
	vm       string
	sequence uint64
	index    bool
}

func (a *audit) report(key string, class Allowance, err error) {
	if a.allowed[class] {
		return
	}
	a.violations = append(a.violations, Violation{Key: key, Class: class, Err: err})
}

func (a *audit) run(ctx context.Context) error {
	keys, err := a.list(ctx)
	if err != nil {
		return err
	}
	identities, checkpoints := a.classify(keys)
	held := make(map[string]control.Record, len(identities))
	for _, id := range identities {
		record, err := a.records.Read(ctx, id)
		if err != nil {
			a.report(a.base+control.RecordPrefix+id, 0,
				fmt.Errorf("the control record does not parse: %w", err))
			continue
		}
		held[id] = record
	}
	for _, id := range identities {
		record, known := held[id]
		if !known {
			continue
		}
		a.checkRecord(ctx, record)
	}
	a.checkReachability(checkpoints, held)
	return nil
}

// list reads the whole namespace, in ascending key order.
func (a *audit) list(ctx context.Context) ([]string, error) {
	prefix, err := platform.NewObjectPrefix(a.base)
	if err != nil {
		return nil, err
	}
	var keys []string
	token := ""
	for {
		page, err := a.store.List(ctx, platform.ListRequest{Prefix: prefix, ContinuationToken: token})
		if err != nil {
			return nil, err
		}
		for _, entry := range page.Objects {
			keys = append(keys, entry.Key.String())
		}
		if page.NextContinuationToken == "" {
			break
		}
		token = page.NextContinuationToken
	}
	slices.Sort(keys)
	return keys, nil
}

// classify takes every key apart into the control records and the checkpoint
// objects of the deployment, and reports a key that is neither. A key nothing
// in this deployment writes is a violation on its own: the check's whole
// premise is that it knows every object there is.
func (a *audit) classify(keys []string) (identities []string, checkpoints []object) {
	for _, key := range keys {
		rest, inside := strings.CutPrefix(key, a.base)
		if !inside {
			a.report(key, 0, errors.New("the object lies outside the deployment's prefix"))
			continue
		}
		if id, isRecord := strings.CutPrefix(rest, control.RecordPrefix); isRecord {
			if !control.ValidID(id) {
				a.report(key, 0, errors.New("the control namespace holds an object that is not a record"))
				continue
			}
			identities = append(identities, id)
			continue
		}
		found, ok := parseCheckpointKey(rest)
		if !ok {
			a.report(key, 0, errors.New("the key names no record and no checkpoint object"))
			continue
		}
		found.key = key
		checkpoints = append(checkpoints, found)
	}
	return identities, checkpoints
}

// parseCheckpointKey takes apart "vm/<id>/ckpt/<sequence>/index" and
// "vm/<id>/ckpt/<sequence>/part/<n>", which is every key the checkpoint store
// writes.
func parseCheckpointKey(rest string) (object, bool) {
	tail, inside := strings.CutPrefix(rest, "vm/")
	if !inside {
		return object{}, false
	}
	id, tail, found := strings.Cut(tail, "/ckpt/")
	if !found || !control.ValidID(id) {
		return object{}, false
	}
	number, tail, found := strings.Cut(tail, "/")
	if !found {
		return object{}, false
	}
	sequence, err := strconv.ParseUint(number, 10, 64)
	if err != nil || sequence == 0 || strconv.FormatUint(sequence, 10) != number {
		return object{}, false
	}
	if tail == indexObject {
		return object{vm: id, sequence: sequence, index: true}, true
	}
	part, inside := strings.CutPrefix(tail, "part/")
	if !inside {
		return object{}, false
	}
	value, err := strconv.ParseUint(part, 10, 32)
	if err != nil || strconv.FormatUint(value, 10) != part {
		return object{}, false
	}
	return object{vm: id, sequence: sequence}, true
}

// indexObject is the name the checkpoint store gives the object holding a
// checkpoint's segments and its root, which is what says a checkpoint was
// published at all.
const indexObject = "index"

// checkRecord opens every checkpoint one record keeps alive, which is what
// fills the reached set. A pin is what keeps a lineage this deployment cannot
// enumerate readable, so a pinned checkpoint that does not open is durable state
// disagreeing with itself: the pin was written on a checkpoint that was already
// published, and nothing after that deletes a pinned one.
func (a *audit) checkRecord(ctx context.Context, record control.Record) {
	key := a.base + control.RecordPrefix + record.VM
	if record.Created {
		a.reach(ctx, key, control.Ref{VM: record.VM, Sequence: record.Selected}, "the selected checkpoint")
	}
	for _, sequence := range record.Pinned {
		a.reach(ctx, key, control.Ref{VM: record.VM, Sequence: sequence}, "a pinned checkpoint")
	}
}

// reach opens one checkpoint a record keeps alive and adds everything it names
// to the reached set. A checkpoint a record says must be there and is not is
// durable state disagreeing with itself, whatever was lost.
func (a *audit) reach(ctx context.Context, key string, ref control.Ref, what string) {
	index, err := a.checkpoints.Open(ctx, ref)
	if err != nil {
		a.report(key, 0, fmt.Errorf("%s %s does not open: %w", what, ref, err))
		return
	}
	keys, violations := a.checkpoints.CheckIndex(ctx, index)
	for _, reached := range keys {
		a.reached[reached.String()] = true
	}
	for _, violation := range violations {
		a.report(violation.Key.String(), 0, violation.Err)
	}
}

// checkReachability reports every checkpoint object no record's selected root,
// no pinned root and no compaction's grace names, grouped into the class of
// host loss that could have left it.
func (a *audit) checkReachability(objects []object, held map[string]control.Record) {
	// A checkpoint is classified whole: its index object says whether a
	// publication finished, and its sequence says which epoch wrote it.
	published := make(map[control.Ref]bool)
	for _, found := range objects {
		if found.index {
			published[control.Ref{VM: found.vm, Sequence: found.sequence}] = true
		}
	}
	for _, found := range objects {
		if a.reached[found.key] {
			continue
		}
		ref := control.Ref{VM: found.vm, Sequence: found.sequence}
		record, exists := held[found.vm]
		switch {
		case !exists:
			a.report(found.key, AllowUnrecordedVM,
				errors.New("the VM this object belongs to has no control record"))
		case !published[ref]:
			a.report(found.key, AllowUnpublishedIndex,
				fmt.Errorf("%s published no index object: a publication that never finished", ref))
		case control.EpochOf(found.sequence) < record.Epoch:
			a.report(found.key, AllowSupersededEpoch,
				fmt.Errorf("%s belongs to epoch %d, superseded by epoch %d",
					ref, control.EpochOf(found.sequence), record.Epoch))
		default:
			a.report(found.key, AllowUnreferencedCheckpoint,
				fmt.Errorf("%s is neither selected nor pinned", ref))
		}
	}
}
