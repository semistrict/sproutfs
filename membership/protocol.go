package membership

// The probes of the protocol every request that routes by the membership
// follows: the sender names the generation it holds, a holder behind it reads
// the membership before it answers, a holder ahead answers that the sender is
// stale, and the sender then reads the membership and asks again. The peer
// server marks the holder's side, and the cache's reads and fills the
// sender's. A campaign over a cluster whose membership changes reaches them.
const (
	// ProbeHolderCaughtUp is a holder behind a request's generation that read
	// the membership before it answered.
	ProbeHolderCaughtUp = "membership/holder-caught-up"
	// ProbeStaleAnswered is a request answered stale: its holder ahead of it,
	// or behind it and unable to read the membership.
	ProbeStaleAnswered = "membership/stale-answered"
	// ProbeNotServed is a request naming a disk the membership, at the
	// request's generation, does not have the holder serve.
	ProbeNotServed = "membership/not-served"
	// ProbeSenderCaughtUp is a sender told it was stale that read the
	// membership and asked again under the newer generation.
	ProbeSenderCaughtUp = "membership/sender-caught-up"
)

// ProtocolProbes is every probe of the protocol.
var ProtocolProbes = []string{ProbeHolderCaughtUp, ProbeStaleAnswered, ProbeNotServed, ProbeSenderCaughtUp}
