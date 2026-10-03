package real

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/semistrict/sproutfs/platform"
)

// A block device's stat file counts sectors of 512 bytes in its seventh field.
func TestDeviceWritesReadSectorsWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stat")
	// The fields of /sys/block/sda/stat: reads, merges, sectors read, ticks,
	// writes, merges, sectors written, ticks, in flight, io ticks, queue ticks.
	stat := "    1500       10    96000     800     2200      30   123456    1700        0     2000     2500\n"
	if err := os.WriteFile(path, []byte(stat), 0o600); err != nil {
		t.Fatal(err)
	}
	written, err := deviceWrites{path: path}.BytesWritten(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if written != 123456*512 {
		t.Fatalf("the device wrote %d bytes, want %d", written, 123456*512)
	}
}

func TestDeviceWritesRefuseAStatTheyCannotRead(t *testing.T) {
	for _, text := range []string{"", "1 2 3 4 5 6", "1 2 3 4 5 6 many", "1 2 3 4 5 6 36028797018963968"} {
		if _, err := parseBlockStat(text); !errors.Is(err, platform.ErrInvalidRange) {
			t.Fatalf("parsing %q returned %v, want %v", text, err, platform.ErrInvalidRange)
		}
	}
}
