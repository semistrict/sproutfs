package checkpoint

import "github.com/semistrict/sproutfs/rank"

// IndexBytes is an index's encoded form, so a test can require that two indexes
// are the same table rather than merely that they behave alike.
func IndexBytes(index *Index) ([]byte, error) { return index.encode() }

// LiveBuilders reports how many publications hold a part builder right now,
// which is what the host-wide builder budget bounds.
func LiveBuilders(s *Store) int {
	s.builderMu.Lock()
	defer s.builderMu.Unlock()
	return s.liveBuilders
}

// HeldIndices is the indices of stripes of page at of window the cache's disk
// holds under code, in index order: what a test of where fills put stripes
// asks of each cache.
func (c *Cache) HeldIndices(window rank.Window, at uint32, code rank.Code) []int {
	if c.disk == nil {
		return nil
	}
	var held []int
	for index := range code.Width() {
		if c.disk.holdsStripe(windowKey(window, at), indexOf(code, index)) {
			held = append(held, index)
		}
	}
	return held
}
