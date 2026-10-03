package real_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/internal/real"
)

func TestDiskMissingRemovalStillRequiresDirectoryDurability(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission failures")
	}
	root := t.TempDir()
	directory := filepath.Join(root, "nested")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	disk, err := real.NewDisk(root)
	if err != nil {
		t.Fatal(err)
	}
	// Traversal and unlink are permitted, but opening the parent for fsync
	// fails. Absence alone cannot resolve an earlier uncertain unlink.
	if err := os.Chmod(directory, 0o300); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(directory, 0o700)
	err = disk.Remove(t.Context(), "nested/missing")
	if err == nil || errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("reported durable absence without syncing its directory: %v", err)
	}
}

func TestDiskPersistsSyncedContent(t *testing.T) {
	t.Parallel()
	disk, err := real.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := disk.Open(t.Context(), "logs/tail", platform.OpenOptions{Create: true, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt(t.Context(), []byte("record"), 0); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	file, err = disk.Open(t.Context(), "logs/tail", platform.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	buffer := make([]byte, len("record"))
	if _, err := file.ReadAt(t.Context(), buffer, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if got, want := string(buffer), "record"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

func TestDiskListsNestedFilesByPrefix(t *testing.T) {
	t.Parallel()
	disk, err := real.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"logs/c", "other", "logs/a"} {
		file, err := disk.Open(t.Context(), name, platform.OpenOptions{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	names, err := disk.List(t.Context(), "logs")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"logs/a", "logs/c"}) {
		t.Fatalf("List = %v, want [logs/a logs/c]", names)
	}
}

func TestDiskRejectsTraversal(t *testing.T) {
	t.Parallel()
	disk, err := real.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := disk.Open(t.Context(), "../outside", platform.OpenOptions{Create: true}); !errors.Is(err, platform.ErrInvalidPath) {
		t.Fatalf("Open traversal error = %v, want ErrInvalidPath", err)
	}
}

// A locked file refuses a second lock, here from another disk over the same
// directory, until the handle holding it closes. flock locks the open file
// description, so a second handle in one process is refused exactly as a
// handle in another process is. A
// handle that asks for no lock is not refused, and a lock does not combine with
// a truncation.
func TestALockedFileRefusesASecondLockUntilItCloses(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first, err := real.NewDisk(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := real.NewDisk(root)
	if err != nil {
		t.Fatal(err)
	}
	locked := platform.OpenOptions{Create: true, Lock: true}
	held, err := first.Open(t.Context(), "cache-0", locked)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Open(t.Context(), "cache-0", locked); !errors.Is(err, platform.ErrLocked) {
		t.Fatalf("a second lock of a held file returned %v, want %v", err, platform.ErrLocked)
	}
	plain, err := second.Open(t.Context(), "cache-0", platform.OpenOptions{})
	if err != nil {
		t.Fatalf("a handle that asks for no lock was refused: %v", err)
	}
	if err := plain.Close(); err != nil {
		t.Fatal(err)
	}
	other, err := second.Open(t.Context(), "cache-1", locked)
	if err != nil {
		t.Fatalf("another file's lock was refused: %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := second.Open(t.Context(), "cache-0", locked)
	if err != nil {
		t.Fatalf("the lock of a closed handle was not given up: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	truncating := platform.OpenOptions{Lock: true, Truncate: true}
	if _, err := first.Open(t.Context(), "cache-0", truncating); !errors.Is(err, platform.ErrInvalidPath) {
		t.Fatalf("a lock with a truncation returned %v, want %v", err, platform.ErrInvalidPath)
	}
}
