package sim

import (
	"math"
	"time"
)

// Random is a schedule-independent deterministic choice source. Callers must
// use a stable semantic id for each choice (for example, "append/42/drop").
// The same seed, namespace, and id always produce the same result.
type Random struct {
	runtime   *Runtime
	namespace string
}

func (r *Runtime) Random(namespace string) Random {
	if namespace == "" {
		panic("sim: random namespace must not be empty")
	}
	return Random{runtime: r, namespace: namespace}
}

func (r Random) Uint64(id string) uint64 {
	if id == "" {
		panic("sim: random choice id must not be empty")
	}
	return r.runtime.sample(r.namespace + "\x00" + id)
}

func (r Random) Intn(id string, limit int) int {
	if limit <= 0 {
		panic("sim: random limit must be positive")
	}
	return int(r.Uint64(id) % uint64(limit))
}

func (r Random) Float64(id string) float64 {
	return float64(r.Uint64(id)>>11) / (1 << 53)
}

func (r Random) Chance(id string, probability float64) bool {
	if math.IsNaN(probability) || probability < 0 || probability > 1 {
		panic("sim: probability must be between zero and one")
	}
	return r.Float64(id) < probability
}

func (r Random) Duration(id string, maximum time.Duration) time.Duration {
	if maximum <= 0 {
		panic("sim: maximum duration must be positive")
	}
	return time.Duration(r.Uint64(id) % uint64(maximum))
}
