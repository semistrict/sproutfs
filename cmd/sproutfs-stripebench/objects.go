package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"

	"github.com/klauspost/reedsolomon"
)

// code is an erasure code of k data stripes and m parity stripes. Any k of the
// k+m stripes rebuild the object. The code 1+0 is the whole object on one
// server.
type code struct{ k, m int }

func (c code) n() int         { return c.k + c.m }
func (c code) String() string { return fmt.Sprintf("%d+%d", c.k, c.m) }

func parseCode(s string) (code, error) {
	k, m, ok := strings.Cut(s, "+")
	if !ok {
		return code{}, fmt.Errorf("code %q: want k+m, such as 4+2", s)
	}
	data, err := strconv.Atoi(k)
	if err != nil {
		return code{}, fmt.Errorf("code %q: %w", s, err)
	}
	parity, err := strconv.Atoi(m)
	if err != nil {
		return code{}, fmt.Errorf("code %q: %w", s, err)
	}
	if data < 1 || parity < 0 || data+parity > 255 {
		return code{}, fmt.Errorf("code %q: want k of 1 or more and k+m of at most 255", s)
	}
	return code{data, parity}, nil
}

func parseCodes(s string) ([]code, error) {
	var codes []code
	for part := range strings.SplitSeq(s, ",") {
		c, err := parseCode(strings.TrimSpace(part))
		if err != nil {
			return nil, err
		}
		codes = append(codes, c)
	}
	if len(codes) == 0 || len(codes) > 255 {
		return nil, fmt.Errorf("codes %q: want 1 to 255 codes", s)
	}
	return codes, nil
}

// layout is how one code stripes an object of a fixed size. The last data
// stripe is padded with zeros.
type layout struct {
	code
	objectBytes int
	stripeBytes int
	enc         reedsolomon.Encoder // nil when the code has no parity
}

func newLayout(c code, objectBytes int) (*layout, error) {
	l := &layout{code: c, objectBytes: objectBytes, stripeBytes: (objectBytes + c.k - 1) / c.k}
	if c.m > 0 {
		enc, err := reedsolomon.New(c.k, c.m)
		if err != nil {
			return nil, fmt.Errorf("code %v: %w", c, err)
		}
		l.enc = enc
	}
	return l, nil
}

// stripes splits an object into its k+m stripes, which share one buffer.
func (l *layout) stripes(object []byte) ([][]byte, error) {
	if len(object) != l.objectBytes {
		return nil, fmt.Errorf("code %v: object of %d bytes, want %d", l.code, len(object), l.objectBytes)
	}
	all := make([]byte, l.n()*l.stripeBytes)
	copy(all, object)
	out := make([][]byte, l.n())
	for i := range out {
		out[i] = all[i*l.stripeBytes : (i+1)*l.stripeBytes : (i+1)*l.stripeBytes]
	}
	if l.enc != nil {
		if err := l.enc.Encode(out); err != nil {
			return nil, fmt.Errorf("code %v: encode: %w", l.code, err)
		}
	}
	return out, nil
}

// join rebuilds the object into out from the stripes in hand, nil where a
// stripe is missing. The code is systematic, so when every data stripe is in
// hand it only copies them. It reports whether it had to decode.
func (l *layout) join(stripes [][]byte, out []byte) (decoded bool, err error) {
	if len(stripes) != l.n() || len(out) != l.objectBytes {
		return false, fmt.Errorf("code %v: join of %d stripes into %d bytes", l.code, len(stripes), len(out))
	}
	if slices.ContainsFunc(stripes[:l.k], func(s []byte) bool { return s == nil }) {
		if l.enc == nil {
			return false, fmt.Errorf("code %v: a data stripe is missing and there is no parity", l.code)
		}
		if err := l.enc.ReconstructData(stripes); err != nil {
			return false, fmt.Errorf("code %v: decode: %w", l.code, err)
		}
		decoded = true
	}
	rest := out
	for _, s := range stripes[:l.k] {
		rest = rest[copy(rest, s):]
	}
	return decoded, nil
}

// objectSet is what servers and clients must agree on to agree on every byte
// and every placement: the objects, their size, the seed they are derived
// from, the codes and how many servers there are.
type objectSet struct {
	servers     int
	objects     int
	objectBytes int
	seed        uint64
	codes       []code
}

func (s objectSet) validate() error {
	if s.objects < 1 || s.objectBytes < 1 {
		return errors.New("want at least one object of at least one byte")
	}
	for _, c := range s.codes {
		// A server answers with at most one stripe of an object, so every
		// stripe needs a server of its own.
		if c.n() > s.servers {
			return fmt.Errorf("code %v needs %d servers, and there are %d", c, c.n(), s.servers)
		}
	}
	return nil
}

func (s objectSet) layouts() ([]*layout, error) {
	out := make([]*layout, len(s.codes))
	for i, c := range s.codes {
		l, err := newLayout(c, s.objectBytes)
		if err != nil {
			return nil, err
		}
		out[i] = l
	}
	return out, nil
}

// fingerprint is a hash of the set, which a client checks against each
// server's before it reads.
func (s objectSet) fingerprint() uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d/%d/%d/%d/%v", s.servers, s.objects, s.objectBytes, s.seed, s.codes)
	return h.Sum64()
}

// fill writes object's bytes into out. They are incompressible, as a
// compressed page is.
func (s objectSet) fill(object uint32, out []byte) {
	var key [32]byte
	binary.LittleEndian.PutUint64(key[:], s.seed)
	binary.LittleEndian.PutUint32(key[8:], object)
	// ChaCha8's Read never fails.
	_, _ = rand.NewChaCha8(key).Read(out)
}

// rank orders servers for an object by rendezvous hashing, best first. Every
// server has the same weight, so ranking by the hash is ranking by the plan's
// w / -ln(u). Ties go to the lower server.
func rank(servers []int, object uint32) []int {
	out := slices.Clone(servers)
	slices.SortFunc(out, func(a, b int) int {
		sa, sb := score(a, object), score(b, object)
		switch {
		case sa > sb:
			return -1
		case sa < sb:
			return 1
		default:
			return a - b
		}
	})
	return out
}

// score is a 64-bit hash of a server and an object.
func score(server int, object uint32) uint64 {
	return mix(uint64(server)<<32 | uint64(object))
}

// readerScore is a 64-bit hash of a reader, a server and an object.
func readerScore(reader uint64, server int, object uint32) uint64 {
	return mix(score(server, object) ^ reader)
}

// mix is splitmix64's step and finalizer.
func mix(z uint64) uint64 {
	z += 0x9e3779b97f4a7c15
	z = (z ^ z>>30) * 0xbf58476d1ce4e5b9
	z = (z ^ z>>27) * 0x94d049bb133111eb
	return z ^ z>>31
}

// everyServer is 0 to n-1.
func everyServer(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}
