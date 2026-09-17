// Command sproutfs-guest-witness is what a guest answers with when it is asked
// whether its memory and its disk are what it wrote.
//
// A soak over a real cluster forks a VM, migrates it, stops it, starts it
// elsewhere and loses the host under it, and every one of those either keeps
// the guest's bytes exactly or is a defect. Nothing outside the guest can say
// which: the host sees pages and the control plane sees checkpoints, and both
// of them would report a successful migration of a guest whose memory came back
// holding another instant's bytes. So the guest is asked.
//
//	witness fill --memory 256M --disk /var/witness --seed 7
//	    fill a buffer of that size and a file of the same size with the pattern
//	    of (seed, step 0), and stay resident serving a unix socket. A witness
//	    that is already resident is refilled in place, which is what a fork's
//	    child does once it has been checked against its parent: from there it
//	    diverges under a seed of its own.
//	witness mutate --step 3
//	    rewrite the pages step 3 takes, in memory and in the file, with the
//	    pattern of (this witness's seed, 3). A scattered fraction of the range,
//	    so the checkpoint behind it has a scattered dirty set to seal.
//	witness check --seed 7 --step 3
//	    require every byte of memory and every byte of the file to be what
//	    (seed, step) says, reading the file again rather than trusting the
//	    buffer. Exits non-zero naming the first byte that is not.
//	witness grow /
//	    give the filesystem mounted there the pages its device gained, which is
//	    what a guest cold started onto a larger root volume runs. See grow.go
//	    for why it is this binary's job and not resize2fs's.
//
// The pattern is a pure function of the seed, the step and the page, so the
// expectation lives in the script that drives the soak and not in the guest:
// nothing about what a VM should hold is carried across a fork, a migration or
// a stop, and a guest cannot agree with itself about the wrong thing.
//
// It is a static binary of the standard library alone, built and installed into
// both guest images the way the guest agent is.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// version is what this binary says it is, stamped at link time with the build's
// `git describe`, as every other command in this repository is.
var version = "dev"

const usage = `sproutfs-guest-witness says whether a guest's memory and disk are what it wrote.

  witness fill --memory 256M --disk /var/witness --seed S [--socket PATH]
  witness mutate --step K [--socket PATH]
  witness check --seed S --step K [--socket PATH]
  witness check --seed S --step K --disk-only --disk /var/witness
  witness grow MOUNTPOINT
  witness version

The pattern is a pure function of (seed, step, page), so what a guest must hold
is arithmetic the script can do for itself.`

const (
	// defaultSocket is where a resident witness serves. /run is the guest's own
	// tmpfs, so a witness that died leaves nothing behind a reboot.
	defaultSocket = "/run/sproutfs-witness.sock"
	// serveArg is the internal verb the resident half is started with. It is
	// never typed: fill starts it, with the listening socket already bound and
	// handed over as a descriptor, so that the request fill makes is answered
	// exactly when the filling is finished rather than after a poll.
	serveArg = "serve"
	// listenerFD is where that socket arrives in the resident half.
	listenerFD = 3
)

// requestTimeout bounds one request from the outside. A fill of a few hundred
// megabytes and a check that reads the same off the disk are the long ones, and
// they run at memory and PMEM speed; this is the bound on a witness that is
// wedged rather than on the work. It is a variable so that a test of what
// happens when it is reached need not take ten minutes to make its point.
var requestTimeout = 10 * time.Minute

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "sproutfs-guest-witness: %v\n", err)
		os.Exit(1)
	}
}

// options is one command line, parsed.
type options struct {
	socket, disk, log string
	memory            int64
	seed, step        uint64
	// seeded and stepped report the flags that were given, so that a missing
	// one is refused rather than taken as zero: step 0 is the fill and seed 0
	// is a perfectly good seed.
	seeded, stepped bool
	// diskOnly checks the file alone, with no witness resident and no memory to
	// check. It is what a cold-started guest answers with: its memory and the
	// process that held it were discarded, and its disk is exactly what the last
	// checkpoint published.
	diskOnly bool
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("no command\n\n%s", usage)
	}
	command, rest := args[0], args[1:]
	if command == "help" || command == "-h" || command == "--help" {
		fmt.Println(usage)
		return nil
	}
	if command == "version" {
		fmt.Println(version)
		return nil
	}
	// The one command whose argument is not a flag: a mount point is what it
	// acts on, and there is nothing else to say about it.
	if command == "grow" {
		if len(rest) != 1 || strings.HasPrefix(rest[0], "-") {
			return fmt.Errorf("grow takes one argument, the mount point to grow\n\n%s", usage)
		}
		return growAt(rest[0])
	}
	parsed, err := parse(rest)
	if err != nil {
		return fmt.Errorf("%w\n\n%s", err, usage)
	}
	switch command {
	case serveArg:
		return serve(parsed)
	case "fill":
		if !parsed.seeded {
			return errors.New("fill needs --seed")
		}
		if parsed.disk == "" {
			return errors.New("fill needs --disk, the path of the file it writes")
		}
		if parsed.memory == 0 {
			return errors.New("fill needs --memory, such as 256M")
		}
		return fill(parsed)
	case "mutate":
		if !parsed.stepped || parsed.step == 0 {
			return errors.New("mutate needs --step, 1 or higher: step 0 is the fill")
		}
		return ask(parsed.socket, fmt.Sprintf("mutate %d", parsed.step))
	case "check":
		if !parsed.seeded || !parsed.stepped {
			return errors.New("check needs --seed and --step: the expectation is the caller's")
		}
		if parsed.diskOnly {
			if parsed.disk == "" {
				return errors.New("check --disk-only needs --disk, the path of the file it reads")
			}
			return checkFile(parsed.disk, parsed.seed, parsed.step)
		}
		return ask(parsed.socket, fmt.Sprintf("check %d %d", parsed.seed, parsed.step))
	default:
		return fmt.Errorf("no command named %q\n\n%s", command, usage)
	}
}

// parse reads the flags, which are --name value or --name=value in any order.
func parse(args []string) (options, error) {
	parsed := options{socket: defaultSocket}
	for len(args) > 0 {
		argument := args[0]
		args = args[1:]
		name, value, inline := strings.Cut(strings.TrimPrefix(argument, "--"), "=")
		if !strings.HasPrefix(argument, "--") {
			return options{}, fmt.Errorf("unexpected argument %q", argument)
		}
		// The one flag that stands alone: it carries no value and means itself.
		if name == "disk-only" {
			if inline {
				return options{}, errors.New("--disk-only takes no value")
			}
			parsed.diskOnly = true
			continue
		}
		if !inline {
			if len(args) == 0 {
				return options{}, fmt.Errorf("--%s needs a value", name)
			}
			value, args = args[0], args[1:]
		}
		var err error
		switch name {
		case "socket":
			parsed.socket = value
		case "disk":
			parsed.disk = value
		case "log":
			parsed.log = value
		case "memory":
			parsed.memory, err = parseSize(value)
		case "seed":
			parsed.seed, err = strconv.ParseUint(value, 10, 64)
			parsed.seeded = err == nil
		case "step":
			parsed.step, err = strconv.ParseUint(value, 10, 64)
			parsed.stepped = err == nil
		default:
			return options{}, fmt.Errorf("no flag named --%s", name)
		}
		if err != nil {
			return options{}, fmt.Errorf("--%s is %q: %w", name, value, err)
		}
	}
	return parsed, nil
}

// fill makes this guest's memory and disk the pattern of (seed, step 0).
//
// A witness that is already resident is refilled in place, which is what keeps
// this one verb one thing to the caller: a fork's child runs the same command
// its parent did and comes out under a seed of its own. Otherwise the resident
// half is started here.
//
// The socket is bound before that half exists and handed to it as a descriptor,
// so there is nothing to poll for: the request below connects at once and is
// answered exactly when the filling is finished. The alternative — start the
// process, then try to connect until it is listening — turns "is the guest
// filled" into a sleep loop that can only guess.
func fill(parsed options) error {
	if err := ask(parsed.socket, fmt.Sprintf("fill %d", parsed.seed)); err == nil {
		return nil
	} else if !unreachable(err) {
		return err
	}
	// Whatever is at the path is a witness that is gone: a unix socket outlives
	// the process that bound it, and binding over it is refused.
	if err := os.Remove(parsed.socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clearing the stale witness socket %s: %w", parsed.socket, err)
	}
	listener, err := net.Listen("unix", parsed.socket)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", parsed.socket, err)
	}
	unix, ok := listener.(*net.UnixListener)
	if !ok {
		return errors.Join(fmt.Errorf("%s is not a unix socket", parsed.socket), listener.Close())
	}
	// A unix listener the resident half is to keep must not be unlinked when
	// this process closes its own copy of it.
	unix.SetUnlinkOnClose(false)
	socket, err := unix.File()
	if err != nil {
		return errors.Join(fmt.Errorf("handing over the witness socket: %w", err), listener.Close())
	}
	started := start(parsed, socket)
	// This process's own copies of the socket go now, before the request below
	// rather than when this function returns. While anything holds them the
	// socket goes on listening whether the resident half is there or not, so a
	// resident half that could not start — a witness file it cannot open, a size
	// the guest cannot allocate — would leave the request queued on a listener
	// nothing will ever accept from, and what is an immediate failure with a
	// reason would be a wait for the request timeout. With them closed the
	// socket is the resident half's alone: if it is gone, connecting is refused
	// at once.
	closed := errors.Join(socket.Close(), listener.Close())
	if started != nil {
		return errors.Join(started, closed)
	}
	if err := ask(parsed.socket, fmt.Sprintf("fill %d", parsed.seed)); err != nil {
		// And the reason is the resident half's own. It wrote it to its log and
		// died, and nothing outside the guest ever reads that file, so it is
		// carried out here with the failure it explains.
		return fmt.Errorf("%w%s", err, lastWords(logPath(parsed)))
	}
	return nil
}

// logPath is where the resident half says what it is doing and why it stopped.
func logPath(parsed options) string {
	if parsed.log != "" {
		return parsed.log
	}
	return filepath.Join(os.TempDir(), "sproutfs-guest-witness.log")
}

// lastWords is the end of that log, for a failure that has no other explanation
// anywhere a caller can reach.
func lastWords(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	const keep = 512
	if len(raw) > keep {
		raw = raw[len(raw)-keep:]
	}
	said := strings.TrimSpace(string(raw))
	if said == "" {
		return ""
	}
	return fmt.Sprintf("; the witness log %s ends: %s", path, said)
}

// start runs the resident half, detached from whatever ran this: a witness
// outlives the exec that filled it, and a session that ends must not take the
// guest's memory with it.
func start(parsed options, socket *os.File) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding this binary: %w", err)
	}
	log, err := os.OpenFile(logPath(parsed), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("the witness log %s: %w", logPath(parsed), err)
	}
	defer log.Close()
	quiet, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("opening %s: %w", os.DevNull, err)
	}
	defer quiet.Close()
	arguments := []string{self, serveArg,
		"--disk", parsed.disk,
		"--memory", strconv.FormatInt(parsed.memory, 10),
		"--socket", parsed.socket}
	process, err := os.StartProcess(self, arguments, &os.ProcAttr{
		Files: []*os.File{quiet, log, log, socket},
		// A session of its own, so the shell that ran the fill hanging up does
		// not hang up the witness.
		Sys: &syscall.SysProcAttr{Setsid: true},
	})
	if err != nil {
		return fmt.Errorf("starting the resident witness: %w", err)
	}
	// It is nobody's child: this process exits as soon as the fill is answered,
	// and the guest's init reaps it.
	return process.Release()
}

// serve is the resident half: it holds the buffer and the file and answers the
// socket its caller bound and handed over. It never returns.
func serve(parsed options) error {
	file := os.NewFile(listenerFD, parsed.socket)
	if file == nil {
		return errors.New("the witness was started without its socket")
	}
	listener, err := net.FileListener(file)
	if err != nil {
		return fmt.Errorf("taking over the witness socket: %w", err)
	}
	defer listener.Close()
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing the handed-over socket: %w", err)
	}
	held, err := open(parsed.disk, parsed.memory)
	if err != nil {
		return err
	}
	defer held.close()
	fmt.Fprintf(os.Stderr, "sproutfs-guest-witness: resident on %s, %d bytes of memory and %s\n",
		parsed.socket, parsed.memory, parsed.disk)
	return serveOn(listener, held)
}

// serveOn answers on one listener until it is closed. One request at a time: a
// check that reads every byte must not run beside a mutate that rewrites some
// of them, and two callers asking at once are answered one after the other
// rather than both out of a half-written state.
func serveOn(listener net.Listener, held *state) error {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return fmt.Errorf("accepting on %s: %w", listener.Addr(), err)
		}
		answer(held, connection)
	}
}

// answer reads one request and writes one reply. One at a time: a check that
// reads every byte must not run beside a mutate that rewrites some of them, and
// the caller is one script.
func answer(held *state, connection net.Conn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(requestTimeout))
	line, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "sproutfs-guest-witness: reading a request: %v\n", err)
		return
	}
	reply := perform(held, strings.Fields(strings.TrimSpace(line)))
	if _, err := connection.Write([]byte(reply + "\n")); err != nil {
		fmt.Fprintf(os.Stderr, "sproutfs-guest-witness: writing a reply: %v\n", err)
	}
}

// perform runs one request and reports the line to answer with: "ok" and what
// it did, or "error" and why. A mismatch is an error like any other — the
// caller wants the offset and a non-zero exit, not a status code.
func perform(held *state, request []string) string {
	fail := func(format string, args ...any) string {
		return "error " + fmt.Sprintf(format, args...)
	}
	if len(request) == 0 {
		return fail("empty request")
	}
	switch request[0] {
	case "fill":
		seed, err := number(request, 1)
		if err != nil {
			return fail("%v", err)
		}
		if err := held.fill(seed); err != nil {
			return fail("%v", err)
		}
		return fmt.Sprintf("ok filled %d bytes with seed %d", held.size, seed)
	case "mutate":
		step, err := number(request, 1)
		if err != nil {
			return fail("%v", err)
		}
		pages, err := held.mutate(step)
		if err != nil {
			return fail("%v", err)
		}
		return fmt.Sprintf("ok rewrote %d of %d pages at step %d", pages, held.pages(), step)
	case "check":
		seed, err := number(request, 1)
		if err != nil {
			return fail("%v", err)
		}
		step, err := number(request, 2)
		if err != nil {
			return fail("%v", err)
		}
		if err := held.check(seed, step); err != nil {
			return fail("%v", err)
		}
		return fmt.Sprintf("ok %d bytes of memory and disk hold (seed %d, step %d)",
			held.size, seed, step)
	default:
		return fail("no request named %q", request[0])
	}
}

// number reads one argument of a request.
func number(request []string, index int) (uint64, error) {
	if index >= len(request) {
		return 0, fmt.Errorf("%s takes %d arguments", request[0], index)
	}
	value, err := strconv.ParseUint(request[index], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number", request[0], request[index])
	}
	return value, nil
}

// ask sends one request to the resident witness and reports what it said. An
// "error" reply is this command's own failure, so a script reads an exit status
// and the first byte that was wrong.
func ask(socket, request string) error {
	connection, err := net.Dial("unix", socket)
	if err != nil {
		return fmt.Errorf("reaching the witness at %s: %w", socket, err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(requestTimeout)); err != nil {
		return err
	}
	if _, err := connection.Write([]byte(request + "\n")); err != nil {
		return fmt.Errorf("asking the witness: %w", err)
	}
	line, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		return fmt.Errorf("reading the witness's answer: %w", err)
	}
	answer := strings.TrimSpace(line)
	if rest, failed := strings.CutPrefix(answer, "error "); failed {
		return errors.New(rest)
	}
	fmt.Println(strings.TrimPrefix(answer, "ok "))
	return nil
}

// unreachable reports a witness that is not there, which is what makes a fill
// start one rather than fail. Anything else — a witness that answered with a
// refusal, a socket it could not read — is this command's own failure.
func unreachable(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}
