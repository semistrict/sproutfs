package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/platform"
)

// The cases, by the name each sample carries.
const (
	caseCold       = "cold"
	caseRestore    = "restore"
	caseForkLocal  = "fork-local"
	caseForkRemote = "fork-remote"
)

// hostAPI is what the driver asks of a host. *hostapi.Client is one.
type hostAPI interface {
	Status(ctx context.Context) (hostapi.Status, error)
	Create(ctx context.Context, request hostapi.CreateRequest) (hostapi.CreateResult, error)
	Open(ctx context.Context, id string, request hostapi.OpenRequest) (hostapi.OpenResult, error)
	Fork(ctx context.Context, parent string, request hostapi.ForkRequest) (hostapi.ForkResult, error)
	Receive(ctx context.Context, handoff hostapi.Handoff) (hostapi.ReceiveResult, error)
	Released(ctx context.Context, id string) error
	Abandoned(ctx context.Context, id string) (hostapi.AbandonedResult, error)
	Exec(ctx context.Context, id string, request hostapi.ExecRequest) (hostapi.ExecResult, error)
	Stop(ctx context.Context, id string, request hostapi.StopRequest) (hostapi.StopResult, error)
	Delete(ctx context.Context, id string) error
}

// target is one host: its name and page address as it reports them, and its API.
type target struct {
	name, page string
	api        hostAPI
}

// sample is one start as the driver saw it. Times are milliseconds on the
// driver's clock from the start's first request: the create, the open, or the
// fork on the parent's host.
type sample struct {
	Case  string `json:"case"`
	Round int    `json:"round"`
	// VM is the VM that started: the child of a fork.
	VM     string `json:"vm"`
	Parent string `json:"parent,omitempty"`
	// Host runs the VM, and From is the host a restored VM was stopped on or a
	// fork's parent runs on.
	Host string `json:"host"`
	From string `json:"from,omitempty"`
	// ForkMS is a fork's round trip to the parent's host.
	ForkMS float64 `json:"fork_ms,omitempty"`
	// RequestAtMS is when the request that starts the VM was sent: a create, an
	// open, or a receive. RequestMS is its round trip, and Started reports that
	// it succeeded, which is when the host logged the start.
	RequestAtMS float64 `json:"request_at_ms"`
	RequestMS   float64 `json:"request_ms"`
	Started     bool    `json:"started"`
	// AgentMS is when the guest's agent first answered a command, and Execs
	// how many commands that took.
	AgentMS float64 `json:"agent_ms,omitempty"`
	Execs   int     `json:"execs,omitempty"`
	// StopMS is the suspend after a restore, which publishes the memory the
	// next round restores.
	StopMS float64 `json:"stop_ms,omitempty"`
	Error  string  `json:"error,omitempty"`
}

// driver starts VMs one at a time and writes a sample for each.
type driver struct {
	clock    platform.Clock
	hosts    []target
	template string
	// tag makes this run's identities its own: a VM identity is never reused.
	tag string
	// interval is the least time between two starts, so a start does not
	// queue behind the work the previous one left running: a fork child's
	// root, a parent's checkpoint, a suspend's upload.
	interval time.Duration
	// poll is the time between two commands to an agent that has not
	// answered, and patience how long the driver waits for one to answer.
	poll, patience time.Duration
	write          func(sample) error
	last           time.Time
}

// execTimeout bounds one command to the agent: an agent that does not answer
// in it is asked again.
const execTimeout = 5 * time.Second

func (d *driver) id(kind string, round int) string {
	return fmt.Sprintf("startbench-%s-%s-%d", d.tag, kind, round)
}

func ms(duration time.Duration) float64 { return float64(duration.Microseconds()) / 1000 }

// pace waits until interval has passed since the last start began.
func (d *driver) pace(ctx context.Context) error {
	if !d.last.IsZero() {
		if wait := d.interval - d.clock.Since(d.last); wait > 0 {
			if err := d.clock.Sleep(ctx, wait); err != nil {
				return err
			}
		}
	}
	d.last = d.clock.Now()
	return nil
}

// answer is when an agent first answered.
type answer struct {
	at    time.Duration
	execs int
	err   error
}

// agent asks the agent of id on h to run a command until it answers, from now
// on, and reports when it did measured from began. It runs beside the request
// that starts the VM, so an answer is not held back by a request that returns
// after the guest runs. stop ends the asking and waits for it.
func (d *driver) agent(ctx context.Context, h target, id string, began time.Time) (wait func() answer, stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan answer, 1)
	go func() { done <- d.ask(ctx, h, id, began) }()
	return func() answer {
			result := <-done
			cancel()
			return result
		}, func() {
			cancel()
			<-done
		}
}

func (d *driver) ask(ctx context.Context, h target, id string, began time.Time) answer {
	var last error
	for execs := 1; ; execs++ {
		attempt, cancel := context.WithTimeout(ctx, execTimeout)
		result, err := h.api.Exec(attempt, id, hostapi.ExecRequest{Cmd: "true"})
		cancel()
		if err == nil && result.Exit == 0 {
			return answer{at: d.clock.Since(began), execs: execs}
		}
		if err == nil {
			err = fmt.Errorf("the command exited %d: %s", result.Exit, result.Stderr)
		}
		last = err
		if d.clock.Since(began) >= d.patience {
			return answer{execs: execs, err: fmt.Errorf("the agent of %s did not answer within %s: %w",
				id, d.patience, last)}
		}
		if err := d.clock.Sleep(ctx, d.poll); err != nil {
			return answer{execs: execs, err: errors.Join(err, last)}
		}
	}
}

// cold creates a VM from the template on the first host, rounds times, and
// deletes each once its agent has answered.
func (d *driver) cold(ctx context.Context, rounds int) error {
	h := d.hosts[0]
	for round := range rounds {
		if err := d.pace(ctx); err != nil {
			return err
		}
		id := d.id(caseCold, round)
		s := sample{Case: caseCold, Round: round, VM: id, Host: h.name}
		began := d.clock.Now()
		wait, stop := d.agent(ctx, h, id, began)
		_, err := h.api.Create(ctx, hostapi.CreateRequest{ID: id, Template: d.template})
		s.RequestMS = ms(d.clock.Since(began))
		if err != nil {
			stop()
			s.Error = fmt.Sprintf("create: %v", err)
		} else {
			s.Started = true
			d.answered(&s, wait())
		}
		d.remove(ctx, h, id, &s)
		if err := d.write(s); err != nil {
			return err
		}
	}
	return nil
}

// restores suspends one VM and opens it again, rounds times, two rounds on each
// host in turn: one restore in two is on the host that suspended it, and the
// other on another host.
func (d *driver) restores(ctx context.Context, rounds int) error {
	first := d.hosts[0]
	id := d.id(caseRestore, 0)
	if err := d.prepare(ctx, first, id); err != nil {
		return err
	}
	defer d.remove(ctx, first, id, nil)
	if _, err := first.api.Stop(ctx, id, hostapi.StopRequest{Suspend: true}); err != nil {
		return fmt.Errorf("suspending %s: %w", id, err)
	}
	last := first
	for round := range rounds {
		if err := d.pace(ctx); err != nil {
			return err
		}
		h := d.hosts[(round/2)%len(d.hosts)]
		s := sample{Case: caseRestore, Round: round, VM: id, Host: h.name, From: last.name}
		began := d.clock.Now()
		wait, stop := d.agent(ctx, h, id, began)
		_, err := h.api.Open(ctx, id, hostapi.OpenRequest{})
		s.RequestMS = ms(d.clock.Since(began))
		if err != nil {
			stop()
			s.Error = fmt.Sprintf("open: %v", err)
			if err := d.write(s); err != nil {
				return err
			}
			continue
		}
		s.Started = true
		d.answered(&s, wait())
		suspended := d.clock.Now()
		_, err = h.api.Stop(ctx, id, hostapi.StopRequest{Suspend: true})
		s.StopMS = ms(d.clock.Since(suspended))
		if err != nil {
			s.Error = strings.TrimPrefix(s.Error+"; ", "; ") + fmt.Sprintf("suspend: %v", err)
			if err := d.write(s); err != nil {
				return err
			}
			return fmt.Errorf("suspending %s on %s: %w", id, h.name, err)
		}
		last = h
		if err := d.write(s); err != nil {
			return err
		}
	}
	return nil
}

// forks forks one running parent on the first host, rounds times, into a child
// on the same host or on the second, and deletes each child once its agent has
// answered. Each fork is carried as the orchestrator carries one: the fork on
// the parent's host, the receive on the child's, and the release on the
// parent's.
func (d *driver) forks(ctx context.Context, rounds int, remote bool) error {
	source, destination, kind := d.hosts[0], d.hosts[0], caseForkLocal
	address := ""
	if remote {
		destination, kind, address = d.hosts[1], caseForkRemote, d.hosts[1].page
	}
	parent := d.id(kind+"-parent", 0)
	if err := d.prepare(ctx, source, parent); err != nil {
		return err
	}
	defer d.remove(ctx, source, parent, nil)
	for round := range rounds {
		if err := d.pace(ctx); err != nil {
			return err
		}
		child := d.id(kind, round)
		s := sample{Case: kind, Round: round, VM: child, Parent: parent, Host: destination.name, From: source.name}
		began := d.clock.Now()
		forked, err := source.api.Fork(ctx, parent, hostapi.ForkRequest{IDs: []string{child}, Destination: address})
		s.ForkMS = ms(d.clock.Since(began))
		if err == nil && len(forked.Handoffs) != 1 {
			err = fmt.Errorf("the fork handed over %d children, want 1", len(forked.Handoffs))
		}
		if err != nil {
			s.Error = fmt.Sprintf("fork: %v", err)
			if err := d.write(s); err != nil {
				return err
			}
			continue
		}
		sent := d.clock.Now()
		s.RequestAtMS = ms(sent.Sub(began))
		wait, stop := d.agent(ctx, destination, child, began)
		_, err = destination.api.Receive(ctx, forked.Handoffs[0])
		s.RequestMS = ms(d.clock.Since(sent))
		if err != nil {
			stop()
			s.Error = fmt.Sprintf("receive: %v", err)
			if _, err := source.api.Abandoned(ctx, child); err != nil {
				slog.ErrorContext(ctx, "sproutfs-startbench: giving up a fork's hold", "vm", child, "error", err)
			}
		} else {
			s.Started = true
			if err := source.api.Released(ctx, child); err != nil {
				s.Error = fmt.Sprintf("release: %v", err)
			}
			d.answered(&s, wait())
		}
		d.remove(ctx, destination, child, &s)
		if err := d.write(s); err != nil {
			return err
		}
	}
	return nil
}

// prepare creates a VM the rounds start from and waits for its agent.
func (d *driver) prepare(ctx context.Context, h target, id string) error {
	began := d.clock.Now()
	if _, err := h.api.Create(ctx, hostapi.CreateRequest{ID: id, Template: d.template}); err != nil {
		return fmt.Errorf("creating %s on %s: %w", id, h.name, err)
	}
	if result := d.ask(ctx, h, id, began); result.err != nil {
		return errors.Join(result.err, h.api.Delete(ctx, id))
	}
	return nil
}

// answered records when the agent answered, or why it did not.
func (d *driver) answered(s *sample, result answer) {
	s.Execs = result.execs
	if result.err != nil {
		s.Error = strings.TrimPrefix(s.Error+"; ", "; ") + fmt.Sprintf("agent: %v", result.err)
		return
	}
	s.AgentMS = ms(result.at)
}

// remove deletes a VM the driver started, and records a failure in s.
func (d *driver) remove(ctx context.Context, h target, id string, s *sample) {
	if err := h.api.Delete(ctx, id); err != nil {
		slog.ErrorContext(ctx, "sproutfs-startbench: deleting a VM", "vm", id, "host", h.name, "error", err)
		if s != nil {
			s.Error = strings.TrimPrefix(s.Error+"; ", "; ") + fmt.Sprintf("delete: %v", err)
		}
	}
}

// run drives every case named, in order.
func (d *driver) run(ctx context.Context, cases []string, rounds int) error {
	for _, name := range cases {
		slog.InfoContext(ctx, "sproutfs-startbench: a case begins", "case", name, "rounds", rounds)
		var err error
		switch name {
		case caseCold:
			err = d.cold(ctx, rounds)
		case caseRestore:
			err = d.restores(ctx, rounds)
		case caseForkLocal:
			err = d.forks(ctx, rounds, false)
		case caseForkRemote:
			err = d.forks(ctx, rounds, true)
		default:
			err = fmt.Errorf("no case named %q", name)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func runDrive(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("drive", flag.ContinueOnError)
	hosts := flags.String("hosts", "", "the hosts' API base URLs, separated by commas; the first runs every parent")
	tokenFile := flags.String("token-file", "", "a file holding the deployment's API token")
	template := flags.String("template", "alpine", "the template a VM is created from")
	cases := flags.String("cases", strings.Join([]string{caseCold, caseRestore, caseForkLocal, caseForkRemote}, ","),
		"the cases to run, in order")
	rounds := flags.Int("count", 300, "starts per case")
	tag := flags.String("tag", "", "what makes this run's VM identities its own")
	interval := flags.Duration("interval", 250*time.Millisecond, "the least time between two starts")
	poll := flags.Duration("poll", 5*time.Millisecond, "the time between two commands to an agent")
	patience := flags.Duration("patience", 60*time.Second, "how long to wait for an agent to answer")
	out := flags.String("out", "samples.jsonl", "where the samples go, one JSON line each")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *tag == "" || *rounds <= 0 {
		return errors.New("drive needs -tag and a positive -count")
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		return fmt.Errorf("reading the API token: %w", err)
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	var targets []target
	for _, base := range strings.Split(*hosts, ",") {
		api := hostapi.NewClient(strings.TrimSpace(base), client, strings.TrimSpace(string(token)))
		status, err := api.Status(ctx)
		if err != nil {
			return fmt.Errorf("asking %s for its status: %w", base, err)
		}
		targets = append(targets, target{name: status.Host, page: status.PageAddress, api: api})
	}
	if len(targets) < 2 {
		return errors.New("drive needs two hosts: a fork to another host has to have somewhere to go")
	}
	file, err := os.Create(*out)
	if err != nil {
		return err
	}
	buffered := bufio.NewWriter(file)
	encoder := json.NewEncoder(buffered)
	d := &driver{clock: platform.WallClock(), hosts: targets, template: *template, tag: *tag,
		interval: *interval, poll: *poll, patience: *patience,
		write: func(s sample) error {
			if s.Error != "" {
				slog.WarnContext(ctx, "sproutfs-startbench: a start failed", "case", s.Case, "round", s.Round,
					"vm", s.VM, "error", s.Error)
			}
			if err := encoder.Encode(s); err != nil {
				return err
			}
			return buffered.Flush()
		}}
	err = d.run(ctx, strings.Split(*cases, ","), *rounds)
	return errors.Join(err, file.Close())
}
