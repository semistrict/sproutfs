package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// The orchestrator is the membership's usual controller (package
// membership). It surveys the host pods, and moves the membership towards
// them one step a pass: a join, a leave, or a change of weight is one
// generation, and a host drains before it leaves. Nothing depends on there
// being one controller. Each step is a compare-and-set of the object, so two
// orchestrators at once each take a step from what the other left.

// MembershipInterval is how often the orchestrator takes a step of the
// membership. A join, then, is a pass to list the host and one to serve its
// disk, and a leave four passes.
const MembershipInterval = 5 * time.Second

// noteMembers keeps the member each host pod reported in one survey. A pod
// that answered without one keeps no disk. A pod that did not answer keeps
// what it reported before: a quiet host may be serving its windows perfectly
// well, and a membership that dropped it would drain it and move every window
// it holds, and move them back when it answered. Readers mark a host that
// does not answer down on their own. A pod the Kubernetes API no longer lists
// is gone, and its member with it. The report of a quiet pod shows the member
// it keeps.
func (o *orchestrator) noteMembers(ctx context.Context, hosts []liveHost) {
	o.memberMu.Lock()
	defer o.memberMu.Unlock()
	if o.reported == nil {
		o.reported = make(map[string]host.Member)
	}
	o.leaving = make(map[string]bool)
	listed := make(map[string]bool, len(hosts))
	machines := make(map[string]bool)
	for index := range hosts {
		report := &hosts[index].report
		listed[report.Name] = true
		// A machine stays in the pool while any host pod is listed on it, a
		// terminating one too: a pod replaced on its node keeps the node's
		// journal disk reserved for the pod that follows.
		if report.Node != "" {
			machines[report.Node] = true
		}
		if report.Terminating && !sim.Bug(ctx, "orchestrator-keep-terminating-host") {
			o.leaving[report.Name] = true
		}
		answered := report.Error == ""
		switch {
		case answered && report.Member != nil:
			o.reported[report.Name] = *report.Member
		case answered, sim.Bug(ctx, "orchestrator-drop-quiet-member"):
			delete(o.reported, report.Name)
		}
		if kept, found := o.reported[report.Name]; found && !answered {
			report.Member = &kept
		}
	}
	for name := range o.reported {
		if !listed[name] {
			delete(o.reported, name)
		}
	}
	o.pool = slices.Sorted(maps.Keys(machines))
}

// want is what the membership is moved towards: every host pod the
// Kubernetes API lists that reported a disk, as it last did, and the
// deployment's code with the codes it replaced. The code is the deployment's
// setting and never follows the hosts: a drain, a join or a restart of this
// process that changed it would leave every stripe stored under the old one
// to the store.
func (o *orchestrator) want(ctx context.Context) membership.Want {
	o.memberMu.Lock()
	reported := maps.Clone(o.reported)
	leaving := maps.Clone(o.leaving)
	pool := slices.Clone(o.pool)
	o.memberMu.Unlock()
	var want membership.Want
	if o.shards != nil && o.shards.Journals != nil {
		want.Pool = pool
	}
	owners := make(map[rank.Identity]string, len(reported))
	disks := 0
	for _, name := range slices.Sorted(maps.Keys(reported)) {
		wanted, err := reported[name].Host()
		if err != nil {
			slog.WarnContext(ctx, "sproutfs-orchestrator: a host reports a member no membership can hold",
				"host", name, "member", reported[name], "error", err)
			continue
		}
		// Two pods over one cache file is a copied disk. The membership
		// keeps the first by name, so every host routes to the same one.
		if owner, taken := owners[wanted.ID]; taken {
			slog.WarnContext(ctx, "sproutfs-orchestrator: two hosts report one member",
				"member", wanted.ID.String(), "listed", owner, "left_out", name)
			continue
		}
		owners[wanted.ID] = name
		// A pod being deleted drains: its shards move off it while it still
		// answers, before its node goes.
		wanted.Leaving = leaving[name]
		want.Hosts = append(want.Hosts, wanted)
		disks += len(wanted.Disks)
	}
	want.Code, want.Earlier = o.code, o.earlier
	if sim.Bug(ctx, "orchestrator-code-follows-the-hosts") {
		want.Code, want.Earlier = rank.CodeFor(disks), nil
	}
	return want
}

// StepMembership surveys the host pods, unless a survey is less than a second
// old, and takes one step of the membership towards them. It reports the
// membership it leaves and whether it changed it.
func (o *orchestrator) StepMembership(ctx context.Context) (membership.Membership, bool, error) {
	if o.members == nil {
		return membership.Membership{}, false, nil
	}
	if _, err := o.recent(ctx); err != nil {
		return membership.Membership{}, false, err
	}
	var next membership.Membership
	var changed bool
	var err error
	if o.shards != nil {
		next, changed, err = o.stepShards(ctx)
	} else {
		next, changed, err = o.members.Reconcile(ctx, o.want(ctx))
	}
	if changed {
		slog.InfoContext(ctx, "sproutfs-orchestrator: the membership took a step", "generation", next.Generation(),
			"members", len(next.Members()), "disks", len(next.Disks()), "code", next.Code().String())
	}
	return next, changed, err
}

// stepShards takes one step of the membership with the deployment's shards,
// listed from their claims, and attaches and detaches them as the membership
// it leaves calls for. A listing of the claims that failed takes no step: a
// shard missing from the list would be taken for one the deployment no
// longer has.
func (o *orchestrator) stepShards(ctx context.Context) (membership.Membership, bool, error) {
	volumes, err := o.shardVolumes(ctx)
	if err != nil {
		return membership.Membership{}, false, fmt.Errorf("listing the shards' claims: %w", err)
	}
	pass := *o.shards
	pass.Volumes = volumes
	return pass.Pass(ctx, o.want(ctx))
}

// SteppingMembership takes a step of the membership every interval for as
// long as ctx lives.
func (o *orchestrator) SteppingMembership(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = MembershipInterval
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for step := 1; ; step++ {
		// Each step is a task of its own, and goes on beside the requests
		// being served when a controlled run chooses.
		stepping := sim.WithTask(ctx, "membership step "+strconv.Itoa(step))
		if err := sim.Admit(stepping, "orchestrator/membership-step"); err != nil {
			return
		}
		if _, _, err := o.StepMembership(stepping); err != nil && !errors.Is(err, context.Canceled) {
			slog.WarnContext(ctx, "sproutfs-orchestrator: a step of the membership failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
