package checkpoint

import "google.golang.org/protobuf/proto"

// IndexBytes is an index's encoded form, so a test can require that two indexes
// are the same table rather than merely that they behave alike. An index that
// deferred its index object encodes without segments, which is the root it
// recorded.
func IndexBytes(index *Index) ([]byte, error) {
	message, err := index.rootMessage(!index.deferred())
	if err != nil {
		return nil, err
	}
	return proto.MarshalOptions{Deterministic: true}.Marshal(message)
}

// Deferred reports whether an index is one that deferred its index object.
func Deferred(index *Index) bool { return index.deferred() }

// LoadedPages reports how many page entries an index holds decoded.
func LoadedPages(index *Index) int {
	index.mu.Lock()
	defer index.mu.Unlock()
	var pages int
	for _, held := range index.loaded {
		pages += len(held.pages)
	}
	return pages
}

// LiveBuilders reports how many publications hold a part builder right now,
// which is what the host-wide builder budget bounds.
func LiveBuilders(s *Store) int {
	s.builderMu.Lock()
	defer s.builderMu.Unlock()
	return s.liveBuilders
}
