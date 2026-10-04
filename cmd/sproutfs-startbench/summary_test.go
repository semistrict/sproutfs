package main

import (
	"math"
	"strings"
	"testing"
)

// The hosts' logs as kubectl prints them: a header per pod, lines of other
// kinds, and the start lines, not in time order across pods.
const podsLog = `=== pod/sproutfs-host-a
{"time":"2026-10-04T10:00:00.100Z","level":"INFO","msg":"host: assembled","host":"a"}
{"time":"2026-10-04T10:00:01.000Z","level":"INFO","msg":"host: a VM runs","vm":"c0","how":"create","resumed":false,"total_ms":28,"running_ms":25,"store_ms":12,"steps":[{"name":"template","at_ms":0,"took_ms":0.1},{"name":"fork","at_ms":0.1,"took_ms":5},{"name":"root","at_ms":5.1,"took_ms":10},{"name":"vmm start","at_ms":15.1,"took_ms":9.9},{"name":"vmm process","at_ms":15.1,"took_ms":8},{"name":"vmm ready","at_ms":23.1,"took_ms":1.9},{"name":"register","at_ms":26,"took_ms":1}],"store":[{"kind":"control record conditional put","calls":2,"ms":8,"max_ms":4},{"kind":"index get","calls":1,"ms":4,"max_ms":4}],"store_after":[],"attach":[{"region":"root","ms":1,"populate_ms":0.2,"populated_pages":3,"populate_commands":1}]}
{"time":"2026-10-04T10:00:02.000Z","level":"INFO","msg":"host: a VM's first faults","vm":"c0","how":"create","running_ms":25,"window_ms":1000,"faults":[{"region":"ram0","before":10,"before_waited_ms":3,"after":40,"after_waited_ms":30,"first_ms":16,"first_waited_ms":0.5}]}
{"time":"2026-10-04T10:00:03.000Z","level":"INFO","msg":"host: a VM runs","vm":"r","how":"open","resumed":true,"total_ms":19,"running_ms":18,"store_ms":9,"steps":[{"name":"open","at_ms":0,"took_ms":8},{"name":"state","at_ms":8,"took_ms":3},{"name":"release","at_ms":17,"took_ms":1}],"store":[],"store_after":[],"attach":[]}
{"time":"2026-10-04T10:00:05.000Z","level":"INFO","msg":"host: a VM forked","vm":"p","how":"fork","children":["f0"],"local":false,"total_ms":10,"store_ms":0,"steps":[{"name":"confirm","at_ms":0,"took_ms":2},{"name":"seal","at_ms":2,"took_ms":4},{"name":"pause","at_ms":2,"took_ms":3},{"name":"pin","at_ms":6,"took_ms":0.5}],"store":[{"kind":"control record get","calls":1,"ms":2,"max_ms":2}],"store_after":[],"attach":[]}
not JSON at all
=== pod/sproutfs-host-b
{"time":"2026-10-04T10:00:05.050Z","level":"INFO","msg":"host: a VM runs","vm":"f0","how":"receive","total_ms":35,"running_ms":15,"store_ms":5,"steps":[{"name":"open","at_ms":0,"took_ms":5},{"name":"release","at_ms":13,"took_ms":2},{"name":"post-copy","at_ms":15,"took_ms":19}],"store":[{"kind":"control record conditional put","calls":1,"ms":5,"max_ms":5}],"store_after":[],"attach":[]}
{"time":"2026-10-04T10:00:04.000Z","level":"INFO","msg":"host: a VM runs","vm":"r","how":"open","resumed":true,"total_ms":21,"running_ms":20,"store_ms":9,"steps":[{"name":"open","at_ms":0,"took_ms":9},{"name":"state","at_ms":9,"took_ms":4},{"name":"release","at_ms":19,"took_ms":1}],"store":[],"store_after":[],"attach":[]}
`

func summarySamples() []sample {
	return []sample{
		{Case: caseCold, Round: 0, VM: "c0", Host: "a", RequestMS: 30, Started: true, AgentMS: 500, Execs: 9},
		{Case: caseCold, Round: 1, VM: "c1", Host: "a", RequestMS: 3, Error: "create: refused"},
		{Case: caseRestore, Round: 0, VM: "r", Host: "a", From: "a", RequestMS: 20, Started: true, AgentMS: 40},
		{Case: caseRestore, Round: 1, VM: "r", Host: "b", From: "a", RequestMS: 24, Started: true, AgentMS: 45},
		{Case: caseForkRemote, Round: 0, VM: "f0", Parent: "p", Host: "b", From: "a", ForkMS: 12, RequestAtMS: 13,
			RequestMS: 40, Started: true, AgentMS: 50},
	}
}

// Each started sample is joined with its own lines: a VM restored twice is
// matched in the order its lines were logged, whichever pod logged them.
func TestEachStartIsJoinedWithItsOwnLines(t *testing.T) {
	lines, err := hostLines(strings.NewReader(podsLog))
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 6 {
		t.Fatalf("read %d start lines, want 6", len(lines))
	}
	starts := join(summarySamples(), lines)
	if len(starts) != 5 {
		t.Fatalf("joined %d starts, want 5", len(starts))
	}
	if starts[0].runs.TotalMS != 28 || starts[0].faults.RunningMS != 25 || starts[0].forked != nil {
		t.Fatalf("the cold start was joined with %+v and %+v", starts[0].runs, starts[0].faults)
	}
	if starts[1].runs != nil || starts[1].faults != nil {
		t.Fatalf("a create that failed was joined with %+v", starts[1].runs)
	}
	if starts[2].runs.TotalMS != 19 || starts[3].runs.TotalMS != 21 {
		t.Fatalf("the restores were joined with lines of %v and %v ms, want 19 and 21",
			starts[2].runs.TotalMS, starts[3].runs.TotalMS)
	}
	if starts[4].runs.TotalMS != 35 || starts[4].forked.TotalMS != 10 {
		t.Fatalf("the fork was joined with %+v and %+v", starts[4].runs, starts[4].forked)
	}
	// From the fork's request: the receive was sent at 13 ms, took 2.5 ms each
	// way beyond the host's 35, and the guest ran 15 ms into it.
	for i, want := range []float64{26, math.NaN(), 18.5, 21.5, 30.5} {
		if starts[i].runs == nil {
			continue
		}
		if got := toRunning(starts[i]); got != want {
			t.Fatalf("start %d ran %v ms after its request, want %v", i, got, want)
		}
	}
}

func TestPercentilesAreNearestRank(t *testing.T) {
	values := []float64{5, 1, 4, 2, 3, 10, 9, 8, 7, 6}
	for _, c := range []struct{ p, want float64 }{{0.5, 5}, {0.9, 9}, {0.99, 10}, {1, 10}, {0, 1}} {
		if got := percentile(values, c.p); got != c.want {
			t.Fatalf("p%v of 1..10 is %v, want %v", c.p*100, got, c.want)
		}
	}
	if got := percentile(nil, 0.5); !math.IsNaN(got) {
		t.Fatalf("the median of nothing is %v", got)
	}
}

// The report gives each case's time to running and to the agent's answer, its
// steps with the largest named, its store calls, attaches and faults, and the
// store's own times.
func TestTheSummaryReportsEveryCase(t *testing.T) {
	lines, err := hostLines(strings.NewReader(podsLog))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	store := &storeTimes{Create: []float64{30, 20}, Get: []float64{9}, Update: []float64{31}, Delete: []float64{15}}
	if err := summarize(&out, join(summarySamples(), lines), store); err != nil {
		t.Fatal(err)
	}
	want := `## From the request to the guest

| Case | starts | failed | to running p50 | p90 | p99 | max | agent answers p50 | p99 | store p50 | p99 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| cold | 1 | 1 | 26.0 | 26.0 | 26.0 | 26.0 | 500 | 500 | 12.0 | 12.0 |
| restore, same host | 1 | 0 | 18.5 | 18.5 | 18.5 | 18.5 | 40.0 | 40.0 | 9.00 | 9.00 |
| restore, other host | 1 | 0 | 21.5 | 21.5 | 21.5 | 21.5 | 45.0 | 45.0 | 9.00 | 9.00 |
| fork-remote | 1 | 0 | 30.5 | 30.5 | 30.5 | 30.5 | 50.0 | 50.0 | 5.00 | 5.00 |

## cold

| Step | p50 ms | p99 ms |
| --- | --- | --- |
| request: one way | 1.00 | 1.00 |
| template | 0.10 | 0.10 |
| fork | 5.00 | 5.00 |
| root | 10.0 | 10.0 |
| vmm start | 9.90 | 9.90 |
| vmm process | 8.00 | 8.00 |
| vmm ready | 1.90 | 1.90 |

The largest step at the median: root, 10.0 ms.

Store calls before the guest runs, per start:

| | p50 | p99 |
| --- | --- | --- |
| control record conditional put | 8.00 | 8.00 |
| control record conditional put calls | 2.00 | 2.00 |
| index get | 4.00 | 4.00 |
| index get calls | 1.00 | 1.00 |

Attach:

| | p50 | p99 |
| --- | --- | --- |
| root attach | 1.00 | 1.00 |
| root populate | 0.20 | 0.20 |
| root populated pages | 3.00 | 3.00 |

Faults:

| | p50 | p99 |
| --- | --- | --- |
| ram0 faults before running | 10.0 | 10.0 |
| ram0 faults in the first second | 40.0 | 40.0 |
| ram0 ms waited in them | 30.0 | 30.0 |
| ram0 first fault, ms from running | -9.00 | -9.00 |
| ram0 first fault, ms waited | 0.50 | 0.50 |

## restore, same host

| Step | p50 ms | p99 ms |
| --- | --- | --- |
| request: one way | 0.50 | 0.50 |
| open | 8.00 | 8.00 |
| state | 3.00 | 3.00 |
| release | 1.00 | 1.00 |

The largest step at the median: open, 8.00 ms.

## restore, other host

| Step | p50 ms | p99 ms |
| --- | --- | --- |
| request: one way | 1.50 | 1.50 |
| open | 9.00 | 9.00 |
| state | 4.00 | 4.00 |
| release | 1.00 | 1.00 |

The largest step at the median: open, 9.00 ms.

## fork-remote

| Step | p50 ms | p99 ms |
| --- | --- | --- |
| parent: confirm | 2.00 | 2.00 |
| parent: seal | 4.00 | 4.00 |
| parent: pause | 3.00 | 3.00 |
| parent: pin | 0.50 | 0.50 |
| parent: fork round trip beyond the host | 2.00 | 2.00 |
| control plane: fork answer to receive request | 1.00 | 1.00 |
| request: one way | 2.50 | 2.50 |
| open | 5.00 | 5.00 |
| release | 2.00 | 2.00 |

The largest step at the median: open, 5.00 ms.

Store calls before the guest runs, per start:

| | p50 | p99 |
| --- | --- | --- |
| control record conditional put | 5.00 | 5.00 |
| control record conditional put calls | 1.00 | 1.00 |
| parent: control record get | 2.00 | 2.00 |
| parent: control record get calls | 1.00 | 1.00 |

## The store alone

| Call | count | p50 ms | p90 | p99 | max |
| --- | --- | --- | --- | --- | --- |
| create if absent | 2 | 20.0 | 30.0 | 30.0 | 30.0 |
| read | 1 | 9.00 | 9.00 | 9.00 | 9.00 |
| compare-and-set | 1 | 31.0 | 31.0 | 31.0 | 31.0 |
| delete | 1 | 15.0 | 15.0 | 15.0 | 15.0 |
`
	if got := out.String(); got != want {
		t.Fatalf("the summary is\n%s\nwant\n%s", got, want)
	}
}
