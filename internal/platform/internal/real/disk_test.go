package real_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/internal/real"
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
