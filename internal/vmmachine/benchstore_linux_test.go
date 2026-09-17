//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"bytes"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/platform"
)

func TestBenchmarkStoreReadSurvivesReplacement(t *testing.T) {
	for _, remove := range []bool{false, true} {
		name := "overwrite"
		if remove {
			name = "delete"
		}
		t.Run(name, func(t *testing.T) {
			store, err := newDiskObjectStore(t.TempDir(), objectLatency{Get: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			key, err := platform.NewObjectKey("changing")
			if err != nil {
				t.Fatal(err)
			}
			put := func(body string) {
				t.Helper()
				_, err := store.Put(t.Context(), platform.PutRequest{Key: key, Size: int64(len(body)), Body: bytes.NewReader([]byte(body))})
				if err != nil {
					t.Fatal(err)
				}
			}
			put("before")
			synctest.Test(t, func(t *testing.T) {
				go func() {
					result, err := store.Get(t.Context(), platform.GetRequest{Key: key})
					if err != nil {
						t.Error(err)
						return
					}
					defer result.Body.Close()
					body, err := io.ReadAll(result.Body)
					if err != nil || string(body) != "before" {
						t.Errorf("in-flight read = %q, %v; want before", body, err)
					}
				}()
				// Get has selected its version and is waiting on simulated latency.
				synctest.Wait()
				if remove {
					if err := store.Delete(t.Context(), platform.DeleteRequest{Key: key}); err != nil {
						t.Fatal(err)
					}
				} else {
					put("after")
				}
				time.Sleep(time.Second)
				synctest.Wait()
			})
		})
	}
}
