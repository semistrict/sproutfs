package peer

import "context"

// Admitter orders the decision to make one request to a peer against
// everything else a controlled run is running. It is given what the request is
// for — the VM and the volume of a page request — and returns the reason the
// request must not be made, which is normally the cancellation the caller has
// just been given.
//
// A deployment installs none. It exists because deciding to ask the source for
// pages is a decision, not an I/O operation: a destination that is cancelled
// while its post-copy stream is between two requests either sends the next one
// and has it refused on the wire, or abandons it before anything is sent, and
// both are correct. Without a point a controller can order, which of the two
// happens is the Go runtime's choice, and a recording of the run cannot be
// reproduced. Pooling makes it unavoidable rather than incidental: an open
// connection is taken without dialing, so the request reaches the wire with no
// adapter operation between the cancellation and the send.
//
// A request that has to wait for room — its class's budget at the peer, a
// slot on a connection, a dial another request began, or the host's background
// budget — is offered to the admitter again each time it is woken. One room
// given back wakes every request waiting for it, beside the request whose dial
// or reply gave it back, and with no point between the wake and the wire the
// Go runtime would choose which of them takes which connection and goes first
// on it.
type Admitter func(ctx context.Context, memoryRegion string) error

type admissionKey struct{}

// WithAdmission installs admit for every page request made to a Peer under ctx.
// The stream a destination runs behind its guest inherits this context, so one
// call at the top of a controlled workload covers the requests of every memory region
// it receives.
func WithAdmission(ctx context.Context, admit Admitter) context.Context {
	if admit == nil {
		panic("nil peer admitter")
	}
	return context.WithValue(ctx, admissionKey{}, admit)
}

// readmit offers a request woken from a wait for room to the admitter again,
// as it was before it first looked for room. A request made under no admission
// name is never admitted, and is not here either.
func (p *Peer) readmit(ctx context.Context, memoryRegion string) error {
	if memoryRegion == "" || p.table.bug("peer-woken-requests-go-on-together") {
		return nil
	}
	return admit(ctx, memoryRegion)
}

// admit runs the context's admitter, where a controlled run installed one.
func admit(ctx context.Context, memoryRegion string) error {
	admitter, ok := ctx.Value(admissionKey{}).(Admitter)
	if !ok {
		return nil
	}
	return admitter(ctx, memoryRegion)
}
