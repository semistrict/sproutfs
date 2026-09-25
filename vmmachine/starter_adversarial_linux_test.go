//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmachine"
)

// TestStarterChild is the process an adversarial Starter starts in place of a
// VMM, behaving as SPROUTFS_STARTER_CHILD says. It is not a test of its own.
func TestStarterChild(t *testing.T) {
	switch os.Getenv("SPROUTFS_STARTER_CHILD") {
	case "":
		return
	case "idle":
		// A process that is not a VMM at all: it binds nothing and connects
		// to nothing.
		readConsole()
	case "exit":
		fmt.Println("the fake VMM exits before binding its API socket")
		os.Exit(3)
	case "jailer":
		// A jailer that runs the VMM in a PID namespace of its own: the
		// process its Starter reports is not the one that connects.
		inner := exec.Command(os.Args[0], os.Args[1:]...)
		inner.Env = append(os.Environ(), "SPROUTFS_STARTER_CHILD=inner")
		inner.Stdout, inner.Stderr = os.Stdout, os.Stderr
		if err := inner.Run(); err != nil {
			fmt.Println("the jailed VMM failed:", err)
			os.Exit(4)
		}
		readConsole()
	case "inner":
		// The listener is never closed, so its path stays bound after the
		// exit. The connection is closed at once: the peer check is made
		// against the connecting process, which is gone by then.
		if _, err := net.Listen("unix", childArgument("--api-sock")); err != nil {
			fmt.Println(err)
			os.Exit(5)
		}
		connection, err := net.Dial("unix", childArgument("--ram"))
		if err != nil {
			fmt.Println(err)
			os.Exit(6)
		}
		_ = connection.Close()
		os.Exit(0)
	default:
		t.Fatalf("unknown SPROUTFS_STARTER_CHILD %q", os.Getenv("SPROUTFS_STARTER_CHILD"))
	}
}

// readConsole reads the console Spawn gives the process until it closes, which
// is after the process was killed. A child blocked on nothing else would be
// ended by the runtime as a deadlock instead.
func readConsole() {
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(7)
}

// childArgument is the value after name on this process's command line.
func childArgument(name string) string {
	for i, arg := range os.Args {
		if arg == name && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return ""
}

// fakeVMM is a process an adversarial Starter started: how it ended, and how
// often it was released.
type fakeVMM struct {
	*vmmachine.Child
	closeErr error
	closes   int
	exit     error
}

func (f *fakeVMM) Wait() error {
	f.exit = f.Child.Wait()
	return f.exit
}

func (f *fakeVMM) Close() error {
	f.closes++
	return errors.Join(f.Child.Close(), f.closeErr)
}

// outcome is what became of the process: how it ended, how often its Starter
// released it, and whether it is gone from the process table.
func (f *fakeVMM) outcome() string {
	if f == nil {
		return "no VMM"
	}
	reaped := errors.Is(syscall.Kill(f.PID(), 0), syscall.ESRCH)
	return fmt.Sprintf("ended by %v, released %d, reaped %t", f.exit, f.closes, reaped)
}

// adversarialStarter is a Starter that gets its side of the contract wrong in
// the way start says. Every Prepare it makes that succeeds is held to its
// placement: the paths the VMM is given, and who owns what was made for it.
type adversarialStarter struct {
	t         *testing.T
	placement vmmachine.Placement
	start     func(s *adversarialStarter, ctx context.Context, launch *vmmachine.Launch) (vmmachine.VMM, error)
	vmm       *fakeVMM
}

func (s *adversarialStarter) Boots() bool { return true }

func (s *adversarialStarter) Start(ctx context.Context, launch *vmmachine.Launch) (vmmachine.VMM, error) {
	return s.start(s, ctx, launch)
}

// prepare is Prepare with the placement, checked against it.
func (s *adversarialStarter) prepare(ctx context.Context, launch *vmmachine.Launch) (*vmmachine.Memory, error) {
	memory, err := launch.Prepare(ctx, s.placement)
	if err != nil {
		return nil, err
	}
	within := cmp.Or(s.placement.Within, s.placement.Directory)
	given := [][2]string{{memory.Directory, s.placement.Directory}, {memory.Within, within},
		{memory.APISocket, within + "/api.sock"}, {memory.RAM, within + "/ram.sock"}}
	for _, path := range given {
		if path[0] != path[1] {
			s.t.Errorf("the VMM was given %s, want %s", path[0], path[1])
		}
	}
	owners, err := ownersOf(memory.Directory)
	if err != nil {
		return nil, err
	}
	want := map[string]string{".": "65534:65534", "ram.sock": "65534:65534", "state": "65534:65534"}
	if !maps.Equal(owners, want) {
		s.t.Errorf("the prepared directory belonged to %v, want %v", owners, want)
	}
	return memory, nil
}

// spawn starts this test binary as the VMM, running test with env set.
func (s *adversarialStarter) spawn(test, env string, args ...string) (vmmachine.VMM, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	command := exec.Command(executable, append([]string{"-test.run=^" + test + "$", "--"}, args...)...)
	command.Env = append(os.Environ(), env)
	child, err := vmmachine.Spawn(command, "")
	if err != nil {
		return nil, err
	}
	s.vmm = &fakeVMM{Child: child}
	return s.vmm, nil
}

// adversarialPlacement is where each adversarial Starter puts its process:
// under parent, named by the VMM as a chroot would name it, and given to
// another user.
func adversarialPlacement(parent string) vmmachine.Placement {
	return vmmachine.Placement{Directory: filepath.Join(parent, "placed"), Within: "/vms/placed",
		Owner: &vmmachine.Owner{UID: 65534, GID: 65534}}
}

// TestAdversarialStarters holds Start to a Starter that breaks its side of the
// contract. Each start fails with the error that names what the Starter did,
// and leaves nothing behind: the placed directory is gone, the scratch closes,
// the pager maps nothing, and a process the Starter started was killed, reaped
// and released exactly once.
func TestAdversarialStarters(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("giving a directory to another user needs root; run the Firecracker Lima qualification script")
	}
	type starterCase struct {
		name    string
		restore bool
		// within replaces the placement's Within, and existing makes its
		// Directory before the start.
		within   string
		existing bool
		start    func(s *adversarialStarter, ctx context.Context, launch *vmmachine.Launch) (vmmachine.VMM, error)
		// want is the start's error, with PARENT for the placement's parent.
		want string
		// vmm is what became of the Starter's process, and remains every
		// file left under the placement's parent.
		vmm     string
		remains []string
	}
	cases := []starterCase{
		{
			name: "a VMM started without Prepare",
			start: func(s *adversarialStarter, ctx context.Context, launch *vmmachine.Launch) (vmmachine.VMM, error) {
				return s.spawn("TestStarterChild", "SPROUTFS_STARTER_CHILD=idle")
			},
			want: "vmmachine: the Starter of vm started a VMM without preparing its memory",
			vmm:  "ended by signal: killed, released 1, reaped true",
		},
		{
			name: "a launch prepared twice",
			start: func(s *adversarialStarter, ctx context.Context, launch *vmmachine.Launch) (vmmachine.VMM, error) {
				if _, err := s.prepare(ctx, launch); err != nil {
					return nil, err
				}
				_, err := launch.Prepare(ctx, s.placement)
				return nil, err
			},
			want: "vmmachine: starting the VMM of vm: vmmachine: a launch is prepared once",
			vmm:  "no VMM",
		},
		{
			name: "a Starter that fails after Prepare",
			start: func(s *adversarialStarter, ctx context.Context, launch *vmmachine.Launch) (vmmachine.VMM, error) {
				if _, err := s.prepare(ctx, launch); err != nil {
					return nil, err
				}
				return nil, errors.New("the jailer refused its arguments")
			},
			want: "vmmachine: starting the VMM of vm: the jailer refused its arguments",
			vmm:  "no VMM",
		},
		{
			name: "a VMM in a PID namespace of its own",
			start: func(s *adversarialStarter, ctx context.Context, launch *vmmachine.Launch) (vmmachine.VMM, error) {
				memory, err := s.prepare(ctx, launch)
				if err != nil {
					return nil, err
				}
				// The fake VMM has no chroot, so it is given the host paths.
				api, _ := memory.Path("api.sock")
				ram, _ := memory.Path("ram.sock")
				return s.spawn("TestStarterChild", "SPROUTFS_STARTER_CHILD=jailer", "--api-sock", api, "--ram", ram)
			},
			want: "vmmachine: pager peer is not the supervised VMM",
			vmm:  "ended by signal: killed, released 1, reaped true",
		},
		{
			name: "a VMM that exits before binding its API socket",
			start: func(s *adversarialStarter, ctx context.Context, launch *vmmachine.Launch) (vmmachine.VMM, error) {
				if _, err := s.prepare(ctx, launch); err != nil {
					return nil, err
				}
				return s.spawn("TestStarterChild", "SPROUTFS_STARTER_CHILD=exit")
			},
			want: "vmmachine: startup: process exited: exit status 3: the fake VMM exits before binding its API socket\n",
			vmm:  "ended by exit status 3, released 1, reaped true",
		},
		{
			name:     "a placed directory that already exists",
			existing: true,
			start: func(s *adversarialStarter, ctx context.Context, launch *vmmachine.Launch) (vmmachine.VMM, error) {
				_, err := s.prepare(ctx, launch)
				return nil, err
			},
			want:    "vmmachine: starting the VMM of vm: mkdir PARENT/placed: file exists",
			vmm:     "no VMM",
			remains: []string{"placed", "placed/kept"},
		},
		{
			name:   "a relative Within",
			within: "vms/placed",
			start: func(s *adversarialStarter, ctx context.Context, launch *vmmachine.Launch) (vmmachine.VMM, error) {
				_, err := s.prepare(ctx, launch)
				return nil, err
			},
			want: `vmmachine: starting the VMM of vm: vmmachine: the VMM's view of its directory is not absolute: "vms/placed"`,
			vmm:  "no VMM",
		},
	}
	for _, key := range []string{"snapshot_path", "mem_file_path", "mem_backend", "pmem_overrides", "resume_vm"} {
		cases = append(cases, starterCase{
			name:    "a restore whose load names " + key,
			restore: true,
			start: func(s *adversarialStarter, ctx context.Context, launch *vmmachine.Launch) (vmmachine.VMM, error) {
				memory, err := s.prepare(ctx, launch)
				if err != nil {
					return nil, err
				}
				memory.Load[key] = "the Starter's own"
				api, _ := memory.Path("api.sock")
				return s.spawn("TestStartupAttachmentChild", "SPROUTFS_STARTUP_CHILD=1", "--api-sock", api)
			},
			want: fmt.Sprintf("vmmachine: the Starter's load request names %q, which this package loads", key),
			vmm:  "ended by signal: killed, released 1, reaped true",
		})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			config, _, pager := startupFixture(t)
			parent := t.TempDir()
			placement := adversarialPlacement(parent)
			if c.within != "" {
				placement.Within = c.within
			}
			if c.existing {
				if err := os.Mkdir(placement.Directory, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(placement.Directory, "kept"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			starter := &adversarialStarter{t: t, placement: placement, start: c.start}
			config.Starter = starter
			if c.restore {
				config.RestoreState = []byte("bounded VMM state")
			}
			ctx := sim.WithRuntime(t.Context(), sim.New(sim.Config{}))
			p, err := vmmachine.Start(ctx, config)
			if p != nil {
				t.Fatal("a Starter that broke its contract started a process")
			}
			want := strings.ReplaceAll(c.want, "PARENT", parent)
			if err == nil || err.Error() != want {
				t.Fatalf("start = %v, want %s", err, want)
			}
			if got := starter.vmm.outcome(); got != c.vmm {
				t.Fatalf("the Starter's process: %s, want %s", got, c.vmm)
			}
			if got := filesUnder(t, parent); !slices.Equal(got, c.remains) {
				t.Fatalf("the start left %v under the placement's parent, want %v", got, c.remains)
			}
			if err := config.Scratch.Close(); err != nil {
				t.Fatalf("the scratch did not close: %v", err)
			}
			stats, err := pager.Stats(t.Context())
			if err != nil || stats.LogicalPages != 0 {
				t.Fatalf("the start left the pager mapping: %+v %v", stats, err)
			}
		})
	}
}

// TestAFailedReleaseIsReportedOnce: a Starter whose release of its process
// fails is told so by Close, and a Close retried after it neither releases
// the process again nor forgets that the release failed.
func TestAFailedReleaseIsReportedOnce(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("giving a directory to another user needs root; run the Firecracker Lima qualification script")
	}
	config, _, _ := startupFixture(t)
	parent := t.TempDir()
	placement := adversarialPlacement(parent)
	// This fake VMM has no chroot, so it names its directory as the host does.
	placement.Within = ""
	starter := &adversarialStarter{t: t, placement: placement,
		start: func(s *adversarialStarter, ctx context.Context, launch *vmmachine.Launch) (vmmachine.VMM, error) {
			memory, err := s.prepare(ctx, launch)
			if err != nil {
				return nil, err
			}
			document := map[string]any{"machine-config": map[string]any{"vcpu_count": 1}}
			if err := memory.Configure(document); err != nil {
				return nil, err
			}
			raw, err := json.Marshal(document)
			if err != nil {
				return nil, err
			}
			path, err := memory.WriteFile(ctx, "config.json", raw)
			if err != nil {
				return nil, err
			}
			vmm, err := s.spawn("TestStartupAttachmentChild", "SPROUTFS_STARTUP_CHILD=1",
				"--api-sock", memory.APISocket, "--config-file", path)
			if err != nil {
				return nil, err
			}
			s.vmm.closeErr = errors.New("the jailer's cgroup is busy")
			return vmm, nil
		}}
	config.Starter = starter
	p, err := vmmachine.Start(sim.WithRuntime(t.Context(), sim.New(sim.Config{})), config)
	if err != nil {
		t.Fatal(err)
	}
	const want = "vmmachine: releasing the VMM of vm: the jailer's cgroup is busy"
	for attempt := range 2 {
		if err := p.Close(); err == nil || err.Error() != want {
			t.Fatalf("close %d = %v, want %s", attempt, err, want)
		}
	}
	if got := starter.vmm.outcome(); got != "ended by signal: killed, released 1, reaped true" {
		t.Fatalf("the Starter's process: %s", got)
	}
	if got := filesUnder(t, parent); len(got) != 0 {
		t.Fatalf("the closed process left %v under the placement's parent", got)
	}
	if err := config.Scratch.Close(); err != nil {
		t.Fatalf("the scratch did not close: %v", err)
	}
}

// filesUnder is every path under dir, relative to it, in lexical order.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != dir {
			relative, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			paths = append(paths, relative)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return paths
}
