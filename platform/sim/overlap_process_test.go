package sim_test

import (
	"testing"

	"github.com/semistrict/sproutfs/internal/testrepro"
)

func TestDynamicOverlapReproducesAcrossProcesses(t *testing.T) {
	testrepro.AcrossProcesses(t, "TestDynamicOverlapTraceFiles")
}
