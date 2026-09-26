// Command sproutfsctl drives a sproutfs demo deployment: create a VM, attach to
// its console, fork it, move it between hosts, kill a host and recover the VMs
// it was running. It is a thin client of the orchestrator's HTTP API and holds
// nothing of its own.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/api/orch"
	"github.com/semistrict/sproutfs/internal/jsonhttp"
)

// consolePoll is how often an attached console asks for what the guest has
// printed since the last window.
const consolePoll = 200 * time.Millisecond

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	invocation, err := parse(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n\n%s\n", err, usage)
		os.Exit(2)
	}
	client := orch.NewClient(orchestrator(), nil, jsonhttp.Token(os.Getenv(jsonhttp.TokenEnv)))
	if err := execute(ctx, client, invocation, os.Stdin, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

// orchestrator is where the deployment's control plane is, which through
// kubectl port-forward is a local port.
func orchestrator() string {
	if value := strings.TrimSpace(os.Getenv("SPROUTFS_ORCHESTRATOR")); value != "" {
		return value
	}
	return "http://localhost:8080"
}

// execute runs one parsed command against the orchestrator.
func execute(ctx context.Context, client *orch.Client, command invocation,
	in io.Reader, out, problems io.Writer) error {
	switch command.Command {
	case "help":
		_, err := fmt.Fprintln(out, usage)
		return err
	case "create":
		request := orch.CreateRequest{Template: command.Template,
			Memory: command.Memory, Disk: command.Disk, VCPUs: command.VCPUs, Ephemeral: command.Ephemeral,
			Pull: command.Pull}
		if command.From != "" {
			request.From = &host.CheckpointRef{VM: command.From, Checkpoint: command.FromCheckpoint}
		}
		result, err := client.Create(ctx, request)
		if err != nil {
			return err
		}
		// A VM created from a checkpoint with VMM state resumes where that
		// checkpoint's pause left the guest, and says so; every other VM booted.
		how := ""
		if result.Result.Resumed {
			how = " resumed"
		}
		_, err = fmt.Fprintf(out,
			"%s%s on %s (template %.2fs, fork %.2fs, boot %.2fs, root %.2fs, total %.2fs)\n",
			result.Result.VM.ID, how, result.Host, float64(result.Result.Template),
			float64(result.Result.Fork), float64(result.Result.Boot),
			float64(result.Result.Root), float64(result.Result.Total))
		return err
	case "import-template":
		image, err := os.Open(command.Target)
		if err != nil {
			return err
		}
		defer image.Close()
		result, err := client.ImportTemplate(ctx, image, host.ImportTemplateRequest{Memory: command.Memory})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "%s at checkpoint %d (%d bytes of RAM, %.2fs)\n", result.Template.ID,
			result.Checkpoint, result.Template.MemoryBytes, float64(result.Seconds))
		return err
	case "list":
		vms, err := client.VMs(ctx)
		if err != nil {
			return err
		}
		table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(table, "VM\tHOST\tSTATE\tCHECKPOINT\tLOSS\tPRIVATE")
		for _, vm := range vms {
			h, state := vm.Host, vm.State
			if h == "" {
				h = "-"
			}
			if state == "" {
				state = "-"
			}
			// A migration in flight names both ends: a VM between hosts is
			// reported by neither of them.
			if vm.From != "" || vm.To != "" {
				state = fmt.Sprintf("%s %s->%s", state, dash(vm.From), dash(vm.To))
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%d\t%s\t%s\n", vm.ID, h, state, vm.Checkpoint,
				loss(vm), mib(vm.PrivateBytes))
		}
		return table.Flush()
	case "hosts":
		hosts, err := client.Hosts(ctx)
		if err != nil {
			return err
		}
		table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		// Resident is bytes rather than pages: a host runs two pagers of their
		// own pages, and one column of pages would be adding the two.
		fmt.Fprintln(table, "HOST\tREADY\tRUNNING\tSERVING\tRESIDENT\tSHARED\tSERVED\tSTATE")
		for _, h := range hosts {
			state := "ok"
			if h.Error != "" {
				state = h.Error
			}
			fmt.Fprintf(table, "%s\t%t\t%d\t%d\t%d\t%d\t%d\t%s\n", h.Name, h.Ready,
				len(h.Running), len(h.Serving),
				h.Pager.ResidentBytes(), h.Pager.SharedPages(), h.Pages.Served, state)
		}
		return table.Flush()
	case "store":
		hosts, err := client.Hosts(ctx)
		if err != nil {
			return err
		}
		// One row per host and operation, so a run that wants totals over a
		// phase subtracts two readings of the same shape.
		table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(table, "HOST\tOP\tCALLS\tFAILED\tBYTES")
		for _, h := range hosts {
			for _, row := range []struct {
				op    string
				count host.StoreCount
			}{
				{"head", h.Store.Head}, {"get", h.Store.Get}, {"put", h.Store.Put},
				{"delete", h.Store.Delete}, {"list", h.Store.List},
			} {
				fmt.Fprintf(table, "%s\t%s\t%d\t%d\t%d\n", h.Name, row.op,
					row.count.Calls, row.count.Failures, row.count.Bytes)
			}
		}
		return table.Flush()
	case "fork":
		result, err := client.Fork(ctx, command.Target,
			orch.ForkRequest{Count: command.Count, To: command.To, Pull: command.Pull})
		if err != nil {
			return err
		}
		table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(table, "FORK\tHOST\tPAUSE\tSTART\tTOTAL")
		for _, child := range result.Children {
			fmt.Fprintf(table, "%s\t%s\t%.3f\t%.3f\t%.3f\n", child, result.To,
				float64(result.Capture), float64(result.Start), float64(result.Total))
		}
		return table.Flush()
	case "migrate":
		result, err := client.Migrate(ctx, command.Target, command.To)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out,
			"%s moved from %s to %s: pause %.3fs, stream %.3fs, %d pages from the source, %d unpublished\n",
			result.VM, result.From, result.To, float64(result.Pause), float64(result.Stream),
			result.PeerPages, result.Unpublished)
		return err
	case "capture":
		result, err := client.Capture(ctx, command.Target, orch.CaptureRequest{New: command.New, Keep: command.Keep})
		if err != nil {
			return err
		}
		if command.New {
			_, err = fmt.Fprintf(out, "%s captured into %s at checkpoint %d on %s in %.3fs\n",
				command.Target, result.Result.VM, result.Result.Checkpoint, result.Host,
				float64(result.Result.Publish))
			return err
		}
		_, err = fmt.Fprintf(out, "%s checkpoint %d%s on %s: pause %.3fs, publish %.3fs\n",
			result.Result.VM, result.Result.Checkpoint, keptNote(command.Keep), result.Host,
			float64(result.Result.Pause), float64(result.Result.Publish))
		return err
	case "kill-host":
		result, err := client.Kill(ctx, command.Target)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "killed %s\n", result.Host)
		return err
	case "recover":
		result, err := client.Recover(ctx, command.Target, command.Force)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "%s reopened on %s from checkpoint %d in %.3fs\n",
			result.Result.VM.ID, result.Host, result.Result.VM.Checkpoint, float64(result.Result.Total))
		return err
	case "stop":
		result, err := client.Stop(ctx, command.Target, orch.StopRequest{Suspend: command.Suspend, Keep: command.Keep})
		if err != nil {
			return err
		}
		verb := "stopped"
		if command.Suspend {
			verb = "suspended"
		}
		_, err = fmt.Fprintf(out, "%s %s on %s at checkpoint %d%s in %.3fs\n",
			verb, result.VM, result.Host, result.Checkpoint, keptNote(command.Keep), float64(result.Total))
		return err
	case "kept":
		result, err := client.Kept(ctx, command.Target)
		if err != nil {
			return err
		}
		// STATE says whether a create from the checkpoint resumes the guest,
		// and FORKED whether a VM was created from it, which is the one a
		// release refuses.
		table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(table, "CHECKPOINT\tTIME\tSTATE\tFORKED")
		for _, kept := range result.Kept {
			fmt.Fprintf(table, "%d\t%s\t%t\t%t\n", kept.Checkpoint, kept.Time.UTC().Format(time.RFC3339),
				kept.State, kept.Forked)
		}
		return table.Flush()
	case "release":
		if err := client.Release(ctx, command.Target, command.Checkpoint); err != nil {
			return err
		}
		_, err := fmt.Fprintf(out, "released checkpoint %d of %s\n", command.Checkpoint, command.Target)
		return err
	case "start":
		result, err := client.Start(ctx, command.Target, orch.StartRequest{To: command.To,
			Cold: command.Cold, Memory: command.Memory, Disk: command.Disk, VCPUs: command.VCPUs,
			Pull: command.Pull})
		if err != nil {
			return err
		}
		// A cold start says so: the checkpoint it names is the one that
		// discarded the memory rather than the one the stop published, and the
		// guest booted its kernel rather than coming back where it was.
		how := "started"
		if result.Result.Cold {
			how = "cold started"
		}
		_, err = fmt.Fprintf(out, "%s %s on %s from checkpoint %d in %.3fs\n",
			result.Result.VM.ID, how, result.Host, result.Result.VM.Checkpoint,
			float64(result.Result.Total))
		return err
	case "check":
		result, err := client.Check(ctx)
		if err != nil {
			return err
		}
		// Every violation is printed, because each names an object an operator
		// has to look at; the exit status is what a scripted run reads.
		for _, violation := range result.Violations {
			if _, err := fmt.Fprintf(out, "[%s] %s: %s\n",
				violation.Class, violation.Key, violation.Message); err != nil {
				return err
			}
		}
		if !result.OK {
			return fmt.Errorf("the deployment disagrees with itself: %d violations",
				len(result.Violations))
		}
		_, err = fmt.Fprintln(out, "the deployment's durable state agrees with itself")
		return err
	case "delete":
		if err := client.Delete(ctx, command.Target); err != nil {
			return err
		}
		_, err := fmt.Fprintf(out, "deleted %s\n", command.Target)
		return err
	case "exec":
		result, err := client.Exec(ctx, command.Target,
			orch.ExecRequest{Cmd: command.Cmd, Timeout: command.Timeout.Seconds()})
		if err != nil {
			return err
		}
		// The guest's own streams, kept apart: a flow that reads what a command
		// printed should not have to sift a diagnostic out of it.
		if _, err := io.WriteString(out, result.Result.Stdout); err != nil {
			return err
		}
		if _, err := io.WriteString(problems, result.Result.Stderr); err != nil {
			return err
		}
		if result.Result.Exit != 0 {
			return fmt.Errorf("%s exited %d on %s", command.Target, result.Result.Exit, result.Host)
		}
		return nil
	case "console":
		return console(ctx, client, command.Target, in, out, command.For)
	default:
		return fmt.Errorf("%w: no command named %q", errUsage, command.Command)
	}
}

// dash writes an empty host as something a column can hold.
func dash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// loss writes one VM's loss window as a column: how long its host has held a
// write no checkpoint of it covers, and a mark on a VM already past the window,
// whose stores that host is holding back until a checkpoint lands. A VM with
// nothing unpublished — and one no host reports, which holds nothing anywhere —
// shows a dash rather than a zero, because zero would read as a VM that is
// somehow always durable.
func loss(vm orch.VM) string {
	if vm.LossWindow <= 0 {
		return "-"
	}
	age := vm.LossWindow.Round(time.Second).String()
	if vm.Waiting {
		return age + " waiting"
	}
	return age
}

// mib writes a byte count as whole mebibytes, which is the unit a guest's
// memory is talked about in and near enough the pager's own page that a column
// of them reads as pages. Nothing held shows a dash rather than 0 MiB, so a VM
// that has written nothing since its last checkpoint is told apart at a glance
// from one whose host does not report it.
func mib(bytes uint64) string {
	if bytes == 0 {
		return "-"
	}
	return fmt.Sprintf("%d MiB", bytes/(1<<20))
}

// console attaches to one VM's serial console: everything the guest prints is
// written out, and every line typed in is sent to it. It ends when the input
// ends or the command is interrupted.
//
// A positive follow keeps the session open for that long whatever the input
// does, which is what a scripted flow needs: a command piped in is exhausted
// long before the guest has finished answering it.
func console(ctx context.Context, client *orch.Client, vm string, in io.Reader, out io.Writer,
	follow time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	typed := make(chan error, 1)
	go func() { typed <- send(ctx, client, vm, in) }()

	var until <-chan time.Time
	if follow > 0 {
		timer := time.NewTimer(follow)
		defer timer.Stop()
		until = timer.C
	}
	since := int64(0)
	for {
		window, err := client.Console(ctx, vm, since)
		if err != nil {
			return err
		}
		// The host retains only the newest 1 MiB, so a reader that fell behind
		// is told where its window really starts rather than shown a gap.
		if window.Dropped {
			if _, err := fmt.Fprintf(out, "\n[sproutfsctl: console output before offset %d was dropped]\n", window.Offset); err != nil {
				return err
			}
		}
		if window.Data != "" {
			if _, err := io.WriteString(out, window.Data); err != nil {
				return err
			}
		}
		since = window.Next
		select {
		case err := <-typed:
			if err != nil || follow <= 0 {
				return err
			}
			// The input is exhausted and the session is not: a nil channel
			// never fires again, so what the guest prints back is still read.
			typed = nil
		case <-until:
			return nil
		case <-ctx.Done():
			return nil
		case <-time.After(consolePoll):
		}
	}
}

// send forwards typed lines to the guest, which is what makes a console session
// interactive.
func send(ctx context.Context, client *orch.Client, vm string, in io.Reader) error {
	reader := bufio.NewScanner(in)
	for reader.Scan() {
		if err := client.WriteConsole(ctx, vm, reader.Text()+"\n"); err != nil {
			return err
		}
	}
	return reader.Err()
}

// keptNote is what a capture or a stop that kept its checkpoint adds to the
// checkpoint it names.
func keptNote(kept bool) string {
	if kept {
		return " (kept)"
	}
	return ""
}
