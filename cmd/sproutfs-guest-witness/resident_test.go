package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The resident half: the witness that stays in the guest holding its memory,
// and the socket everything reaches it over. The state and the pattern are
// tested beside this; what is tested here is the part a soak actually uses —
// one process filled once and asked again after a fork, a migration, a stop and
// a host loss, over a socket, by a command that is run and exits each time.
//
// serveTestEnv is how the test binary is asked to be that resident half. fill
// starts it by running this executable again with the `serve` verb, so a test
// of fill spawns the test binary; this makes that spawn run the witness instead
// of the tests.
const serveTestEnv = "SPROUTFS_WITNESS_TEST_SERVE"

func TestMain(m *testing.M) {
	if os.Getenv(serveTestEnv) != "" && len(os.Args) > 1 && os.Args[1] == serveArg {
		if err := run(os.Args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "sproutfs-guest-witness: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// resident starts a witness serving on a socket of its own, in this process, and
// gives back the path everything asks it over. The socket is short: a unix
// socket path is a hundred-odd bytes on every system this runs on, and a test
// directory under the usual temporary root is most of that already.
func resident(t *testing.T, seed uint64) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "w")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	held, err := open(filepath.Join(dir, "witness"), witnessSize)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = serveOn(listener, held)
	}()
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
		<-done
		if err := held.close(); err != nil {
			t.Error(err)
		}
	})
	if err := ask(socket, fmt.Sprintf("fill %d", seed)); err != nil {
		t.Fatal(err)
	}
	return socket
}

// TestTheResidentWitnessAnswersOneRequestAtATime is the round trip a soak makes:
// a guest is filled, worked on and asked whether it still holds what it wrote,
// each of those a command of its own reaching the one resident process.
func TestTheResidentWitnessAnswersOneRequestAtATime(t *testing.T) {
	socket := resident(t, 6)
	if err := run([]string{"check", "--seed", "6", "--step", "0", "--socket", socket}); err != nil {
		t.Fatalf("a witness just filled under seed 6 did not hold (6, 0): %v", err)
	}
	if err := run([]string{"mutate", "--step", "1", "--socket", socket}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"check", "--seed", "6", "--step", "1", "--socket", socket}); err != nil {
		t.Fatalf("a witness mutated to step 1 did not hold (6, 1): %v", err)
	}
	// And the step it was at before is not what it holds any more, which is what
	// says a mutate reached the guest rather than being reported and dropped.
	err := run([]string{"check", "--seed", "6", "--step", "0", "--socket", socket})
	if err == nil {
		t.Fatal("a witness at step 1 agreed that it held step 0")
	}
	if !strings.Contains(err.Error(), "differs at offset ") {
		t.Fatalf("the refusal is %q, want the first byte that was wrong", err)
	}
}

// TestAWitnessCheckedAgainstAnotherSeedIsRefused. A guest that is whole can
// still be the wrong guest: two children of one fork both hold a whole witness,
// and only the seed tells them apart. This is the one thing that catches a
// child that came back holding its sibling's bytes, so it is the one thing that
// must not be reported as agreement.
func TestAWitnessCheckedAgainstAnotherSeedIsRefused(t *testing.T) {
	socket := resident(t, 6)
	err := run([]string{"check", "--seed", "7", "--step", "0", "--socket", socket})
	if err == nil {
		t.Fatal("a witness filled under seed 6 agreed that it held seed 7")
	}
	if !strings.Contains(err.Error(), "(seed 7, step 0)") {
		t.Fatalf("the refusal is %q, want the seed and step it was checked against", err)
	}
}

// TestARefilledWitnessIsTheChildDivergingFromItsParent. A fork's child runs the
// same fill its parent did, and what it reaches is the witness it inherited:
// already resident, holding the parent's bytes. It is refilled in place under a
// seed of its own, and from there it is a different guest — which is exactly
// what fill has to do without a second process and without the caller knowing
// which case it is in.
func TestARefilledWitnessIsTheChildDivergingFromItsParent(t *testing.T) {
	socket := resident(t, 6)
	dir := filepath.Dir(socket)
	// The same command line the parent ran, in a guest that is now the child.
	if err := run([]string{"fill", "--seed", "11", "--socket", socket,
		"--disk", filepath.Join(dir, "witness"), "--memory", "1M"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"check", "--seed", "11", "--step", "0", "--socket", socket}); err != nil {
		t.Fatalf("a child refilled under seed 11 did not hold (11, 0): %v", err)
	}
	if err := run([]string{"check", "--seed", "6", "--step", "0", "--socket", socket}); err == nil {
		t.Fatal("a child refilled under seed 11 still agreed that it held its parent's seed 6")
	}
}

// TestTwoChecksOnOneSocketBothAnswer. A check reads every byte of a guest's
// memory and its file and takes as long as that takes, and nothing stops a flow
// — or a flow and an operator — from asking twice at once. One is answered
// after the other rather than both being answered out of a half-read state.
func TestTwoChecksOnOneSocketBothAnswer(t *testing.T) {
	socket := resident(t, 6)
	var wait sync.WaitGroup
	failures := make([]error, 2)
	for index := range failures {
		wait.Add(1)
		go func() {
			defer wait.Done()
			failures[index] = ask(socket, "check 6 0")
		}()
	}
	wait.Wait()
	for index, err := range failures {
		if err != nil {
			t.Fatalf("the %d of two checks made at once failed: %v", index, err)
		}
	}
}

// TestAResidentWitnessThatCannotStartSaysWhyAtOnce.
//
// The resident half is started with the socket already bound and handed to it,
// so that the fill's own request is answered exactly when the filling is
// finished. That leaves one case to get right: a resident half that cannot
// start at all — a disk that is full, a path that is not writable, a size the
// guest cannot allocate. Until it is reached, the socket goes on listening for
// as long as anything holds it, so a fill that kept its own copy would queue its
// request on a listener nothing will ever accept from and wait out the request
// timeout. In the soak that is the agent killing the command at its timeout and
// reporting 124: five minutes spent, and the reason — which the resident half
// wrote down before it died — never leaving the guest.
func TestAResidentWitnessThatCannotStartSaysWhyAtOnce(t *testing.T) {
	was := requestTimeout
	requestTimeout = 2 * time.Second
	t.Cleanup(func() { requestTimeout = was })
	t.Setenv(serveTestEnv, "1")

	dir, err := os.MkdirTemp("", "w")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// A witness file that is a directory: the resident half opens it, cannot,
	// and exits. Nothing is left running, so nothing has to be cleaned up.
	disk := filepath.Join(dir, "witness")
	if err := os.Mkdir(disk, 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "log")

	began := time.Now()
	err = run([]string{"fill", "--seed", "6", "--socket", filepath.Join(dir, "s"),
		"--disk", disk, "--memory", "1M", "--log", log})
	took := time.Since(began)
	if err == nil {
		t.Fatal("a fill whose witness file is a directory reported success")
	}
	if took >= requestTimeout {
		t.Fatalf("it took %s to fail, which is the request timeout: a resident half "+
			"that is gone has to be noticed rather than waited for", took)
	}
	if !strings.Contains(err.Error(), disk) {
		t.Fatalf("it failed with %q, want the witness's own reason, which names %s", err, disk)
	}
}
