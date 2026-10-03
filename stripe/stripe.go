// Package stripe splits an envelope into the stripes of an erasure code and
// rebuilds it from any k of them.
//
// Under the code k+m, an envelope is cut into k data stripes of equal length,
// the last padded with zeros, and m parity stripes are computed from them by
// Reed-Solomon (github.com/klauspost/reedsolomon). Any k distinct indices
// rebuild the envelope. The code is systematic: stripes 0 to k-1 are the
// envelope itself, so a reader that holds those does no decoding. A code with
// k = 1 is whole copies: every stripe is the envelope.
//
// Each stripe carries its code, its index and the envelope's length, which is
// all a reader needs besides k of them. A stripe of another code is never
// used: changing a deployment's code costs a refill, never wrong bytes.
//
// The stripes a reader is given may be wrong: a stripe whose own checksum
// holds can still be another envelope's or damaged before it was summed. So
// Join checks the envelope it rebuilds, and when that fails with more than k
// stripes in hand, it rebuilds from other sets of k until one passes, then
// names every stripe that does not belong to the envelope that passed, so its
// holder can be told to drop it.
package stripe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"

	"github.com/klauspost/reedsolomon"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// MaxLength is the longest envelope a stripe can describe.
const MaxLength = math.MaxInt32

// maxAttempts bounds how many sets of k stripes Join rebuilds from before it
// gives up. Leaving each one out of k+1 stripes takes k+1 of them, and every
// set of four from the six of 4+2 takes fifteen.
const maxAttempts = 64

// ErrTooFew reports fewer than k distinct indices of the code among the
// stripes given, so no envelope can be rebuilt. It is a miss.
var ErrTooFew = errors.New("stripe: fewer than k stripes of the code")

// ErrWrong reports stripes of which no set of k rebuilds an envelope that
// passes its check. It is a miss too.
var ErrWrong = errors.New("stripe: no k of the stripes rebuild the envelope")

// Stripe is one stripe of an envelope under a code.
type Stripe struct {
	// Code is the code the stripe was cut under, and Index its place in it:
	// 0 to k-1 the data, k to k+m-1 the parity.
	Code  rank.Code
	Index int
	// Length is the envelope's length, which tells a reader where the
	// padding of the last data stripe begins.
	Length int
	Bytes  []byte
}

func (s Stripe) String() string {
	return fmt.Sprintf("stripe %d of %s, %d bytes of an envelope of %d", s.Index, s.Code, len(s.Bytes), s.Length)
}

// Size is the length of each stripe of an envelope of length bytes under code.
func Size(code rank.Code, length int) int {
	if length <= 0 || code.K < 1 {
		return 0
	}
	return (length + code.K - 1) / code.K
}

// Split cuts envelope into its k+m stripes under code, in index order. The
// stripes share one buffer, and under a code with k = 1 they are the envelope
// itself, so neither the envelope nor a stripe may be changed while the other
// is in use.
func Split(code rank.Code, envelope []byte) ([]Stripe, error) {
	if err := code.Validate(); err != nil {
		return nil, err
	}
	if len(envelope) > MaxLength {
		return nil, fmt.Errorf("stripe: an envelope of %d bytes is longer than %d", len(envelope), MaxLength)
	}
	stripes := make([]Stripe, code.Width())
	if code.K == 1 {
		for index := range stripes {
			stripes[index] = Stripe{Code: code, Index: index, Length: len(envelope), Bytes: envelope}
		}
		return stripes, nil
	}
	size := Size(code, len(envelope))
	all := make([]byte, code.Width()*size)
	copy(all, envelope)
	shards := make([][]byte, code.Width())
	for index := range shards {
		shards[index] = all[index*size : (index+1)*size : (index+1)*size]
	}
	if code.M > 0 && size > 0 {
		encoder, err := encoderFor(code)
		if err != nil {
			return nil, err
		}
		if err := encoder.Encode(shards); err != nil {
			return nil, fmt.Errorf("stripe: encoding under %s: %w", code, err)
		}
	}
	for index := range stripes {
		stripes[index] = Stripe{Code: code, Index: index, Length: len(envelope), Bytes: shards[index]}
	}
	return stripes, nil
}

// Joined is what Join rebuilt and what it found.
type Joined struct {
	// Envelope is the envelope rebuilt. It may share the bytes of a stripe
	// given.
	Envelope []byte
	// Used is the positions, among the stripes given, of the k that rebuilt
	// it, and Wrong the positions of the stripes of the code found not to be
	// the envelope's: malformed, or rebuilding an envelope that fails its
	// check. A stripe of another code is neither.
	Used, Wrong []int
}

// Join rebuilds the envelope under code from any k stripes of distinct
// indices among stripes, taking them in the order given, and checks it with
// check: nil accepts any envelope whose padding is zeros. Stripes of another
// code are left out. If the first k fail, it tries other sets of k, up to a
// bound, and once a set passes it compares every stripe of the code with the
// envelope's own to name the wrong ones. It reports ErrTooFew when fewer than
// k distinct indices are given, and ErrWrong when no set passes; Wrong then
// still names the malformed stripes, and, under a code with k = 1, every
// stripe that failed alone.
func Join(ctx context.Context, code rank.Code, stripes []Stripe, check func([]byte) error) (Joined, error) {
	if err := code.Validate(); err != nil {
		return Joined{}, err
	}
	var joined Joined
	var candidates []int
	for at, s := range stripes {
		switch {
		case s.Code != code && !sim.Bug(ctx, "stripe-mix-codes"):
			// A stripe of another code is a miss, never a part of this one.
		case !wellFormed(code, s):
			joined.Wrong = append(joined.Wrong, at)
		default:
			candidates = append(candidates, at)
		}
	}
	attempts, failed := 0, false
	found := false
	for subset := range subsets(code, stripes, candidates) {
		if attempts == maxAttempts {
			break
		}
		attempts++
		envelope, err := rebuild(code, stripes, subset)
		if err == nil && check != nil {
			err = check(envelope)
		}
		if err == nil {
			joined.Envelope, joined.Used, found = envelope, slices.Clone(subset), true
			break
		}
		failed = true
		if code.K == 1 {
			// A whole copy is checked alone, so its failure names it.
			joined.Wrong = append(joined.Wrong, subset[0])
		}
		if sim.Bug(ctx, "stripe-stop-at-first-failure") {
			break
		}
	}
	switch {
	case found && failed:
		wrong, err := mismatched(code, stripes, candidates, joined.Envelope)
		if err != nil {
			return Joined{}, err
		}
		joined.Wrong = append(joined.Wrong, wrong...)
	case !found && attempts == 0:
		return finish(joined), fmt.Errorf("%w: %d of %d stripes are of %s", ErrTooFew, len(candidates), len(stripes), code)
	case !found:
		return finish(joined), fmt.Errorf("%w: %d sets of %d tried under %s", ErrWrong, attempts, code.K, code)
	}
	return finish(joined), nil
}

// finish puts the wrong positions in order, once each.
func finish(joined Joined) Joined {
	slices.Sort(joined.Wrong)
	joined.Wrong = slices.Compact(joined.Wrong)
	return joined
}

// Valid reports whether s is shaped as a stripe of its own code: a code that
// can store an envelope, an index of it, and the length of a stripe of the
// envelope it says it belongs to.
func (s Stripe) Valid() bool { return s.Code.Validate() == nil && wellFormed(s.Code, s) }

// wellFormed reports whether s is shaped as a stripe of code: an index of the
// code, and the length of a stripe of the envelope it says it belongs to.
func wellFormed(code rank.Code, s Stripe) bool {
	return s.Index >= 0 && s.Index < code.Width() && s.Length >= 0 && s.Length <= MaxLength &&
		len(s.Bytes) == Size(code, s.Length)
}

// subsets yields the sets of k candidates, as positions among stripes, that
// could rebuild an envelope: distinct indices of one envelope length, in the
// order of the candidates, the first k first.
func subsets(code rank.Code, stripes []Stripe, candidates []int) func(yield func([]int) bool) {
	return func(yield func([]int) bool) {
		chosen := make([]int, 0, code.K)
		var walk func(from int) bool
		walk = func(from int) bool {
			if len(chosen) == code.K {
				return yield(chosen)
			}
			for at := from; at <= len(candidates)-(code.K-len(chosen)); at++ {
				next := stripes[candidates[at]]
				if len(chosen) > 0 && stripes[chosen[0]].Length != next.Length ||
					slices.ContainsFunc(chosen, func(position int) bool { return stripes[position].Index == next.Index }) {
					continue
				}
				chosen = append(chosen, candidates[at])
				if !walk(at + 1) {
					return false
				}
				chosen = chosen[:len(chosen)-1]
			}
			return true
		}
		walk(0)
	}
}

// errPadding reports an envelope rebuilt with bytes past its end that are not
// the zeros it was padded with.
var errPadding = errors.New("stripe: the rebuilt envelope's padding is not zeros")

// rebuild is the envelope the stripes at subset make: k stripes of distinct
// indices and one envelope length.
func rebuild(code rank.Code, stripes []Stripe, subset []int) ([]byte, error) {
	length := stripes[subset[0]].Length
	if code.K == 1 {
		return stripes[subset[0]].Bytes, nil
	}
	size := Size(code, length)
	envelope := make([]byte, 0, code.K*size)
	if size == 0 {
		return envelope, nil
	}
	shards := make([][]byte, code.Width())
	for _, position := range subset {
		shards[stripes[position].Index] = stripes[position].Bytes
	}
	if slices.ContainsFunc(shards[:code.K], func(shard []byte) bool { return shard == nil }) {
		encoder, err := encoderFor(code)
		if err != nil {
			return nil, err
		}
		if err := encoder.ReconstructData(shards); err != nil {
			return nil, fmt.Errorf("stripe: decoding under %s: %w", code, err)
		}
	}
	for _, shard := range shards[:code.K] {
		envelope = append(envelope, shard...)
	}
	if slices.ContainsFunc(envelope[length:], func(b byte) bool { return b != 0 }) {
		return nil, errPadding
	}
	return envelope[:length], nil
}

// mismatched is the positions of the candidates that are not the stripes of
// envelope at their index.
func mismatched(code rank.Code, stripes []Stripe, candidates []int, envelope []byte) ([]int, error) {
	own, err := Split(code, envelope)
	if err != nil {
		return nil, err
	}
	var wrong []int
	for _, position := range candidates {
		s := stripes[position]
		if s.Length != len(envelope) || !bytes.Equal(s.Bytes, own[s.Index].Bytes) {
			wrong = append(wrong, position)
		}
	}
	return wrong, nil
}

// encoders holds one Reed-Solomon encoder for each code a host has used,
// because building one computes its matrices. An encoder is safe for
// concurrent use.
var encoders sync.Map

func encoderFor(code rank.Code) (reedsolomon.Encoder, error) {
	if encoder, ok := encoders.Load(code); ok {
		return encoder.(reedsolomon.Encoder), nil
	}
	encoder, err := reedsolomon.New(code.K, code.M)
	if err != nil {
		return nil, fmt.Errorf("stripe: the code %s: %w", code, err)
	}
	stored, _ := encoders.LoadOrStore(code, encoder)
	return stored.(reedsolomon.Encoder), nil
}
