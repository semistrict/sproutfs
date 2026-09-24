package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// invocation is one command line, parsed. Parsing is separate from running so
// that what the CLI accepts can be asserted without a deployment.
type invocation struct {
	Command string
	// Target is the VM or host the command acts on, empty for the commands
	// that act on the deployment.
	Target string
	// Template is the guest image create starts from, Count the forks fork
	// takes, and To the host migrate moves to and fork places its children on.
	Template string
	Count    int
	To       string
	// For is how long a console session stays attached whatever its input does.
	// Zero ends it with the input, which is what an interactive session wants.
	For time.Duration
	// Cmd is the shell command exec runs in a guest, which is everything after
	// the -- that ends this CLI's own arguments. Timeout bounds it.
	Cmd     string
	Timeout time.Duration
	// Force is the operator's own evidence that a VM's host is gone, which
	// recover needs when the pod is still listed but does not answer.
	Force bool
	// Cold starts a VM without its memory: the host discards every page of it
	// and the VMM state with it, and boots the kernel from the root volume.
	// Memory and Disk are the shape it comes back at, which only a cold start
	// may change, because it is the one moment nothing in memory describes it.
	Cold         bool
	Memory, Disk uint64
	// Suspend stops a VM with its memory and its VMM state published beside
	// its disks, so a start resumes it rather than booting it.
	Suspend bool
}

// errUsage reports a command line this CLI will not run. Its message is what
// the user is shown, followed by the usage text.
var errUsage = errors.New("usage")

const usage = `sproutfsctl drives a sproutfs demo deployment through its orchestrator.

  sproutfsctl create [--template NAME]     create a VM and boot it
  sproutfsctl list                         list the VMs, their hosts and their states
  sproutfsctl hosts                        list the host pods
  sproutfsctl store                        what each host's object store has served
  sproutfsctl console VM [--for 10s]       attach to a VM's serial console
  sproutfsctl exec VM [--timeout 30s] -- CMD
                                           run a shell command in a VM's guest
  sproutfsctl fork VM [--count N] [--to HOST]
                                           fork a running VM, here or on another host
  sproutfsctl migrate VM [--to HOST]       move a VM to another host
  sproutfsctl capture VM                   take a checkpoint now
  sproutfsctl kill-host HOST               delete a host pod, losing its unpublished writes
  sproutfsctl recover VM [--force]         reopen a VM whose host is gone
  sproutfsctl stop VM [--suspend]          checkpoint a VM's disks and close it, keeping
                                           the VM; --suspend keeps its memory too,
                                           so a start resumes it rather than booting it
  sproutfsctl start VM [--to HOST] [--cold] [--memory 1G] [--disk 4G]
                                           open a stopped VM on a host again;
                                           --cold discards its memory and boots
                                           its kernel, and only a cold start may
                                           resize it
  sproutfsctl delete VM                    close and delete a VM
  sproutfsctl check                        check the deployment's durable state

The orchestrator is SPROUTFS_ORCHESTRATOR, http://localhost:8080 by default,
which is where kubectl port-forward puts it, and SPROUTFS_API_TOKEN is the
deployment's shared token every request carries.`

// commands is what each command takes: whether it names a VM or a host, and
// which flags it accepts.
var commands = map[string]struct {
	target string // "vm", "host" or "" for none
	flags  []string
	// switches are the flags that stand alone: they carry no value and mean
	// themselves.
	switches []string
	// trailing takes everything after a -- as one command line, which is what
	// keeps a guest's own flags and quoting out of this CLI's parser.
	trailing bool
}{
	"create":    {flags: []string{"template"}},
	"list":      {},
	"hosts":     {},
	"store":     {},
	"console":   {target: "vm", flags: []string{"for"}},
	"exec":      {target: "vm", flags: []string{"timeout"}, trailing: true},
	"fork":      {target: "vm", flags: []string{"count", "to"}},
	"migrate":   {target: "vm", flags: []string{"to"}},
	"capture":   {target: "vm"},
	"kill-host": {target: "host"},
	"recover":   {target: "vm", switches: []string{"force"}},
	"stop":      {target: "vm", switches: []string{"suspend"}},
	"start":     {target: "vm", flags: []string{"to", "memory", "disk"}, switches: []string{"cold"}},
	"delete":    {target: "vm"},
	"check":     {},
}

// parse reads one command line. Flags are --name value or --name=value, in any
// order after the command's argument.
func parse(args []string) (invocation, error) {
	if len(args) == 0 {
		return invocation{}, fmt.Errorf("%w: no command", errUsage)
	}
	name := args[0]
	if name == "help" || name == "-h" || name == "--help" {
		return invocation{Command: "help"}, nil
	}
	spec, known := commands[name]
	if !known {
		return invocation{}, fmt.Errorf("%w: no command named %q", errUsage, name)
	}
	result := invocation{Command: name, Count: 1}
	rest := args[1:]
	if spec.target != "" {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return invocation{}, fmt.Errorf("%w: %s needs the name of a %s", errUsage, name, spec.target)
		}
		result.Target, rest = rest[0], rest[1:]
	}
	for len(rest) > 0 {
		argument := rest[0]
		rest = rest[1:]
		if spec.trailing && argument == "--" {
			result.Cmd = strings.Join(rest, " ")
			if strings.TrimSpace(result.Cmd) == "" {
				return invocation{}, fmt.Errorf("%w: %s needs a command after --", errUsage, name)
			}
			return result, nil
		}
		if !strings.HasPrefix(argument, "--") {
			return invocation{}, fmt.Errorf("%w: %s takes no argument %q", errUsage, name, argument)
		}
		flag, value, inline := strings.Cut(strings.TrimPrefix(argument, "--"), "=")
		if contains(spec.switches, flag) {
			if inline {
				return invocation{}, fmt.Errorf("%w: --%s takes no value", errUsage, flag)
			}
			switch flag {
			case "force":
				result.Force = true
			case "cold":
				result.Cold = true
			case "suspend":
				result.Suspend = true
			}
			continue
		}
		if !contains(spec.flags, flag) {
			return invocation{}, fmt.Errorf("%w: %s takes no --%s", errUsage, name, flag)
		}
		if !inline {
			if len(rest) == 0 {
				return invocation{}, fmt.Errorf("%w: --%s needs a value", errUsage, flag)
			}
			value, rest = rest[0], rest[1:]
		}
		if value == "" {
			return invocation{}, fmt.Errorf("%w: --%s needs a value", errUsage, flag)
		}
		switch flag {
		case "template":
			result.Template = value
		case "to":
			result.To = value
		case "count":
			count, err := strconv.Atoi(value)
			if err != nil || count < 1 {
				return invocation{}, fmt.Errorf("%w: --count is %q, want a positive number", errUsage, value)
			}
			result.Count = count
		case "for":
			period, err := time.ParseDuration(value)
			if err != nil || period <= 0 {
				return invocation{}, fmt.Errorf("%w: --for is %q, want a duration such as 10s", errUsage, value)
			}
			result.For = period
		case "timeout":
			period, err := time.ParseDuration(value)
			if err != nil || period <= 0 {
				return invocation{}, fmt.Errorf("%w: --timeout is %q, want a duration such as 30s", errUsage, value)
			}
			result.Timeout = period
		case "memory", "disk":
			size, err := parseBytes(value)
			if err != nil {
				return invocation{}, fmt.Errorf("%w: --%s is %q, want a size such as 1G", errUsage, flag, value)
			}
			if flag == "memory" {
				result.Memory = size
			} else {
				result.Disk = size
			}
		}
	}
	// A shape is a cold boot's and nothing else's: a warm start brings the VM
	// back at the shape its memory describes, and a flag that quietly did
	// nothing would be worse than one that is not accepted.
	if !result.Cold && (result.Memory != 0 || result.Disk != 0) {
		return invocation{}, fmt.Errorf(
			"%w: --memory and --disk need --cold, which is the one moment a VM's shape can change",
			errUsage)
	}
	if spec.trailing {
		return invocation{}, fmt.Errorf("%w: %s needs a command after --", errUsage, name)
	}
	return result, nil
}

// parseBytes reads a size the way an operator writes one: plain bytes, or a
// number with a K, M or G suffix, with or without an iB after it.
func parseBytes(text string) (uint64, error) {
	trimmed := strings.TrimSpace(text)
	unit := uint64(1)
	for _, suffix := range []struct {
		name  string
		scale uint64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30},
	} {
		if rest, found := strings.CutSuffix(trimmed, suffix.name); found {
			trimmed, unit = rest, suffix.scale
			break
		}
	}
	count, err := strconv.ParseUint(trimmed, 10, 64)
	if err != nil || count == 0 {
		return 0, fmt.Errorf("%q is not a size such as 1G", text)
	}
	return count * unit, nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
