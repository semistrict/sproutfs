package vmmemory

import "iter"

// aliasSet is the bindings that map one resident page. Almost every page has
// exactly one — a page is a guest's own until a fork shares it — so that one is
// held inline, and only a page with two or more keeps a map of them: a map for
// every page was the largest thing a resident page cost the host's heap after
// the page struct itself. It is protected by Host.mu, as the page's aliases
// always were.
type aliasSet struct {
	one  *binding
	more map[*binding]struct{}
}

func (a *aliasSet) add(b *binding) {
	switch {
	case a.more != nil:
		a.more[b] = struct{}{}
	case a.one == nil || a.one == b:
		a.one = b
	default:
		a.more = map[*binding]struct{}{a.one: {}, b: {}}
		a.one = nil
	}
}

func (a *aliasSet) remove(b *binding) {
	if a.more == nil {
		if a.one == b {
			a.one = nil
		}
		return
	}
	delete(a.more, b)
	if len(a.more) == 1 {
		for last := range a.more {
			a.one = last
		}
		a.more = nil
	}
}

func (a *aliasSet) len() int {
	if a.more != nil {
		return len(a.more)
	}
	if a.one != nil {
		return 1
	}
	return 0
}

func (a *aliasSet) clear() { *a = aliasSet{} }

// all yields every alias once, in no particular order.
func (a *aliasSet) all() iter.Seq[*binding] {
	return func(yield func(*binding) bool) {
		if a.more == nil {
			if a.one != nil {
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
