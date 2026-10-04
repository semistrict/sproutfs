// Package rank says which cache disks hold a window's stripes.
//
// Every host holds the membership (package membership), whose disks and code
// make a List. For each window, weighted rendezvous hashing ranks every disk:
// each scores the window by w / -ln(u), where u is a 64-bit hash of the
// disk's identity and the window mapped into (0, 1) and w is the disk's
// weight. Ties go to the lower identity. The disks ranked 1 to k+m hold the
// window's stripes, and a list shorter than k+m takes them round its disks.
// Ranks are over disks, not hosts, so a disk that moves to another host keeps
// its windows.
//
// Placement is computed, not recorded, so two hosts that hold one generation
// of the membership rank every window alike. The ranking uses integer
// arithmetic only, so hosts of different architectures agree too.
package rank

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/bits"
	"slices"
	"strconv"
	"strings"

	"github.com/semistrict/sproutfs/platform"
)

// ErrInvalid reports a code, a cache or a list that cannot rank anything.
var ErrInvalid = errors.New("rank: invalid")

// Identity names one cache. It is a random value written in the cache file's
// header when the file is made, so a host that restarts over the same file
// keeps it, and with it every window it holds.
type Identity [16]byte

func (i Identity) String() string { return hex.EncodeToString(i[:]) }

// IsZero reports an identity no cache has.
func (i Identity) IsZero() bool { return i == Identity{} }

// ParseIdentity reads an identity as String writes it.
func ParseIdentity(text string) (Identity, error) {
	var identity Identity
	decoded, err := hex.DecodeString(text)
	if err != nil || len(decoded) != len(identity) {
		return Identity{}, fmt.Errorf("%w: cache identity %q, want %d hex bytes", ErrInvalid, text, len(identity))
	}
	copy(identity[:], decoded)
	return identity, nil
}

// Cache is one cache disk as a list ranks it, at the address of the host
// that serves it, or at none while no host does.
type Cache struct {
	Identity Identity
	// Weight is how many windows the cache holds against the others: twice
	// the weight, about twice the windows. It comes from the size of the disk
	// the cache is given, rounded to WeightStep, never from the share the
	// disk limiter moves all the time, because every change of a weight
	// moves windows.
	Weight uint32
	// Address is where the host that serves the disk answers.
	Address platform.Address
}

// WeightStep is the coarse step a cache's disk is rounded to for its weight.
const WeightStep = 16 << 30

// Weight is the weight of a cache given bytes of disk: the disk in steps of
// WeightStep, rounded to the nearest, and at least one for any disk at all.
func Weight(bytes int64) uint32 {
	if bytes <= 0 {
		return 0
	}
	steps := (bytes + WeightStep/2) / WeightStep
	return uint32(min(max(steps, 1), int64(^uint32(0))))
}

// Code is the deployment's erasure code: K data stripes and M parity stripes
// of each envelope, any K of which rebuild it. K = 1 is whole copies.
type Code struct {
	K, M int
}

// maxWidth bounds k+m. A wider code would cut a 4 KiB page into stripes of a
// few dozen bytes.
const maxWidth = 32

// Width is k+m: how many caches hold a window.
func (c Code) Width() int { return c.K + c.M }

func (c Code) String() string { return strconv.Itoa(c.K) + "+" + strconv.Itoa(c.M) }

// Validate refuses a code that cannot store an envelope.
func (c Code) Validate() error {
	if c.K < 1 || c.M < 0 || c.Width() > maxWidth {
		return fmt.Errorf("%w: code %s, want k at least 1, m at least 0 and k+m at most %d",
			ErrInvalid, c, maxWidth)
	}
	return nil
}

// ParseCode reads a code written as k+m, such as 4+2.
func ParseCode(text string) (Code, error) {
	k, m, found := strings.Cut(strings.TrimSpace(text), "+")
	data, dataErr := strconv.Atoi(k)
	parity, parityErr := strconv.Atoi(m)
	if !found || dataErr != nil || parityErr != nil {
		return Code{}, fmt.Errorf("%w: code %q, want k+m such as 4+2", ErrInvalid, text)
	}
	code := Code{K: data, M: parity}
	return code, code.Validate()
}

// CodeFor is the code for a cluster that usually runs hosts hosts: none to
// spare for one host, whole copies for two, and two parity stripes from four
// hosts on, so that one host can be drained while another is slow.
func CodeFor(hosts int) Code {
	switch {
	case hosts <= 1:
		return Code{K: 1, M: 0}
	case hosts == 2:
		return Code{K: 1, M: 1}
	case hosts == 3:
		return Code{K: 2, M: 1}
	case hosts <= 5:
		return Code{K: 2, M: 2}
	default:
		return Code{K: 4, M: 2}
	}
}

// List is the disks windows are ranked over and the deployment's code. It is
// a value: a host replaces its list, it never changes one.
type List struct {
	code   Code
	caches []Cache
	// seeds is each cache's hash of its identity, which every window's score
	// for that cache starts from.
	seeds []uint64
}

// NewList is the list of caches under code. Every cache needs an identity
// and a weight, and no two may share an identity. The membership makes one
// of its disks.
func NewList(code Code, caches []Cache) (List, error) {
	if err := code.Validate(); err != nil {
		return List{}, err
	}
	sorted := slices.Clone(caches)
	slices.SortFunc(sorted, func(a, b Cache) int { return bytes.Compare(a.Identity[:], b.Identity[:]) })
	seeds := make([]uint64, len(sorted))
	for at, cache := range sorted {
		if cache.Identity.IsZero() || cache.Weight == 0 {
			return List{}, fmt.Errorf("%w: cache %s of weight %d, want an identity and a weight",
				ErrInvalid, cache.Identity, cache.Weight)
		}
		if at > 0 && sorted[at-1].Identity == cache.Identity {
			return List{}, fmt.Errorf("%w: cache %s is listed twice", ErrInvalid, cache.Identity)
		}
		seeds[at] = seedOf(cache.Identity)
	}
	return List{code: code, caches: sorted, seeds: seeds}, nil
}

// Alone is the list of a host that knows no other cache: its own, under the
// code of one host. It ranks first for every window and stores each envelope
// whole. A host with no cache of its own is alone with nobody.
func Alone(self Cache) List {
	list, err := NewList(CodeFor(1), []Cache{self})
	if err != nil {
		return List{code: CodeFor(1)}
	}
	return list
}

// Code is the deployment's code.
func (l List) Code() Code { return l.code }

// Caches is every cache of the list, in identity order.
func (l List) Caches() []Cache { return slices.Clone(l.caches) }

// Len is how many caches the list holds.
func (l List) Len() int { return len(l.caches) }

// Equal reports two lists with the same code and the same caches.
func (l List) Equal(other List) bool {
	return l.code == other.code && slices.Equal(l.caches, other.caches)
}

// Without is the list less the cache of identity, as a host that has not yet
// heard of that cache holds it.
func (l List) Without(identity Identity) List {
	kept := slices.DeleteFunc(l.Caches(), func(cache Cache) bool { return cache.Identity == identity })
	without, _ := NewList(l.code, kept)
	return without
}

// Ranks is the caches ranked 1 to k+m for window, in rank order: all of them
// when the list holds fewer.
func (l List) Ranks(window Window) []Cache {
	scores := l.score(window)
	slices.SortFunc(scores, compare)
	ranks := make([]Cache, min(len(scores), l.code.Width()))
	for at := range ranks {
		ranks[at] = l.caches[scores[at].at]
	}
	return ranks
}

// Holders is the cache each of window's k+m stripes goes on: stripe i on rank
// ((i - 1) mod n) + 1 of the n caches ranked for it, so a list shorter than
// k+m takes the stripes round its caches and a cache may hold two. An empty
// list has no holders.
func (l List) Holders(window Window) []Cache {
	ranks := l.Ranks(window)
	if len(ranks) == 0 {
		return nil
	}
	holders := make([]Cache, l.code.Width())
	for stripe := range holders {
		holders[stripe] = ranks[stripe%len(ranks)]
	}
	return holders
}

// scored is one cache's score for one window: its weight over its distance,
// with the distance -log2(u) in fixed point.
type scored struct {
	at       int
	weight   uint64
	distance uint64
	identity Identity
}

func (l List) score(window Window) []scored {
	digest := window.digest()
	out := make([]scored, len(l.caches))
	for at, cache := range l.caches {
		out[at] = scored{at: at, weight: uint64(cache.Weight), distance: distance(mix(digest ^ l.seeds[at])),
			identity: cache.Identity}
	}
	return out
}

// compare orders a before b when a scores higher: w_a / d_a > w_b / d_b,
// which is w_a * d_b > w_b * d_a with every term positive, compared exactly
// in 128 bits. Equal scores go to the lower identity.
func compare(a, b scored) int {
	aHigh, aLow := bits.Mul64(a.weight, b.distance)
	bHigh, bLow := bits.Mul64(b.weight, a.distance)
	switch {
	case aHigh != bHigh:
		return descending(aHigh, bHigh)
	case aLow != bLow:
		return descending(aLow, bLow)
	}
	return bytes.Compare(a.identity[:], b.identity[:])
}

func descending(a, b uint64) int {
	if a > b {
		return -1
	}
	return 1
}

// seedOf is a cache's hash of its identity, so that identities that differ
// in one bit score windows unrelatedly.
func seedOf(identity Identity) uint64 {
	sum := sha256.Sum256(append([]byte("sproutfs rank cache\x00"), identity[:]...))
	return binary.BigEndian.Uint64(sum[:8])
}

// mix is the finalizer of splitmix64: a bijection of 64 bits whose every
// output bit depends on every input bit.
func mix(z uint64) uint64 {
	z ^= z >> 30
	z *= 0xbf58476d1ce4e5b9
	z ^= z >> 27
	z *= 0x94d049bb133111eb
	z ^= z >> 31
	return z
}

// Pick orders a window's ranks for one reader: the want caches that score
// highest for this reader and window first, then the rest, each part in rank
// order. The readers of one window so spread their requests over every cache
// ranked for it, and one reader always asks the same ones first. A reader of
// a window does not count itself: ranks is the caches it may ask.
func Pick(ranks []Cache, reader Identity, window Window, want int) []Cache {
	if want >= len(ranks) {
		return slices.Clone(ranks)
	}
	scored := make([]picked, len(ranks))
	seed := mix(window.digest() ^ seedOf(reader))
	for at, cache := range ranks {
		scored[at] = picked{at: at, score: mix(seed ^ seedOf(cache.Identity)), identity: cache.Identity}
	}
	slices.SortFunc(scored, func(a, b picked) int {
		if a.score != b.score {
			return descending(a.score, b.score)
		}
		return bytes.Compare(a.identity[:], b.identity[:])
	})
	chosen := make([]bool, len(ranks))
	for _, choice := range scored[:max(want, 0)] {
		chosen[choice.at] = true
	}
	order := make([]Cache, 0, len(ranks))
	for _, first := range []bool{true, false} {
		for at, cache := range ranks {
			if chosen[at] == first {
				order = append(order, cache)
			}
		}
	}
	return order
}

// picked is one rank's score for one reader of a window.
type picked struct {
	at       int
	score    uint64
	identity Identity
}
