package vmmemory

import (
	"fmt"
	"runtime"
	"strings"
	"sync"

	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// The mapping audit keeps what the pager has installed for each page of a
// memory region, as the mapping commands left it, and checks the pager's
// decisions against it. A fault completes a page's trapped access by
// resolving it writable or read-only from what the region's bindings say,
// and resolving writable is valid only for a page mapped writable: a binding
// that became writable while its page stayed mapped read-only is a store the
// guest loses. The audit fails at the first resolve that disagrees with the
// mapping; where the region is let go with a page its binding says is
// writable mapped read-only, which is the same disagreement before any
// resolve meets it (agree); and where the region's own dirty state is bound
// to a root's page (auditBind). It names the commands that last
// changed the page, with their callers, and what the binding says.
//
// Production has none: auditMappings is off, and every hook is one nil
// check. The package's tests turn it on (export_test.go).
var auditMappings bool

// installed is what the guest's mapping of one page is.
type installed uint8

const (
	notInstalled installed = iota
	// installedReadOnly is a page mapped read-only, or a zero mapping: a
	// store into it traps, and only a new mapping makes it writable.
	installedReadOnly
	// installedWritable is a page mapped writable and not write-protected.
	installedWritable
	// installedProtected is a page mapped writable and write-protected: a
	// store traps, and a writable resolve takes the protection off.
	installedProtected
)

func (i installed) String() string {
	return [...]string{"not installed", "read-only", "writable", "write-protected"}[i]
}

// auditHistory is how many of a page's latest commands a finding names.
const auditHistory = 8

type mappingAudit struct {
	mu sync.Mutex
	// pages is every page a command has touched; one never touched is not
	// installed. Sparse, as a region's metadata is.
	pages map[uint64]*auditedPage
	// broken marks a region a mapping command failed ambiguously in: what it
	// installed is unknown, and the region is terminal, so nothing is
	// checked from there.
	broken bool
}

type auditedPage struct {
	state   installed
	history [auditHistory]auditEvent
	next    int
}

type auditEvent struct {
	what    string
	callers [12]uintptr
}

func newMappingAudit() *mappingAudit {
	if !auditMappings {
		return nil
	}
	return &mappingAudit{pages: make(map[uint64]*auditedPage)}
}

// installed records that a command left the pages [first, first+count) in
// state. A nil audit records nothing.
func (a *mappingAudit) installed(first uint64, count int, state installed, what string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for page := first; page < first+uint64(count); page++ {
		a.recordLocked(page, state, what)
	}
}

// refused names a refused command in the histories of [first,
// first+count), whose state it did not change.
func (a *mappingAudit) refused(first uint64, count int, what string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for page := first; page < first+uint64(count); page++ {
		a.recordLocked(page, a.stateLocked(page), what+", refused")
	}
}

// recordedMapped names, in the histories of [first, last), a change to what
// the bindings say is mapped, which changes nothing installed: a binding that
// says mapped where nothing is installed is what a resolve or a protect then
// fails on, and its history says who set it. Caller may hold bindingsMu.
func (r *MemoryRegion) recordedMapped(first, last uint64, mapped bool, what string) {
	a := r.audit
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for page := first; page < last; page++ {
		a.recordLocked(page, a.stateLocked(page), fmt.Sprintf("%s: binding mapped=%t", what, mapped))
	}
}

// madeOwn names, in a page's history, a transition that made it the
// region's own dirty state, which changes nothing it has installed.
func (r *MemoryRegion) madeOwn(page uint64, what string) {
	a := r.audit
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recordLocked(page, a.stateLocked(page), what)
}

// resolved checks a resolve of [first, first+count) against what is
// installed, and records a writable one taking a protection off.
func (a *mappingAudit) resolved(first uint64, count int, writable bool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.broken {
		return
	}
	for page := first; page < first+uint64(count); page++ {
		state := a.stateLocked(page)
		valid := state == installedWritable || state == installedProtected
		if !writable {
			valid = state == installedReadOnly || state == installedProtected
		}
		if !valid {
			panic(a.findingLocked(page, fmt.Sprintf("resolved writable=%t over a page %s", writable, state)))
		}
		if writable {
			a.recordLocked(page, installedWritable, "resolve writable")
		}
	}
}

// protecting checks a write-protection of [first, first+count) before it is
// issued: a page is protected only where it is installed, since the pager
// protects what its bindings say the guest maps.
func (a *mappingAudit) protecting(first uint64, count int) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.broken {
		return
	}
	for page := first; page < first+uint64(count); page++ {
		if a.stateLocked(page) == notInstalled {
			panic(a.findingLocked(page, "write-protected over a page not installed"))
		}
	}
}

// protected records a write-protection of [first, first+count): a page
// mapped writable is protected, and a read-only one stays read-only.
func (a *mappingAudit) protected(first uint64, count int) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for page := first; page < first+uint64(count); page++ {
		if a.stateLocked(page) == installedWritable {
			a.recordLocked(page, installedProtected, "protect")
		}
	}
}

// agree checks the pages [first, last) where the region is given up: a
// page its binding says is writable is never mapped read-only, because the
// fault that completes its next trapped access resolves it writable
// (loadBound), and a mapping can take that only where it was mapped
// writable. A transition may leave the two apart while it holds the page's
// lock, which that fault takes first: a give-back maps the page it puts the
// guest back on before it rebinds, and a store's rule makes a page of
// another window private before the store's command maps it. So a page whose
// lock something holds is passed over, and one whose lock is free is checked
// under it.
func (r *MemoryRegion) agree(first, last uint64) {
	a := r.audit
	if a == nil {
		return
	}
	a.mu.Lock()
	var readOnly []uint64
	if !a.broken {
		for page, p := range a.pages {
			if page >= first && page < last && p.state == installedReadOnly {
				readOnly = append(readOnly, page)
			}
		}
	}
	a.mu.Unlock()
	for _, page := range readOnly {
		if finding := r.disagreement(page); finding != "" {
			panic(finding)
		}
	}
}

// disagreement is agree's finding for one page mapped read-only, "" where its
// binding is not writable, or where something holds the page's lock.
func (r *MemoryRegion) disagreement(index uint64) string {
	h := r.host
	b := r.lookupBinding(index)
	if b == nil {
		return ""
	}
	h.mu.Lock()
	page := b.page
	h.mu.Unlock()
	if page == nil || !frameOf(page).mu.TryLock() {
		return ""
	}
	defer h.unlockPage(page)
	r.bindingsMu.Lock()
	writable := b.mapped && b.writable()
	r.bindingsMu.Unlock()
	h.mu.Lock()
	same := b.page == page
	h.mu.Unlock()
	if !writable || !same {
		return ""
	}
	described := r.describeBinding(index)
	a := r.audit
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.broken || a.stateLocked(index) != installedReadOnly {
		return ""
	}
	return a.findingLocked(index, "is mapped read-only while its binding is writable, as the region is let go: "+
		described)
}

// agreeRecorded fails where what the bindings say of a page of [first, last)
// is not what the commands installed: recorded mapped where nothing is, which
// a resolve or a protect then meets, or recorded unmapped where something is,
// which a revocation skips. The mapper calls it as it records a command's
// outcome, with the pages held, so the two cannot be apart there.
func (r *MemoryRegion) agreeRecorded(first, last uint64) {
	a := r.audit
	if a == nil {
		return
	}
	for page := first; page < last; page++ {
		r.bindingsMu.Lock()
		b, zeroRun := r.lookupLocked(page)
		recorded := zeroRun || b != nil && b.mapped
		r.bindingsMu.Unlock()
		a.mu.Lock()
		state := a.stateLocked(page)
		if !a.broken && (state != notInstalled) != recorded {
			finding := a.findingLocked(page, fmt.Sprintf("is recorded mapped=%t over a page %s", recorded, state))
			a.mu.Unlock()
			panic(finding)
		}
		a.mu.Unlock()
	}
}

// auditBind fails where page index, the region's own dirty state, is bound
// to a root's page: the guest would read the bytes it stored over.
func (r *MemoryRegion) auditBind(index uint64, dirty bool, p *zirconvm.VmPage) {
	if r.audit == nil || !dirty {
		return
	}
	h := r.host
	h.mu.Lock()
	root := frameOf(p).layer == nil
	h.mu.Unlock()
	if root {
		panic(fmt.Sprintf("vmmemory: mapping audit: page %d, the region's own dirty state, bound to a root's page: %s",
			index, r.describeBinding(index)))
	}
}

// fail marks the region's mapping unknown after an ambiguous failure.
func (a *mappingAudit) fail() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.broken = true
	a.mu.Unlock()
}

func (a *mappingAudit) stateLocked(page uint64) installed {
	if p := a.pages[page]; p != nil {
		return p.state
	}
	return notInstalled
}

func (a *mappingAudit) recordLocked(page uint64, state installed, what string) {
	p := a.pages[page]
	if p == nil {
		p = new(auditedPage)
		a.pages[page] = p
	}
	p.state = state
	event := &p.history[p.next%auditHistory]
	event.what = what
	clear(event.callers[:])
	runtime.Callers(4, event.callers[:])
	p.next++
}

func (a *mappingAudit) findingLocked(page uint64, what string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "vmmemory: mapping audit: page %d %s; its latest commands, newest first:", page, what)
	p := a.pages[page]
	if p == nil {
		return b.String()
	}
	for i := 1; i <= min(p.next, auditHistory); i++ {
		event := p.history[(p.next-i)%auditHistory]
		fmt.Fprintf(&b, "\n  %s", event.what)
		frames := runtime.CallersFrames(event.callers[:])
		for {
			frame, more := frames.Next()
			if frame.Function == "" {
				break
			}
			fmt.Fprintf(&b, "\n    %s %s:%d", frame.Function, frame.File, frame.Line)
			if !more {
				break
			}
		}
	}
	return b.String()
}

// describeBinding is what the region's binding of page says, for a finding.
func (r *MemoryRegion) describeBinding(page uint64) string {
	r.bindingsMu.Lock()
	b, zero := r.lookupLocked(page)
	if b == nil {
		r.bindingsMu.Unlock()
		return fmt.Sprintf("no binding (zero run %t)", zero)
	}
	described := fmt.Sprintf("binding dirty %t, checkpoint %t, protected %t, mapped %t", b.dirty, b.checkpoint != nil,
		b.protected, b.mapped)
	r.bindingsMu.Unlock()
	h := r.host
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case b.page == nil:
		return described + ", no page"
	case frameOf(b.page).layer == r:
		return described + fmt.Sprintf(", its own page at slot %d", frameOf(b.page).slot)
	case frameOf(b.page).layer == nil:
		return described + fmt.Sprintf(", a root's page at slot %d", frameOf(b.page).slot)
	}
	return described + ", another region's page"
}
