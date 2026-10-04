package checkpoint

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"runtime"
	"testing"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform/sim"
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
// the replies holders of the stripes at indices of its envelope under code
// send, one stripe each.
type stripeReplies struct {
	code    rank.Code
	key     diskKey
	page    []byte
	replies []peer.StripesReply
}

func newStripeReplies(t testing.TB, code rank.Code, indices []int) stripeReplies {
	t.Helper()
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

// rebuilt is the read of one window that takes the replies and rebuilds the
// page from them, as a read of the cluster does once its replies have come.
// The caller finishes it, which gives the replies back.
func (s stripeReplies) rebuilt(ctx context.Context, t testing.TB, codecs *blob.Codecs) *windowRead {
	window := s.key.rankWindow()
	w := &windowRead{r: &clusterReader{ctx: ctx}, codecs: codecs, window: window, code: s.code,
		wants: []clusterWant{{key: s.key, maximum: PageSize2MiB, valid: validPage}},
		pages: []uint32{uint32(s.key.Page - window.Page(0))},
		held:  make([][]heldStripe, 1), tried: make([]int, 1), out: make([][]byte, 1),
		envelopes: make([][]byte, 1), answered: make(map[rank.Identity][][]int)}
	for at, reply := range s.replies {
		w.take(ctx, stripeAnswer{cache: rank.Cache{Identity: rank.Identity{byte(at + 1)}}, reply: reply})
	}
	w.join(ctx)
	if w.out[0] == nil {
		w.finish()
		t.Fatal("the stripes rebuilt no page")
	}
	return w
}

// readFromReplies is one read of the page from its replies into dst, through
// the page cache as a read of a run takes it.
func (s stripeReplies) readFromReplies(ctx context.Context, t testing.TB, cache *Cache, codecs *blob.Codecs,
	dst []byte) {
	run := []pageRead{{number: s.key.Page, dst: dst}}
	data, release, err := cache.getAll(ctx, []cacheKey{s.key.cacheKey},
		func(ctx context.Context, _ []int) (fetched, error) {
			w := s.rebuilt(ctx, t, codecs)
			defer w.finish()
			// A read of the cluster returns pages of its own, as fromCluster
			// takes them.
			return fetched{data: w.out, owned: []bool{true}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	fillPages(run, data)
	release()
}

// rebuildCases are the stripes a reader rebuilds a page from: its four data
// stripes, and two data and two parity stripes, under 4+2.
var rebuildCases = []struct {
	name    string
	indices []int
}{{"data", []int{0, 1, 2, 3}}, {"parity", []int{0, 1, 4, 5}}}

// A 2 MiB page read from the cluster is copied twice past the network: its
// data stripes into one buffer that is its envelope, and the page out of that
// buffer into the caller's. Nothing else the size of a page is made: the
// stripes are read where their replies hold them, the page is a view of its
// raw envelope, and the memory tier keeps that buffer as it is. So a read
// allocates one buffer of 2 MiB and some bookkeeping, from data stripes and
// with parity, against five buffers the size of the page before.
func TestAReadOfThePageFromTheClusterMakesOneBufferOfIt(t *testing.T) {
	for _, c := range rebuildCases {
		t.Run(c.name, func(t *testing.T) {
			s := newStripeReplies(t, rank.Code{K: 4, M: 2}, c.indices)
			cache := readerCache(t)
			dst := make([]byte, PageSize2MiB)
			const reads = 8
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for range reads {
				s.readFromReplies(t.Context(), t, cache, blob.Default(), dst)
			}
			runtime.ReadMemStats(&after)
			if !bytes.Equal(dst, s.page) {
				t.Fatal("the read filled its buffer with other bytes than the page")
			}
			envelope := uint64(PageSize2MiB + blob.HeaderSize)
			if took := (after.TotalAlloc - before.TotalAlloc) / reads; took < envelope || took > envelope+64<<10 {
				t.Fatalf("a read allocated %d bytes, want one envelope of %d and at most 64 KiB beside it",
					took, envelope)
			}
		})
	}
}

// What a read rebuilt from a peer's stripes is its own, never a view of a
// reply's buffer: that buffer goes back to the peer's pool when the read
// finishes, and the next reply is read into it. Under 1+1 the envelope is a
// stripe's own bytes, so the read keeps a copy; under 4+2 it is rebuilt into
// a buffer of its own. Once the read has finished, every reply's buffer is
// written over, and the page and its envelope are still the page's.
func TestAPageReadFromAPeerOutlivesItsReplysBuffer(t *testing.T) {
	for _, c := range []struct {
		code    rank.Code
		indices []int
	}{{rank.Code{K: 1, M: 1}, []int{1}}, {rank.Code{K: 4, M: 2}, []int{0, 1, 2, 3}},
		{rank.Code{K: 4, M: 2}, []int{0, 1, 4, 5}}} {
		t.Run(fmt.Sprintf("%s/%v", c.code, c.indices), func(t *testing.T) {
			ctx := sim.WithRuntime(t.Context(), sim.New(sim.Config{}))
			s := newStripeReplies(t, c.code, c.indices)
			w := s.rebuilt(ctx, t, blob.Default())
			w.finish()
			for _, reply := range s.replies {
				clear(reply.Payload)
			}
			if !bytes.Equal(w.out[0], s.page) {
				t.Fatal("the page changed once its replies' buffers were written over")
			}
			if page, err := blob.Decode(t.Context(), w.envelopes[0], PageSize2MiB); err != nil ||
				!bytes.Equal(page, s.page) {
				t.Fatalf("the envelope kept for a repair changed once its replies' buffers were written over: %v", err)
			}
		})
	}
}

// The memory tier keeps the bytes a fetch owns as they are and copies the
// rest: a page the cluster rebuilt into a buffer of its own is kept with no
// copy, and a page that is a slice of a buffer the store's request holds is
// copied, so the entry outlives that buffer and holds no more than the page.
func TestTheCacheKeepsTheBytesAFetchOwnsAndCopiesTheRest(t *testing.T) {
	cache := readerCache(t)
	owned := []byte("owned page")
	request := []byte("a request's buffer holding a page and more")
	shared := request[2:9]
	data, release, err := cache.getAll(t.Context(), []cacheKey{cacheKeyOf("owned"), cacheKeyOf("shared")},
		func(context.Context, []int) (fetched, error) {
			return fetched{data: [][]byte{owned, shared}, owned: []bool{true, false}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if &data[0][0] != &owned[0] {
		t.Fatal("the cache copied bytes the fetch owned")
	}
	if &data[1][0] == &shared[0] || string(data[1]) != "request" || cap(data[1]) != len(data[1]) {
		t.Fatalf("the cache kept %q in %d bytes of a buffer, want a copy of %q of its own", data[1], cap(data[1]),
			shared)
	}
}

// A page fills the caller's buffer from the byte the read starts at, and only
// what the page does not cover is zeroed: a member published when the volume
// was shorter reads as zeros past its end, whatever the buffer held.
func TestAPageFillsItsBufferAndZerosOnlyPastTheMember(t *testing.T) {
	dirty := func(n int) []byte { return bytes.Repeat([]byte{0xff}, n) }
	whole, short, past := dirty(8), dirty(8), dirty(4)
	fillPages([]pageRead{{within: 2, dst: whole}, {within: 0, dst: short}, {within: 6, dst: past}},
		[][]byte{[]byte("0123456789"), []byte("abc"), []byte("xyz")})
	if string(whole) != "23456789" || string(short) != "abc\x00\x00\x00\x00\x00" ||
		string(past) != "\x00\x00\x00\x00" {
		t.Fatalf("filled %q, %q and %q", whole, short, past)
	}
}

// BenchmarkAReaderRebuildsA2MiBPage is the reader's own work for one 2 MiB
// page read from the cluster, from its four data stripes and from two data
// and two parity stripes.
func BenchmarkAReaderRebuildsA2MiBPage(b *testing.B) {
	for _, c := range rebuildCases {
		b.Run(c.name, func(b *testing.B) {
			s := newStripeReplies(b, rank.Code{K: 4, M: 2}, c.indices)
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
