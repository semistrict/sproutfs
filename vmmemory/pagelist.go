package vmmemory

// pageList is an ordered list of resident pages whose links live in the pages
// themselves: the recency list every resident page is on and the idle list of
// the pages no memory region maps. A list that allocated an element per page cost each
// resident page one more heap object, and a 4 KiB pager holds millions. The
// lists are protected by Host.mu.
type pageList struct {
	head, tail *resident
	n          int
	// links is where one page keeps its links for this list.
	links func(*resident) *pageLinks
}

// pageLinks is one page's place on one pageList.
type pageLinks struct {
	prev, next *resident
	listed     bool
}

func (l *pageList) len() int                    { return l.n }
func (l *pageList) front() *resident            { return l.head }
func (l *pageList) next(pg *resident) *resident { return l.links(pg).next }
func (l *pageList) contains(pg *resident) bool  { return l.links(pg).listed }
func (l *pageList) moveToBack(pg *resident)     { l.remove(pg); l.pushBack(pg) }
func recentLinks(pg *resident) *pageLinks       { return &pg.recent }
func idleLinks(pg *resident) *pageLinks         { return &pg.idle }

// pushBack puts a page on the list, newest last. A page already on it is left
// where it is.
func (l *pageList) pushBack(pg *resident) {
	at := l.links(pg)
	if at.listed {
		return
	}
	*at = pageLinks{prev: l.tail, listed: true}
	if l.tail != nil {
		l.links(l.tail).next = pg
	} else {
		l.head = pg
	}
	l.tail = pg
	l.n++
}

// remove takes a page off the list, if it is on it.
func (l *pageList) remove(pg *resident) {
	at := l.links(pg)
	if !at.listed {
		return
	}
	if at.prev != nil {
		l.links(at.prev).next = at.next
	} else {
		l.head = at.next
	}
	if at.next != nil {
		l.links(at.next).prev = at.prev
	} else {
		l.tail = at.prev
	}
	*at = pageLinks{}
	l.n--
}
