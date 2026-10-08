package vmmemory_test

import (
	"bytes"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/vmmemory"
)

// A fork point's child whose client refuses to give up the page it maps from
// the point is terminal, and keeps the page: its end is its own. Its parent
// unseals, reads and stores as if the child were gone, and the page goes
// back once the child is closed. Before 2026-10-08 the child's refusal failed
// the parent's unseal.
func TestAChildWhoseMappingCannotBeTakenFailsNotItsParentsUnseal(t *testing.T) {
	vmmemory.SetCheckpointBatchPages(t, 2)
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 64, LogicalPages: 64, DirtyPages: 32,
			ReadAheadPages: 1})
		parent, err := forkParent(f.ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		b := f.newBacking(forkCampaignPages)
		b.source = parent.point
		for page, value := range parent.lent {
			b.data[page*f.pageSize] = value
		}
		r, m := f.attach(b)
		child := &forkGuest{name: "child", region: r, m: m, want: bytes.Clone(parent.lent)}
		for page := range uint64(forkCampaignPages) {
			if err := child.step(f.ctx, page, nil); err != nil {
				t.Fatal(err)
			}
		}
		m.refuseRevoke = true
		if err := parent.region.Unseal(f.ctx); err != nil {
			t.Fatalf("the parent's unseal failed of its child's refusal: %v", err)
		}
		if vmmemory.Failed(child.region) == nil {
			t.Fatal("the child whose revocation was refused is not terminal")
		}
		for page := range uint64(forkCampaignPages) {
			value := byte(0xe0 + page)
			if err := parent.step(f.ctx, page, &value); err != nil {
				t.Fatal(err)
			}
		}
		for page := range uint64(forkCampaignPages) {
			if err := parent.step(f.ctx, page, nil); err != nil {
				t.Fatal(err)
			}
		}
		for _, g := range []*forkGuest{child, &parent.forkGuest} {
			clear(g.m.pages)
			if err := g.region.Detach(f.ctx); err != nil {
				t.Fatalf("%s detaching: %v", g.name, err)
			}
		}
		if held := f.a.addresses(); held != 0 {
			t.Fatalf("the arena holds %d pages once both are detached, want none", held)
		}
	})
}
