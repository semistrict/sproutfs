package real_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
)

// pattern is a guest's pages: each byte drawn from its page and offset.
type pattern struct{}

func (pattern) ReadPage(_ context.Context, _ string, page uint64, dst []byte) error {
	for at := range dst {
		dst[at] = byte(page*31 + uint64(at)*7 + uint64(at>>12))
	}
	return nil
}

// A hot tier reads and fills through each provider's adapter alike, with no
// call of either provider's own: the regional bucket and the hot bucket are
// two buckets of one emulator, Cloud Storage's or S3's, reached the way a
// deployment reaches them. A read misses the hot tier, is served by the
// regional bucket, and fills the hot tier behind it with a create-if-absent
// PUT of the same object; the next read is served by the hot tier alone.
func TestAHotTierReadsAndFillsThroughEveryProvidersAdapter(t *testing.T) {
	for _, provider := range []struct {
		name string
		open func(*testing.T) platform.ObjectStore
	}{{"gcs", newConditionalFakeGCS}, {"s3", newFakeS3}} {
		t.Run(provider.name, func(t *testing.T) {
			regional, hot := provider.open(t), provider.open(t)
			tier, err := checkpoint.NewHotTier(t.Context(), checkpoint.HotTierConfig{Store: hot, SkipPublications: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(tier.Close)
			store, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: regional, HotTier: tier})
			if err != nil {
				t.Fatal(err)
			}
			const size = 3 * checkpoint.PageSize2MiB
			root, err := store.Root(t.Context(), control.Ref{VM: "vm", Sequence: 1},
				map[string]checkpoint.VolumeSpec{"root": {Size: size, PageSize: checkpoint.PageSize2MiB}})
			if err != nil {
				t.Fatal(err)
			}
			publication := store.Begin(root, control.Ref{VM: "vm", Sequence: 2})
			for page := range uint64(3) {
				publication.Dirty("root", page)
			}
			if _, err := publication.Commit(t.Context(), pattern{}); err != nil {
				t.Fatal(err)
			}
			want := make([]byte, size)
			for page := range uint64(3) {
				_ = pattern{}.ReadPage(t.Context(), "root", page, want[page*checkpoint.PageSize2MiB:][:checkpoint.PageSize2MiB])
			}
			read := func() {
				t.Helper()
				index, err := store.Open(t.Context(), control.Ref{VM: "vm", Sequence: 2})
				if err != nil {
					t.Fatal(err)
				}
				if err := tier.Settle(t.Context()); err != nil {
					t.Fatal(err)
				}
				got := make([]byte, size)
				if err := store.Read(t.Context(), index, "root", 0, got); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Fatal("the guest read back as other bytes than were published")
				}
				if err := tier.Settle(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			read()
			if got := tier.Stats(); got.Hits != 1 || got.Misses != 2 || got.Sent != 2 || got.Present != 0 {
				t.Fatalf("a cold read did %+v to the hot tier, want the index object and the part missed and sent", got)
			}
			for _, name := range []string{"vm/vm/ckpt/2/index", "vm/vm/ckpt/2/part/0"} {
				key, err := platform.NewObjectKey(name)
				if err != nil {
					t.Fatal(err)
				}
				copied, _, err := platform.ReadObject(t.Context(), hot, key, 0, 1<<30, checkpoint.ErrCorrupt)
				if err != nil {
					t.Fatal(err)
				}
				original, _, err := platform.ReadObject(t.Context(), regional, key, 0, 1<<30, checkpoint.ErrCorrupt)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(copied, original) {
					t.Fatalf("the hot tier's %s is not the regional bucket's", name)
				}
			}
			read()
			if got := tier.Stats(); got.Hits != 4 || got.Misses != 2 {
				t.Fatalf("a warm read did %+v to the hot tier, want three more hits and no miss", got)
			}
		})
	}
}
