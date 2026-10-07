package host_test

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	hostapi "github.com/semistrict/sproutfs/api/host"
)

var updateMetrics = flag.Bool("update-metrics", false, "rewrite testdata/metrics.txt from Metrics")

// The exposition of a Status with every field set is fixed in
// testdata/metrics.txt, so a change to how Metrics writes its text shows up as
// a change to that file.
func TestMetricsOfAFullStatusMatchTheGoldenFile(t *testing.T) {
	body := hostapi.Metrics(fullStatus())
	path := filepath.Join("testdata", "metrics.txt")
	if *updateMetrics {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if body != string(want) {
		t.Fatalf("Metrics no longer writes %s; rerun with -update-metrics if the change is meant", path)
	}
}

// fullStatus is a Status with every field set to a value of its own: numbers
// that grow from field to field, past the range Go prints a float64 in without
// an exponent, two elements in every slice, and every reason a map is read by.
// Numbers stay below 2^26, so that a product of two, as the arena's bytes are,
// is still a whole number a float64 holds exactly; and a status never counts
// more hedges won than it made.
func fullStatus() hostapi.Status {
	var status hostapi.Status
	var n uint64
	fill(reflect.ValueOf(&status).Elem(), &n)
	status.CacheRead.StoreHedgesWon = status.CacheRead.StoreHedges / 2
	return status
}

// mapKeys are the keys Metrics reads a map by.
var mapKeys = func() []string {
	var keys []string
	for _, list := range [][]string{hostapi.FillDropReasons, hostapi.HotTierFailures, hostapi.HotTierDropReasons} {
		keys = append(keys, list...)
	}
	return keys
}()

func fill(v reflect.Value, n *uint64) {
	next := func() uint64 {
		*n++
		return *n * *n * 1_000_003
	}
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Unix(int64(next()), 0).UTC()))
			return
		}
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i), n)
			}
		}
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem(), n)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 2, 2))
		for i := range v.Len() {
			fill(v.Index(i), n)
		}
	case reflect.Array:
		for i := range v.Len() {
			fill(v.Index(i), n)
		}
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		for _, key := range mapKeys {
			value := reflect.New(v.Type().Elem()).Elem()
			fill(value, n)
			v.SetMapIndex(reflect.ValueOf(key).Convert(v.Type().Key()), value)
		}
	case reflect.String:
		v.SetString("s" + time.Duration(next()).String())
	case reflect.Bool:
		v.SetBool(next()%2 == 0)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(next() % (1 << min(26, v.Type().Bits()-2))))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(next() % (1 << min(26, v.Type().Bits()-1)))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(next()) / 7)
	}
}
