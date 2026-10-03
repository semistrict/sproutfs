package main

import (
	"fmt"
	"time"
)

type walkSettings struct {
	steps, scan, chunk uint64
	budget             time.Duration
}

// phase is what one phase of a walk did: how many requests it made, how long
// it took from its first request to its last answer, and each request's time in
// microseconds, in order. A phase stopped by its budget says so, and how far it
// got is the length of Micros.
type phase struct {
	Requests uint64  `json:"requests"`
	Seconds  float64 `json:"seconds"`
	Stopped  bool    `json:"stopped,omitempty"`
	Micros   []int64 `json:"micros"`
}

// walkReport is what a walk prints. ConnectAt is when the connection was up,
// and ConnectMicros how long that took from the process's start, which is part
// of what a restored guest pays before its first request.
type walkReport struct {
	ConnectAt     time.Time `json:"-"`
	ConnectMicros int64     `json:"connect_micros"`
	Chase         phase     `json:"chase"`
	Scan          phase     `json:"scan"`
}

// walk follows the chain and then reads the sorted set in rank order. It
// returns what it measured even when it fails part way, so a walk that found a
// wrong byte still says how far it got and how long each step took.
func walk(c *client, d dataset, settings walkSettings) (walkReport, error) {
	report := walkReport{ConnectAt: time.Now()}
	report.Chase.Micros = make([]int64, 0, settings.steps)
	began := time.Now()
	key := d.step(0)
	for s := range settings.steps {
		if time.Since(began) > settings.budget {
			report.Chase.Stopped = true
			break
		}
		asked := time.Now()
		got, err := c.do([]byte("GET"), keyName(key))
		took := time.Since(asked)
		report.Chase.Micros = append(report.Chase.Micros, took.Microseconds())
		report.Chase.Requests++
		if err != nil {
			return report, fmt.Errorf("step %d, GET %s: %w", s, keyName(key), err)
		}
		if got.Nil {
			return report, fmt.Errorf("step %d: %s is missing", s, keyName(key))
		}
		next, err := d.checkValue(key, got.Bulk)
		if err != nil {
			return report, fmt.Errorf("step %d: %w", s, err)
		}
		if want := d.step(s + 1); next != want {
			return report, fmt.Errorf("step %d: %s names %s next, and the chain goes to %s",
				s, keyName(key), keyName(next), keyName(want))
		}
		key = next
	}
	report.Chase.Seconds = time.Since(began).Seconds()

	report.Scan.Micros = make([]int64, 0, (settings.scan+settings.chunk-1)/settings.chunk)
	began = time.Now()
	for first := uint64(0); first < settings.scan; first += settings.chunk {
		if time.Since(began) > settings.budget {
			report.Scan.Stopped = true
			break
		}
		last := min(first+settings.chunk, settings.scan) - 1
		asked := time.Now()
		got, err := c.do([]byte("ZRANGE"), []byte(zset), []byte(fmt.Sprint(first)), []byte(fmt.Sprint(last)))
		took := time.Since(asked)
		report.Scan.Micros = append(report.Scan.Micros, took.Microseconds())
		report.Scan.Requests++
		if err != nil {
			return report, fmt.Errorf("ZRANGE %d %d: %w", first, last, err)
		}
		if uint64(len(got.Array)) != last-first+1 {
			return report, fmt.Errorf("ZRANGE %d %d answered %d members", first, last, len(got.Array))
		}
		for at, raw := range got.Array {
			member, err := parseName('m', raw)
			if err != nil {
				return report, fmt.Errorf("ZRANGE %d %d: %w", first, last, err)
			}
			if want := d.memberAt(first + uint64(at)); member != want {
				return report, fmt.Errorf("rank %d holds %s, and its score belongs to %s",
					first+uint64(at), raw, memberName(want))
			}
		}
	}
	report.Scan.Seconds = time.Since(began).Seconds()
	return report, nil
}
