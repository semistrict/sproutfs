package rank

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
)

// cacheOf is a cache whose identity starts with n, of the given weight.
func cacheOf(n byte, weight uint32) Cache {
	return Cache{Identity: Identity{n, 0x5a, n ^ 0xff}, Weight: weight,
		Address: platform.Address(fmt.Sprintf("10.0.0.%d:8081", n))}
}

func listOf(t *testing.T, code Code, caches ...Cache) List {
	t.Helper()
	list, err := NewList(code, caches)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// The cluster cache is turned on for a share of windows by a hash of the
// window: none at 0, every one at 100, and about the share in between, within
// half a point over 100,000 windows. Raising the share only adds windows, so a
// rollout never moves a window back out of the cluster.
func TestTheClusterShareIsAShareOfWindows(t *testing.T) {
	const windows = 100000
	counts := make(map[int]int)
	percents := []int{-5, 0, 1, 25, 50, 99, 100, 150}
	for at := range uint64(windows) {
		window := windowOf("vm-share", at/97+1, at)
		previous := true
		for _, percent := range slices.Backward(percents) {
			in := window.InShare(percent)
			if in && !previous {
				t.Fatalf("window %d is in the share of %d percent and not in a larger one", at, percent)
			}
			previous = in
			if in {
				counts[percent]++
			}
		}
	}
	for _, percent := range percents {
		want := min(max(percent, 0), 100) * windows / 100
		if got := counts[percent]; math.Abs(float64(got-want)) > windows/200 {
			t.Fatalf("a share of %d percent holds %d of %d windows, want %d", percent, got, windows, want)
		}
	}
	if counts[0] != 0 || counts[-5] != 0 || counts[100] != windows || counts[150] != windows {
		t.Fatalf("shares of 0 and 100 percent hold %d and %d windows, want none and all", counts[0], counts[100])
	}
}

// windowOf is the window of one 2 MiB span of a VM's disk.
func windowOf(vm string, sequence, span uint64) Window {
	return Window{Ref: control.Ref{VM: vm, Sequence: sequence}, Volume: "disk", Number: span}
}

// identities are the identities of caches, in order.
func identities(caches []Cache) []byte {
	out := make([]byte, len(caches))
	for at, cache := range caches {
		out[at] = cache.Identity[0]
	}
	return out
}

// The logarithm a score is built from is integer arithmetic. It is exact at
// every power of two and within a few units of the 32nd fractional bit
// everywhere else, because every step truncates rather than rounds.
func TestLog2IsExactAtPowersOfTwoAndTruncatesBetween(t *testing.T) {
	for power := range uint64(64) {
		if got := log2(1 << power); got != power<<fractionBits {
			t.Fatalf("log2(2^%d) is %d, want %d", power, got, power<<fractionBits)
		}
	}
	random := rand.New(rand.NewPCG(7, 11))
	for range 10000 {
		m := random.Uint64()>>random.UintN(64) | 1
		want := math.Floor(math.Log2(float64(m)) * (1 << fractionBits))
		got := float64(log2(m))
		if got > want+1 || got < want-4 {
			t.Fatalf("log2(%d) is %v, want %v less at most four units", m, got, want)
		}
	}
	// The distance is never zero, so a weight over it is always a score.
	if got := distance(^uint64(0)); got != 1 {
		t.Fatalf("the distance of the largest hash is %d, want one unit", got)
	}
	if got := distance(0); got != 64<<fractionBits {
		t.Fatalf("the distance of hash zero is %d, want %d", got, uint64(64)<<fractionBits)
	}
}

// The ranks are the caches in the order of w / -ln(u), which a float
// reference computes the slow way. A different base of logarithm only scales
// every distance, so it does not change the order.
func TestRanksFollowWeightOverDistance(t *testing.T) {
	caches := []Cache{cacheOf(1, 1), cacheOf(2, 3), cacheOf(3, 2), cacheOf(4, 7), cacheOf(5, 1),
		cacheOf(6, 4), cacheOf(7, 2), cacheOf(8, 5)}
	list := listOf(t, Code{K: 8, M: 0}, caches...)
	for span := range uint64(5000) {
		window := windowOf("vm-1", 3, span)
		digest := window.digest()
		reference := slices.Clone(list.caches)
		score := func(cache Cache) float64 {
			u := float64(mix(digest^seedOf(cache.Identity))|1) / math.Exp2(64)
			return float64(cache.Weight) / -math.Log(u)
		}
		slices.SortFunc(reference, func(a, b Cache) int {
			if score(a) > score(b) {
				return -1
			}
			return 1
		})
		if got := list.Ranks(window); !slices.Equal(identities(got), identities(reference)) {
			t.Fatalf("span %d ranks %v, want %v", span, identities(got), identities(reference))
		}
	}
}

// A weight is a share of windows: a cache of weight w ranks first for about
// w / Σw of them. Four caches of weights one to four rank first for a tenth,
// a fifth, three tenths and two fifths of 100,000 windows, each within half a
// percentage point.
func TestWeightsSpreadWindowsInProportion(t *testing.T) {
	list := listOf(t, Code{K: 1, M: 0}, cacheOf(1, 1), cacheOf(2, 2), cacheOf(3, 3), cacheOf(4, 4))
	const windows = 100000
	first := map[byte]int{}
	for span := range uint64(windows) {
		first[list.Ranks(windowOf("vm-weights", 9, span))[0].Identity[0]]++
	}
	for n, weight := range map[byte]float64{1: 1, 2: 2, 3: 3, 4: 4} {
		share := float64(first[n]) / windows
		if want := weight / 10; math.Abs(share-want) > 0.005 {
			t.Fatalf("cache %d of weight %v ranks first for %.4f of the windows, want %.2f; all %v",
				n, weight, share, want, first)
		}
	}
}

// Ranks are a function of the list's caches alone: the order a host learned
// them in changes nothing, and neither does building the list again. So two
// hosts with the same list ask the same caches for every window. The ranks of
// a few windows are written out, so a host of any architecture, which runs
// this same test, ranks them alike.
func TestHostsWithTheSameListRankAlike(t *testing.T) {
	caches := []Cache{cacheOf(1, 1), cacheOf(2, 2), cacheOf(3, 1), cacheOf(4, 3), cacheOf(5, 1),
		cacheOf(6, 1), cacheOf(7, 2)}
	one := listOf(t, Code{K: 4, M: 2}, caches...)
	reversed := slices.Clone(caches)
	slices.Reverse(reversed)
	other := listOf(t, Code{K: 4, M: 2}, reversed...)
	if !one.Equal(other) {
		t.Fatalf("two lists of the same caches differ: %v and %v", one.Caches(), other.Caches())
	}
	for span := range uint64(2000) {
		window := windowOf("vm-alike", 4, span)
		if a, b := identities(one.Ranks(window)), identities(other.Ranks(window)); !slices.Equal(a, b) {
			t.Fatalf("span %d ranks %v on one host and %v on the other", span, a, b)
		}
	}
	written := map[Window][]byte{
		windowOf("vm-alike", 4, 0): {1, 4, 6, 7, 2, 3},
		windowOf("vm-alike", 4, 1): {2, 4, 7, 3, 6, 1},
		windowOf("vm-alike", 5, 0): {3, 1, 6, 4, 5, 2},
		SegmentWindow(control.Ref{VM: "vm-alike", Sequence: 4}, "disk", 0): {4, 2, 3, 7, 5, 1},
	}
	for window, want := range written {
		if got := identities(one.Ranks(window)); !slices.Equal(got, want) {
			t.Errorf("%+v ranks %v, want %v", window, got, want)
		}
	}
}

// When a cache joins or leaves, a window's first k+m change only if that
// cache is among them: a join pushes one holder out, and a leave pulls in the
// cache ranked next, and every other holder keeps its order. So a window loses
// at most one of its stripes to any one change of the list.
func TestAJoinOrALeaveChangesAWindowsHoldersByAtMostOneCache(t *testing.T) {
	random := rand.New(rand.NewPCG(3, 5))
	caches := make([]Cache, 0, 9)
	for n := range byte(9) {
		caches = append(caches, cacheOf(n+1, uint32(1+random.IntN(4))))
	}
	code := Code{K: 4, M: 2}
	before := listOf(t, code, caches[:8]...)
	joined := listOf(t, code, caches...)
	left := before.Without(caches[2].Identity)
	moved := map[string]int{}
	for span := range uint64(5000) {
		window := windowOf("vm-churn", 1, span)
		was := identities(before.Ranks(window))
		for _, change := range []struct {
			name    string
			list    List
			changed byte
			rest    []byte
		}{
			{"join", joined, caches[8].Identity[0], identities(joined.Ranks(window))},
			{"leave", left, caches[2].Identity[0], identities(left.Ranks(window))},
		} {
			now := change.rest
			out := slices.DeleteFunc(slices.Clone(was), func(n byte) bool { return slices.Contains(now, n) })
			in := slices.DeleteFunc(slices.Clone(now), func(n byte) bool { return slices.Contains(was, n) })
			if len(out) > 1 || len(in) > 1 || len(out) != len(in) {
				t.Fatalf("a %s moved %v out of span %d's holders and %v in: %v to %v",
					change.name, out, span, in, was, now)
			}
			if len(out) == 1 {
				moved[change.name]++
				// A join takes the place of the last holder; a leave's place
				// goes to the cache ranked next.
				if change.name == "join" && (in[0] != change.changed || out[0] != was[len(was)-1]) {
					t.Fatalf("a join moved %v in and %v out of span %d: %v to %v", in, out, span, was, now)
				}
				if change.name == "leave" && out[0] != change.changed {
					t.Fatalf("a leave moved %v out of span %d: %v to %v", out, span, was, now)
				}
			}
			kept := slices.DeleteFunc(slices.Clone(was), func(n byte) bool { return slices.Contains(out, n) })
			order := slices.DeleteFunc(slices.Clone(now), func(n byte) bool { return slices.Contains(in, n) })
			if !slices.Equal(kept, order) {
				t.Fatalf("a %s reordered span %d's holders: %v to %v", change.name, span, was, now)
			}
		}
	}
	if want := map[string]int{"join": 3562, "leave": 4759}; !equalCounts(moved, want) {
		t.Fatalf("the changes moved a holder of %v of 5,000 windows, want %v", moved, want)
	}
}

func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

// Equal scores go to the lower identity, whatever order they are compared in.
// Two caches score a window alike when their weights over their distances
// are equal, which at 32 fractional bits two hashes can reach.
func TestEqualScoresGoToTheLowerIdentity(t *testing.T) {
	low := scored{weight: 2, distance: 6 << fractionBits, identity: Identity{1}}
	high := scored{weight: 1, distance: 3 << fractionBits, identity: Identity{2}}
	if got := compare(low, high); got != -1 {
		t.Fatalf("the lower identity compares %d against an equal score, want -1", got)
	}
	if got := compare(high, low); got != 1 {
		t.Fatalf("the higher identity compares %d against an equal score, want 1", got)
	}
	better := scored{weight: 3, distance: 6 << fractionBits, identity: Identity{9}}
	if got := compare(better, low); got != -1 {
		t.Fatalf("a higher score compares %d against a lower identity, want -1", got)
	}
	// The products are compared in all 128 bits: these two differ only in the
	// high word.
	wide := scored{weight: math.MaxUint32, distance: 63 << fractionBits, identity: Identity{3}}
	narrow := scored{weight: math.MaxUint32 - 1, distance: 63 << fractionBits, identity: Identity{1}}
	if got := compare(wide, narrow); got != -1 {
		t.Fatalf("the heavier of two caches at one distance compares %d, want -1", got)
	}
	if got := compare(narrow, wide); got != 1 {
		t.Fatalf("the lighter of two caches at one distance compares %d, want 1", got)
	}
}

// A list shorter than k+m takes a window's stripes round its caches: stripe i
// on rank ((i - 1) mod n) + 1. A host alone holds every stripe, which at its
// own code of 1+0 is the envelope whole.
func TestHoldersGoRoundAShortList(t *testing.T) {
	two := listOf(t, Code{K: 4, M: 2}, cacheOf(1, 1), cacheOf(2, 1))
	window := windowOf("vm-short", 2, 0)
	ranks := identities(two.Ranks(window))
	if want := []byte{2, 1}; !slices.Equal(ranks, want) {
		t.Fatalf("two caches rank %v, want %v", ranks, want)
	}
	if got, want := identities(two.Holders(window)), []byte{2, 1, 2, 1, 2, 1}; !slices.Equal(got, want) {
		t.Fatalf("4+2 over two caches puts the stripes on %v, want %v", got, want)
	}
	five := listOf(t, Code{K: 4, M: 2}, cacheOf(1, 1), cacheOf(2, 1), cacheOf(3, 1), cacheOf(4, 1), cacheOf(5, 1))
	ranked := identities(five.Ranks(window))
	if got, want := identities(five.Holders(window)), append(slices.Clone(ranked), ranked[0]); !slices.Equal(got, want) {
		t.Fatalf("4+2 over five caches puts the stripes on %v, want %v", got, want)
	}
	self := cacheOf(7, 3)
	alone := Alone(self)
	if alone.Code() != (Code{K: 1, M: 0}) || alone.Len() != 1 {
		t.Fatalf("a host alone holds %v under %s", alone.Caches(), alone.Code())
	}
	for span := range uint64(64) {
		w := windowOf("vm-alone", 1, span)
		if got := alone.Holders(w); len(got) != 1 || got[0] != self {
			t.Fatalf("a host alone puts span %d's stripes on %v, want itself whole", span, got)
		}
	}
	nobody := Alone(Cache{})
	if nobody.Len() != 0 || len(nobody.Ranks(window)) != 0 || nobody.Holders(window) != nil {
		t.Fatalf("a host with no cache ranks %v and holds %v", nobody.Ranks(window), nobody.Holders(window))
	}
}

// A window is one volume's aligned 2 MiB span of one checkpoint's pages: 512
// pages at 4 KiB, one at 2 MiB. A segment is a window of its own, apart from
// the span of the same number, and so is every other checkpoint's span. A
// window knows how many pages it spans, which turns a page of it into a page
// of the volume, and which no rank depends on.
func TestAWindowIsOneSpanOfOneCheckpointsVolume(t *testing.T) {
	ref := control.Ref{VM: "vm-w", Sequence: 7}
	page := func(n uint64) control.Identity { return control.Identity{Ref: ref, Volume: "ram0", Page: n} }
	if got := PageWindow(page(511), 4096); got != (Window{Ref: ref, Volume: "ram0", Number: 0, Pages: 512}) {
		t.Fatalf("page 511 of a 4 KiB volume is in %+v", got)
	}
	if got := PageWindow(page(512), 4096); got != (Window{Ref: ref, Volume: "ram0", Number: 1, Pages: 512}) ||
		got.Page(5) != 517 {
		t.Fatalf("page 512 of a 4 KiB volume is in %+v, whose page 5 is %d", got, got.Page(5))
	}
	if got := PageWindow(page(3), 2<<20); got != (Window{Ref: ref, Volume: "ram0", Number: 3, Pages: 1}) ||
		got.Page(0) != 3 {
		t.Fatalf("page 3 of a 2 MiB volume is in %+v, whose page 0 is %d", got, got.Page(0))
	}
	if got := SegmentWindow(ref, "ram0", 9); got != (Window{Ref: ref, Volume: "ram0", Segment: true, Number: 9,
		Pages: 1}) {
		t.Fatalf("segment 9 is the window %+v", got)
	}
	spanned := PageWindow(page(512), 4096)
	if unspanned := (Window{Ref: ref, Volume: "ram0", Number: 1}); spanned.digest() != unspanned.digest() ||
		spanned.InShare(50) != unspanned.InShare(50) {
		t.Fatal("how many pages a window spans changed what ranks it")
	}
	digests := map[uint64]Window{}
	for _, window := range []Window{
		{Ref: ref, Volume: "ram0", Number: 1},
		{Ref: ref, Volume: "ram0", Number: 1, Segment: true},
		{Ref: control.Ref{VM: "vm-w", Sequence: 8}, Volume: "ram0", Number: 1},
		{Ref: control.Ref{VM: "vm-wr", Sequence: 7}, Volume: "am0", Number: 1},
		{Ref: control.Ref{VM: "vm-w", Sequence: 7}, Volume: "ram0", Number: 2},
	} {
		digest := window.digest()
		if earlier, seen := digests[digest]; seen {
			t.Fatalf("%+v and %+v share a digest", earlier, window)
		}
		digests[digest] = window
	}
}

// The code for a cluster follows the table for small clusters, and a code is
// written k+m.
func TestTheCodeForEachSizeOfCluster(t *testing.T) {
	want := map[int]string{0: "1+0", 1: "1+0", 2: "1+1", 3: "2+1", 4: "2+2", 5: "2+2", 6: "4+2", 40: "4+2"}
	for hosts, code := range want {
		if got := CodeFor(hosts).String(); got != code {
			t.Fatalf("%d hosts get %s, want %s", hosts, got, code)
		}
	}
	parsed, err := ParseCode(" 6+2 ")
	if err != nil || parsed != (Code{K: 6, M: 2}) || parsed.Width() != 8 {
		t.Fatalf("6+2 parses as %v, %v", parsed, err)
	}
	// The widest code a list takes is 32 stripes.
	if widest, err := ParseCode("16+16"); err != nil || widest.Width() != 32 {
		t.Fatalf("16+16 parses as %v, %v", widest, err)
	}
	for _, bad := range []string{"", "4", "4+", "+2", "0+2", "4+-1", "30+3", "a+b"} {
		if _, err := ParseCode(bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%q parses with %v, want ErrInvalid", bad, err)
		}
	}
}

// The code of a deployment that sets none is 4+2, whatever its hosts.
func TestTheDefaultCodeIsFixed(t *testing.T) {
	if DefaultCode != (Code{K: 4, M: 2}) {
		t.Fatalf("the default code is %s, want 4+2", DefaultCode)
	}
}

// A list names the codes the deployment used before its own, newest first,
// and a read tries its own code first. Under an earlier code the caches rank
// a window in the same order, so its ranks under a narrower code are the
// first of its ranks under a wider one. A list less a cache keeps its codes,
// and two lists that differ only in their earlier codes differ.
func TestAListNamesTheCodesItUsedBefore(t *testing.T) {
	caches := []Cache{cacheOf(1, 1), cacheOf(2, 1), cacheOf(3, 2), cacheOf(4, 1), cacheOf(5, 1), cacheOf(6, 3),
		cacheOf(7, 1)}
	now, before, first := Code{K: 2, M: 1}, Code{K: 4, M: 2}, Code{K: 1, M: 1}
	list, err := NewList(now, caches, before, first)
	if err != nil {
		t.Fatal(err)
	}
	if got := list.Codes(); !slices.Equal(got, []Code{now, before, first}) {
		t.Fatalf("the list's codes are %v, want 2+1, 4+2, 1+1", got)
	}
	if got := list.Earlier(); !slices.Equal(got, []Code{before, first}) {
		t.Fatalf("the earlier codes are %v, want 4+2, 1+1", got)
	}
	for at := range uint64(64) {
		window := windowOf("vm-codes", 3, at)
		wide := list.Under(before)
		if wide.Code() != before || len(wide.Earlier()) != 0 || len(wide.Ranks(window)) != 6 {
			t.Fatalf("under 4+2 the list is %s with %v and %d ranks", wide.Code(), wide.Earlier(), len(wide.Ranks(window)))
		}
		if !slices.Equal(list.Ranks(window), wide.Ranks(window)[:3]) {
			t.Fatalf("window %d ranks %v under 2+1 and %v under 4+2", at, list.Ranks(window), wide.Ranks(window))
		}
		alone := listOf(t, before, caches...)
		if !slices.Equal(wide.Holders(window), alone.Holders(window)) {
			t.Fatalf("window %d is held by %v under the earlier code and %v by a list of that code",
				at, wide.Holders(window), alone.Holders(window))
		}
	}
	without := list.Without(caches[0].Identity)
	if !slices.Equal(without.Codes(), list.Codes()) || without.Len() != len(caches)-1 {
		t.Fatalf("less a cache the list holds %d caches under %v", without.Len(), without.Codes())
	}
	if plain := listOf(t, now, caches...); plain.Equal(list) || !plain.Equal(listOf(t, now, caches...)) {
		t.Fatal("a list without earlier codes equals one with them")
	}
	if other, err := NewList(now, caches, first, before); err != nil || other.Equal(list) {
		t.Fatalf("earlier codes in another order make %v, %v, want a list unequal to this one", other.Codes(), err)
	}
}

// A list refuses earlier codes it cannot read under: one it could not store
// under, its own code again, one named twice, and more than MaxEarlierCodes.
func TestAListRefusesEarlierCodesItCannotRead(t *testing.T) {
	caches := []Cache{cacheOf(1, 1)}
	for name, earlier := range map[string][]Code{
		"no data stripe": {{K: 0, M: 1}},
		"its own code":   {{K: 2, M: 1}},
		"named twice":    {{K: 4, M: 2}, {K: 4, M: 2}},
		"too many":       {{K: 1, M: 0}, {K: 1, M: 1}, {K: 2, M: 2}, {K: 4, M: 2}},
	} {
		if _, err := NewList(Code{K: 2, M: 1}, caches, earlier...); !errors.Is(err, ErrInvalid) {
			t.Fatalf("a list with %s is refused with %v, want ErrInvalid", name, err)
		}
	}
	if list, err := NewList(Code{K: 2, M: 1}, caches, Code{K: 1, M: 0}, Code{K: 1, M: 1}, Code{K: 4, M: 2}); err != nil ||
		len(list.Earlier()) != MaxEarlierCodes {
		t.Fatalf("three earlier codes make %v, %v", list.Earlier(), err)
	}
}

// A list refuses a cache it could not rank: one with no identity, one of no
// weight, and one listed twice, and a code it could not store under.
func TestAListRefusesWhatItCannotRank(t *testing.T) {
	for name, attempt := range map[string]func() error{
		"no identity": func() error { _, err := NewList(Code{K: 1}, []Cache{{Weight: 1}}); return err },
		"no weight":   func() error { _, err := NewList(Code{K: 1}, []Cache{cacheOf(1, 0)}); return err },
		"twice": func() error {
			_, err := NewList(Code{K: 1}, []Cache{cacheOf(1, 1), cacheOf(2, 1), cacheOf(1, 2)})
			return err
		},
		"no data stripe": func() error { _, err := NewList(Code{K: 0, M: 2}, nil); return err },
	} {
		if err := attempt(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("a list with %s is refused with %v, want ErrInvalid", name, err)
		}
	}
}

// A weight is the disk in steps of 16 GiB, rounded to the nearest, and at
// least one for any disk at all.
func TestAWeightIsTheDiskInCoarseSteps(t *testing.T) {
	for bytes, want := range map[int64]uint32{-1: 0, 0: 0, 1: 1, 8<<30 - 1: 1, 16 << 30: 1, 24<<30 - 1: 1,
		24 << 30: 2, 1 << 40: 64, 1 << 62: 1 << 28} {
		if got := Weight(bytes); got != want {
			t.Fatalf("a disk of %d bytes weighs %d, want %d", bytes, got, want)
		}
	}
}

// An identity reads back from the hex it is written as, and nothing else
// reads as one.
func TestAnIdentityReadsBackFromItsHex(t *testing.T) {
	identity := Identity{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10}
	text := identity.String()
	if text != "0123456789abcdeffedcba9876543210" {
		t.Fatalf("the identity reads as %s", text)
	}
	parsed, err := ParseIdentity(text)
	if err != nil || parsed != identity {
		t.Fatalf("%s parses as %v, %v", text, parsed, err)
	}
	for _, bad := range []string{"", "0123", text + "00", "zz23456789abcdeffedcba9876543210"} {
		if _, err := ParseIdentity(bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%q parses with %v, want ErrInvalid", bad, err)
		}
	}
}

// A reader of a window picks want of its ranks to ask first. Over 60,000
// readers of one window, every rank is picked by want in every six of them,
// within a point, so a hot window's readers spread over all its holders; over
// 60,000 windows read by one reader, the same. A reader picks the same ranks
// every time it reads a window, the picks come first and the rest after, each
// part in rank order, and a want of every rank or more leaves the ranks as
// they are.
func TestReadersPickRanksEvenlyAndAlwaysTheSame(t *testing.T) {
	var caches []Cache
	for n := range byte(6) {
		caches = append(caches, cacheOf(n+1, 1))
	}
	list := listOf(t, Code{K: 4, M: 2}, caches...)
	const draws, want = 60000, 5
	random := rand.New(rand.NewPCG(7, 11))
	spread := func(name string, draw func(at int) (Identity, Window)) {
		counts := make(map[Identity]int)
		for at := range draws {
			reader, window := draw(at)
			ranks := list.Ranks(window)
			order := Pick(ranks, reader, window, want)
			if again := Pick(ranks, reader, window, want); !slices.Equal(order, again) {
				t.Fatalf("%s: a reader picked %v and then %v", name, identities(order), identities(again))
			}
			if !slices.Equal(sorted(order), sorted(ranks)) {
				t.Fatalf("%s: picked %v of the ranks %v", name, identities(order), identities(ranks))
			}
			for _, part := range [][]Cache{order[:want], order[want:]} {
				if !slices.IsSortedFunc(part, func(a, b Cache) int {
					return slices.Index(ranks, a) - slices.Index(ranks, b)
				}) {
					t.Fatalf("%s: a part of the picks %v is not in rank order %v", name, identities(part), identities(ranks))
				}
			}
			for _, cache := range order[:want] {
				counts[cache.Identity]++
			}
		}
		for _, cache := range caches {
			if got, expected := counts[cache.Identity], draws*want/len(caches); math.Abs(float64(got-expected)) > draws/100 {
				t.Fatalf("%s: cache %s was picked %d times of %d reads, want about %d", name, cache.Identity, got,
					draws, expected)
			}
		}
	}
	hot := windowOf("vm-hot", 3, 9)
	spread("readers of one window", func(int) (Identity, Window) {
		var reader Identity
		for at := range reader {
			reader[at] = byte(random.Uint32())
		}
		return reader, hot
	})
	reader := cacheOf(9, 1).Identity
	spread("one reader of many windows", func(at int) (Identity, Window) {
		return reader, windowOf("vm-many", uint64(at/512+1), uint64(at))
	})
	ranks := list.Ranks(hot)
	if got := Pick(ranks, reader, hot, len(ranks)); !slices.Equal(got, ranks) {
		t.Fatalf("a want of every rank picked %v of %v", identities(got), identities(ranks))
	}
	if got := Pick(ranks, reader, hot, 0); !slices.Equal(got, ranks) {
		t.Fatalf("a want of none picked %v of %v", identities(got), identities(ranks))
	}
}

// sorted is caches in identity order.
func sorted(caches []Cache) []Cache {
	sorted := slices.Clone(caches)
	slices.SortFunc(sorted, func(a, b Cache) int { return slices.Compare(a.Identity[:], b.Identity[:]) })
	return sorted
}
