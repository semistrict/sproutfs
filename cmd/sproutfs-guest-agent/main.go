// Command sproutfs-guest-agent runs inside a demo guest. It is what makes a VM
// useful to anything but a person at a serial console: the host asks it to run
// a command, or proxies an HTTP request to it, and it answers.
//
// It is reached over the VM's virtio-vsock device rather than over a network,
// so the guest has no address, nothing allocates one, and a fork or a migration
// carries nothing: the device is part of the VMM state a restore replays, and
// the socket on the other side of it belongs to whichever host process is
// running the machine.
//
// The guest image runs it from init, and it is static: the image carries no Go
// runtime and no shared libraries of ours.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/semistrict/sproutfs/internal/api/guest"
)

// shutdownTimeout bounds the orderly stop. The agent holds nothing, so this is
// only about letting a command that is already running finish reporting.
const shutdownTimeout = 5 * time.Second

func main() {
	log.SetFlags(0)
	log.SetPrefix("sproutfs-guest-agent: ")
	if err := run(); err != nil {
		log.Printf("exiting: %v", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	port := guest.Port
	if raw := os.Getenv("SPROUTFS_AGENT_PORT"); raw != "" {
		if _, err := fmt.Sscanf(raw, "%d", &port); err != nil || port <= 0 || port > 0xffff {
			return fmt.Errorf("SPROUTFS_AGENT_PORT is %q, want a port", raw)
		}
	}
	listener, err := listen(port)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler: newServer(0),
		// A command runs for as long as its own timeout allows, so only the
		// read of the request itself is bounded here.
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	failed := make(chan error, 1)
	go func() {
		log.Printf("serving on vsock port %d", port)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- err
			return
		}
		failed <- nil
	}()
	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}
