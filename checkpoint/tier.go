package checkpoint

import (
	"context"
	"io"

	"github.com/semistrict/sproutfs/platform"
)

// Every read of a checkpoint object is one operation over one object: the
// tail of an index object and the rest of its root, one segment, one member,
// one extent of a part. The operation is the request and what is made of its
// reply, the decoding included, and it runs against a tier: the bucket it
// reads from. So a read that a tier answers wrongly, with an object that is
// not there, a reply cut short or bytes that do not decode, can be run again
// whole against another tier. Store.readObject is where that choice is made.

// tier is one bucket a read of a checkpoint object runs against, and what the
// read learned of the object there: its whole size, and its bytes where a read
// fetched all of them.
type tier struct {
	objects platform.ObjectStore
	size    int64
	whole   []byte
}

// readObject runs read, an operation over the object key: against the hot
// tier first where the store has one, and against the regional bucket where
// it has none or the hot tier does not answer.
func (s *Store) readObject(ctx context.Context, key platform.ObjectKey, read func(context.Context, *tier) error) error {
	if s.hot == nil {
		return read(ctx, s.regional())
	}
	return s.hot.read(ctx, key, s.objects, read)
}

// regional is a tier of the regional bucket alone, for a read that must see
// what the store holds and nothing else.
func (s *Store) regional() *tier { return &tier{objects: s.objects} }

// readRange reads exactly length bytes at offset of one object. The response
// must echo the key and return exactly the bytes asked for; anything else is a
// corrupt read rather than a short one.
func (t *tier) readRange(ctx context.Context, key platform.ObjectKey, offset, length uint64, maximum int64) ([]byte, error) {
	if length == 0 || length > uint64(maximum) {
		return nil, ErrCorrupt
	}
	result, err := t.objects.Get(ctx, platform.GetRequest{
		Key: key, Range: &platform.ByteRange{Offset: int64(offset), Length: int64(length)}})
	if err != nil {
		return nil, err
	}
	defer result.Body.Close()
	if result.Metadata.Key != key || result.Metadata.ETag == "" || result.ContentLength != int64(length) {
		return nil, ErrCorrupt
	}
	data, err := io.ReadAll(io.LimitReader(result.Body, int64(length)+1))
	if err != nil {
		return nil, err
	}
	if uint64(len(data)) != length {
		return nil, ErrCorrupt
	}
	t.learned(result.Metadata.Size, offset, data)
	return data, nil
}

// readSuffix reads the last suffix bytes of one object and reports the size of
// the whole object with them; an object shorter than the suffix comes back
// whole. The response must echo the key and return exactly as many bytes as its
// own size says a suffix that long holds; anything else is a corrupt read
// rather than a short one.
func (t *tier) readSuffix(ctx context.Context, key platform.ObjectKey, suffix int64) ([]byte, uint64, error) {
	result, err := t.objects.Get(ctx, platform.GetRequest{
		Key: key, Range: &platform.ByteRange{Suffix: suffix}})
	if err != nil {
		return nil, 0, err
	}
	defer result.Body.Close()
	if result.Metadata.Key != key || result.Metadata.ETag == "" || result.Metadata.Size < 0 ||
		result.ContentLength != min(result.Metadata.Size, suffix) {
		return nil, 0, ErrCorrupt
	}
	data, err := io.ReadAll(io.LimitReader(result.Body, result.ContentLength+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(data)) != result.ContentLength {
		return nil, 0, ErrCorrupt
	}
	t.learned(result.Metadata.Size, uint64(result.Metadata.Size)-uint64(len(data)), data)
	return data, uint64(result.Metadata.Size), nil
}

// learned notes what one reply said of the object: its size, and its bytes
// when the reply held all of them.
func (t *tier) learned(size int64, offset uint64, data []byte) {
	t.size = size
	if offset == 0 && int64(len(data)) == size {
		t.whole = data
	}
}
