package host_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/rank"
)

// The list of caches crosses the control plane as JSON, from the orchestrator
// to every host. A host that read it ranks every window as the orchestrator's
// own list does, because ranks are a function of the list alone. The codes
// the deployment used before cross with it, newest first, and a list with
// none writes none.
func TestTheListOfCachesCrossesTheWireIntact(t *testing.T) {
	caches := []rank.Cache{
		{Identity: rank.Identity{3, 1}, Weight: 2, Address: "10.0.0.3:8081"},
		{Identity: rank.Identity{1, 7}, Weight: 1, Address: "10.0.0.1:8081"},
		{Identity: rank.Identity{2, 9}, Weight: 5, Address: "10.0.0.2:8081"},
	}
	plain, err := json.Marshal(hostapi.CachesOf(rank.Alone(caches[0])))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"k":1,"m":0,"caches":[{"identity":"03010000000000000000000000000000","weight":2,"address":"10.0.0.3:8081"}]}`; string(plain) != want {
		t.Fatalf("a list with no earlier code is written as %s, want %s", plain, want)
	}
	list, err := rank.NewList(rank.Code{K: 2, M: 1}, caches, rank.Code{K: 4, M: 2}, rank.Code{K: 1, M: 1})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(hostapi.CachesOf(list))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"k":2,"m":1,"earlier":["4+2","1+1"],"caches":[` +
		`{"identity":"01070000000000000000000000000000","weight":1,"address":"10.0.0.1:8081"},` +
		`{"identity":"02090000000000000000000000000000","weight":5,"address":"10.0.0.2:8081"},` +
		`{"identity":"03010000000000000000000000000000","weight":2,"address":"10.0.0.3:8081"}]}`; string(encoded) != want {
		t.Fatalf("the list is written as %s, want %s", encoded, want)
	}
	var decoded hostapi.Caches
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	read, err := decoded.List()
	if err != nil {
		t.Fatal(err)
	}
	if !read.Equal(list) {
		t.Fatalf("the list read back is %v under %s, want %v under %s", read.Caches(), read.Code(),
			list.Caches(), list.Code())
	}
	for span := range uint64(256) {
		window := rank.Window{Ref: control.Ref{VM: "vm-wire", Sequence: 2}, Volume: "ram0", Number: span}
		if !slices.Equal(read.Ranks(window), list.Ranks(window)) {
			t.Fatalf("span %d ranks %v read back and %v as sent", span, read.Ranks(window), list.Ranks(window))
		}
	}
}

// A list no host could rank by is refused as it is read: a cache whose
// identity is not one, a code no envelope can be stored under, an earlier
// code that is not one, and the list's own code named again as earlier.
func TestAListNoHostCouldRankByIsRefused(t *testing.T) {
	for name, caches := range map[string]hostapi.Caches{
		"a bad identity":     {K: 1, M: 1, Caches: []hostapi.Cache{{Identity: "nope", Weight: 1}}},
		"no weight":          {K: 1, M: 1, Caches: []hostapi.Cache{{Identity: rank.Identity{1}.String()}}},
		"no data stripe":     {K: 0, M: 1},
		"a bad earlier code": {K: 1, M: 1, Earlier: []string{"4"}},
		"its own code again": {K: 1, M: 1, Earlier: []string{"1+1"}},
	} {
		if _, err := caches.List(); !errors.Is(err, rank.ErrInvalid) {
			t.Fatalf("a list with %s is read with %v, want rank.ErrInvalid", name, err)
		}
	}
}
