package host_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/semistrict/sproutfs/internal/testnet"
	"github.com/semistrict/sproutfs/vmmigrate"
)

// authenticated has host n reach the cluster over mutual TLS: it presents a
// certificate identity issued and admits only peers trusted issued. The first
// peer it refuses, and why, is sent on the channel returned.
func (h *hostHarness) authenticated(t *testing.T, n int, identity, trusted *testnet.Authority) <-chan error {
	t.Helper()
	refusals := make(chan error, 1)
	transport, err := testnet.MutualTLS(h.hostID(n), identity, trusted, func(err error) {
		select {
		case refusals <- err:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	h.configs[n].Network = h.network.Over(transport)
	return refusals
}

func newAuthority(t *testing.T, name string) *testnet.Authority {
	t.Helper()
	authority, err := testnet.NewAuthority(name)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

// migrateFourPages starts the hosts, with each host's transport as the test set
// it up, and migrates a VM whose guest stored four pages from host 0 toward
// host 1. It returns the handoff and where host 1's machine will be once it
// receives the VM.
func migrateFourPages(t *testing.T, h *hostHarness, pagers []*hostPagers) (vmmigrate.Handoff, **machine) {
	t.Helper()
	received := new(*machine)
	h.configs[1].Migration.StartVM = starter(t, pagers[1], received)
	h.start(t)
	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	source, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	for page := range uint64(4) {
		source.store("ram0", page, byte(page+1))
	}
	if err := h.hosts[0].AddMachine("vm-1", source); err != nil {
		t.Fatal(err)
	}
	handoff, err := h.hosts[0].Migrate(t.Context(), "vm-1", h.pages[1])
	if err != nil {
		t.Fatal(err)
	}
	return handoff, received
}

// Hosts that authenticate one another on a transport of their own migrate a
// VM as hosts on plain TCP do: the destination fetches the pages only the
// source holds from the source's peer server.
func TestHostsMigrateOverATransportOfTheirOwn(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	deployment := newAuthority(t, "deployment")
	for n := range h.configs {
		h.authenticated(t, n, deployment, deployment)
	}
	handoff, received := migrateFourPages(t, h, pagers)
	taken, err := h.hosts[1].Receive(t.Context(), handoff)
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	if got, err := (*received).memoryRegions["ram0"].Resident(); err != nil || !slices.Equal(got, []uint64{0, 1, 2, 3}) {
		t.Fatalf("destination resident pages: got %v, %v, want the four the guest stored", got, err)
	}
	if served := h.hosts[0].Status().Pages.Served; served != 4 {
		t.Fatalf("the source served %d pages, want the four only it held", served)
	}
}

// receiveRefused has host 1 receive a handoff whose pages it can never fetch:
// a receive waits for them until its caller gives up, which happens once refusals
// has a refusal on it. It returns that refusal and what the receive returned.
func receiveRefused(t *testing.T, h *hostHarness, handoff vmmigrate.Handoff, refusals <-chan error) (refusal, err error) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(t.Context())
	received := make(chan error, 1)
	go func() {
		taken, err := h.hosts[1].Receive(ctx, handoff)
		if err == nil {
			taken.Close()
		}
		received <- err
	}()
	refusal = <-refusals
	cancel(errors.New("the test gave up on a source that was refused"))
	return refusal, <-received
}

// A peer server serves no peer its transport cannot authenticate. The
// destination's certificate comes from an authority the source does not
// trust, so the source refuses it and serves none of the pages it holds; the
// receive fails once its caller stops waiting for them.
func TestAPeerServerServesNoPeerItCannotAuthenticate(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	deployment, stranger := newAuthority(t, "deployment"), newAuthority(t, "stranger")
	sourceRefusals := h.authenticated(t, 0, deployment, deployment)
	// The destination trusts the source, so only the source can refuse.
	h.authenticated(t, 1, stranger, deployment)
	handoff, _ := migrateFourPages(t, h, pagers)
	refusal, err := receiveRefused(t, h, handoff, sourceRefusals)
	if !strings.Contains(refusal.Error(), "host-0 refused host-1") {
		t.Fatalf("the source's refusal is %v, want one of the destination", refusal)
	}
	if err == nil {
		t.Fatal("a destination the source refused received the VM")
	}
	if stats := h.hosts[0].Status().Pages; stats.Requests != 0 || stats.Served != 0 {
		t.Fatalf("the source answered %d requests and served %d pages to a peer it refused", stats.Requests, stats.Served)
	}
}

// A destination fetches nothing from a source its transport cannot
// authenticate. The source's certificate comes from an authority the
// destination does not trust, so the destination refuses it before asking for
// a page, and the receive fails once its caller stops waiting.
func TestADestinationRefusesASourceItCannotAuthenticate(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	deployment, stranger := newAuthority(t, "deployment"), newAuthority(t, "stranger")
	// The source trusts the destination, so only the destination can refuse.
	h.authenticated(t, 0, stranger, deployment)
	destinationRefusals := h.authenticated(t, 1, deployment, deployment)
	handoff, _ := migrateFourPages(t, h, pagers)
	refusal, err := receiveRefused(t, h, handoff, destinationRefusals)
	if !strings.Contains(refusal.Error(), "host-1 refused host-0") {
		t.Fatalf("the destination's refusal is %v, want one of the source", refusal)
	}
	if err == nil {
		t.Fatal("a destination received the VM from a source it refused")
	}
	if stats := h.hosts[0].Status().Pages; stats.Requests != 0 || stats.Served != 0 {
		t.Fatalf("the source answered %d requests and served %d pages to a destination that refused it",
			stats.Requests, stats.Served)
	}
}
