package main

import (
	"bufio"
	"fmt"
	"math"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"time"
)

// clientStats is what the client process's Go runtime did over a case: its
// garbage collector and its scheduler. Merged records add them up.
type clientStats struct {
	GCCycles uint64 `json:"gc_cycles"`
	// GCPause is the time the world was stopped for the collector.
	GCPause time.Duration `json:"gc_pause_ns"`
	// GCCPU is the CPU time the collector used: its pauses, its marking and
	// the goroutines it made help.
	GCCPU time.Duration `json:"gc_cpu_ns"`
	// GCPauses is the stop-the-world pauses, one by one.
	GCPauses histogram `json:"gc_pauses"`
	// Sched is how long goroutines waited, ready to run, for a thread.
	Sched histogram `json:"sched_latency"`
}

func (s *clientStats) add(o clientStats) {
	s.GCCycles += o.GCCycles
	s.GCPause += o.GCPause
	s.GCCPU += o.GCCPU
	s.GCPauses.add(o.GCPauses)
	s.Sched.add(o.Sched)
}

// runtimeSample is the runtime's counters at one moment.
type runtimeSample struct {
	cycles   uint64
	pauseCPU float64 // seconds, GOMAXPROCS times the pauses
	gcCPU    float64 // seconds
	pauses   *metrics.Float64Histogram
	sched    *metrics.Float64Histogram
	procs    int
}

var runtimeMetrics = []string{
	"/gc/cycles/total:gc-cycles",
	"/cpu/classes/gc/pause:cpu-seconds",
	"/cpu/classes/gc/total:cpu-seconds",
	"/sched/pauses/total/gc:seconds",
	"/sched/latencies:seconds",
}

func sampleRuntime() runtimeSample {
	samples := make([]metrics.Sample, len(runtimeMetrics))
	for i, name := range runtimeMetrics {
		samples[i].Name = name
	}
	metrics.Read(samples)
	return runtimeSample{
		cycles:   samples[0].Value.Uint64(),
		pauseCPU: samples[1].Value.Float64(),
		gcCPU:    samples[2].Value.Float64(),
		pauses:   samples[3].Value.Float64Histogram(),
		sched:    samples[4].Value.Float64Histogram(),
		procs:    runtime.GOMAXPROCS(0),
	}
}

// since is what the runtime did between before and a.
func (a runtimeSample) since(before runtimeSample) clientStats {
	return clientStats{
		GCCycles: a.cycles - before.cycles,
		// The runtime counts a pause as GOMAXPROCS times its length.
		GCPause:  seconds((a.pauseCPU - before.pauseCPU) / float64(a.procs)),
		GCCPU:    seconds(a.gcCPU - before.gcCPU),
		GCPauses: histogramSince(before.pauses, a.pauses),
		Sched:    histogramSince(before.sched, a.sched),
	}
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// histogramSince is what a runtime histogram of seconds counted between
// before and after, each observation put at the middle of its bucket, or at
// the bucket's finite bound when the other is infinite.
func histogramSince(before, after *metrics.Float64Histogram) histogram {
	var h histogram
	for i, n := range after.Counts {
		if n -= before.Counts[i]; n == 0 {
			continue
		}
		lo, hi := after.Buckets[i], after.Buckets[i+1]
		v := (lo + hi) / 2
		switch {
		case math.IsInf(lo, -1):
			v = hi
		case math.IsInf(hi, 1):
			v = lo
		}
		h.observeN(seconds(max(v, 0)), n)
	}
	return h
}

// hostStats is what the client's host did over a case, from Linux's counters.
// Merged records add them up over the hosts.
type hostStats struct {
	// Hosts is how many hosts' counters are summed: none off Linux.
	Hosts int `json:"hosts"`
	// CPU time by kind, over all the host's CPUs. User includes nice, and Idle
	// includes waiting for I/O.
	User    time.Duration `json:"user_ns"`
	System  time.Duration `json:"system_ns"`
	IRQ     time.Duration `json:"irq_ns"`
	SoftIRQ time.Duration `json:"softirq_ns"`
	Steal   time.Duration `json:"steal_ns"`
	Idle    time.Duration `json:"idle_ns"`
	// TCP segments sent, segments sent again, and retransmission timeouts.
	TCPOut      uint64 `json:"tcp_out_segs"`
	TCPRetrans  uint64 `json:"tcp_retrans_segs"`
	TCPTimeouts uint64 `json:"tcp_timeouts"`
}

func (s *hostStats) add(o hostStats) {
	s.Hosts += o.Hosts
	s.User += o.User
	s.System += o.System
	s.IRQ += o.IRQ
	s.SoftIRQ += o.SoftIRQ
	s.Steal += o.Steal
	s.Idle += o.Idle
	s.TCPOut += o.TCPOut
	s.TCPRetrans += o.TCPRetrans
	s.TCPTimeouts += o.TCPTimeouts
}

// since is what one host's counters counted between before and s. Where
// either could not be read, it is nothing.
func (s hostStats) since(before hostStats) hostStats {
	if s.Hosts == 0 || before.Hosts == 0 {
		return hostStats{}
	}
	return hostStats{
		Hosts:       1,
		User:        s.User - before.User,
		System:      s.System - before.System,
		IRQ:         s.IRQ - before.IRQ,
		SoftIRQ:     s.SoftIRQ - before.SoftIRQ,
		Steal:       s.Steal - before.Steal,
		Idle:        s.Idle - before.Idle,
		TCPOut:      s.TCPOut - before.TCPOut,
		TCPRetrans:  s.TCPRetrans - before.TCPRetrans,
		TCPTimeouts: s.TCPTimeouts - before.TCPTimeouts,
	}
}

// clockTick is Linux's USER_HZ, the unit of /proc/stat, which is 100 on every
// architecture Linux supports.
const clockTick = 10 * time.Millisecond

// parseHost reads one host's counters from the text of /proc/stat,
// /proc/net/snmp and /proc/net/netstat.
func parseHost(stat, snmp, netstat string) (hostStats, error) {
	line, _, _ := strings.Cut(stat, "\n")
	fields := strings.Fields(line)
	// cpu user nice system idle iowait irq softirq steal ...
	if len(fields) < 9 || fields[0] != "cpu" {
		return hostStats{}, fmt.Errorf("/proc/stat begins %q, want the cpu line", line)
	}
	var ticks [8]time.Duration
	for i := range ticks {
		n, err := strconv.ParseUint(fields[i+1], 10, 64)
		if err != nil {
			return hostStats{}, fmt.Errorf("/proc/stat: %w", err)
		}
		ticks[i] = time.Duration(n) * clockTick
	}
	counters := map[string]uint64{}
	for _, text := range []string{snmp, netstat} {
		if err := parseCounters(text, counters); err != nil {
			return hostStats{}, err
		}
	}
	s := hostStats{
		Hosts: 1,
		User:  ticks[0] + ticks[1], System: ticks[2], Idle: ticks[3] + ticks[4],
		IRQ: ticks[5], SoftIRQ: ticks[6], Steal: ticks[7],
	}
	for name, into := range map[string]*uint64{
		"Tcp:OutSegs": &s.TCPOut, "Tcp:RetransSegs": &s.TCPRetrans, "TcpExt:TCPTimeouts": &s.TCPTimeouts,
	} {
		n, ok := counters[name]
		if !ok {
			return hostStats{}, fmt.Errorf("no counter %s", name)
		}
		*into = n
	}
	return s, nil
}

// parseCounters reads the text of /proc/net/snmp or /proc/net/netstat into
// counters, as "Tcp:RetransSegs". The files hold pairs of lines under one
// prefix: the counters' names, then their values.
func parseCounters(text string, counters map[string]uint64) error {
	lines := bufio.NewScanner(strings.NewReader(text))
	for lines.Scan() {
		names := strings.Fields(lines.Text())
		if !lines.Scan() {
			return fmt.Errorf("counters %v have no values", names)
		}
		values := strings.Fields(lines.Text())
		if len(names) == 0 || len(values) != len(names) || values[0] != names[0] {
			return fmt.Errorf("counters %q do not match values %q", names, values)
		}
		for i, name := range names[1:] {
			// Some of /proc/net/snmp's values, such as Tcp's MaxConn, are
			// signed; none of those this reads is.
			n, err := strconv.ParseInt(values[i+1], 10, 64)
			if err != nil {
				return fmt.Errorf("counter %s%s: %w", names[0], name, err)
			}
			counters[names[0]+name] = uint64(n)
		}
	}
	return lines.Err()
}
