package stripe_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/stripe"
)

// table is the codes a deployment runs, from one host to six or more.
var table = []rank.Code{{K: 1, M: 0}, {K: 1, M: 1}, {K: 2, M: 1}, {K: 2, M: 2}, {K: 4, M: 2}}

// lengths are envelope lengths at the edges of a code: none, one byte, fewer
// bytes than stripes, lengths that do and do not divide by k, and a page.
var lengths = []int{0, 1, 2, 3, 5, 47, 4093, 4096, 4097}

// simulated is a context whose runtime enables the guards SPROUTFS_SIM_BUG
// names, so a guard's test runs the guard.
func simulated(t *testing.T) context.Context {
	return sim.WithRuntime(t.Context(), sim.New(sim.Config{}))
}

// envelopeOf is length bytes no other length shares.
func envelopeOf(length int) []byte {
	envelope := make([]byte, length)
	for at := range envelope {
		envelope[at] = byte(at*7 + length*13 + 1)
	}
	return envelope
}

// checkOf is the check an envelope's SHA-256 makes: it passes the envelope
// and nothing else.
func checkOf(envelope []byte) func([]byte) error {
	want := sha256.Sum256(envelope)
	return func(rebuilt []byte) error {
		if sha256.Sum256(rebuilt) != want {
			return errors.New("the envelope fails its SHA-256")
		}
		return nil
	}
}

// combinations is every set of size of 0 to n-1, in order.
func combinations(n, size int) [][]int {
	var all [][]int
	var walk func(from int, chosen []int)
	walk = func(from int, chosen []int) {
		if len(chosen) == size {
			all = append(all, slices.Clone(chosen))
			return
		}
		for at := from; at < n; at++ {
			walk(at+1, append(chosen, at))
		}
	}
	walk(0, nil)
	return all
}

// pick is the stripes at indices, in the order given.
func pick(stripes []stripe.Stripe, indices []int) []stripe.Stripe {
	picked := make([]stripe.Stripe, len(indices))
	for at, index := range indices {
		picked[at] = stripes[index]
	}
	return picked
}

// positions is 0 to n-1.
func positions(n int) []int {
	all := make([]int, n)
	for at := range all {
		all[at] = at
	}
	return all
}

func mustSplit(t *testing.T, code rank.Code, envelope []byte) []stripe.Stripe {
	t.Helper()
	stripes, err := stripe.Split(code, envelope)
	if err != nil {
		t.Fatalf("splitting %d bytes under %s: %v", len(envelope), code, err)
	}
	return stripes
}

// An envelope splits into k+m stripes of one length, each naming its code,
// its index and the envelope's length, and the first k are the envelope
// itself followed by zeros: the code is systematic. Under k = 1 every stripe
// is the envelope whole.
func TestAnEnvelopeSplitsIntoStripesThatNameThemselves(t *testing.T) {
	for _, code := range table {
		for _, length := range lengths {
			envelope := envelopeOf(length)
			stripes := mustSplit(t, code, envelope)
			if len(stripes) != code.Width() {
				t.Fatalf("%s cut %d bytes into %d stripes", code, length, len(stripes))
			}
			size := (length + code.K - 1) / code.K
			var data []byte
			for index, s := range stripes {
				if s.Code != code || s.Index != index || s.Length != length || len(s.Bytes) != size {
					t.Fatalf("%s: stripe %d of %d bytes says %v", code, index, length, s)
				}
				if index < code.K {
					data = append(data, s.Bytes...)
				}
				if code.K == 1 && !bytes.Equal(s.Bytes, envelope) {
					t.Fatalf("%s: stripe %d of %d bytes is not the envelope whole", code, index, length)
				}
			}
			if !bytes.Equal(data[:length], envelope) || slices.ContainsFunc(data[length:], func(b byte) bool { return b != 0 }) {
				t.Fatalf("%s: the data stripes of %d bytes are not the envelope and zeros", code, length)
			}
		}
	}
}

// Every envelope rebuilds from every set of k of its k+m stripes, given in
// any order, for every code a deployment runs and lengths down to none and
// one byte. The k given are the k used, and none is wrong.
func TestEveryEnvelopeRebuildsFromEveryKOfItsStripes(t *testing.T) {
	ctx := simulated(t)
	for _, code := range table {
		for _, length := range lengths {
			envelope := envelopeOf(length)
			stripes := mustSplit(t, code, envelope)
			for _, indices := range combinations(code.Width(), code.K) {
				// The stripes arrive last index first.
				slices.Reverse(indices)
				for _, check := range []func([]byte) error{nil, checkOf(envelope)} {
					joined, err := stripe.Join(ctx, code, pick(stripes, indices), check)
					if err != nil || !bytes.Equal(joined.Envelope, envelope) ||
						!slices.Equal(joined.Used, positions(code.K)) || len(joined.Wrong) != 0 {
						t.Fatalf("%s: %d bytes from indices %v rebuilt %d bytes, used %v, wrong %v: %v",
							code, length, indices, len(joined.Envelope), joined.Used, joined.Wrong, err)
					}
				}
			}
		}
	}
}

// allocated is the bytes the heap gave out, on average, over runs calls of f.
func allocated(runs int, f func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		f()
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / uint64(runs)
}

// A rebuilt envelope is one buffer of its own, the length of k stripes, into
// which each data stripe in hand is copied once and a missing one is rebuilt
// in its place: from the data stripes and from two of them and two parity
// stripes, a 2 MiB envelope takes one buffer of 2 MiB and a few KiB beside
// it, the heap's rounding and the decoder's own, far less than a stripe. Changing
// the stripes after leaves it alone. Under k = 1 it is the stripe given, and
// nothing is allocated for it.
func TestARebuiltEnvelopeIsABufferOfItsOwn(t *testing.T) {
	ctx := t.Context()
	const length = 2<<20 + 48
	for _, c := range []struct {
		code    rank.Code
		indices []int
	}{
		{rank.Code{K: 4, M: 2}, []int{0, 1, 2, 3}},
		{rank.Code{K: 4, M: 2}, []int{0, 4, 2, 5}},
		{rank.Code{K: 2, M: 2}, []int{3, 1}},
		{rank.Code{K: 1, M: 1}, []int{1}},
	} {
		t.Run(fmt.Sprintf("%s/%v", c.code, c.indices), func(t *testing.T) {
			envelope := envelopeOf(length)
			given := pick(mustSplit(t, c.code, envelope), c.indices)
			for at := range given {
				given[at].Bytes = slices.Clone(given[at].Bytes)
			}
			var joined stripe.Joined
			var err error
			took := allocated(8, func() { joined, err = stripe.Join(ctx, c.code, given, nil) })
			if err != nil || !bytes.Equal(joined.Envelope, envelope) {
				t.Fatalf("rebuilt %d bytes: %v", len(joined.Envelope), err)
			}
			buffer := uint64(0)
			if c.code.K > 1 {
				buffer = uint64(c.code.K * stripe.Size(c.code, length))
			}
			if took < buffer || took > buffer+32<<10 {
				t.Fatalf("a rebuild allocated %d bytes, want one buffer of %d and at most 32 KiB beside it", took, buffer)
			}
			shares := &joined.Envelope[0] == &given[0].Bytes[0]
			if shares != (c.code.K == 1) {
				t.Fatalf("the envelope shares the first stripe's bytes: %v, want %v", shares, c.code.K == 1)
			}
			if c.code.K == 1 {
				return
			}
			for _, s := range given {
				clear(s.Bytes)
			}
			if !bytes.Equal(joined.Envelope, envelope) {
				t.Fatal("the envelope changed with the stripes it was rebuilt from")
			}
		})
	}
}

// Fewer than k distinct indices rebuild nothing, however many stripes they
// are: a stripe given twice counts once.
func TestFewerThanKIndicesAreAMiss(t *testing.T) {
	ctx := simulated(t)
	for _, code := range table {
		envelope := envelopeOf(4097)
		stripes := mustSplit(t, code, envelope)
		given := slices.Repeat(stripes[:code.K-1], 2)
		joined, err := stripe.Join(ctx, code, given, checkOf(envelope))
		if !errors.Is(err, stripe.ErrTooFew) || joined.Envelope != nil || len(joined.Wrong) != 0 {
			t.Fatalf("%s: %d stripes of %d indices rebuilt %d bytes, wrong %v: %v", code, len(given), code.K-1,
				len(joined.Envelope), joined.Wrong, err)
		}
	}
}

// One wrong stripe among k+1 is found: wherever it falls among the first k a
// reader takes, the reader rebuilds the envelope from the k that are right,
// and names the wrong one, so its holder can be told to drop it. A wrong
// stripe the reader did not need is not used. Every code with a parity
// stripe, every set of k+1 indices, every position of the wrong one, and
// lengths down to one byte.
func TestOneWrongStripeAmongKPlusOneIsFound(t *testing.T) {
	ctx := simulated(t)
	for _, code := range table {
		if code.M == 0 {
			continue
		}
		for _, length := range lengths[1:] {
			envelope := envelopeOf(length)
			for _, indices := range combinations(code.Width(), code.K+1) {
				for bad := range indices {
					stripes := mustSplit(t, code, envelope)
					given := pick(stripes, indices)
					damaged := slices.Clone(given[bad].Bytes)
					damaged[len(damaged)/2] ^= 0x5a
					given[bad].Bytes = damaged
					joined, err := stripe.Join(ctx, code, given, checkOf(envelope))
					if err != nil || !bytes.Equal(joined.Envelope, envelope) {
						t.Fatalf("%s: %d bytes from indices %v with %d wrong rebuilt %d bytes: %v", code, length,
							indices, indices[bad], len(joined.Envelope), err)
					}
					wantWrong, wantUsed := []int{bad}, slices.DeleteFunc(positions(code.K+1), func(at int) bool { return at == bad })
					if bad == code.K {
						// The extra stripe was never needed.
						wantWrong, wantUsed = nil, positions(code.K)
					}
					if !slices.Equal(joined.Wrong, wantWrong) || !slices.Equal(joined.Used, wantUsed) {
						t.Fatalf("%s: %d bytes from indices %v with %d wrong used %v and found %v wrong, want %v and %v",
							code, length, indices, indices[bad], joined.Used, joined.Wrong, wantUsed, wantWrong)
					}
				}
			}
		}
	}
}

// With exactly k stripes and one of them wrong, nothing rebuilds. Which one
// is wrong cannot be told, except under k = 1, where each stripe is a whole
// copy checked alone.
func TestOneWrongStripeAmongKIsAMiss(t *testing.T) {
	ctx := simulated(t)
	for _, code := range table {
		envelope := envelopeOf(4097)
		given := pick(mustSplit(t, code, envelope), positions(code.K))
		damaged := slices.Clone(given[0].Bytes)
		damaged[0] ^= 1
		given[0].Bytes = damaged
		joined, err := stripe.Join(ctx, code, given, checkOf(envelope))
		var wantWrong []int
		if code.K == 1 {
			wantWrong = []int{0}
		}
		if !errors.Is(err, stripe.ErrWrong) || joined.Envelope != nil || !slices.Equal(joined.Wrong, wantWrong) {
			t.Fatalf("%s: k stripes with one wrong rebuilt %d bytes and found %v wrong: %v", code,
				len(joined.Envelope), joined.Wrong, err)
		}
	}
}

// A stripe of another code is never used, even where its length fits: a
// stripe of 2+1 and one of 2+2 of one envelope are the same length, and
// their data stripes are even the same bytes. Given k-1 stripes of the code
// and every stripe of another, a reader has too few. Given the other code's
// stripes first, it rebuilds from the code's own alone.
func TestAStripeOfAnotherCodeIsNeverMixedIn(t *testing.T) {
	ctx := simulated(t)
	for _, code := range table {
		for _, other := range table {
			if other == code {
				continue
			}
			envelope := envelopeOf(4096)
			own, foreign := mustSplit(t, code, envelope), mustSplit(t, other, envelope)
			given := slices.Concat(foreign, own[:code.K-1])
			joined, err := stripe.Join(ctx, code, given, nil)
			if !errors.Is(err, stripe.ErrTooFew) || joined.Envelope != nil || len(joined.Wrong) != 0 {
				t.Fatalf("%s with every stripe of %s and %d of its own rebuilt %d bytes, used %v, wrong %v: %v",
					code, other, code.K-1, len(joined.Envelope), joined.Used, joined.Wrong, err)
			}
			given = slices.Concat(foreign, own[code.M:])
			joined, err = stripe.Join(ctx, code, given, checkOf(envelope))
			if err != nil || !bytes.Equal(joined.Envelope, envelope) ||
				!slices.Equal(joined.Used, positions(code.K + len(foreign))[len(foreign):]) || len(joined.Wrong) != 0 {
				t.Fatalf("%s after every stripe of %s rebuilt %d bytes from %v, wrong %v: %v", code, other,
					len(joined.Envelope), joined.Used, joined.Wrong, err)
			}
		}
	}
}

// A stripe shaped as no stripe of the code is wrong without a check: an
// index past the code, a length that is not a stripe of the envelope it
// names, or a negative envelope. The rest still rebuild.
func TestAMalformedStripeIsWrong(t *testing.T) {
	ctx := simulated(t)
	code := rank.Code{K: 2, M: 2}
	envelope := envelopeOf(4097)
	stripes := mustSplit(t, code, envelope)
	past, short, negative := stripes[0], stripes[1], stripes[2]
	past.Index = code.Width()
	short.Bytes = short.Bytes[1:]
	negative.Length = -1
	given := []stripe.Stripe{past, short, negative, stripes[3], stripes[0]}
	joined, err := stripe.Join(ctx, code, given, checkOf(envelope))
	if err != nil || !bytes.Equal(joined.Envelope, envelope) || !slices.Equal(joined.Used, []int{3, 4}) ||
		!slices.Equal(joined.Wrong, []int{0, 1, 2}) {
		t.Fatalf("rebuilt %d bytes from %v, wrong %v: %v", len(joined.Envelope), joined.Used, joined.Wrong, err)
	}
}

// A stripe that lies about its envelope's length is wrong, and so is one
// given under an index another stripe holds rightly: the reader tries the
// other and names the liar.
func TestAStripeThatLiesAboutItselfIsFound(t *testing.T) {
	ctx := simulated(t)
	code := rank.Code{K: 2, M: 2}
	envelope := envelopeOf(4095)
	stripes := mustSplit(t, code, envelope)
	// One more byte of envelope fits in the same stripe length.
	longer := stripes[0]
	longer.Length = 4096
	impostor := stripes[2]
	impostor.Index = 1
	given := []stripe.Stripe{longer, impostor, stripes[1], stripes[0], stripes[3]}
	joined, err := stripe.Join(ctx, code, given, checkOf(envelope))
	if err != nil || !bytes.Equal(joined.Envelope, envelope) || !slices.Equal(joined.Wrong, []int{0, 1}) {
		t.Fatalf("rebuilt %d bytes from %v, wrong %v: %v", len(joined.Envelope), joined.Used, joined.Wrong, err)
	}
}

// Rebuilt bytes past the envelope's end must be the zeros it was padded
// with, so a wrong stripe is caught without a check when it touches them.
func TestPaddingThatIsNotZerosIsWrong(t *testing.T) {
	ctx := simulated(t)
	code := rank.Code{K: 2, M: 1}
	envelope := envelopeOf(4095)
	stripes := mustSplit(t, code, envelope)
	last := slices.Clone(stripes[1].Bytes)
	last[len(last)-1] = 1
	stripes[1].Bytes = last
	joined, err := stripe.Join(ctx, code, stripes, nil)
	if err != nil || !bytes.Equal(joined.Envelope, envelope) || !slices.Equal(joined.Used, []int{0, 2}) ||
		!slices.Equal(joined.Wrong, []int{1}) {
		t.Fatalf("rebuilt %d bytes from %v, wrong %v: %v", len(joined.Envelope), joined.Used, joined.Wrong, err)
	}
}

// The search for k stripes that rebuild an envelope is bounded. Under 4+4,
// with five of eight stripes wrong, every set of four holds a wrong one, and
// the reader stops after 64 of the 70 sets.
func TestTheSearchForKStripesIsBounded(t *testing.T) {
	code := rank.Code{K: 4, M: 4}
	envelope := envelopeOf(4096)
	stripes := mustSplit(t, code, envelope)
	for index := range 5 {
		damaged := slices.Clone(stripes[index].Bytes)
		damaged[0] ^= 1
		stripes[index].Bytes = damaged
	}
	joined, err := stripe.Join(simulated(t), code, stripes, checkOf(envelope))
	if !errors.Is(err, stripe.ErrWrong) || err.Error() != "stripe: no k of the stripes rebuild the envelope: 64 sets of 4 tried under 4+4" ||
		joined.Envelope != nil || len(joined.Wrong) != 0 {
		t.Fatalf("five wrong stripes of eight rebuilt %d bytes, wrong %v: %v", len(joined.Envelope), joined.Wrong, err)
	}
}

// A stripe is valid when it is shaped as a stripe of its own code: every
// stripe Split cuts is, and one of no code, of an index past its code, of a
// negative envelope, or of bytes too long for its envelope, is not.
func TestAStripeIsValidForItsOwnCode(t *testing.T) {
	for _, code := range table {
		for _, length := range lengths {
			for _, s := range mustSplit(t, code, envelopeOf(length)) {
				if !s.Valid() {
					t.Fatalf("%v cut by Split is not valid", s)
				}
			}
		}
	}
	good := mustSplit(t, rank.Code{K: 2, M: 1}, envelopeOf(10))[2]
	for _, broken := range []func(s *stripe.Stripe){
		func(s *stripe.Stripe) { s.Code = rank.Code{K: 0, M: 3} },
		func(s *stripe.Stripe) { s.Index = 3 },
		func(s *stripe.Stripe) { s.Index = -1 },
		func(s *stripe.Stripe) { s.Length = -1 },
		func(s *stripe.Stripe) { s.Bytes = append(slices.Clone(s.Bytes), 0) },
	} {
		s := good
		broken(&s)
		if s.Valid() {
			t.Fatalf("%v is valid", s)
		}
	}
	empty := stripe.Stripe{Code: rank.Code{K: 4, M: 2}, Index: 5}
	if !empty.Valid() {
		t.Fatalf("%v of an empty envelope is not valid", empty)
	}
}

// A code that cannot store an envelope is refused, splitting and joining.
func TestAnInvalidCodeIsRefused(t *testing.T) {
	for _, code := range []rank.Code{{K: 0, M: 1}, {K: 2, M: -1}, {K: 30, M: 3}} {
		if _, err := stripe.Split(code, envelopeOf(10)); !errors.Is(err, rank.ErrInvalid) {
			t.Fatalf("splitting under %s: %v", code, err)
		}
		if _, err := stripe.Join(t.Context(), code, nil, nil); !errors.Is(err, rank.ErrInvalid) {
			t.Fatalf("joining under %s: %v", code, err)
		}
	}
}

// Size is a stripe's length: the envelope over k, rounded up.
func TestSizeIsTheEnvelopeOverK(t *testing.T) {
	for _, c := range []struct {
		code         rank.Code
		length, want int
	}{
		{rank.Code{K: 4, M: 2}, 0, 0}, {rank.Code{K: 4, M: 2}, 1, 1}, {rank.Code{K: 4, M: 2}, 4, 1},
		{rank.Code{K: 4, M: 2}, 5, 2}, {rank.Code{K: 1, M: 1}, 7, 7}, {rank.Code{K: 2, M: 1}, -3, 0},
	} {
		if got := stripe.Size(c.code, c.length); got != c.want {
			t.Fatalf("a stripe of %d bytes under %s is %d bytes, want %d", c.length, c.code, got, c.want)
		}
	}
}

// benchmarkLengths are the envelopes a 2 MiB page and a 4 KiB page make.
var benchmarkLengths = []int{2 << 20, 4 << 10}

// BenchmarkSplit cuts an envelope into the stripes of 4+2.
func BenchmarkSplit(b *testing.B) {
	code := rank.Code{K: 4, M: 2}
	for _, length := range benchmarkLengths {
		envelope := envelopeOf(length)
		b.Run(fmt.Sprintf("%s/%d", code, length), func(b *testing.B) {
			b.SetBytes(int64(length))
			for b.Loop() {
				if _, err := stripe.Split(code, envelope); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJoin rebuilds an envelope of 4+2 from its data stripes, which
// only copies them, and from two data and two parity stripes, which decodes.
func BenchmarkJoin(b *testing.B) {
	code := rank.Code{K: 4, M: 2}
	for _, length := range benchmarkLengths {
		stripes, err := stripe.Split(code, envelopeOf(length))
		if err != nil {
			b.Fatal(err)
		}
		for _, c := range []struct {
			name    string
			indices []int
		}{{"data", []int{0, 1, 2, 3}}, {"decode", []int{0, 2, 4, 5}}} {
			given := pick(stripes, c.indices)
			b.Run(fmt.Sprintf("%s/%d/%s", code, length, c.name), func(b *testing.B) {
				b.SetBytes(int64(length))
				for b.Loop() {
					if _, err := stripe.Join(b.Context(), code, given, nil); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
