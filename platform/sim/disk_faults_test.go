package sim_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// diskOperations is every operation of a disk and its files that the device
// can fail, by the name its I/O error site carries.
var diskOperations = []string{"open", "read", "write", "truncate", "punch_hole", "allocate", "sync", "size",
	"remove", "rename", "list", "sync_namespace", "allocated"}

// spaceOperations is every operation the filesystem can refuse for want of
// space, by the name its site carries.
var spaceOperations = []string{"open", "write", "allocate", "rename", "sync"}

// modelFile is what one file holds as a writer that knows its own operations
// sees it: its bytes now, and the bytes its last sync made durable.
type modelFile struct {
	volatile, durable []byte
	synced            bool
	handle            platform.File
}

// diskModel runs operations against a disk under Buggify and holds what each
// should have left, failed or not.
type diskModel struct {
	t       *testing.T
	runtime *sim.Runtime
	disk    *sim.Disk
	files   map[string]*modelFile
}

// Under Buggify the device fails every operation of a disk at random, and the
// filesystem refuses those that need space, and what a failed one leaves is
// what the real adapter's leaves: nothing, except a create, a remove or a
// rename whose directory sync failed, which happened, and a write, which may
// leave a prefix of its bytes. A failed sync makes nothing durable, which a
// power loss at the end of each seed shows. Across the seeds every site
// fires.
func TestEveryDiskOperationFailsAtRandomAndLeavesWhatTheDeviceWould(t *testing.T) {
	fired := map[string]bool{}
	for seed := uint64(1); seed <= 64; seed++ {
		synctest.Test(t, func(t *testing.T) {
			runtime := sim.New(sim.Config{Seed: seed, Buggify: true})
			m := &diskModel{t: t, runtime: runtime, disk: runtime.NewDisk("disk", sim.DiskConfig{}),
				files: map[string]*modelFile{}}
			random := runtime.Random("disk-faults-test")
			for step := range 800 {
				id := fmt.Sprintf("%d", step)
				name := []string{"a", "b", "c"}[random.Intn(id+"/name", 3)]
				m.step(diskOperations[random.Intn(id+"/operation", len(diskOperations))], name,
					[]string{"a", "b", "c"}[random.Intn(id+"/to", 3)], random, id)
			}
			m.loseThePower()
			for site := range runtime.FiredSites() {
				fired[site] = true
			}
		})
	}
	for _, operation := range diskOperations {
		if !fired["sim/disk/io-error/"+operation] {
			t.Errorf("the device never failed a %s", operation)
		}
	}
	for _, operation := range spaceOperations {
		if !fired["sim/disk/no-space/"+operation] {
			t.Errorf("the filesystem never refused a %s for want of space", operation)
		}
	}
}

// quietly runs check with the runtime's sites off, so what the disk holds is
// read without a fault.
func (m *diskModel) quietly(check func()) {
	m.runtime.SetBuggify(false)
	defer m.runtime.SetBuggify(true)
	check()
}

// names is what the disk lists, read quietly.
func (m *diskModel) names() []string {
	var names []string
	m.quietly(func() {
		var err error
		if names, err = m.disk.List(m.t.Context(), ""); err != nil {
			m.t.Fatal(err)
		}
	})
	return names
}

// failed reports whether err is the device failing or the filesystem out of
// space, and fails the test on any other error.
func (m *diskModel) failed(operation string, err error) bool {
	m.t.Helper()
	if err != nil && !errors.Is(err, platform.ErrInjectedFault) && !errors.Is(err, platform.ErrNoSpace) {
		m.t.Fatalf("%s: %v", operation, err)
	}
	return err != nil
}

func (m *diskModel) step(operation, name, to string, random sim.Random, id string) {
	t, ctx := m.t, m.t.Context()
	t.Helper()
	file := m.files[name]
	switch operation {
	case "open":
		handle, err := m.disk.Open(ctx, name, platform.OpenOptions{Create: true})
		existed := file != nil
		if m.failed("open", err) {
			created := slices.Contains(m.names(), name)
			switch {
			case errors.Is(err, platform.ErrNoSpace) && (existed || created || !errors.Is(err, platform.ErrFileUnchanged)):
				t.Fatalf("an open refused for want of space changed %s or did not say it was unchanged", name)
			case existed && !created:
				t.Fatalf("a failed open of %s removed it", name)
			case !existed && created:
				// The create happened before its directory sync failed.
				m.files[name] = &modelFile{}
			}
			return
		}
		if !existed {
			file = &modelFile{}
			m.files[name] = file
		}
		file.handle = handle
	case "remove":
		err := m.disk.Remove(ctx, name)
		if file == nil {
			// A file that is not there stays not there, however the remove
			// ends.
			if !errors.Is(err, platform.ErrNotFound) {
				m.failed("remove", err)
			}
			return
		}
		if m.failed("remove", err) && slices.Contains(m.names(), name) {
			return
		}
		delete(m.files, name)
	case "rename":
		err := m.disk.Rename(ctx, name, to)
		if file == nil {
			if !errors.Is(err, platform.ErrNotFound) {
				m.failed("rename", err)
			}
			if want := m.wantNames(); !slices.Equal(m.names(), want) {
				t.Fatalf("a rename of %s, which is not there, left %v, want %v", name, m.names(), want)
			}
			return
		}
		if name == to {
			m.failed("rename", err)
			return
		}
		if m.failed("rename", err) && slices.Contains(m.names(), name) {
			return
		}
		if errors.Is(err, platform.ErrNoSpace) {
			t.Fatalf("a rename of %s refused for want of space moved it", name)
		}
		m.files[to] = file
		delete(m.files, name)
	case "list":
		names, err := m.disk.List(ctx, "")
		if m.failed("list", err) {
			return
		}
		if want := m.wantNames(); !slices.Equal(names, want) {
			t.Fatalf("the disk lists %v, want %v", names, want)
		}
	case "sync_namespace":
		m.failed("sync_namespace", m.disk.SyncNamespace(ctx))
	default:
		if file == nil || file.handle == nil {
			return
		}
		m.fileStep(operation, name, file, random, id)
	}
}

func (m *diskModel) fileStep(operation, name string, file *modelFile, random sim.Random, id string) {
	t, ctx := m.t, m.t.Context()
	t.Helper()
	handle := file.handle
	size := int64(len(file.volatile))
	switch operation {
	case "read":
		if size == 0 {
			return
		}
		offset := int64(random.Intn(id+"/offset", int(size)))
		got := make([]byte, size-offset)
		n, err := handle.ReadAt(ctx, got, offset)
		if m.failed("read", err) {
			return
		}
		if !bytes.Equal(got[:n], file.volatile[offset:]) {
			t.Fatalf("%s read %d bytes at %d that were not written there", name, n, offset)
		}
	case "write":
		data := bytes.Repeat([]byte{byte(1 + random.Intn(id+"/byte", 255))}, 1+random.Intn(id+"/length", 9000))
		offset := int64(random.Intn(id+"/offset", int(size)+1))
		n, err := handle.WriteAt(ctx, data, offset)
		if m.failed("write", err) && errors.Is(err, platform.ErrNoSpace) && n != 0 {
			t.Fatalf("a write refused for want of space wrote %d bytes", n)
		}
		file.volatile = written(file.volatile, offset, data[:n])
	case "truncate":
		to := int64(random.Intn(id+"/size", 20000))
		if !m.failed("truncate", handle.Truncate(ctx, to)) {
			file.volatile = resized(file.volatile, to)
		}
	case "punch_hole":
		offset, length := int64(random.Intn(id+"/offset", 20000)), int64(1+random.Intn(id+"/length", 9000))
		err := handle.(platform.SparseFile).PunchHole(ctx, offset, length)
		if !m.failed("punch_hole", err) && offset < size {
			clear(file.volatile[offset:min(offset+length, size)])
		}
	case "allocate":
		offset, length := int64(random.Intn(id+"/offset", 20000)), int64(1+random.Intn(id+"/length", 9000))
		err := handle.(platform.AllocatingFile).Allocate(ctx, offset, length)
		if !m.failed("allocate", err) && offset+length > size {
			file.volatile = resized(file.volatile, offset+length)
		}
	case "sync":
		if !m.failed("sync", handle.Sync(ctx)) {
			file.durable, file.synced = slices.Clone(file.volatile), true
		}
	case "size":
		got, err := handle.Size(ctx)
		if !m.failed("size", err) && got != size {
			t.Fatalf("%s is %d bytes, want %d", name, got, size)
		}
	case "allocated":
		_, err := handle.(platform.FileAllocation).Allocated(ctx)
		m.failed("allocated", err)
	}
}

// wantNames is the files the model holds, in order.
func (m *diskModel) wantNames() []string {
	var names []string
	for name := range m.files {
		names = append(names, name)
	}
	slices.Sort(names)
	if names == nil {
		names = []string{}
	}
	return names
}

// loseThePower loses the disk's power and checks that each file holds what
// its last successful sync made durable, and that a file never synced is
// gone.
func (m *diskModel) loseThePower() {
	t, ctx := m.t, m.t.Context()
	m.quietly(func() {
		if err := m.disk.PowerLoss(ctx); err != nil {
			t.Fatal(err)
		}
		for name, file := range m.files {
			handle, err := m.disk.Open(ctx, name, platform.OpenOptions{})
			if !file.synced {
				if !errors.Is(err, platform.ErrNotFound) {
					t.Errorf("%s, never synced, survived the power loss: %v", name, err)
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(file.durable)+1)
			n, err := handle.ReadAt(ctx, got, 0)
			if err != nil && !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			if !bytes.Equal(got[:n], file.durable) {
				t.Errorf("%s holds %d bytes after the power loss, not the %d its last sync made durable", name, n,
					len(file.durable))
			}
		}
	})
}

// written is data written over file at offset, extending it with zeroes.
func written(file []byte, offset int64, data []byte) []byte {
	file = resized(file, max(int64(len(file)), offset+int64(len(data))))
	copy(file[offset:], data)
	return file
}

// resized is file cut or grown with zeroes to size.
func resized(file []byte, size int64) []byte {
	if size <= int64(len(file)) {
		return file[:size]
	}
	return append(file, make([]byte, size-int64(len(file)))...)
}
