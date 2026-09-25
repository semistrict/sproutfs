package vmmachine_test

import (
	"slices"
	"strings"
	"testing"
)

// Match complete fields on a complete line. Flush status may include an
// additional offset field after the expected disk value.
func containsGuestStatus(output, want string) bool {
	wanted := strings.Fields(want)
	for {
		line, rest, complete := strings.Cut(output, "\n")
		if !complete {
			return false
		}
		fields := strings.Fields(line)
		if len(fields) >= len(wanted) && slices.Equal(fields[:len(wanted)], wanted) {
			return true
		}
		output = rest
	}
}

func TestGuestStatusRequiresCompleteFields(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
		want   string
		match  bool
	}{
		{"exact", "SPROUTFS_VALUE ram=73 disk=41\n", "SPROUTFS_VALUE ram=73 disk=41", true},
		{"wrong disk", "SPROUTFS_VALUE ram=73 disk=410\n", "SPROUTFS_VALUE ram=73 disk=41", false},
		{"wrong RAM", "SPROUTFS_VALUE ram=730 disk=41\n", "SPROUTFS_VALUE ram=73 disk=41", false},
		{"incomplete line", "SPROUTFS_VALUE ram=73 disk=41", "SPROUTFS_VALUE ram=73 disk=41", false},
		{"embedded status", "echo SPROUTFS_VALUE ram=73 disk=41\n", "SPROUTFS_VALUE ram=73 disk=41", false},
		{"other output", "booting\nSPROUTFS_VALUE ram=73 disk=41\r\n", "SPROUTFS_VALUE ram=73 disk=41", true},
		{"flush offset", "SPROUTFS_FLUSH disk=41 offset=8192\n", "SPROUTFS_FLUSH disk=41", true},
		{"wrong flush", "SPROUTFS_FLUSH disk=410 offset=8192\n", "SPROUTFS_FLUSH disk=41", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := containsGuestStatus(test.output, test.want); got != test.match {
				t.Fatalf("status match = %t, want %t for %q", got, test.match, test.output)
			}
		})
	}
}
