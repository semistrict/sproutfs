package peer

import "context"

// Admitter orders the decision to make one request to a source against
// everything else a controlled run is running. It is given the region the
// request is for — the VM and the volume — and returns the reason the request
// must not be made, which is normally the cancellation the caller has just
// been given.
//
// A deployment installs none. It exists because deciding to ask the source for
// pages is a decision, not an I/O operation: a destination that is cancelled
// while its post-copy stream is between two requests either sends the next one
// and has it refused on the wire, or abandons it before anything is sent, and
// both are correct. Without a point a controller can order, which of the two
// happens is the Go runtime's choice, and a recording of the run cannot be
// reproduced. Pooling makes it unavoidable rather than incidental: an idle
// connection is taken without dialing, so the request reaches the wire with no
// adapter operation between the cancellation and the send.
type Admitter func(ctx context.Context, region string) error

type admissionKey struct{}

// WithAdmission installs admit for every request made by a Source under ctx.
// The stream a destination runs behind its guest inherits this context, so one
// call at the top of a controlled workload covers the requests of every region
// it receives.
func WithAdmission(ctx context.Context, admit Admitter) context.Context {
	if admit == nil {
		panic("nil peer admitter")
	}
	return context.WithValue(ctx, admissionKey{}, admit)
}

// admit runs the context's admitter, where a controlled run installed one.
func admit(ctx context.Context, region string) error {
	admitter, ok := ctx.Value(admissionKey{}).(Admitter)
	if !ok {
		return nil
	}
	return admitter(ctx, region)
}

type streamKey struct{}

// WithStream marks the requests made under ctx as the post-copy stream's. The
// stream fetches pages behind a running guest, and a guest fault waits on the
// source while it does. So a Source keeps one connection that stream requests
// never use, and records the two kinds of request in separate histograms.
func WithStream(ctx context.Context) context.Context {
	return context.WithValue(ctx, streamKey{}, true)
}

// streaming reports a request made by the post-copy stream.
func streaming(ctx context.Context) bool {
	stream, _ := ctx.Value(streamKey{}).(bool)
	return stream
}
