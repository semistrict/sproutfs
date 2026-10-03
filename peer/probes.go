package peer

import (
	"context"

	"github.com/semistrict/sproutfs/platform/sim"
)

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
)

// Probes is every probe this package registers.
var Probes = []string{ProbeBusy, ProbeIncompatible, ProbeWaitedForBudget, ProbeLateReply,
	ProbeFellBackToVersionOne}

func probeWaitedForBudget(ctx context.Context) { sim.Probe(ctx, ProbeWaitedForBudget) }
func probeLateReply(ctx context.Context)       { sim.Probe(ctx, ProbeLateReply) }
func probeFellBack(ctx context.Context)        { sim.Probe(ctx, ProbeFellBackToVersionOne) }
