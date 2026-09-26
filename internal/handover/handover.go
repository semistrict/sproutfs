// Package handover is how a deployment carries one handoff to a destination
// that takes it.
//
// A migration's source stops the guest and gives its volumes up before any
// destination is asked to take the VM. From then on the pages no checkpoint has
// exist only in the source's pages, which it serves until it is told a
// destination has them all, or until its own hold deadline. A receive that
// fails leaves the handoff exactly as good as it was: the destination
// published nothing, so the control record still selects the checkpoint the
// handoff names, and the source still serves every page. So a failed receive
// is tried again, on the same destination or another, for as long as the
// source holds the pages. Giving up earlier loses the guest's writes since its
// last checkpoint for nothing.
//
// This is the policy alone: when to try again, where, and when to stop. What
// counts as evidence that another attempt is safe is the caller's, because
// only the caller can see the deployment.
package handover

import (
	"context"
	"slices"
	"time"

	"github.com/semistrict/sproutfs/platform/sim"
)

// ProbeRetried marks a receive tried again after one failed.
const ProbeRetried = "handover/retried"

// Policy is how the receive of one handoff is retried.
type Policy struct {
	// Pause is the wait before the first retry. Each wait after it doubles, up
	// to MaxPause. A destination that is briefly unreachable is back within a
	// few seconds, and one that is not costs a look every MaxPause.
	Pause, MaxPause time.Duration
	// PerDestination is how many attempts in a row one destination gets before
	// the next goes to another host that can take the VM. A failure is often
	// that host's own, so trying it for the whole hold wastes the hold.
	PerDestination int
}

// Default is the deployment's policy. A source holds a handoff for four
// checkpoint intervals, four minutes by default, which this spends in about
// twenty looks.
var Default = Policy{Pause: time.Second, MaxPause: 15 * time.Second, PerDestination: 2}

// Attempts is one handoff's receives. It is not safe for concurrent use: one
// handoff is carried by one caller.
type Attempts struct {
	policy   Policy
	deadline time.Time
	at       string
	streak   int
	failures map[string]int
	pause    time.Duration
}

// Begin starts the receives of a handoff whose source holds its pages for
// hold from now, at the destination first. A hold of zero is a source that
// promises nothing, so its handoff is tried once.
func (p Policy) Begin(now time.Time, hold time.Duration, first string) *Attempts {
	return &Attempts{policy: p, deadline: now.Add(hold), at: first, failures: map[string]int{}}
}

// At is the destination the next receive goes to.
func (a *Attempts) At() string { return a.at }

// Failed records that the receive at At failed.
func (a *Attempts) Failed() {
	a.failures[a.at]++
	a.streak++
}

// Wait is how long to wait before looking at the deployment again, and false
// once the source's hold ends before then. The handoff is not worth another
// attempt after that: the source gives the pages up on its own, and a receive
// started then fails for want of them.
func (a *Attempts) Wait(ctx context.Context, now time.Time) (time.Duration, bool) {
	if sim.Bug(ctx, "migration-give-up-first-receive") {
		return 0, false
	}
	switch {
	case a.pause == 0:
		a.pause = a.policy.Pause
	case a.pause < a.policy.MaxPause:
		a.pause = min(2*a.pause, a.policy.MaxPause)
	}
	if !now.Add(a.pause).Before(a.deadline) {
		return 0, false
	}
	return a.pause, true
}

// Next chooses the destination of the next receive from the hosts that could
// take the VM now, in the caller's order of preference, and reports false when
// there is none. The destination that failed last is kept for PerDestination
// attempts in a row. After that the next goes to the candidate that has failed
// least, the earliest of them on a tie, and back to the same one only when
// nothing else can take the VM.
func (a *Attempts) Next(ctx context.Context, candidates []string) (string, bool) {
	if len(candidates) == 0 {
		return "", false
	}
	next := a.at
	if a.streak >= a.policy.PerDestination || !slices.Contains(candidates, a.at) {
		next = a.leastFailed(candidates)
	}
	if next != a.at {
		a.at, a.streak = next, 0
	}
	sim.Probe(ctx, ProbeRetried)
	return next, true
}

// leastFailed is the candidate other than At that has failed least, or At
// itself when it is the only one.
func (a *Attempts) leastFailed(candidates []string) string {
	best := ""
	for _, candidate := range candidates {
		if candidate == a.at {
			continue
		}
		if best == "" || a.failures[candidate] < a.failures[best] {
			best = candidate
		}
	}
	if best == "" {
		return a.at
	}
	return best
}
