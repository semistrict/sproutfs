// Package pageranges is the interval map the pager keeps page state in: a
// state constant over half-open runs of pages, held so that replacing one
// run costs the depth of its boundaries rather than its pages.
package pageranges

// State is what a Map holds for every page of a run. Default state is
// implicit; adjacent equal ranges merge. The immutable AVL nodes make
// replacement cost proportional to boundary depth, including when a large
// range replaces many fragments. Owners synchronize root updates.
type State struct {
	Generation uint64
	Zero       bool
}
type entry struct {
	start, end uint64
	state      State
}
type node struct {
	item          entry
	left, right   *node
	height, count int
}

// Map is the interval map itself. Its zero value is an empty map, whose every
// page holds the default state.
type Map struct{ root *node }

func (n *node) depth() int {
	if n == nil {
		return 0
	}
	return n.height
}
func (n *node) size() int {
	if n == nil {
		return 0
	}
	return n.count
}
func branch(left *node, item entry, right *node) *node {
	return &node{item: item, left: left, right: right, height: 1 + max(left.depth(), right.depth()), count: 1 + left.size() + right.size()}
}
func balance(left *node, item entry, right *node) *node {
	if left.depth() > right.depth()+1 {
		if left.left.depth() >= left.right.depth() {
			return branch(left.left, left.item, branch(left.right, item, right))
		}
		pivot := left.right
		return branch(branch(left.left, left.item, pivot.left), pivot.item, branch(pivot.right, item, right))
	}
	if right.depth() > left.depth()+1 {
		if right.right.depth() >= right.left.depth() {
			return branch(branch(left, item, right.left), right.item, right.right)
		}
		pivot := right.left
		return branch(branch(left, item, pivot.left), pivot.item, branch(pivot.right, right.item, right.right))
	}
	return branch(left, item, right)
}
func join(left *node, item entry, right *node) *node {
	if left.depth() > right.depth()+1 {
		return balance(left.left, left.item, join(left.right, item, right))
	}
	if right.depth() > left.depth()+1 {
		return balance(join(left, item, right.left), right.item, right.right)
	}
	return branch(left, item, right)
}
func split(n *node, position uint64) (*node, *node) {
	if n == nil {
		return nil, nil
	}
	item := n.item
	if position <= item.start {
		left, middle := split(n.left, position)
		return left, join(middle, item, n.right)
	}
	if position >= item.end {
		middle, right := split(n.right, position)
		return join(n.left, item, middle), right
	}
	left, right := item, item
	left.end, right.start = position, position
	return join(n.left, left, nil), join(nil, right, n.right)
}
func first(n *node) *node {
	for n != nil && n.left != nil {
		n = n.left
	}
	return n
}
func last(n *node) *node {
	for n != nil && n.right != nil {
		n = n.right
	}
	return n
}

// Set gives every page of [start, end) one state, replacing whatever the runs
// it covers held.
func (r *Map) Set(start, end uint64, state State) {
	if start >= end {
		return
	}
	left, rest := split(r.root, start)
	_, right := split(rest, end)
	if state == (State{}) {
		if head := first(right); head != nil {
			_, right = split(right, head.item.end)
			r.root = join(left, head.item, right)
		} else {
			r.root = left
		}
		return
	}
	if tail := last(left); tail != nil && tail.item.end == start && tail.item.state == state {
		start = tail.item.start
		left, _ = split(left, start)
	}
	if head := first(right); head != nil && head.item.start == end && head.item.state == state {
		end = head.item.end
		_, right = split(right, end)
	}
	r.root = join(left, entry{start: start, end: end, state: state}, right)
}

// Run returns the state at start and the end of its constant run, capped by
// limit. Default-state gaps stop at the next explicit range.
func (r *Map) Run(start, limit uint64) (State, uint64) {
	for n := r.root; n != nil; {
		if start < n.item.start {
			limit = min(limit, n.item.start)
			n = n.left
		} else if start >= n.item.end {
			n = n.right
		} else {
			return n.item.state, min(limit, n.item.end)
		}
	}
	return State{}, limit
}

// Get is the state of one page.
func (r *Map) Get(page uint64) State { state, _ := r.Run(page, ^uint64(0)); return state }
