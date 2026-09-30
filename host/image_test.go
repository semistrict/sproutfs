package host_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/volume"
)

// sparseImageSize is a guest image with two runs of data far apart: a page at
// the front and a page three MiB in, and holes everywhere else.
const (
	sparseImageSize = 4 << 20
	sparseTail      = 3 << 20
)

// sparseVolumes are a template's volumes for the sparse image.
var sparseVolumes = []volume.VolumeSpec{
	{Name: "ram0", Size: 8192, PageSize: migrationPageSize},
	{Name: "root", Size: sparseImageSize, PageSize: migrationPageSize},
}

// denseSparseImage is the sparse image written out in full.
func denseSparseImage() []byte {
	image := make([]byte, sparseImageSize)
	copy(image, bytes.Repeat([]byte{0xa5}, 4096))
	copy(image[sparseTail:], bytes.Repeat([]byte{0x5a}, 4096))
	return image
}

// holeReader is a SparseSource that fails the test when anything reads its
// holes.
type holeReader struct {
	*bytes.Reader
	t       *testing.T
	extents []host.Extent
	read    int64
}

func (r *holeReader) DataExtents() ([]host.Extent, error) { return r.extents, nil }

func (r *holeReader) Read(p []byte) (int, error) {
	at := r.Size() - int64(r.Len())
	n, err := r.Reader.Read(p)
	for offset := at; offset < at+int64(n); offset++ {
		inside := false
		for _, extent := range r.extents {
			inside = inside || (offset >= extent.Offset && offset < extent.Offset+extent.Length)
		}
		if !inside {
			r.t.Errorf("the import read offset %d, which is a hole", offset)
			break
		}
	}
	r.read += int64(n)
	return n, err
}

// rootReads is the whole root volume of a VM created from a template.
func rootReads(t *testing.T, h *hostHarness, id string, template *host.ImportedTemplate) []byte {
	t.Helper()
	vm, err := h.hosts[0].Volumes().Fork(t.Context(), id, template.Point)
	if err != nil {
		t.Fatalf("creating %s from %s: %v", id, template.ID(), err)
	}
	defer vm.Close(t.Context())
	data := make([]byte, sparseImageSize)
	if err := vm.Volume("root").Read(t.Context(), 0, data); err != nil {
		t.Fatal(err)
	}
	return data
}

// An import reads only the extents a sparse source says hold data, in the pass
// that names the template and in the pass that fills it. The holes are hashed
// as zeroes, so the template is the one the dense image names, and a VM created
// from it reads the dense image.
func TestASparseImageIsReadOnlyWhereItHoldsData(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	h.start(t)
	dense := denseSparseImage()
	source := &holeReader{Reader: bytes.NewReader(dense), t: t,
		extents: []host.Extent{{Offset: 0, Length: 4096}, {Offset: sparseTail, Length: 4096}}}
	template := templateOn(t, h, 0, host.TemplateImport{Image: "sparse", Volumes: sparseVolumes,
		Root: "root", Source: source})
	if want := templateIDOf(dense); template.ID() != want {
		t.Fatalf("the sparse image is template %s, want the dense image's %s", template.ID(), want)
	}
	if source.read != 2*2*4096 {
		t.Fatalf("the import read %d bytes, want the two data pages twice: %d", source.read, 2*2*4096)
	}
	if !bytes.Equal(rootReads(t, h, "vm-sparse", template), dense) {
		t.Fatal("a VM created from the sparse image does not read the dense image")
	}
}

// A sparse file on a filesystem that reports its holes is the same template as
// its dense image, and a VM created from it reads the dense image.
func TestASparseFileIsTheTemplateOfItsDenseImage(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	h.start(t)
	dense := denseSparseImage()
	file, err := os.Create(filepath.Join(t.TempDir(), "image"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, run := range []int64{0, sparseTail} {
		if _, err := file.WriteAt(dense[run:run+4096], run); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Truncate(sparseImageSize); err != nil {
		t.Fatal(err)
	}
	template := templateOn(t, h, 0, host.TemplateImport{Image: "sparse-file", Volumes: sparseVolumes,
		Root: "root", Source: file})
	if want := templateIDOf(dense); template.ID() != want {
		t.Fatalf("the sparse file is template %s, want the dense image's %s", template.ID(), want)
	}
	if !bytes.Equal(rootReads(t, h, "vm-sparse-file", template), dense) {
		t.Fatal("a VM created from the sparse file does not read the dense image")
	}
}

// A source that reports extents out of order, overlapping, or past its end is
// refused rather than imported as something other than its bytes.
func TestASparseSourceWithImpossibleExtentsIsRefused(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	h.start(t)
	for _, extents := range [][]host.Extent{
		{{Offset: sparseTail, Length: 4096}, {Offset: 0, Length: 4096}},
		{{Offset: 0, Length: 8192}, {Offset: 4096, Length: 4096}},
		{{Offset: sparseImageSize - 4096, Length: 8192}},
		{{Offset: 0, Length: 0}},
	} {
		source := &holeReader{Reader: bytes.NewReader(denseSparseImage()), t: t, extents: extents}
		_, err := h.hosts[0].TemplateOf(t.Context(), host.TemplateImport{Image: "impossible",
			Volumes: sparseVolumes, Root: "root", Source: source})
		if err == nil {
			t.Fatalf("extents %+v were imported", extents)
		}
	}
}

// A host counts the templates it writes and the image it reads: an import
// reads the data twice, once for the digest and once for the template, and a
// template another host already wrote costs only its digest.
func TestAHostCountsItsImportsAndTheImageItReads(t *testing.T) {
	h := newSizedHostHarness(t, 2)
	h.start(t)
	image := denseSparseImage()
	request := func() host.TemplateImport {
		return host.TemplateImport{Image: "sparse", Volumes: sparseVolumes, Root: "root",
			Source: &holeReader{Reader: bytes.NewReader(image), t: t,
				extents: []host.Extent{{Offset: 0, Length: 4096}, {Offset: sparseTail, Length: 4096}}}}
	}
	templateOn(t, h, 0, request())
	templateOn(t, h, 1, request())
	writer, reader := h.hosts[0].Activity(), h.hosts[1].Activity()
	if writer.Imports != (host.Outcomes{Succeeded: 1}) || writer.ImportTime.Count != 1 || writer.ImageBytes != 2*2*4096 {
		t.Fatalf("the importing host counts %+v, %d timings and %d image bytes, want one import of two data pages read twice",
			writer.Imports, writer.ImportTime.Count, writer.ImageBytes)
	}
	if reader.Imports != (host.Outcomes{}) || reader.ImageBytes != 2*4096 {
		t.Fatalf("the second host counts %+v and %d image bytes, want no import and one digest of two pages",
			reader.Imports, reader.ImageBytes)
	}
}
