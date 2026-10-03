package peer

import "github.com/semistrict/sproutfs/platform/sim"

// The places in this package a campaign must reach, as sim.Probe marks them. A
// fault is worth injecting only where it makes code run, so a campaign over
// the peer server asserts that every one of these fired across its seeds.
const (
	// ProbeBusy is a request answered BUSY: its peer's class at its budget.
	ProbeBusy = "peer/busy"
	// ProbeIncompatible is a hello answered INCOMPATIBLE.
	ProbeIncompatible = "peer/incompatible"
	// ProbeWaitedForBudget is a request that waited for its class's budget at
	// this end before it was sent.
	ProbeWaitedForBudget = "peer/waited-for-budget"
	// ProbeLateReply is a reply that arrived for a request its caller had
	// already given up on.
	ProbeLateReply = "peer/late-reply"
	// ProbeFellBackToVersionOne is a hello closed unanswered, after which the
	// dialer spoke version 1.
	ProbeFellBackToVersionOne = "peer/fell-back-to-version-one"
	// ProbeDeadConnection is a connection closed for hearing nothing.
	ProbeDeadConnection = "peer/dead-connection"
	// ProbeMarkedDown is a peer marked down by a hard failure.
	ProbeMarkedDown = "peer/marked-down"
	// ProbeProbed is a down peer dialed again to see whether it is back.
	ProbeProbed = "peer/probed"
	// ProbeSkippedDown is a request that could do without a down peer and
	// was not sent to it.
	ProbeSkippedDown = "peer/skipped-down"
)

// Probes is every probe this package registers.
var Probes = []string{ProbeBusy, ProbeIncompatible, ProbeWaitedForBudget, ProbeLateReply,
	ProbeFellBackToVersionOne, ProbeDeadConnection, ProbeMarkedDown, ProbeProbed, ProbeSkippedDown}

// The fault-injection sites in this package. Each is a fault the peer server
// must survive without its callers seeing more than a slower answer or an
// error they retry.
const (
	// SiteBusy answers BUSY as a server whose peer is at its budget does: a
	// campaign with one migration at a time never makes a server busy on its
	// own.
	SiteBusy = "peer/busy"
	// SiteSlowAnswer delays building one reply by up to two seconds, as a
	// disk that stalls does: the replies behind it on its connection wait,
	// and its caller may give up on it.
	SiteSlowAnswer = "peer/slow-answer"
	// SiteStall stops a server reading one connection for up to six seconds,
	// as a process paused by its host does: its pings go unanswered, and a
	// stall past DeadAfter is a dead connection to its dialer.
	SiteStall = "peer/stall"
)

// Sites is every fault-injection site this package registers.
var Sites = []string{SiteBusy, SiteSlowAnswer, SiteStall}

// probe marks name reached on the table's runtime, whoever's request reached
// it, and bug reports an in-tree bug guard enabled there.
func (t *Table) probe(name string)  { sim.Probe(t.ctx, name) }
func (t *Table) bug(id string) bool { return sim.Bug(t.ctx, id) }

// probe and bug are the same on the server's runtime.
func (s *Server) probe(name string)  { sim.Probe(s.ctx, name) }
func (s *Server) bug(id string) bool { return sim.Bug(s.ctx, id) }
