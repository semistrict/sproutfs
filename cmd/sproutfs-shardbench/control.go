package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/platform/adapters"
	"github.com/semistrict/sproutfs/rank"
)

// controller is the bench's controller: the members it reaches, the shard
// control it takes passes of, and the members it is taking away.
type controller struct {
	members  []string
	control  *membership.ShardControl
	client   *http.Client
	interval time.Duration
	leaving  map[string]bool
}

// event is one step of a move, and how long after the move began it was
// seen.
type event struct {
	Step    string  `json:"step"`
	Seconds float64 `json:"seconds"`
}

// round is one move: how its member was taken away, and its steps.
type round struct {
	How    string  `json:"how"`
	From   string  `json:"from"`
	To     string  `json:"to"`
	Events []event `json:"events"`
	Total  float64 `json:"seconds"`
}

func runControl(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("control", flag.ContinueOnError)
	membersFlag := flags.String("members", "", "comma-separated control URLs of the members")
	volume := flags.String("volume", "", "the shard's volume: projects/<p>/zones/<z>/disks/<name>")
	bucket := flags.String("bucket", "", "Cloud Storage bucket")
	prefix := flags.String("prefix", "", "prefix of this run's objects in the bucket")
	interval := flags.Duration("interval", time.Second, "how often the controller takes a pass")
	fill := flags.Int64("fill-bytes", 8<<30, "bytes of a VM the serving member publishes onto the shard first")
	rounds := flags.Int("rounds", 3, "moves of each kind")
	out := flags.String("out", "results.json", "where the rounds are written")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *membersFlag == "" || *volume == "" || *bucket == "" || *prefix == "" {
		return errors.New("control needs -members, -volume, -bucket and -prefix")
	}
	store, closer, err := adapters.NewGCS(ctx, "", *bucket, *prefix)
	if err != nil {
		return err
	}
	defer closer.Close()
	members, err := membership.NewStore(membership.Config{ObjectStore: store})
	if err != nil {
		return err
	}
	disks, err := adapters.NewGCENetworkDisks(ctx, "", "", "")
	if err != nil {
		return err
	}
	c := &controller{members: strings.Split(*membersFlag, ","), interval: *interval,
		control: &membership.ShardControl{Store: members, Disks: disks, Volumes: []string{*volume}},
		client:  &http.Client{Timeout: 10 * time.Minute}, leaving: map[string]bool{}}
	if _, _, err := c.serving(ctx, *volume, 10*time.Minute); err != nil {
		return err
	}
	server, _, err := c.serving(ctx, *volume, time.Minute)
	if err != nil {
		return err
	}
	var filled json.RawMessage
	if err := c.post(ctx, server+fmt.Sprintf("/fill?bytes=%d", *fill), &filled); err != nil {
		return fmt.Errorf("filling the shard through %s: %w", server, err)
	}
	slog.InfoContext(ctx, "control: the shard is filled", "member", server, "fill", string(filled))
	var results []round
	for at := range 2 * *rounds {
		how := "drained"
		if at%2 == 1 {
			how = "died"
		}
		moved, err := c.move(ctx, *volume, how)
		if err != nil {
			return err
		}
		slog.InfoContext(ctx, "control: a move", "how", how, "seconds", moved.Total, "events", moved.Events)
		results = append(results, moved)
	}
	encoded, err := json.MarshalIndent(map[string]any{"interval": interval.Seconds(), "volume": *volume,
		"fill": filled, "rounds": results}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(*out, encoded, 0o644)
}

// want is every member that answers, as it reports itself, leaving where the
// controller is taking it away.
func (c *controller) want() (membership.Want, map[rank.Identity]string) {
	want := membership.Want{Code: rank.CodeFor(1)}
	urls := map[rank.Identity]string{}
	for _, url := range c.members {
		var reported hostapi.Member
		if err := c.get(url+"/member", &reported); err != nil {
			continue
		}
		self, err := reported.Host()
		if err != nil {
			continue
		}
		self.Leaving = c.leaving[url]
		want.Hosts = append(want.Hosts, self)
		urls[self.ID] = url
	}
	return want, urls
}

// pass is one pass of the controller.
func (c *controller) pass(ctx context.Context) (membership.Membership, map[rank.Identity]string, membership.Want) {
	want, urls := c.want()
	m, _, err := c.control.Pass(ctx, want)
	if err != nil {
		slog.WarnContext(ctx, "control: a pass", "error", err)
	}
	return m, urls, want
}

// serving takes passes until the shard serves on a member that holds it, and
// returns that member's URL and how long it took.
func (c *controller) serving(ctx context.Context, volume string, limit time.Duration) (string, time.Duration, error) {
	began := time.Now()
	id := membership.ShardIdentity(volume)
	for time.Since(began) < limit {
		m, urls, want := c.pass(ctx)
		disk, listed := m.Disk(id)
		if listed && disk.State == membership.Serving && holds(want, disk.Member, id) {
			return urls[disk.Member], time.Since(began), nil
		}
		if err := sleep(ctx, c.interval); err != nil {
			return "", 0, err
		}
	}
	return "", 0, fmt.Errorf("the shard did not serve within %v", limit)
}

// holds reports whether member reports holding disk open.
func holds(want membership.Want, member, disk rank.Identity) bool {
	for _, host := range want.Hosts {
		if host.ID == member {
			return slices.ContainsFunc(host.Disks, func(held membership.Disk) bool { return held.ID == disk })
		}
	}
	return false
}

// move takes away the member that serves the shard, by draining it or by
// ending its process, and times each step until the shard serves on a
// member again.
func (c *controller) move(ctx context.Context, volume, how string) (round, error) {
	from, _, err := c.serving(ctx, volume, time.Minute)
	if err != nil {
		return round{}, err
	}
	id := membership.ShardIdentity(volume)
	began := time.Now()
	moved := round{How: how, From: from}
	seen := map[string]bool{}
	note := func(step string) {
		if !seen[step] {
			seen[step] = true
			moved.Events = append(moved.Events, event{Step: step, Seconds: time.Since(began).Seconds()})
		}
	}
	switch how {
	case "drained":
		c.leaving[from] = true
	case "died":
		// The reply never comes: the process ends as it reads the request.
		_ = c.post(ctx, from+"/exit", nil)
	}
	for time.Since(began) < 10*time.Minute {
		m, urls, want := c.pass(ctx)
		disk, _ := m.Disk(id)
		described, err := c.control.Disks.Describe(ctx, volume)
		attached := err == nil && len(described.Machines) > 0
		switch {
		case disk.State == membership.Releasing:
			note("releasing")
			if !holds(want, disk.Member, id) {
				note("closed")
			}
			if err == nil && !attached {
				note("detached")
			}
		case disk.State == membership.Released:
			note("released")
		case disk.State == membership.Attaching && urls[disk.Member] != from:
			note("assigned")
			if attached {
				note("attached")
			}
			if holds(want, disk.Member, id) {
				note("opened")
			}
		case disk.State == membership.Serving && urls[disk.Member] != "" && urls[disk.Member] != from:
			note("serving")
			moved.To, moved.Total = urls[disk.Member], time.Since(began).Seconds()
			delete(c.leaving, from)
			return moved, nil
		}
		if err := sleep(ctx, c.interval); err != nil {
			return round{}, err
		}
	}
	return round{}, fmt.Errorf("the shard did not move off %s within ten minutes", from)
}

func (c *controller) get(url string, into any) error {
	response, err := c.client.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, response.Status)
	}
	return json.NewDecoder(response.Body).Decode(into)
}

func (c *controller) post(ctx context.Context, url string, into any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s: %s", url, response.Status)
	}
	if into == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(into)
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}
