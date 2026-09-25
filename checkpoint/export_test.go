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
