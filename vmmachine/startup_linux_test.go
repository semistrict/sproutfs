//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/internal/vmwire"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/adapters"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmachine"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/volume"
)

// consoleBanner is what this fixture's VMM writes to its console, which is the
// output a diagnostic of a process that died carries.
const consoleBanner = "sproutfs startup child ready"

// This child speaks only the descriptor exchange needed to reach admission.
// It never uses guest memory; a pipe substitutes for UFFD until admission ends.
func TestStartupAttachmentChild(t *testing.T) {
	if os.Getenv("SPROUTFS_STARTUP_CHILD") != "1" {
		return
	}
	var configPath, apiPath string
	for i, arg := range os.Args {
		if arg == "--config-file" && i+1 < len(os.Args) {
			configPath = os.Args[i+1]
		}
		if arg == "--api-sock" && i+1 < len(os.Args) {
			apiPath = os.Args[i+1]
		}
	}
	var config struct {
		Memory struct {
			Socket string `json:"socket_path"`
		} `json:"managed-memory"`
	}
	if configPath != "" {
		raw, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &config); err != nil {
			t.Fatal(err)
		}
	}
	api, err := net.Listen("unix", apiPath)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	go func() {
		_ = http.Serve(api, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/vm" {
				// A VMM refuses a pause it cannot take — a VM already paused,
				// a state it will not leave — by answering. It is running when
				// it does, which is the whole difference from silence.
				if os.Getenv("SPROUTFS_STARTUP_CHILD_REFUSE_PAUSE") == "1" {
					http.Error(w, "the vCPUs are not in a state that can pause", http.StatusBadRequest)
					return
				}
				// A pause that takes a while is how a test opens the window in
				// which a caller can go away: this blocks until the test drops
				// the gate file, announcing first that it is inside the
				// request.
				if gate := os.Getenv("SPROUTFS_STARTUP_CHILD_PAUSE_GATE"); gate != "" {
					_ = os.WriteFile(gate+".entered", []byte("1"), 0o600)
					for {
						if _, err := os.Stat(gate); err == nil {
							break
						}
						time.Sleep(time.Millisecond)
					}
				}
			}
			if r.URL.Path == "/snapshot/create" {
				var request struct {
					Path string `json:"snapshot_path"`
					Seal bool   `json:"seal"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				// A seal the host could not finish in time comes back to the
				// VMM as an error, and the VMM answers the capture with one.
				// It is still running: only the checkpoint failed.
				if request.Seal && os.Getenv("SPROUTFS_STARTUP_CHILD_REFUSE_SEAL") == "1" {
					http.Error(w, "sealing a managed memory region failed", http.StatusBadRequest)
					return
				}
				if err := os.WriteFile(request.Path, []byte("bounded VMM state"), 0o600); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			if r.URL.Path == "/snapshot/load" {
				// A restore attaches its memory inside the load request, which
				// is where a real VMM's session is built too. An attachment the
				// pager refuses leaves this side with nothing but the
				// descriptor that never came, and that is all it can report.
				var request struct {
					Backend struct {
						Path string `json:"backend_path"`
					} `json:"mem_backend"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if err := attachChild(request.Backend.Path); err != nil {
					http.Error(w, "expected exactly one backing descriptor", http.StatusBadRequest)
					return
				}
			}
			_, _ = w.Write([]byte("{}"))
		}))
	}()
	// Console output lives in the parent's memory, so a test that has no
	// Process handle yet finds this child through a file of its own instead.
	if err := os.WriteFile(filepath.Join(filepath.Dir(apiPath), "startup.pid"),
		fmt.Appendf(nil, "%d\n", os.Getpid()), 0o600); err != nil {
		t.Fatal(err)
	}
	// The supervisor's console is this child's standard output, so a test that
	// reads the console of a process that died has something to find in it.
	fmt.Println(consoleBanner)
	if config.Memory.Socket == "" {
		// A restore attaches from its load request instead, so this child has
		// nothing left to do on the way there but stay alive for it.
		select {}
	}
	if err := attachChild(config.Memory.Socket); err != nil {
		return
	}
	select {}
}

// attachChild speaks the descriptor exchange one memory region's session begins with
// and, once it has the arena, serves that session's mapping commands on a
// goroutine of its own. It reports the handshake's failure, which is all a VMM
// whose pager refused the memory region ever has.
func attachChild(socket string) error {
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return err
	}
	r, w, err := os.Pipe()
	if err != nil {
		_ = c.Close()
		return err
	}
	// The pipe stands in for UFFD for as long as the session lasts: closing its
	// write end would show the pager an event stream at end of file, which is
	// how a real VMM's UFFD ends only when the VMM is gone.
	attached := false
	defer func() {
		if !attached {
			_ = r.Close()
			_ = w.Close()
			_ = c.Close()
		}
	}()
	if err := vmwire.SendFD(c, vmwire.Frame{Kind: vmwire.Hello, ID: vmwire.Version}, r); err != nil {
		return err
	}
	if err := vmwire.Write(c, vmwire.Frame{Kind: vmwire.MemoryRegion, Flags: uint64(vmmemory.Ram), Length: checkpoint.PageSize2MiB, Offset: 2 << 20}); err != nil {
		return err
	}
	_, arena, err := vmwire.ReceiveFD(c)
	if err != nil {
		return err
	}
	attached = true
	go func() {
		defer r.Close()
		defer w.Close()
		defer c.Close()
		defer arena.Close()
		for {
			f, err := vmwire.Read(c)
			if err != nil {
				return
			}
			if err := vmwire.Write(c, vmwire.Frame{Kind: vmwire.Ack, ID: f.ID}); err != nil {
				return
			}
			if f.Kind == vmwire.Stop {
				return
			}
		}
	}()
	return nil
}

// admissionBacking stands in front of a RAM volume so a test can stall the
// authority check every memory region attachment ends with, which is the admission a
// startup waits on.
type admissionBacking struct {
	vmmemory.Backing
	blocked atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *admissionBacking) Verify(ctx context.Context) error {
	if b.blocked.Load() {
		b.once.Do(func() { close(b.entered) })
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-b.release:
			return platform.ErrUnavailable
		}
	}
	return b.Backing.Verify(ctx)
}

func startupFixture(t *testing.T) (vmmachine.Config, *admissionBacking, *vmmemory.Host) {
	t.Helper()
	runtime := sim.New(sim.Config{ObjectStore: sim.ObjectStoreConfig{HeadLatency: time.Nanosecond, GetLatency: time.Nanosecond, PutLatency: time.Nanosecond, ListLatency: time.Nanosecond, BytesPerSecond: 1 << 50}})
	client, err := control.NewClient(control.Config{ObjectStore: runtime.ObjectStore()})
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: runtime.ObjectStore()})
	if err != nil {
		t.Fatal(err)
	}
	m, err := volume.NewManager(volume.Config{Control: client, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	vm, err := m.Create(t.Context(), "vm", []volume.VolumeSpec{{Name: vmmachine.RAMVolume, Size: checkpoint.PageSize2MiB, PageSize: checkpoint.PageSize2MiB}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vm.Close(context.Background()) })
	// Nonresident data needs no kernel mapping during this descriptor-only
	// fixture. Explicit zeros are exercised with real UFFD in the pager suite.
	if err := vm.Volume(vmmachine.RAMVolume).Write(t.Context(), 0, bytes.Repeat([]byte{1}, checkpoint.PageSize2MiB)); err != nil {
		t.Fatal(err)
	}
	a, err := vmmemory.NewLinuxArena(4, checkpoint.PageSize2MiB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	disk, err := adapters.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spill, err := disk.Open(t.Context(), "spill", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spill.Close() })
	resources := testresource.New()
	h, err := vmmemory.New(t.Context(), resources, vmmemory.Config{PageSize: checkpoint.PageSize2MiB, ResidentPages: 4, LogicalPages: 256, DirtyPages: 4}, a, spill)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(t.TempDir(), "child")
	if err := os.WriteFile(launcher, []byte(fmt.Sprintf("#!/bin/sh\nexport SPROUTFS_STARTUP_CHILD=1\nexec %q -test.run=^TestStartupAttachmentChild$ -- \"$@\"\n", executable)), 0o700); err != nil {
		t.Fatal(err)
	}
	scratch := mustScratch(t)
	stall := &admissionBacking{Backing: vm.Volume(vmmachine.RAMVolume),
		entered: make(chan struct{}), release: make(chan struct{})}
	return vmmachine.Config{Binary: launcher, SeccompFilter: "unused", KernelPath: "unused", Pagers: bothKinds(h), VM: vm,
		VCPUs: 1, Scratch: scratch, Connection: vmmemory.ConnectionConfig{QueuePages: 256},
		Backings: map[string]vmmemory.Backing{vmmachine.RAMVolume: stall}}, stall, h
}

func TestStartupCancellationReleasesStalledAdmission(t *testing.T) {
	for _, stop := range []string{"cancel", "exit"} {
		t.Run(stop, func(t *testing.T) {
			config, n, h := startupFixture(t)
			defer close(n.release)
			cause := errors.New("cancel stalled startup")
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(cause)
			n.blocked.Store(true)
			done := make(chan error, 1)
			go func() {
				p, err := vmmachine.Start(ctx, config)
				if p != nil {
					_ = p.Close()
				}
				done <- err
			}()
			select {
			case <-n.entered:
			case err := <-done:
				t.Fatalf("startup did not reach admission: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("startup never attempted admission")
			}
			if stop == "cancel" {
				cancel(cause)
			} else {
				entries, err := os.ReadDir(config.Scratch.Directory())
				if err != nil || len(entries) != 1 {
					t.Fatalf("find child: %v %v", entries, err)
				}
				pid := childPID(t, filepath.Join(config.Scratch.Directory(), entries[0].Name(), "startup.pid"))
				child, err := os.FindProcess(pid)
				if err != nil {
					t.Fatal(err)
				}
				if err := child.Kill(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if err == nil || stop == "cancel" && !errors.Is(err, cause) {
					t.Fatalf("startup cancellation = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("startup cancellation waited for stalled backing I/O")
			}
			stats, err := h.Stats(t.Context())
			if err != nil || stats.LogicalPages != 0 || stats.ResidentPages != 0 {
				t.Fatalf("startup leaked pager capacity: %+v %v", stats, err)
			}
			entries, err := os.ReadDir(config.Scratch.Directory())
			if err != nil || len(entries) != 0 {
				t.Fatalf("startup retained staging files: %v %v", entries, err)
			}
		})
	}
}

// TestRefusedAttachmentReportsThePagerFailure is the account a restore that
// never attached leaves. The VMM builds its sessions inside the load request,
// so a memory region the pager refuses fails that request, and all the VMM can say is
// that the descriptor never came. The pager's own reason is on this side, in
// the connect result nothing read, and a startup that returns the VMM's message
// alone reports a wire problem for what is an admission refusal.
func TestRefusedAttachmentReportsThePagerFailure(t *testing.T) {
	config, stall, h := startupFixture(t)
	config.RestoreState = []byte("bounded VMM state")
	// The memory region is refused the moment the pager checks its volume, which is
	// what a full logical-page cap does at the same point in the handshake.
	stall.blocked.Store(true)
	close(stall.release)
	p, err := vmmachine.Start(t.Context(), config)
	if p != nil {
		_ = p.Close()
	}
	if !errors.Is(err, platform.ErrUnavailable) {
		t.Fatalf("refused attachment = %v, want the pager's own refusal", err)
	}
	stats, statsErr := h.Stats(t.Context())
	if statsErr != nil || stats.LogicalPages != 0 {
		t.Fatalf("refused attachment leaked pager capacity: %+v %v", stats, statsErr)
	}
}

// childPID reads the process identifier the stalled child printed as its first
// line. The supervisor copies the child's standard output into the console file
// on its own goroutine, so the line lands some time after the child wrote it and
// the file is legitimately short or empty when the test first looks.
func childPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, err := os.ReadFile(pidFile)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		var pid int
		if _, err := fmt.Sscanf(string(raw), "%d\n", &pid); err == nil {
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("the child never announced its pid: %q", raw)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRunningProcessOutlivesStartupContext(t *testing.T) {
	config, _, h := startupFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p, err := vmmachine.Start(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	cancel()
	wait, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer stop()
	if err := p.Wait(wait); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startup context terminated the running process: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	stats, err := h.Stats(t.Context())
	if err != nil || stats.LogicalPages != 0 {
		t.Fatalf("process Close retained capacity: %+v %v", stats, err)
	}
}

func TestProcessCaptureStagesAndRemovesItsStateFile(t *testing.T) {
	config, _, _ := startupFixture(t)
	process, err := vmmachine.Start(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	// The fixture's VMM answers the snapshot request without asking the pager for
	// a checkpoint, so the seal a real one issues over the control protocol is
	// made here: Prepare reports the checkpoint of every memory region and refuses a
	// memory region that has none.
	for name, memoryRegion := range process.MemoryRegions() {
		if err := memoryRegion.Seal(t.Context()); err != nil {
			t.Fatalf("sealing %s: %v", name, err)
		}
	}
	state, sources, err := process.Prepare(t.Context())
	if err != nil {
		t.Fatalf("capture under pressure: %v", err)
	}
	if string(state) != "bounded VMM state" {
		t.Fatalf("state: %q", state)
	}
	if len(sources) == 0 {
		t.Fatal("the capture sealed no memory region")
	}
	if err := process.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(process.Directory(), "state"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("retained staging files: %v %v", entries, err)
	}
	if err := process.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestScratchRejectsCloseUnderALiveVMM(t *testing.T) {
	config, _, _ := startupFixture(t)
	process, err := vmmachine.Start(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	if err := config.Scratch.Close(); !errors.Is(err, vmmachine.ErrScratchBusy) {
		t.Fatalf("scratch closed under a running VMM: %v", err)
	}
	if err := process.Close(); err != nil {
		t.Fatal(err)
	}
	if err := config.Scratch.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestARefusedSealLeavesTheVMMRunning. A seal the host could not finish in time
// is a checkpoint that did not happen: the host has given every page it took
// back to the guest, and the VMM answers the capture with an error while it
// goes on running. Killing the process there ends a guest for a checkpoint that
// was only slow, and loses every write since the last one that landed.
func TestARefusedSealLeavesTheVMMRunning(t *testing.T) {
	t.Setenv("SPROUTFS_STARTUP_CHILD_REFUSE_SEAL", "1")
	config, _, _ := startupFixture(t)
	p, err := vmmachine.Start(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, _, err := p.Prepare(t.Context()); err == nil {
		t.Fatal("the VMM accepted a seal it was told to refuse")
	}
	wait, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer stop()
	if err := p.Wait(wait); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a refused seal ended the VMM: %v", err)
	}
	// The capture's caller unseals and resumes, which is what makes this a
	// failed checkpoint rather than a lost VM.
	if err := p.Release(t.Context()); err != nil {
		t.Fatalf("releasing a VM whose seal was refused: %v", err)
	}
}
