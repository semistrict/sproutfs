package sim_test

import (
	"math"
	"strconv"
	"testing"

	"github.com/semistrict/sproutfs/internal/platform/sim"
)

func TestRandomChanceRejectsValuesOutsideTheProbabilityDomain(t *testing.T) {
	for _, probability := range []float64{-1, math.Nextafter(0, -1), math.Nextafter(1, 2), 2, math.Inf(-1), math.Inf(1), math.NaN()} {
		t.Run(strconv.FormatFloat(probability, 'g', -1, 64), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("invalid probability was accepted")
				}
			}()
			sim.New(sim.Config{Seed: 711}).Random("probability-domain").Chance("decision", probability)
		})
	}
}

func TestRandomChanceSupportsImpossibleAndCertainEvents(t *testing.T) {
	for seed := uint64(1); seed <= 4; seed++ {
		random := sim.New(sim.Config{Seed: seed}).Random("probability-endpoints")
		for choice := range 32 {
			id := strconv.Itoa(choice)
			if random.Chance(id, 0) {
				t.Fatalf("impossible event occurred at seed %d, choice %s", seed, id)
			}
			if !random.Chance(id, 1) {
				t.Fatalf("certain event did not occur at seed %d, choice %s", seed, id)
			}
			if value := random.Float64(id); math.IsNaN(value) || value < 0 || value >= 1 {
				t.Fatalf("sample outside [0,1): %g", value)
			}
		}
	}
}
