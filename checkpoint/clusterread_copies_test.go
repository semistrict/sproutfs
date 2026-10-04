package checkpoint

import (
	"context"
	"math/rand/v2"
	"testing"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/resource"
	"github.com/semistrict/sproutfs/stripe"
)

// A reader's own work for a page it read from the cluster, between the
// replies of its holders and its caller's buffer: each stripe parsed and
// checked, the envelope rebuilt and its SHA-256 checked, the page kept by the
// memory tier and copied into the caller's buffer. The network and the
// holders are not in it.

// stripeReplies is a 2 MiB page of noise, which is stored raw, its key, and
// the replies holders of the stripes at indices of its envelope under 4+2
// send, one stripe each.
type stripeReplies struct {
	code    rank.Code
	key     diskKey
	page    []byte
	replies []peer.StripesReply
}

func newStripeReplies(t testing.TB, indices []int) stripeReplies {
	t.Helper()
	code := rank.Code{K: 4, M: 2}
	geometry, err := GeometryFor(PageSize2MiB)
	if err != nil {
		t.Fatal(err)
	}
	identity := control.Identity{Ref: control.Ref{VM: "vm", Sequence: 2}, Volume: "root", Page: 3}
	key := pageDiskKey(identity, geometry)
	page := make([]byte, PageSize2MiB)
	noise := rand.NewChaCha8([32]byte{1})
	_, _ = noise.Read(page)
	envelope, err := blob.Encode(t.Context(), page)
	if err != nil {
		t.Fatal(err)
	}
	stripes, err := stripe.Split(code, envelope)
	if err != nil {
		t.Fatal(err)
	}
	window := key.rankWindow()
	got := stripeReplies{code: code, key: key, page: page}
	for _, index := range indices {
		item := encodeItem(key, stripes[index])
		got.replies = append(got.replies, peer.StripesReply{Payload: item, Items: []peer.StripeItem{{
			Page: uint32(key.Page - window.Page(0)), Index: index, Length: stripes[index].Length, Size: len(item)}}})
	}
	return got
}

// readerCache is a page cache whose memory tier keeps no page, so every read
// of a page fetches it, as a fault on a page no host's memory holds does.
func readerCache(t testing.TB) *Cache {
	t.Helper()
	budget, err := resource.New(4 << 10)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := NewCache(t.Context(), budget, CacheConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.Close)
	return cache
}

// readFromReplies is one read of the page from its replies into dst, as a
// read of the cluster does it once its replies have come.
func (s stripeReplies) readFromReplies(ctx context.Context, t testing.TB, cache *Cache, codecs *blob.Codecs,
	dst []byte) {
	window := s.key.rankWindow()
	reader := &clusterReader{ctx: ctx}
	run := []pageRead{{number: s.key.Page, dst: dst}}
	data, release, err := cache.getAll(ctx, []cacheKey{s.key.cacheKey},
		func(ctx context.Context, _ []int) ([][]byte, []envelope, error) {
			w := &windowRead{r: reader, codecs: codecs, window: window, code: s.code,
				wants: []clusterWant{{key: s.key, maximum: PageSize2MiB, valid: validPage}},
				pages: []uint32{uint32(s.key.Page - window.Page(0))},
				held:  make([][]heldStripe, 1), tried: make([]int, 1), out: make([][]byte, 1),
				envelopes: make([][]byte, 1), answered: make(map[rank.Identity][][]int)}
			defer w.finish()
			for at, reply := range s.replies {
				w.take(ctx, stripeAnswer{cache: rank.Cache{Identity: rank.Identity{byte(at + 1)}}, reply: reply})
			}
			w.join(ctx)
			if w.out[0] == nil {
				t.Fatal("the stripes rebuilt no page")
			}
			return w.out, nil, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	fillPages(run, data)
	release()
}

// BenchmarkAReaderRebuildsA2MiBPage is the reader's own work for one 2 MiB
// page read from the cluster, from its four data stripes and from two data
// and two parity stripes.
func BenchmarkAReaderRebuildsA2MiBPage(b *testing.B) {
	for _, c := range []struct {
		name    string
		indices []int
	}{{"data", []int{0, 1, 2, 3}}, {"parity", []int{0, 1, 4, 5}}} {
		b.Run(c.name, func(b *testing.B) {
			s := newStripeReplies(b, c.indices)
			cache := readerCache(b)
			codecs := blob.Default()
			dst := make([]byte, PageSize2MiB)
			b.SetBytes(PageSize2MiB)
			b.ReportAllocs()
			for b.Loop() {
				s.readFromReplies(b.Context(), b, cache, codecs, dst)
			}
		})
	}
}
