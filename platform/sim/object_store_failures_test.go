package sim_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// storeFailures is every site at which a store with RequestChaos refuses a
// request, loses the reply to a write it applied, or resets a body.
var storeFailures = []string{"sim/object-store/unavailable/head", "sim/object-store/unavailable/get",
	"sim/object-store/unavailable/put", "sim/object-store/unavailable/delete", "sim/object-store/unavailable/list",
	"sim/object-store/reply-lost/put", "sim/object-store/reply-lost/delete", sim.SiteObjectBodyFails}

// Under Buggify a store with RequestChaos refuses requests, loses the replies
// to writes it applied, and resets bodies partway, and what each leaves is
// what a provider's leaves: a refused request changed nothing, a write whose
// reply was lost was applied, and a body that reset handed over a prefix of
// the object's bytes. Each failure says unavailable. Across the seeds every
// site fires; a store without RequestChaos fires none of them.
func TestTheStoreRefusesAndLosesRepliesAtRandom(t *testing.T) {
	fired := map[string]bool{}
	for seed := uint64(1); seed <= 32; seed++ {
		for _, chaos := range []bool{true, false} {
			synctest.Test(t, func(t *testing.T) {
				runtime := sim.New(sim.Config{Seed: seed, Buggify: true})
				store := runtime.NewObjectStore("store", sim.ObjectStoreConfig{RequestChaos: chaos,
					Hold: time.Millisecond})
				requestsUnderFailures(t, runtime, store)
				for site := range runtime.FiredSites() {
					if !chaos && bytes.HasPrefix([]byte(site), []byte("sim/object-store/")) {
						t.Fatalf("a store without RequestChaos fired %s", site)
					}
					fired[site] = true
				}
			})
		}
	}
	for _, site := range storeFailures {
		if !fired[site] {
			t.Errorf("%s never fired", site)
		}
	}
}

// requestsUnderFailures writes, reads, lists and deletes objects, checking
// what each failure left against what the store holds.
func requestsUnderFailures(t *testing.T, runtime *sim.Runtime, store *sim.ObjectStore) {
	ctx := t.Context()
	random := runtime.Random("store-failures-test")
	held := map[string][]byte{}
	// unavailable reports a failure the store's sites made, and fails the
	// test on any other.
	unavailable := func(what string, err error) bool {
		t.Helper()
		if err != nil && !errors.Is(err, platform.ErrUnavailable) {
			t.Fatalf("%s: %v", what, err)
		}
		return err != nil
	}
	// holds reads what the store holds of name with its sites off.
	holds := func(name string) ([]byte, bool) {
		runtime.SetBuggify(false)
		defer runtime.SetBuggify(true)
		data, _, err := platform.ReadObject(ctx, store, key(t, name), 0, 1<<20, errors.New("corrupt"))
		if errors.Is(err, platform.ErrNotFound) {
			return nil, false
		}
		if err != nil {
			t.Fatal(err)
		}
		return data, true
	}
	for step := range 400 {
		id := fmt.Sprintf("%d", step)
		name := fmt.Sprintf("object-%d", random.Intn(id+"/name", 4))
		switch random.Intn(id+"/request", 5) {
		case 0:
			data := bytes.Repeat([]byte{byte(step)}, 1+random.Intn(id+"/size", 4096))
			_, err := store.Put(ctx, platform.PutRequest{Key: key(t, name), Body: bytes.NewReader(data),
				Size: int64(len(data))})
			if unavailable("put", err) {
				// Refused, it changed nothing; its reply lost, it was applied.
				got, there := holds(name)
				before, had := held[name]
				switch {
				case there && bytes.Equal(got, data):
				case there == had && bytes.Equal(got, before):
					continue
				default:
					t.Fatalf("a failed put of %s left %d bytes it neither had nor was given", name, len(got))
				}
			}
			held[name] = data
		case 1:
			err := store.Delete(ctx, platform.DeleteRequest{Key: key(t, name)})
			if unavailable("delete", err) {
				if _, there := holds(name); there {
					continue
				}
			}
			delete(held, name)
		case 2:
			want, there := held[name]
			result, err := store.Get(ctx, platform.GetRequest{Key: key(t, name)})
			if !there {
				if !errors.Is(err, platform.ErrNotFound) {
					unavailable("get", err)
				}
				continue
			}
			if unavailable("get", err) {
				continue
			}
			got, err := io.ReadAll(result.Body)
			_ = result.Body.Close()
			if unavailable("reading a body", err) && !bytes.HasPrefix(want, got) {
				t.Fatalf("a body of %s that reset handed over %d bytes that are not its start", name, len(got))
			}
			if err == nil && !bytes.Equal(got, want) {
				t.Fatalf("%s read %d bytes, want the %d it holds", name, len(got), len(want))
			}
		case 3:
			metadata, err := store.Head(ctx, key(t, name))
			want, there := held[name]
			if !there && errors.Is(err, platform.ErrNotFound) {
				continue
			}
			if !unavailable("head", err) && metadata.Size != int64(len(want)) {
				t.Fatalf("%s heads at %d bytes, want %d", name, metadata.Size, len(want))
			}
		case 4:
			listed, err := store.List(ctx, platform.ListRequest{})
			if !unavailable("list", err) && len(listed.Objects) != len(held) {
				t.Fatalf("the store lists %d objects, want %d", len(listed.Objects), len(held))
			}
		}
	}
}

func key(t *testing.T, name string) platform.ObjectKey {
	t.Helper()
	k, err := platform.NewObjectKey(name)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
