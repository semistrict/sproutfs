package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseReadsEachCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want invocation
	}{
		{name: "create", args: []string{"create"}, want: invocation{Command: "create", Count: 1}},
		{name: "create with a template", args: []string{"create", "--template", "alpine"},
			want: invocation{Command: "create", Template: "alpine", Count: 1}},
		{name: "create with an inline template", args: []string{"create", "--template=debian"},
			want: invocation{Command: "create", Template: "debian", Count: 1}},
		{name: "list", args: []string{"list"}, want: invocation{Command: "list", Count: 1}},
		{name: "hosts", args: []string{"hosts"}, want: invocation{Command: "hosts", Count: 1}},
		{name: "console", args: []string{"console", "vm-1"},
			want: invocation{Command: "console", Target: "vm-1", Count: 1}},
		{name: "fork", args: []string{"fork", "vm-1"},
			want: invocation{Command: "fork", Target: "vm-1", Count: 1}},
		{name: "fork onto another host", args: []string{"fork", "vm-1", "--to", "host-b"},
			want: invocation{Command: "fork", Target: "vm-1", Count: 1, To: "host-b"}},
		{name: "fork with a count", args: []string{"fork", "vm-1", "--count", "20"},
			want: invocation{Command: "fork", Target: "vm-1", Count: 20}},
		{name: "migrate", args: []string{"migrate", "vm-1", "--to", "sproutfs-host-b"},
			want: invocation{Command: "migrate", Target: "vm-1", To: "sproutfs-host-b", Count: 1}},
		{name: "migrate anywhere", args: []string{"migrate", "vm-1"},
			want: invocation{Command: "migrate", Target: "vm-1", Count: 1}},
		{name: "capture", args: []string{"capture", "vm-1"},
			want: invocation{Command: "capture", Target: "vm-1", Count: 1}},
		{name: "kill-host", args: []string{"kill-host", "sproutfs-host-a"},
			want: invocation{Command: "kill-host", Target: "sproutfs-host-a", Count: 1}},
		{name: "recover", args: []string{"recover", "vm-1"},
			want: invocation{Command: "recover", Target: "vm-1", Count: 1}},
		{name: "recover by force", args: []string{"recover", "vm-1", "--force"},
			want: invocation{Command: "recover", Target: "vm-1", Count: 1, Force: true}},
		{name: "delete", args: []string{"delete", "vm-1"},
			want: invocation{Command: "delete", Target: "vm-1", Count: 1}},
		{name: "stop", args: []string{"stop", "vm-1"},
			want: invocation{Command: "stop", Target: "vm-1", Count: 1}},
		{name: "suspend", args: []string{"stop", "vm-1", "--suspend"},
			want: invocation{Command: "stop", Target: "vm-1", Count: 1, Suspend: true}},
		{name: "start", args: []string{"start", "vm-1"},
			want: invocation{Command: "start", Target: "vm-1", Count: 1}},
		{name: "start on a named host", args: []string{"start", "vm-1", "--to", "sproutfs-host-b"},
			want: invocation{Command: "start", Target: "vm-1", To: "sproutfs-host-b", Count: 1}},
		{name: "create pulling its memory", args: []string{"create", "--pull"},
			want: invocation{Command: "create", Count: 1, Pull: true}},
		{name: "start pulling its memory", args: []string{"start", "vm-1", "--pull"},
			want: invocation{Command: "start", Target: "vm-1", Count: 1, Pull: true}},
		{name: "fork children pulling their memory", args: []string{"fork", "vm-1", "--count", "2", "--pull"},
			want: invocation{Command: "fork", Target: "vm-1", Count: 2, Pull: true}},
		{name: "check", args: []string{"check"}, want: invocation{Command: "check", Count: 1}},
		{name: "help", args: []string{"--help"}, want: invocation{Command: "help"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parse(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("parsed %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseRefusesWhatItCannotRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "nothing", args: nil, want: "usage: no command"},
		{name: "an unknown command", args: []string{"reboot"}, want: `usage: no command named "reboot"`},
		{name: "a VM command with no VM", args: []string{"fork"}, want: "usage: fork needs the name of a vm"},
		{name: "a host command with no host", args: []string{"kill-host"},
			want: "usage: kill-host needs the name of a host"},
		{name: "a flag the command does not take", args: []string{"capture", "vm-1", "--count", "2"},
			want: "usage: capture takes no --count"},
		{name: "a flag with no value", args: []string{"fork", "vm-1", "--count"},
			want: "usage: --count needs a value"},
		{name: "an empty flag value", args: []string{"migrate", "vm-1", "--to="},
			want: "usage: --to needs a value"},
		{name: "a count that is not a number", args: []string{"fork", "vm-1", "--count", "many"},
			want: `usage: --count is "many", want a positive number`},
		{name: "a count of zero", args: []string{"fork", "vm-1", "--count", "0"},
			want: `usage: --count is "0", want a positive number`},
		{name: "a stray argument", args: []string{"fork", "vm-1", "twice"},
			want: `usage: fork takes no argument "twice"`},
		{name: "a second target", args: []string{"list", "vm-1"},
			want: `usage: list takes no argument "vm-1"`},
		{name: "a pull of a VM that is already running", args: []string{"migrate", "vm-1", "--pull"},
			want: "usage: migrate takes no --pull"},
		{name: "a pull with a value", args: []string{"create", "--pull=yes"},
			want: "usage: --pull takes no value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(tc.args)
			if err == nil {
				t.Fatalf("%v was accepted", tc.args)
			}
			if !errors.Is(err, errUsage) {
				t.Fatalf("error %v is not a usage failure", err)
			}
			if err.Error() != tc.want {
				t.Fatalf("error %q, want %q", err, tc.want)
			}
		})
	}
}

func TestUsageNamesEveryCommand(t *testing.T) {
	for name := range commands {
		if !strings.Contains(usage, "sproutfsctl "+name) {
			t.Fatalf("the usage text does not document %s", name)
		}
	}
}

// TestParseExecTakesEverythingAfterTheSeparator: the guest's own flags and
// quoting are the guest's business, so this CLI stops parsing at the --.
func TestParseExecTakesEverythingAfterTheSeparator(t *testing.T) {
	command, err := parse([]string{"exec", "vm-1", "--timeout", "45s", "--", "ls", "-la", "/root"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Command != "exec" || command.Target != "vm-1" {
		t.Fatalf("command %+v", command)
	}
	if command.Cmd != "ls -la /root" {
		t.Fatalf("the command is %q, want \"ls -la /root\"", command.Cmd)
	}
	if command.Timeout != 45*time.Second {
		t.Fatalf("the timeout is %s, want 45s", command.Timeout)
	}
}

// TestParseExecNeedsACommand: there is nothing to run without one.
func TestParseExecNeedsACommand(t *testing.T) {
	for _, args := range [][]string{
		{"exec", "vm-1"},
		{"exec", "vm-1", "--"},
		{"exec", "vm-1", "--timeout", "5s"},
	} {
		if _, err := parse(args); !errors.Is(err, errUsage) {
			t.Fatalf("%v parsed to %v, want a usage error", args, err)
		}
	}
}
