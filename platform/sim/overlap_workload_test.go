package sim_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

const dynamicClients, dynamicRounds = 4, 3

// Every reply creates the client's next request. Accept creates a handler, a
// request creates disk I/O and publication, failed publication creates retry
// work, and the final reply creates readback and power-loss verification. Only
// the initial clients/listener are known to the scheduler before it starts.
func runDynamicWorkload(t *testing.T, seed uint64, reverse bool) (*sim.Scheduler, *sim.Runtime) {
	t.Helper()
	var result *sim.Scheduler
	var adapterRuntime *sim.Runtime
	synctest.Test(t, func(t *testing.T) {
		s := sim.NewScheduler(seed)
		runtime := sim.New(sim.Config{
			Seed: seed, Wait: s.Wait,
			Network:     sim.NetworkConfig{Latency: 3 * time.Millisecond, Jitter: 2 * time.Millisecond},
			ObjectStore: sim.ObjectStoreConfig{PutLatency: 6 * time.Millisecond, GetLatency: 4 * time.Millisecond},
		})
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		listener, err := runtime.Network().Listen("service")
		if err != nil {
			t.Fatal(err)
		}
		// One rejected write and one lost reply after application. Readback must
		// still match acknowledged bytes, regardless of which request hits them.
		runtime.ObjectStore().FailNext(sim.ObjectPut, 1)
		runtime.ObjectStore().FailNextAfterApply(sim.ObjectPut, 1)
		var retries, checked atomic.Int32
		var clients, servers [dynamicClients]platform.Conn
		var wg sync.WaitGroup
		errorsOut := make(chan error, 2*dynamicClients+1)
		report := func(err error) {
			if err != nil {
				errorsOut <- err
				cancel()
			}
		}
		model := func(client, round int) []byte {
			return []byte(fmt.Sprintf("client=%d round=%d value=%016x", client, round,
				runtime.Random("dynamic-data").Uint64(fmt.Sprintf("%d/%d", client, round))))
		}
		objectKey := func(client, round int) platform.ObjectKey {
			key, err := platform.NewObjectKey(fmt.Sprintf("dynamic/client-%d/round-%d", client, round))
			if err != nil {
				panic(err)
			}
			return key
		}
		receive := func(conn platform.Conn) ([]byte, []byte, error) {
			frame, err := conn.Receive(ctx)
			if err != nil {
				return nil, nil, err
			}
			defer frame.Payload.Close()
			data, err := io.ReadAll(frame.Payload)
			return frame.Header, data, err
		}
		serve := func(client int, conn platform.Conn) error {
			if err := dynamicPoint(s, fmt.Sprintf("server/%d/start", client)); err != nil {
				return err
			}
			s.Record(fmt.Sprintf("server/%d", client), "", nil)
			disk := runtime.NewDisk(fmt.Sprintf("server-%d", client), sim.DiskConfig{})
			for round := range dynamicRounds {
				header, data, err := receive(conn)
				if err != nil {
					return err
				}
				if string(header) != strconv.Itoa(round) || !bytes.Equal(data, model(client, round)) {
					return fmt.Errorf("request bytes changed for %d/%d", client, round)
				}
				file, err := disk.Open(ctx, strconv.Itoa(round), platform.OpenOptions{Create: true})
				if err != nil {
					return err
				}
				if _, err := file.WriteAt(ctx, data, 0); err != nil {
					_ = file.Close()
					return err
				}
				if err := file.Sync(ctx); err != nil {
					_ = file.Close()
					return err
				}
				if err := file.Close(); err != nil {
					return err
				}
				for attempt := 0; ; attempt++ {
					_, err := runtime.ObjectStore().Put(ctx, platform.PutRequest{Key: objectKey(client, round), Body: bytes.NewReader(data), Size: int64(len(data))})
					if err == nil {
						break
					}
					if !errors.Is(err, platform.ErrInjectedFault) || attempt >= 3 {
						return err
					}
					retries.Add(1)
					if err := s.Wait(ctx, fmt.Sprintf("retry/%d/%d/%d", client, round, attempt), 2*time.Millisecond, 5*time.Millisecond); err != nil {
						return err
					}
				}
				digest := sha256.Sum256(data)
				if err := conn.Send(ctx, platform.Frame{Header: digest[:]}); err != nil {
					return err
				}
			}
			if err := disk.PowerLoss(ctx); err != nil {
				return err
			}
			for round := range dynamicRounds {
				file, err := disk.Open(ctx, strconv.Itoa(round), platform.OpenOptions{})
				if err != nil {
					return err
				}
				got := make([]byte, len(model(client, round)))
				_, err = file.ReadAt(ctx, got, 0)
				_ = file.Close()
				if err != nil {
					return err
				}
				if !bytes.Equal(got, model(client, round)) {
					return fmt.Errorf("synced disk bytes lost at %d/%d", client, round)
				}
				s.Record(fmt.Sprintf("disk/%d/%d", client, round), "durable", got)
			}
			s.Record(fmt.Sprintf("server/%d", client), "ok", nil)
			return nil
		}
		clientWork := func(client int) error {
			if err := dynamicPoint(s, fmt.Sprintf("client/%d/start", client)); err != nil {
				return err
			}
			s.Record(fmt.Sprintf("client/%d", client), "", nil)
			conn, err := runtime.Network().Dial(ctx, platform.Address(strconv.Itoa(client)), "service")
			if err != nil {
				return err
			}
			clients[client] = conn
			for round := range dynamicRounds {
				data := model(client, round)
				if err := conn.Send(ctx, platform.Frame{Header: []byte(strconv.Itoa(round)), Payload: bytes.NewReader(data), PayloadSize: int64(len(data))}); err != nil {
					return err
				}
				header, _, err := receive(conn)
				if err != nil {
					return err
				}
				digest := sha256.Sum256(data)
				if !bytes.Equal(header, digest[:]) {
					return fmt.Errorf("reply checksum changed for %d/%d", client, round)
				}
				object, err := runtime.ObjectStore().Get(ctx, platform.GetRequest{Key: objectKey(client, round)})
				if err != nil {
					return err
				}
				got, err := io.ReadAll(object.Body)
				_ = object.Body.Close()
				if err != nil {
					return err
				}
				if !bytes.Equal(got, data) {
					return fmt.Errorf("published bytes changed for %d/%d", client, round)
				}
				s.Record(fmt.Sprintf("object/%d/%d", client, round), "acknowledged", got)
				checked.Add(1)
			}
			s.Record(fmt.Sprintf("client/%d", client), "ok", nil)
			return nil
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range dynamicClients {
				conn, err := listener.Accept(ctx)
				if err != nil {
					report(err)
					return
				}
				client, err := strconv.Atoi(string(conn.RemoteAddress()))
				if err != nil || client < 0 || client >= dynamicClients {
					report(fmt.Errorf("invalid client: %s", conn.RemoteAddress()))
					return
				}
				servers[client] = conn
				wg.Add(1)
				go func() { defer wg.Done(); report(serve(client, conn)) }()
			}
		}()
		order := []int{0, 1, 2, 3}
		if reverse {
			slices.Reverse(order)
		}
		for _, client := range order {
			wg.Add(1)
			go func() { defer wg.Done(); report(clientWork(client)) }()
		}
		done := make(chan struct{})
		go func() {
			wg.Wait()
			_ = dynamicPoint(s, "cleanup")
			for i := range dynamicClients {
				if clients[i] != nil {
					_ = clients[i].Close()
				}
				if servers[i] != nil {
					_ = servers[i].Close()
				}
			}
			_ = listener.Close()
			close(done)
		}()
		if err := s.Run(done); err != nil {
			t.Fatal(err)
		}
		close(errorsOut)
		for err := range errorsOut {
			t.Error(err)
		}
		if checked.Load() != dynamicClients*dynamicRounds || retries.Load() != 2 {
			t.Errorf("checked=%d retries=%d", checked.Load(), retries.Load())
		}
		if stats := s.Stats(); stats.Releases < 100 || stats.MaxPending < dynamicClients {
			t.Errorf("insufficient dynamic overlap: releases=%d pending=%d", stats.Releases, stats.MaxPending)
		}
		result, adapterRuntime = s, runtime
	})
	return result, adapterRuntime
}
