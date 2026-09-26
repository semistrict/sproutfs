//go:build linux && (amd64 || arm64)

package vmmemory_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/vmwire"
	"github.com/semistrict/sproutfs/vmmemory"
)

// The hostile sessions of hostile_linux_test.go send the pager a made-up
// descriptor, so the pager cannot resolve a fault and the session ends at the
// first one. Seal, retire and settle never run under a hostile session there.
// The tests here close that gap: a real client process holds a real
// userfaultfd, its guest faults its memory in, and the test seals, settles and
// retires it as a capture does. A proxy sits on the client's control socket. It
// forwards the whole session faithfully, so the guest runs, and then sends the
// pager one frame the client never would. The pager must end that session with
// an error of its own and leave its well-behaved neighbour whole, exactly as it
// does a hostile session that never resolved a fault.

// hostileProxy forwards one memory region's control socket between a real
// client and the pager, and sends the pager frames of its own on demand. It is
// the compromised VMM: the client below it is honest, and everything the pager
// sees that breaks the protocol comes from here.
type hostileProxy struct {
	t      *testing.T
	client *net.UnixConn
	// toPager is the proxy's end of the socket the pager was given. The pager
	// reads the client's control frames here, and the proxy's injected ones.
	toPager *net.UnixConn

	writeMu sync.Mutex
	// lastAck is the last mapping acknowledgement the client sent, which a
	// duplicate injection repeats.
	lastAck   vmwire.Frame
	haveAck   bool
	closeOnce sync.Once
	// forwarding is the two directions, which a teardown waits for so every
	// descriptor the proxy holds is closed before the host counts its own.
	forwarding sync.WaitGroup
}

// proxied starts a hostile proxy in front of a client's socket and returns it
// and the socket to hand the pager. The two forwarding directions run until
// either end closes.
func (fx *hostileFixture) proxied(t *testing.T, client *net.UnixConn) (*hostileProxy, *net.UnixConn) {
	t.Helper()
	ours, theirs := socketPair(t)
	p := &hostileProxy{t: t, client: client, toPager: ours}
	p.forwarding.Add(2)
	go p.pagerToClient()
	go p.clientToPager()
	return p, theirs
}

// wait blocks until both forwarding directions have stopped, which is once the
// pager and the client process have both closed their ends. Every descriptor
// the proxy held is closed by then.
func (p *hostileProxy) wait() { p.forwarding.Wait() }

// shutdown closes both of the proxy's own descriptors, once. The pager's end of
// the socket and the client's process own the other two.
func (p *hostileProxy) shutdown() {
	p.closeOnce.Do(func() {
		p.writeMu.Lock()
		defer p.writeMu.Unlock()
		_ = p.toPager.Close()
		_ = p.client.Close()
	})
}

// pagerToClient forwards everything the pager sends, with the descriptor a FILE
// frame carries. The client is honest, so this direction is never touched.
func (p *hostileProxy) pagerToClient() {
	defer p.forwarding.Done()
	defer p.shutdown()
	for {
		f, file, err := vmwire.Receive(p.toPager)
		if err != nil {
			return
		}
		if file != nil {
			err = vmwire.SendFD(p.client, f, file)
			_ = file.Close()
		} else {
			err = vmwire.WriteBytes(p.client, f.Bytes())
		}
		if err != nil {
			return
		}
	}
}

// clientToPager forwards the client's control frames to the pager, holding the
// HELLO's userfaultfd. It records the last acknowledgement so a duplicate can
// be sent, and stops once the proxy has closed the stream to the pager.
func (p *hostileProxy) clientToPager() {
	defer p.forwarding.Done()
	defer p.shutdown()
	for {
		f, file, err := vmwire.Receive(p.client)
		if err != nil {
			return
		}
		if f.Kind == vmwire.Ack {
			p.writeMu.Lock()
			p.lastAck, p.haveAck = f, true
			p.writeMu.Unlock()
		}
		p.writeMu.Lock()
		if file != nil {
			err = vmwire.SendFD(p.toPager, f, file)
			_ = file.Close()
		} else {
			err = vmwire.WriteBytes(p.toPager, f.Bytes())
		}
		p.writeMu.Unlock()
		if err != nil {
			return
		}
	}
}

// send writes one frame straight to the pager, past the client. Caller has not
// taken the write lock.
func (p *hostileProxy) send(f vmwire.Frame) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err := vmwire.WriteBytes(p.toPager, f.Bytes()); err != nil {
		p.t.Logf("injecting %s into the hostile client's session: %v", vmwire.KindName(f.Kind), err)
	}
}

// hangUp closes the proxy's stream to the pager, which is a VMM that stops
// answering. The pager reads EOF and ends the session.
func (p *hostileProxy) hangUp() { p.shutdown() }

// hostileClientOp is one thing a compromised VMM does after its guest has run
// through a capture. Each is a frame the honest client never sends, or a hang
// up.
type hostileClientOp uint8

const (
	// injectAck sends an acknowledgement the pager awaits nothing for.
	injectAck hostileClientOp = iota
	// duplicateAck repeats the last acknowledgement the client sent.
	duplicateAck
	// injectStaleSeal sends a seal request of id zero.
	injectStaleSeal
	// injectFlush sends a flush on the RAM session, which only PMEM may flush.
	injectFlush
	// injectUnknown sends a control frame of an unknown kind.
	injectUnknown
	// injectGarbage sends a partial frame and hangs up.
	injectGarbage
	// hangUpMidSession closes the stream to the pager.
	hangUpMidSession
	hostileClientOps
)

// apply performs one op and reports what the pager ends the session with.
func (p *hostileProxy) apply(op hostileClientOp) {
	switch op {
	case injectAck:
		p.send(vmwire.Frame{Kind: vmwire.Ack, ID: 1 << 40})
	case duplicateAck:
		p.writeMu.Lock()
		ack, have := p.lastAck, p.haveAck
		p.writeMu.Unlock()
		if !have {
			ack = vmwire.Frame{Kind: vmwire.Ack, ID: 1 << 40}
		}
		p.send(ack)
	case injectStaleSeal:
		p.send(vmwire.Frame{Kind: vmwire.Seal, ID: 0})
	case injectFlush:
		p.send(vmwire.Frame{Kind: vmwire.Flush, ID: 1 << 40})
	case injectUnknown:
		p.send(vmwire.Frame{Kind: 99, ID: 1})
	case injectGarbage:
		p.writeMu.Lock()
		_ = vmwire.WriteBytes(p.toPager, vmwire.Frame{Kind: vmwire.Seal, ID: 1}.Bytes()[:20])
		p.writeMu.Unlock()
		p.hangUp()
	case hangUpMidSession:
		p.hangUp()
	}
}

// hostileClient is a real client process whose RAM control socket a proxy sits
// on. Its guest faults its RAM in, and the test captures it, before the proxy
// misbehaves.
type hostileClient struct {
	process *nativeProcess
	proxy   *hostileProxy
	ram     *kernelBacking
}

// startHostileClient starts a real client of its own tenant beside the
// fixture's neighbour, with a proxy on its RAM session. Its RAM is hostilePages
// pages, every page its own byte.
func (fx *hostileFixture) startHostileClient(t *testing.T) *hostileClient {
	t.Helper()
	var provided []vmmemory.Backing
	var volumes [2]*kernelBacking
	for region := range volumes {
		b := newPagedKernelBacking(byte(30+region), hostilePages*hostilePage, hostilePage)
		b.inTenant("hostileclient")
		for i := range b.data {
			b.data[i] = byte(150 + i/hostilePage)
		}
		volumes[region] = b
		provided = append(provided, b)
	}
	hc := &hostileClient{ram: volumes[1]}
	proxy := func(region int, client *net.UnixConn) *net.UnixConn {
		if region != 1 {
			return nil
		}
		p, pagerSide := fx.proxied(t, client)
		hc.proxy = p
		return pagerSide
	}
	hc.process = startNativeOptions(t, fx.h, "hostileclient", hostilePages,
		vmmemory.ConnectionConfig{Name: "hostileclient", QueuePages: hostilePages,
			CommandTimeout: 5 * time.Second, VerifyInterval: time.Hour}, clientOptions{proxy: proxy}, provided...)
	return hc
}

// capture drives the client's guest through one round the proxy forwards
// whole: it stores into a page of its RAM, which faults on the real
// userfaultfd, seals that RAM, settles the checkpoint, and retires it. A round
// runs every step a hostile session with a made-up descriptor never reaches.
func (hc *hostileClient) capture(ctx context.Context, page int, value byte) error {
	if err := hc.process.ask(fmt.Sprintf("fill 1 %d %d %d", page*hostilePage, hostilePage, value), "filled"); err != nil {
		return err
	}
	if err := hc.process.ask("seal 1", "sealed"); err != nil {
		return err
	}
	r := hc.process.memoryRegion(1)
	if _, err := r.Checkpoint().Settle(ctx); err != nil {
		return err
	}
	published, err := hc.ram.publish(ctx, r.Checkpoint())
	return errors.Join(err, r.Checkpoint().Retire(ctx, published))
}

// hostileClientCases are the frames a compromised VMM sends the pager after
// its guest has run through a capture, each with what the pager ends the
// session saying.
var hostileClientCases = []struct {
	name string
	op   hostileClientOp
	ends string
}{
	{"an acknowledgement of nothing", injectAck, "unexpected mapping ACK"},
	{"a duplicate acknowledgement", duplicateAck, "unexpected mapping ACK"},
	{"a seal request of id zero", injectStaleSeal, "invalid seal request"},
	{"a flush of RAM", injectFlush, "a flush of a ram memory region"},
	{"an unknown control message", injectUnknown, "unexpected managed-memory control message"},
	{"a frame cut short then a hang up", injectGarbage, "EOF"},
	{"a hang up", hangUpMidSession, "EOF"},
}

// A real client whose guest faults its RAM in, is sealed, settled and retired,
// and whose control socket then sends the pager one frame the client never
// would, ends its session with an error of its own. Its well-behaved neighbour
// on the same pager keeps its bytes, and the pager and the host get back
// everything the session held.
func TestAHostileClientWithARealUFFDIsEndedThroughACapture(t *testing.T) {
	for _, c := range hostileClientCases {
		t.Run(c.name, func(t *testing.T) {
			fx := newHostileFixtureFor(t, suiteArena, 8)
			hc := fx.startHostileClient(t)
			// The fixture's baseline is the neighbour alone. The hostile client
			// attaches, runs and is torn down, so the pager must come back to it.
			// Two full captures on the real userfaultfd before the VMM turns.
			for round := range 2 {
				if err := hc.capture(t.Context(), round, byte(160+round)); err != nil {
					t.Fatalf("capturing the honest client before it turns: %v", err)
				}
			}
			end := fx.turnClient(t, hc, c.op)
			if !errorSays(end, c.ends) {
				t.Fatalf("the hostile client's session ended with %q, want it to say %q", end, c.ends)
			}
			fx.requireWhole(t, fmt.Sprintf("a hostile client that sent %s after a capture", c.name))
		})
	}
}

// FuzzHostileClient drives a real client's guest through a chosen number of
// captures on its real userfaultfd — a fault, a seal, a settle and a retire
// each — and then sends the pager one frame the honest client never would. The
// pager must end that session and leave its neighbour whole, whatever the
// number of captures and whichever frame. One fixture lives through every
// input, so the neighbour must survive them all.
func FuzzHostileClient(f *testing.F) {
	for _, c := range hostileClientCases {
		f.Add(uint8(2), uint8(c.op))
	}
	fx := newHostileFixtureFor(f, suiteArena, 8)
	f.Fuzz(func(t *testing.T, captures, op uint8) {
		hc := fx.startHostileClient(t)
		for round := range int(captures % 4) {
			if err := hc.capture(t.Context(), round%hostilePages, byte(160+round)); err != nil {
				t.Fatalf("capturing the honest client before it turns: %v", err)
			}
		}
		if end := fx.turnClient(t, hc, hostileClientOp(op)%hostileClientOps); end == nil {
			t.Fatal("the hostile client's session ended without an error")
		}
		fx.requireWhole(t, fmt.Sprintf("a hostile client with %d captures and op %d", captures%4, op))
	})
}

// turnClient has the proxy misbehave while the neighbour stores, waits for the
// RAM session to end, and tears the client down. It reports how the session
// ended.
func (fx *hostileFixture) turnClient(t *testing.T, hc *hostileClient, op hostileClientOp) error {
	t.Helper()
	fx.stores++
	page := hostilePages/2 + fx.stores%(hostilePages/2)
	value := byte(100 + fx.stores%100)
	stored := make(chan error, 1)
	go func() { stored <- fx.store(t.Context(), page, value) }()

	hc.proxy.apply(op)
	ram := hc.process.connections[1]
	ctx, cancel := context.WithTimeout(t.Context(), hostileBound)
	defer cancel()
	err := ram.Wait(ctx)
	if ctx.Err() != nil {
		t.Fatalf("the pager did not end the hostile client's session within %s", hostileBound)
	}

	select {
	case storeErr := <-stored:
		if storeErr != nil {
			t.Fatalf("the well-behaved process beside the hostile client: %v", storeErr)
		}
	case <-time.After(hostileBound):
		t.Fatalf("a store of the well-behaved process did not complete within %s", hostileBound)
	}
	fx.values[1][page] = value
	// The client process is reaped here, so the pipes the host held to it are
	// closed before the host counts its descriptors.
	hc.process.reap()
	// The hostile session's pages go back to the pager only once its sessions
	// are closed, as they do for the fixture's fake peer. The RAM session's
	// failure ends the client process, so its PMEM session is closed here too.
	// The fixture's cleanup then finds them closed.
	for i, c := range hc.process.connections {
		if c == nil {
			continue
		}
		if closeErr := c.Close(ctx); closeErr != nil {
			t.Errorf("closing the hostile client's %s session: %v", []string{"pmem", "ram"}[i], closeErr)
		}
		hc.process.connections[i] = nil
	}
	// The proxy's own descriptors close with its forwarding, which the pager's
	// closed session and the dead client process both end. Wait for that so the
	// host's descriptor count is what it was before the session.
	hc.proxy.wait()
	return err
}
