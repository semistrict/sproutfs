package main

import (
	"context"
	"log/slog"
	"maps"
	"slices"

	"github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// noteCaches keeps the cache each host pod reported in one survey. A pod that
// answered without a cache has none. A pod that did not answer keeps the cache
// it reported before: a quiet host may be serving its windows perfectly well,
// and a list that dropped it would move every window it holds, on every host,
// and move them back when it answered. Readers mark a host that does not
// answer down on their own. A pod the Kubernetes API no longer lists is gone,
// and its cache with it. The report of a quiet pod shows the cache it keeps.
func (o *orchestrator) noteCaches(ctx context.Context, hosts []liveHost) {
	o.cacheMu.Lock()
	defer o.cacheMu.Unlock()
	if o.caches == nil {
		o.caches = make(map[string]host.Cache)
	}
	listed := make(map[string]bool, len(hosts))
	for index := range hosts {
		report := &hosts[index].report
		listed[report.Name] = true
		answered := report.Error == ""
		switch {
		case answered && report.Cache != nil:
			o.caches[report.Name] = *report.Cache
		case answered, sim.Bug(ctx, "orchestrator-drop-quiet-cache"):
			delete(o.caches, report.Name)
		}
		if kept, found := o.caches[report.Name]; found && !answered {
			report.Cache = &kept
		}
	}
	for name := range o.caches {
		if !listed[name] {
			delete(o.caches, name)
		}
	}
}

// Caches is the list of caches: the cache of every host pod the Kubernetes API
// lists, as that host last reported it, and the deployment's code with the
// codes it replaced. The code is the deployment's setting and never follows
// the caches listed: a drain, a join or a restart of this process that
// changed it would leave every stripe stored under the old one to the store.
func (o *orchestrator) Caches(ctx context.Context) (host.Caches, error) {
	if _, err := o.recent(ctx); err != nil {
		return host.Caches{}, err
	}
	o.cacheMu.Lock()
	reported := maps.Clone(o.caches)
	o.cacheMu.Unlock()
	caches := make([]rank.Cache, 0, len(reported))
	owners := make(map[rank.Identity]string, len(reported))
	for _, name := range slices.Sorted(maps.Keys(reported)) {
		cache, err := reported[name].Rank()
		if err == nil && cache.Weight == 0 {
			err = rank.ErrInvalid
		}
		if err != nil {
			slog.WarnContext(ctx, "sproutfs-orchestrator: a host reports a cache no list can hold",
				"host", name, "cache", reported[name], "error", err)
			continue
		}
		// Two pods over one cache file is a copied disk. The list keeps the
		// first by name, so every host's list names the same one.
		if owner, taken := owners[cache.Identity]; taken {
			slog.WarnContext(ctx, "sproutfs-orchestrator: two hosts report one cache",
				"cache", cache.Identity.String(), "listed", owner, "left_out", name)
			continue
		}
		owners[cache.Identity] = name
		caches = append(caches, cache)
	}
	code := o.code
	if sim.Bug(ctx, "orchestrator-code-follows-the-hosts") {
		code = rank.CodeFor(len(caches))
	}
	list, err := rank.NewList(code, caches, o.earlier...)
	if err != nil {
		return host.Caches{}, err
	}
	return host.CachesOf(list), nil
}
