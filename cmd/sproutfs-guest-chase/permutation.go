package main

// permutation is a bijection of [0, n) drawn from a seed, with its inverse. It
// is a balanced Feistel network over the smallest even number of bits that
// covers n, walked round its cycle until it lands inside [0, n). It holds no
// table, so a chain over tens of millions of keys costs the guest no memory
// beyond the database's own: the loader and the walker each compute what a key
// should hold rather than remember it.
type permutation struct {
	n    uint64
	half uint     // bits in each half of the network's word
	mask uint64   // the low half's bits
	keys []uint64 // one per round
}

// rounds is how many Feistel rounds the network runs. Four make a balanced
// network a pseudorandom permutation, which is all a chain through memory
// needs: no step should land near the one before it.
const rounds = 4

func newPermutation(n, seed uint64) permutation {
	if n == 0 {
		panic("a permutation of nothing")
	}
	bits := uint(1)
	for bits < 64 && uint64(1)<<bits < n {
		bits++
	}
	half := max((bits+1)/2, 1)
	p := permutation{n: n, half: half, mask: uint64(1)<<half - 1, keys: make([]uint64, rounds)}
	state := seed
	for at := range p.keys {
		state += 0x9e3779b97f4a7c15
		p.keys[at] = mix(state)
	}
	return p
}

// mix is splitmix64's finaliser: every bit of its input moves every bit of its
// output.
func mix(z uint64) uint64 {
	z = (z ^ z>>30) * 0xbf58476d1ce4e5b9
	z = (z ^ z>>27) * 0x94d049bb133111eb
	return z ^ z>>31
}

func (p permutation) round(value uint64, key uint64) uint64 { return mix(value^key) & p.mask }

func (p permutation) encrypt(x uint64) uint64 {
	left, right := x>>p.half, x&p.mask
	for _, key := range p.keys {
		left, right = right, left^p.round(right, key)
	}
	return left<<p.half | right
}

func (p permutation) decrypt(x uint64) uint64 {
	left, right := x>>p.half, x&p.mask
	for at := len(p.keys) - 1; at >= 0; at-- {
		left, right = right^p.round(left, p.keys[at]), left
	}
	return left<<p.half | right
}

// At is the image of x, which must be below n. Walking the network's cycle
// from inside [0, n) always comes back inside it, so this ends.
func (p permutation) At(x uint64) uint64 {
	for {
		x = p.encrypt(x)
		if x < p.n {
			return x
		}
	}
}

// Inverse is the x whose image is y.
func (p permutation) Inverse(y uint64) uint64 {
	for {
		y = p.decrypt(y)
		if y < p.n {
			return y
		}
	}
}
