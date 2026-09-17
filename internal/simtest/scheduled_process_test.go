package simtest_test

import (
	"testing"

	"github.com/semistrict/sproutfs/internal/testrepro"
)

func TestScheduledWorldReproducesAcrossProcesses(t *testing.T) {
	testrepro.AcrossProcesses(t, "TestScheduledWorldReproduces")
}
