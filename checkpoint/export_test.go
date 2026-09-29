package checkpoint

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

// LoadedPages reports how many page entries an index holds decoded, which is
// the page table it keeps in memory: the segments it has fetched in the index
// layout, and every segment in the log layout.
func LoadedPages(index *Index) int {
	index.mu.Lock()
	defer index.mu.Unlock()
	var pages int
	for _, held := range index.loaded {
		pages += len(held.pages)
	}
	return pages
}
