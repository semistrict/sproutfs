package host_test

import (
	"context"
	"errors"
	"testing"

	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/vmmachine"
)

// olderStarter reports the revision before this host's and starts nothing.
type olderStarter struct{}

func (olderStarter) Start(context.Context, *vmmachine.Launch) (vmmachine.VMM, error) {
	return nil, errors.New("an olderStarter starts nothing")
}

func (olderStarter) Boots() bool { return true }

func (olderStarter) APIRevision(context.Context) (int, error) { return vmmachine.APIRevision - 1, nil }

// A host whose VMM predates the API it drives refuses to start, before it has
// built anything a VM could run on.
func TestStartRefusesAnOlderVMM(t *testing.T) {
	service, err := host.Start(t.Context(), host.SupervisorConfig{Starter: olderStarter{}})
	if !errors.Is(err, vmmachine.ErrAPIRevision) {
		t.Fatalf("got %v, %v; want ErrAPIRevision", service, err)
	}
}
