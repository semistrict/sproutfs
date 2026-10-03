package real

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/semistrict/sproutfs/platform"
)

// sectorBytes is the unit a block device's stat file counts sectors in. The
// kernel uses 512 bytes there whatever the device's own sector size is.
const sectorBytes = 512

// writtenSectorsField is the position of "sectors written" in a block device's
// stat file, counting from zero.
const writtenSectorsField = 6

// deviceWrites reads the bytes a block device has written from its stat file.
type deviceWrites struct{ path string }

func (d deviceWrites) BytesWritten(ctx context.Context) (uint64, error) {
	if err := context.Cause(ctx); err != nil {
		return 0, err
	}
	text, err := os.ReadFile(d.path)
	if err != nil {
		return 0, err
	}
	return parseBlockStat(string(text))
}

// parseBlockStat reads the bytes written from the text of a block device's stat
// file.
func parseBlockStat(text string) (uint64, error) {
	fields := strings.Fields(text)
	if len(fields) <= writtenSectorsField {
		return 0, fmt.Errorf("a block device's stat has %d fields, want at least %d: %w",
			len(fields), writtenSectorsField+1, platform.ErrInvalidRange)
	}
	sectors, err := strconv.ParseUint(fields[writtenSectorsField], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("a block device's sectors written are %q: %w",
			fields[writtenSectorsField], platform.ErrInvalidRange)
	}
	if sectors > math.MaxUint64/sectorBytes {
		return 0, fmt.Errorf("a block device reports %d sectors written: %w", sectors, platform.ErrInvalidRange)
	}
	return sectors * sectorBytes, nil
}

var _ platform.DeviceWrites = deviceWrites{}
