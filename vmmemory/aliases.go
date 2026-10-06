package vmmemory

import "iter"

// aliasSet is the bindings that map one page. Almost every page has exactly
// one — a page is a guest's own until a fork shares it — so that one is held
// inline, and only a page with two or more keeps a map of them: a map for
// every page was the largest thing a resident page cost the host's heap after
// the page struct itself. It is protected by Host.mu, as the page's aliases
// always were.
type aliasSet[B comparable] struct {
	one  B
	more map[B]struct{}
}

// add reports whether b was not an alias already.
func (a *aliasSet[B]) add(b B) bool {
	var none B
	switch {
	case a.more != nil:
		if _, ok := a.more[b]; ok {
			return false
		}
		a.more[b] = struct{}{}
	case a.one == b:
		return false
	case a.one == none:
		a.one = b
	default:
		a.more = map[B]struct{}{a.one: {}, b: {}}
		a.one = none
	}
	return true
}

// remove reports whether b was an alias.
func (a *aliasSet[B]) remove(b B) bool {
	var none B
	if a.more == nil {
		if a.one != b || b == none {
			return false
		}
		a.one = none
		return true
	}
	if _, ok := a.more[b]; !ok {
		return false
	}
	delete(a.more, b)
	if len(a.more) == 1 {
		for last := range a.more {
			a.one = last
		}
		a.more = nil
	}
	return true
}

func (a *aliasSet[B]) len() int {
	var none B
	if a.more != nil {
		return len(a.more)
	}
	if a.one != none {
		return 1
	}
	return 0
}

// all yields every alias once, in no particular order.
func (a *aliasSet[B]) all() iter.Seq[B] {
	return func(yield func(B) bool) {
		var none B
		if a.more == nil {
			if a.one != none {
				yield(a.one)
			}
			return
		}
		for b := range a.more {
			if !yield(b) {
				return
			}
		}
	}
}
