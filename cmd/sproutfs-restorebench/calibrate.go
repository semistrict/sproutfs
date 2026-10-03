package main

import (
	"bufio"
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/stripe"
)

// calibrateRequest asks a node to time each step of reading a page on its own
// processor, for at least Seconds each.
type calibrateRequest struct {
	Seconds float64 `json:"seconds"`
}

// calibration is what each step of reading a page takes on one processor of
// a node, in microseconds per page, by page size, and what the processor is.
type calibration struct {
	CPU string `json:"cpu"`
	// SHA says the processor has the SHA extensions, which Go's SHA-256 uses.
	SHA bool `json:"sha_instructions"`
	// Steps is each step's microseconds, by page size and then by step.
	Steps map[string]map[string]float64 `json:"steps_us"`
}

// The steps a calibration times. A read of the store decodes; a read of the
// cluster checks each stripe's CRC-32C, joins k stripes, from data stripes
// alone or with parity among them, and decodes. Decoding a page of noise is
// its framing and its SHA-256, since noise is stored raw. Every read copies
// the page into its caller's buffer. Noise and compare are what the bench
// used to do on every read to check it, which no restore does: make the page
// it expected and compare it as strings.
// sink keeps what a timed step makes, so the compiler cannot leave the step out.
var sink [sha256.Size]byte

var calibrationSteps = []string{"sha256", "decode", "crc32c", "copy", "split 4+2", "join 4 data",
	"join 2 data 2 parity", "noise", "compare"}

func (n *node) calibrate(ctx context.Context, request calibrateRequest) (calibration, error) {
	return calibrate(ctx, time.Duration(request.Seconds*float64(time.Second)))
}

func calibrate(ctx context.Context, each time.Duration) (calibration, error) {
	out := calibration{Steps: make(map[string]map[string]float64)}
	out.CPU, out.SHA = processor()
	code, err := rank.ParseCode("4+2")
	if err != nil {
		return calibration{}, err
	}
	for _, size := range []uint64{checkpoint.PageSize2MiB, checkpoint.PageSize4KiB} {
		g, err := newGuest("calibration", size, faultRunBytes/size)
		if err != nil {
			return calibration{}, err
		}
		page := make([]byte, size)
		other := make([]byte, size)
		if err := g.ReadPage(ctx, volume, 1, page); err != nil {
			return calibration{}, err
		}
		envelope, err := blob.Encode(ctx, page)
		if err != nil {
			return calibration{}, err
		}
		stripes, err := stripe.Split(code, envelope)
		if err != nil {
			return calibration{}, err
		}
		withParity := []stripe.Stripe{stripes[0], stripes[1], stripes[4], stripes[5]}
		accept := func([]byte) error { return nil }
		var failed error
		steps := map[string]func(){
			"sha256": func() { sink = sha256.Sum256(page) },
			"decode": func() {
				if _, err := blob.Decode(ctx, envelope, int(size)); err != nil {
					failed = err
				}
			},
			"crc32c": func() { sink[0] = byte(crc32.Checksum(page, crc32c)) },
			"copy":   func() { copy(other, page) },
			"split 4+2": func() {
				if _, err := stripe.Split(code, envelope); err != nil {
					failed = err
				}
			},
			"join 4 data": func() {
				if _, err := stripe.Join(ctx, code, stripes[:4], accept); err != nil {
					failed = err
				}
			},
			"join 2 data 2 parity": func() {
				if _, err := stripe.Join(ctx, code, withParity, accept); err != nil {
					failed = err
				}
			},
			"noise": func() { g.noise(1, other) },
			"compare": func() {
				if string(page) != string(other) {
					failed = errors.New("a page of noise differs from itself")
				}
			},
		}
		copy(other, page)
		timed := make(map[string]float64)
		for _, name := range calibrationSteps {
			timed[name] = timeEach(each, steps[name])
			if failed != nil {
				return calibration{}, fmt.Errorf("%s of a page of %d bytes: %w", name, size, failed)
			}
			if name == "noise" {
				copy(other, page)
			}
		}
		out.Steps[pageSizeName(size)] = timed
	}
	return out, nil
}

// timeEach is the microseconds one call of step takes, over at least each.
func timeEach(each time.Duration, step func()) float64 {
	calls := 0
	began := time.Now()
	for calls == 0 || time.Since(began) < each {
		step()
		calls++
	}
	return float64(time.Since(began).Microseconds()) / float64(calls)
}

// processor is the model of this host's processor and whether it has the SHA
// extensions, from Linux's /proc/cpuinfo; nothing elsewhere.
func processor() (string, bool) {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "", false
	}
	defer file.Close()
	model, sha := "", false
	lines := bufio.NewScanner(file)
	lines.Buffer(make([]byte, 64<<10), 1<<20)
	for lines.Scan() {
		key, value, found := strings.Cut(lines.Text(), ":")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "model name":
			model = cmp.Or(model, strings.TrimSpace(value))
		case "flags":
			sha = sha || slices.Contains(strings.Fields(value), "sha_ni")
		}
	}
	return model, sha
}

// pageSizeName is how results name a page size.
func pageSizeName(size uint64) string {
	if size == checkpoint.PageSize4KiB {
		return "4KiB"
	}
	return "2MiB"
}
