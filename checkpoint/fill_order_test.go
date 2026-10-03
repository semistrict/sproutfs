package checkpoint_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/rank"
)

// A host does its fills one at a time, in the order they were handed over,
// and sends a holder its next keep only once the keep before it is answered.
// Two hosts under 1+1, the first publishing three pages and the segment that
// locates them: it owes the second host a keep of each of the four windows,
// and on the link to the second host's peer server every request is answered
// before the next one goes.
//
// Keeps sent beside each other reach the link, its connection and the
// background budget in whatever order the Go scheduler runs them, so which
// of them a dropped frame or a partition takes is not the seed's choice, and
// a seed does not reproduce its run.
func TestAHostSendsItsKeepsOneAtATime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, fillConfig{hosts: 2, code: rank.Code{K: 1, M: 1}, share: 100})
		sender, holder := c.hosts[0], c.hosts[1]
		if _, _, err := publishFrom(t, sender.store, "vm", publishedPages); err != nil {
			t.Fatal(err)
		}
		c.settle(t)
		if fills := c.fills(); fills.FromPublications != 4 || fills.Sent != 4 || dropped(fills) != 0 {
			t.Fatalf("the publication's fills came to %+v, want four windows and a keep of each", fills)
		}
		requests := sender.name + "->" + string(holder.address)
		answers := string(holder.address) + "->" + sender.name
		inFlight, most, sent := 0, 0, 0
		for _, event := range c.runtime.Trace().Events() {
			if event.Kind != "network" || event.Operation != "send" {
				continue
			}
			switch event.Resource {
			case requests:
				inFlight++
				sent++
			case answers:
				inFlight--
			}
			most = max(most, inFlight)
		}
		// The hello that opens the connection, and the four keeps.
		if sent != 5 || most != 1 {
			t.Fatalf("%s sent %s %d requests with as many as %d unanswered at once, want its hello and four keeps one at a time",
				sender.name, holder.name, sent, most)
		}
	})
}
