package main

import (
	"bytes"
	"fmt"
	"strconv"
)

// dataset is what a load writes and a walk follows, as a pure function of its
// shape and seed, so the walker checks every byte it reads without anything
// having been carried across the suspend.
//
// Keys k:0000000000 up to the count are strings. Each one's value starts with
// the name of the key after it in one cycle through every key, in an order the
// permutation draws, and is padded with letters drawn from the key's own
// number. The keys are written in their numeric order, so the server allocates
// them in that order and a step of the chain lands anywhere in its heap.
//
// The sorted set z holds members m:0000000000 up to the member count, each
// scored by its own place in a second permutation. They too are added in
// numeric order, so the skiplist's links, which follow the scores, jump
// through the heap as the chain does.
type dataset struct {
	keys, members uint64
	valueBytes    int
	seed          uint64
	chain, scores permutation
}

// nameBytes is a key's or a member's name: two letters and ten digits.
const nameBytes = 12

// minimumValueBytes holds the next key's name and the separator after it.
const minimumValueBytes = nameBytes + 1

// zset is the sorted set's key.
const zset = "z"

func newDataset(keys, members uint64, valueBytes int, seed uint64) (dataset, error) {
	switch {
	case keys == 0:
		return dataset{}, fmt.Errorf("a chain needs at least one key")
	case keys > 9_999_999_999 || members > 9_999_999_999:
		return dataset{}, fmt.Errorf("a name holds ten digits, so at most 9999999999 keys and members")
	case valueBytes < minimumValueBytes:
		return dataset{}, fmt.Errorf("a value of %d bytes cannot hold the next key's name, which needs %d",
			valueBytes, minimumValueBytes)
	}
	d := dataset{keys: keys, members: members, valueBytes: valueBytes, seed: seed,
		chain: newPermutation(keys, seed)}
	if members > 0 {
		d.scores = newPermutation(members, mix(seed)^0x5bd1e995)
	}
	return d, nil
}

func name(prefix byte, number uint64) []byte {
	out := make([]byte, 0, nameBytes)
	out = append(out, prefix, ':')
	digits := strconv.AppendUint(nil, number, 10)
	for range 10 - len(digits) {
		out = append(out, '0')
	}
	return append(out, digits...)
}

func keyName(number uint64) []byte    { return name('k', number) }
func memberName(number uint64) []byte { return name('m', number) }

// parseName is the number a key's or a member's name carries.
func parseName(prefix byte, raw []byte) (uint64, error) {
	if len(raw) != nameBytes || raw[0] != prefix || raw[1] != ':' {
		return 0, fmt.Errorf("%q is not a name of the form %c:0000000000", raw, prefix)
	}
	return strconv.ParseUint(string(raw[2:]), 10, 64)
}

// next is the key after key in the chain.
func (d dataset) next(key uint64) uint64 {
	return d.chain.At((d.chain.Inverse(key) + 1) % d.keys)
}

// step is the key the chain visits at step s, counted from the chain's start.
func (d dataset) step(s uint64) uint64 { return d.chain.At(s % d.keys) }

// letters is the alphabet of a value's padding: sixty-four symbols, so each
// carries six bits drawn from the key's number and the padding compresses as
// little as text does.
const letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// value is what key holds.
func (d dataset) value(key uint64) []byte {
	out := make([]byte, 0, d.valueBytes)
	out = append(out, keyName(d.next(key))...)
	out = append(out, ':')
	state := mix(d.seed ^ key*0x9e3779b97f4a7c15)
	for len(out) < d.valueBytes {
		state = mix(state + 0x9e3779b97f4a7c15)
		for word := state; word != 0 && len(out) < d.valueBytes; word >>= 6 {
			out = append(out, letters[word&63])
		}
	}
	return out
}

// score is member's score, its rank in the sorted set.
func (d dataset) score(member uint64) uint64 { return d.scores.At(member) }

// memberAt is the member of the sorted set at rank.
func (d dataset) memberAt(rank uint64) uint64 { return d.scores.Inverse(rank) }

// checkValue says whether got is what key holds, and returns the key it names
// next. The next key is taken from the bytes the server returned, not from the
// permutation, so every step of a walk depends on what the step before it read.
func (d dataset) checkValue(key uint64, got []byte) (uint64, error) {
	if len(got) < minimumValueBytes {
		return 0, fmt.Errorf("%s holds %d bytes, too few to name the next key", keyName(key), len(got))
	}
	next, err := parseName('k', got[:nameBytes])
	if err != nil {
		return 0, fmt.Errorf("%s holds %q: %w", keyName(key), got[:nameBytes], err)
	}
	if want := d.value(key); !bytes.Equal(got, want) {
		at := 0
		for at < min(len(got), len(want)) && got[at] == want[at] {
			at++
		}
		return 0, fmt.Errorf("%s holds %d bytes that differ from the %d it was given at byte %d",
			keyName(key), len(got), len(want), at)
	}
	return next, nil
}
