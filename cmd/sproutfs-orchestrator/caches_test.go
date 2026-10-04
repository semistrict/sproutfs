package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/api/orch"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// cacheNamed is the cache a host of this name reports: an identity drawn from
// the name, the weight given, and the host's page address.
func (d *deployment) cacheNamed(name string, weight uint32) *host.Cache {
	sum := sha256.Sum256([]byte("cache of " + name))
	return &host.Cache{Identity: hex.EncodeToString(sum[:16]), Weight: weight, Address: d.hosts[name].page}
}

// withCaches gives each named host a cache of the weight given.
func (d *deployment) withCaches(weights map[string]uint32) {
	for name, weight := range weights {
		d.hosts[name].cache = d.cacheNamed(name, weight)
	}
}

// simulated is a context the in-tree bug guards answer in.
func simulated(t *testing.T) context.Context {
	return sim.WithRuntime(t.Context(), sim.New(sim.Config{}))
}

// listed reads the orchestrator's list of caches and reports its code and
// the hosts of its caches, by their page addresses.
func listed(t *testing.T, ctx context.Context, d *deployment) (rank.Code, []string) {
	t.Helper()
	caches, err := d.orchestrator.Caches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	list, err := caches.List()
	if err != nil {
		t.Fatalf("the orchestrator served a list no host can read: %v", err)
	}
	addresses := make([]string, 0, list.Len())
	for _, cache := range list.Caches() {
		addresses = append(addresses, string(cache.Address))
	}
	slices.Sort(addresses)
	return list.Code(), addresses
}

// GET /caches is every host's cache as it reported it in /status, with the
// deployment's code. A host that keeps no cache is not in it. With no code
// configured, three caches get the default 4+2, round the three.
func TestTheListOfCachesNamesEveryHostsCache(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}, "host-2": {}, "host-3": {}})
	d.withCaches(map[string]uint32{"host-0": 2, "host-1": 1, "host-2": 4})
	server := httptest.NewServer(newServer(d.orchestrator, "token"))
	defer server.Close()
	caches, err := orch.NewClient(server.URL, server.Client(), "token").Caches(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []host.Cache{*d.cacheNamed("host-0", 2), *d.cacheNamed("host-1", 1), *d.cacheNamed("host-2", 4)}
	slices.SortFunc(want, func(a, b host.Cache) int {
		if a.Identity < b.Identity {
			return -1
		}
		return 1
	})
	if caches.K != 4 || caches.M != 2 || len(caches.Earlier) != 0 || !slices.Equal(caches.Caches, want) {
		t.Fatalf("GET /caches served %+v, want 4+2 over %+v", caches, want)
	}
	hosts, err := d.orchestrator.Hosts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, report := range hosts {
		reported := d.hosts[report.Name].cache
		if (report.Cache == nil) != (reported == nil) || reported != nil && *report.Cache != *reported {
			t.Fatalf("GET /hosts reports %s with cache %+v, want %+v", report.Name, report.Cache, reported)
		}
	}
}

// A code the deployment configures is the list's, whatever the size of the
// cluster, and so are the codes it replaced, newest first.
func TestAConfiguredCodeIsTheLists(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}})
	d.withCaches(map[string]uint32{"host-0": 1, "host-1": 1})
	d.orchestrator.code = rank.Code{K: 1, M: 1}
	d.orchestrator.earlier = []rank.Code{{K: 2, M: 1}, {K: 4, M: 2}}
	caches, err := d.orchestrator.Caches(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	list, err := caches.List()
	if err != nil {
		t.Fatal(err)
	}
	want := []rank.Code{{K: 1, M: 1}, {K: 2, M: 1}, {K: 4, M: 2}}
	if !slices.Equal(list.Codes(), want) || list.Len() != 2 {
		t.Fatalf("the list holds %d caches under %v, want two under 1+1, then 2+1 and 4+2", list.Len(), list.Codes())
	}
}

// The code never follows the hosts. A six-host cluster that sets no code runs
// 4+2, and drained down to two hosts it still does, because a code that
// followed the list would leave every stripe in the cluster to the store. Its
// stripes go round the hosts left.
func TestTheCodeNeverFollowsTheHosts(t *testing.T) {
	ctx := simulated(t)
	names := []string{"host-0", "host-1", "host-2", "host-3", "host-4", "host-5"}
	running := map[string][]string{}
	weights := map[string]uint32{}
	for _, name := range names {
		running[name] = []string{}
		weights[name] = 1
	}
	d := newDeployment(t, running)
	d.withCaches(weights)
	if code, addresses := listed(t, ctx, d); code != (rank.Code{K: 4, M: 2}) || len(addresses) != 6 {
		t.Fatalf("six hosts are listed as %v under %s, want six caches under 4+2", addresses, code)
	}
	for left := 5; left >= 2; left-- {
		gone := names[left]
		if _, err := d.orchestrator.Kill(ctx, gone); err != nil {
			t.Fatal(err)
		}
		if _, err := d.orchestrator.survey(ctx); err != nil {
			t.Fatal(err)
		}
		code, addresses := listed(t, ctx, d)
		if code != (rank.Code{K: 4, M: 2}) || slices.Contains(addresses, d.hosts[gone].page) || len(addresses) != left {
			t.Fatalf("after %s left the list holds %v under %s, want the other %d under 4+2", gone, addresses, code, left)
		}
	}
}

// A host the Kubernetes API still lists that did not answer this survey
// keeps its cache in the list: it may be serving its windows perfectly well,
// and a list that dropped it would move them all. Readers mark it down on
// their own. The report of the quiet host shows the cache it keeps.
func TestAQuietHostStaysInTheListOfCaches(t *testing.T) {
	ctx := simulated(t)
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}, "host-2": {}})
	d.withCaches(map[string]uint32{"host-0": 1, "host-1": 1, "host-2": 1})
	if _, err := d.orchestrator.survey(ctx); err != nil {
		t.Fatal(err)
	}
	d.hosts["host-1"].down = true
	hosts, err := d.orchestrator.survey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	quiet := hosts[slices.IndexFunc(hosts, func(h liveHost) bool { return h.report.Name == "host-1" })].report
	if quiet.Error == "" || quiet.Cache == nil || *quiet.Cache != *d.cacheNamed("host-1", 1) {
		t.Fatalf("the quiet host is reported with error %q and cache %+v, want its cache kept",
			quiet.Error, quiet.Cache)
	}
	code, addresses := listed(t, ctx, d)
	want := []string{d.hosts["host-0"].page, d.hosts["host-1"].page, d.hosts["host-2"].page}
	slices.Sort(want)
	if code != (rank.Code{K: 4, M: 2}) || !slices.Equal(addresses, want) {
		t.Fatalf("with host-1 quiet the list holds %v under %s, want all three under 4+2", addresses, code)
	}
}

// A host that answers with another cache, as one restarted over a new disk
// does, is listed by its new cache alone. A host that answers with none is
// not listed, and neither is a pod the Kubernetes API no longer has.
func TestTheListFollowsWhatEachHostReports(t *testing.T) {
	ctx := simulated(t)
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}, "host-2": {}})
	d.withCaches(map[string]uint32{"host-0": 1, "host-1": 1, "host-2": 1})
	if _, err := d.orchestrator.survey(ctx); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("a new disk"))
	renewed := &host.Cache{Identity: hex.EncodeToString(sum[:16]), Weight: 3, Address: d.hosts["host-0"].page}
	d.hosts["host-0"].cache = renewed
	d.hosts["host-1"].cache = nil
	if _, err := d.orchestrator.Kill(ctx, "host-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.orchestrator.survey(ctx); err != nil {
		t.Fatal(err)
	}
	caches, err := d.orchestrator.Caches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The code stays the deployment's.
	if caches.K != 4 || caches.M != 2 || !slices.Equal(caches.Caches, []host.Cache{*renewed}) {
		t.Fatalf("the list is %+v, want host-0's new cache alone under 4+2", caches)
	}
}

// Two pods that report one cache are a copied disk. The list holds the cache
// once, as the first pod by name reports it, so every host's list names the
// same holder. A cache no list can hold, with no identity or no weight, is
// left out.
func TestTheListHoldsEachCacheOnce(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}, "host-2": {}, "host-3": {}})
	copied := *d.cacheNamed("host-0", 1)
	d.hosts["host-0"].cache = &copied
	twin := copied
	twin.Address = d.hosts["host-1"].page
	d.hosts["host-1"].cache = &twin
	d.hosts["host-2"].cache = &host.Cache{Identity: "not hex", Weight: 1, Address: d.hosts["host-2"].page}
	d.hosts["host-3"].cache = d.cacheNamed("host-3", 0)
	caches, err := d.orchestrator.Caches(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(caches.Caches, []host.Cache{copied}) {
		t.Fatalf("the list is %+v, want host-0's cache once", caches.Caches)
	}
}

// The deployment names its code as k+m, and the codes it replaced as a list
// of them, newest first. A code no host can store under is refused at start,
// and so is an earlier code that is not one, or that is the code itself. With
// no code set, the code is the default, 4+2.
func TestTheCodeIsConfigured(t *testing.T) {
	environment := map[string]string{"SPROUTFS_BUCKET": "bucket", "SPROUTFS_CACHE_CODE": "6+2",
		"SPROUTFS_CACHE_EARLIER_CODES": "4+2, 2+1"}
	lookup := func(name string) string { return environment[name] }
	config, err := loadConfig(lookup)
	if err != nil {
		t.Fatal(err)
	}
	if config.CacheCode != (rank.Code{K: 6, M: 2}) ||
		!slices.Equal(config.CacheEarlierCodes, []rank.Code{{K: 4, M: 2}, {K: 2, M: 1}}) {
		t.Fatalf("the code is %s after %v, want 6+2 after 4+2 and 2+1", config.CacheCode, config.CacheEarlierCodes)
	}
	for name, value := range map[string]string{
		"SPROUTFS_CACHE_CODE":          "0+2",
		"SPROUTFS_CACHE_EARLIER_CODES": "6+2",
	} {
		before := environment[name]
		environment[name] = value
		if _, err := loadConfig(lookup); err == nil {
			t.Fatalf("%s=%s was accepted", name, value)
		}
		environment[name] = before
	}
	environment["SPROUTFS_CACHE_EARLIER_CODES"] = "4+2,two"
	if _, err := loadConfig(lookup); err == nil {
		t.Fatal("an earlier code that is not one was accepted")
	}
	delete(environment, "SPROUTFS_CACHE_CODE")
	delete(environment, "SPROUTFS_CACHE_EARLIER_CODES")
	if config, err := loadConfig(lookup); err != nil || config.CacheCode != rank.DefaultCode ||
		len(config.CacheEarlierCodes) != 0 {
		t.Fatalf("with no code configured the configuration is %s after %v, %v", config.CacheCode,
			config.CacheEarlierCodes, err)
	}
}
