package peer_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// A table's bounds are exact: each limit is itself accepted, and one past it
// is refused.
func TestATableIsCheckedAtItsBounds(t *testing.T) {
	dial := func(context.Context, platform.Address) (platform.Conn, error) { return nil, errors.New("unused") }
	for _, test := range []struct {
		name   string
		config peer.TableConfig
		valid  bool
	}{
		{"one version", peer.TableConfig{Versions: peer.Versions{Min: 2, Max: 2}}, true},
		{"no version", peer.TableConfig{Versions: peer.Versions{Min: 3, Max: 2}}, false},
		{"version zero", peer.TableConfig{Versions: peer.Versions{Min: 0, Max: 2}}, false},
		{"a dead allowance as long as the ping", peer.TableConfig{PingInterval: time.Second, DeadAfter: time.Second}, false},
		{"a dead allowance just past the ping", peer.TableConfig{PingInterval: time.Second,
			DeadAfter: time.Second + time.Nanosecond}, true},
		{"probes that never grow", peer.TableConfig{ProbeFirst: 2 * time.Second, ProbeMax: 2 * time.Second}, true},
		{"probes that would shrink", peer.TableConfig{ProbeFirst: 2 * time.Second,
			ProbeMax: 2*time.Second - time.Nanosecond}, false},
		{"budgets of one byte", peer.TableConfig{Budgets: peer.Budgets{Fault: 1, BulkRead: 1, BulkWrite: 1}}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.config.Dial = dial
			table, err := peer.NewTable(t.Context(), test.config)
			if err == nil {
				_ = table.Close()
			}
			if valid := err == nil; valid != test.valid || (!valid && !errors.Is(err, peer.ErrInvalid)) {
				t.Fatalf("NewTable = %v, want valid %v", err, test.valid)
			}
		})
	}
}

// A table reports every peer it was asked for, in address order.
func TestATableReportsItsPeersInAddressOrder(t *testing.T) {
	table, err := peer.NewTable(t.Context(), peer.TableConfig{
		Dial: func(context.Context, platform.Address) (platform.Conn, error) { return nil, errors.New("unused") }})
	if err != nil {
		t.Fatal(err)
	}
	defer table.Close()
	for _, address := range []platform.Address{"host-c:8081", "host-a:8081", "host-b:8081"} {
		table.Peer(address)
	}
	var got []platform.Address
	for _, status := range table.Status() {
		got = append(got, status.Address)
	}
	if want := []platform.Address{"host-a:8081", "host-b:8081", "host-c:8081"}; !slices.Equal(got, want) {
		t.Fatalf("the table reports %v, want %v", got, want)
	}
}

// A server's bounds are exact too.
func TestAServerIsCheckedAtItsBounds(t *testing.T) {
	const largest = 256 << 20
	everything := peer.Budgets{Fault: largest, BulkRead: largest, BulkWrite: largest}
	for _, test := range []struct {
		name   string
		config peer.ServerConfig
		valid  bool
	}{
		{"the smallest page", peer.ServerConfig{PageSize: 512}, true},
		{"a page below it", peer.ServerConfig{PageSize: 511}, false},
		{"the largest page", peer.ServerConfig{PageSize: largest, Budgets: everything}, true},
		{"a page past it", peer.ServerConfig{PageSize: largest + 1, Budgets: everything}, false},
		{"replies of the largest payload", peer.ServerConfig{PageSize: pageSize, MaxPagesPerRequest: largest / pageSize}, true},
		{"replies past it", peer.ServerConfig{PageSize: pageSize, MaxPagesPerRequest: largest/pageSize + 1}, false},
		{"one request a connection", peer.ServerConfig{PageSize: pageSize, MaxInFlight: 1}, true},
		{"budgets of one page", peer.ServerConfig{PageSize: pageSize,
			Budgets: peer.Budgets{Fault: pageSize, BulkRead: pageSize, BulkWrite: pageSize}}, true},
		{"a budget under a page", peer.ServerConfig{PageSize: pageSize,
			Budgets: peer.Budgets{Fault: pageSize - 1, BulkRead: pageSize, BulkWrite: pageSize}}, false},
		{"one version", peer.ServerConfig{PageSize: pageSize, Versions: peer.Versions{Min: 1, Max: 1}}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := sim.New(sim.Config{Seed: 1})
			test.config.Network, test.config.Address = runtime.Network(), "source"
			server, err := peer.NewServer(t.Context(), test.config)
			if err == nil {
				_ = server.Close()
			}
			if valid := err == nil; valid != test.valid || (!valid && !errors.Is(err, peer.ErrInvalid)) {
				t.Fatalf("NewServer = %v, want valid %v", err, test.valid)
			}
		})
	}
}

// A server takes a listener its caller opened in place of a network, and
// needs one or the other.
func TestAServerTakesAListenerInPlaceOfANetwork(t *testing.T) {
	runtime := sim.New(sim.Config{Seed: 1})
	listener, err := runtime.Network().Listen("source")
	if err != nil {
		t.Fatal(err)
	}
	server, err := peer.NewServer(t.Context(), peer.ServerConfig{Listener: listener, Address: "source", PageSize: pageSize})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	for _, config := range []peer.ServerConfig{{Address: "source"}, {Network: runtime.Network()}} {
		if _, err := peer.NewServer(t.Context(), config); !errors.Is(err, peer.ErrInvalid) {
			t.Fatalf("NewServer(%+v) = %v, want ErrInvalid", config, err)
		}
	}
}
