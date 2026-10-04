package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/platform"
)

// volume is the one volume the guest has: its RAM.
const volume = "ram0"

// guest is a guest's memory, held whole in this process as a pager's arena
// holds it. Its pages compress about as far as the Valkey heap of
// docs/measurements/gce-real-app-restore-2026-10-03.md did (6.09 GB to about
// 2.8 GB): each 64-byte line is 28 bytes of noise and a 36-byte text record
// that names its line's group, which the encoder finds again in the lines
// around it.
type guest struct {
	memory []byte
}

// noiseBytes is how much of each line is noise.
const (
	lineBytes  = 64
	noiseBytes = 28
)

// newGuest builds pages pages of memory, on every processor.
func newGuest(pages uint64) *guest {
	g := &guest{memory: make([]byte, pages*checkpoint.PageSize2MiB)}
	workers := uint64(runtime.GOMAXPROCS(0))
	var wait sync.WaitGroup
	for worker := range workers {
		wait.Go(func() {
			for page := worker; page < pages; page += workers {
				fillPage(page, g.memory[page*checkpoint.PageSize2MiB:][:checkpoint.PageSize2MiB])
			}
		})
	}
	wait.Wait()
	return g
}

// fillPage writes one page's contents.
func fillPage(page uint64, dst []byte) {
	state := page*0x9e3779b97f4a7c15 ^ 0xd1b54a32d192ed03
	var text []byte
	lastGroup := ^uint64(0)
	for at := 0; at+lineBytes <= len(dst); at += lineBytes {
		line := dst[at : at+lineBytes]
		for word := 0; word+8 <= noiseBytes; word += 8 {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
			binary.LittleEndian.PutUint64(line[word:], state)
		}
		binary.LittleEndian.PutUint32(line[24:], uint32(state))
		if group := (page*checkpoint.PageSize2MiB + uint64(at)) / (16 * lineBytes); group != lastGroup {
			text, lastGroup = fmt.Appendf(text[:0], "value:%010d:payload-text-#", group), group
		}
		copy(line[noiseBytes:], text)
	}
}

func (g *guest) pages() uint64 { return uint64(len(g.memory)) / checkpoint.PageSize2MiB }

// timedSource is the guest's memory as a publication reads it, timing each
// read.
type timedSource struct {
	guest *guest
	nanos atomic.Int64
}

func (s *timedSource) ReadPage(_ context.Context, _ string, page uint64, dst []byte) error {
	started := time.Now()
	copy(dst, s.guest.memory[page*checkpoint.PageSize2MiB:][:checkpoint.PageSize2MiB])
	s.nanos.Add(int64(time.Since(started)))
	return nil
}

func (s *timedSource) seconds() float64 { return time.Duration(s.nanos.Load()).Seconds() }

// puts is what one commit's PUTs did.
type puts struct {
	Count int   `json:"puts"`
	Bytes int64 `json:"put_bytes"`
	// Seconds is the PUTs' time summed, and MeanMBPerSecond their bytes over
	// that sum: what one PUT moves a second while it runs.
	Seconds         float64 `json:"put_seconds"`
	MeanMBPerSecond float64 `json:"put_mb_per_s"`
	// Peak is the most PUTs in flight at once, and Mean the PUTs in flight
	// averaged over the commit.
	Peak int     `json:"puts_in_flight_peak"`
	Mean float64 `json:"puts_in_flight_mean"`
}

// putMeter counts the PUTs a store makes and how many are in flight.
type putMeter struct {
	platform.ObjectStore
	mu       sync.Mutex
	inFlight int
	changed  time.Time
	// busy is the integral of the PUTs in flight over time, in PUT-seconds.
	busy  float64
	total puts
}

func (m *putMeter) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inFlight, m.changed, m.busy, m.total = 0, time.Now(), 0, puts{}
}

// step accounts the time since the last change at the old count and moves the
// count by delta.
func (m *putMeter) step(delta int) {
	now := time.Now()
	m.busy += float64(m.inFlight) * now.Sub(m.changed).Seconds()
	m.changed = now
	m.inFlight += delta
	m.total.Peak = max(m.total.Peak, m.inFlight)
}

func (m *putMeter) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	m.mu.Lock()
	m.step(1)
	m.mu.Unlock()
	started := time.Now()
	result, err := m.ObjectStore.Put(ctx, request)
	took := time.Since(started).Seconds()
	m.mu.Lock()
	m.step(-1)
	if err == nil {
		m.total.Count++
		m.total.Bytes += request.Size
		m.total.Seconds += took
	}
	m.mu.Unlock()
	return result, err
}

// measured is what the PUTs did over a commit of seconds.
func (m *putMeter) measured(seconds float64) puts {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.step(0)
	result := m.total
	result.Mean = m.busy / seconds
	if result.Seconds > 0 {
		result.MeanMBPerSecond = float64(result.Bytes) / result.Seconds / 1e6
	}
	return result
}
