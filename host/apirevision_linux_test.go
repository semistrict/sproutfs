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

// blindStarter speaks this host's revision and boots guests that would not act
// on a new generation ID.
type blindStarter struct{ olderStarter }

func (blindStarter) APIRevision(context.Context) (int, error) { return vmmachine.APIRevision, nil }

func (blindStarter) CheckGuest(context.Context) error {
	return errors.New("the guest kernel has no VMGenID driver built in (CONFIG_VMGENID=y)")
}

// A host whose guests would draw the same random bytes in every child of a
// fork point refuses to start, before it has built anything a VM could run on.
func TestStartRefusesAGuestThatWouldNotActOnANewGeneration(t *testing.T) {
	service, err := host.Start(t.Context(), host.SupervisorConfig{Starter: blindStarter{}})
	if !errors.Is(err, vmmachine.ErrGuestDevices) {
		t.Fatalf("got %v, %v; want ErrGuestDevices", service, err)
	}
}
