package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/adapters"
)

// storeTimes is what the store bench measured, in milliseconds, one entry a
// call, by the call: the create of a control record's size if absent, a read
// of it, a compare-and-set of it on its ETag, and its delete.
type storeTimes struct {
	Create []float64 `json:"create"`
	Get    []float64 `json:"get"`
	Update []float64 `json:"update"`
	Delete []float64 `json:"delete"`
}

// recordBytes is about what one control record holds.
const recordBytes = 512

// timeStore makes rounds of the four calls under prefix in store, one call at
// a time, and deletes every object it made.
func timeStore(ctx context.Context, clock platform.Clock, store platform.ObjectStore, prefix string,
	rounds int) (storeTimes, error) {
	var times storeTimes
	body := bytes.Repeat([]byte{'r'}, recordBytes)
	timed := func(into *[]float64, call func() error) error {
		began := clock.Now()
		err := call()
		*into = append(*into, ms(clock.Since(began)))
		return err
	}
	for round := range rounds {
		key, err := platform.NewObjectKey(fmt.Sprintf("%srecord-%d", prefix, round))
		if err != nil {
			return times, err
		}
		var created platform.PutResult
		if err := timed(&times.Create, func() error {
			created, err = store.Put(ctx, platform.PutRequest{Key: key, Body: bytes.NewReader(body),
				Size: recordBytes, Conditions: platform.PutConditions{IfNoneMatch: true}})
			return err
		}); err != nil {
			return times, fmt.Errorf("creating %s: %w", key, err)
		}
		if err := timed(&times.Get, func() error {
			result, err := store.Get(ctx, platform.GetRequest{Key: key})
			if err != nil {
				return err
			}
			_, err = io.Copy(io.Discard, result.Body)
			return errors.Join(err, result.Body.Close())
		}); err != nil {
			return times, fmt.Errorf("reading %s: %w", key, err)
		}
		etag := created.Metadata.ETag
		if err := timed(&times.Update, func() error {
			_, err := store.Put(ctx, platform.PutRequest{Key: key, Body: bytes.NewReader(body),
				Size: recordBytes, Conditions: platform.PutConditions{IfMatch: &etag}})
			return err
		}); err != nil {
			return times, fmt.Errorf("updating %s: %w", key, err)
		}
		if err := timed(&times.Delete, func() error {
			return store.Delete(ctx, platform.DeleteRequest{Key: key})
		}); err != nil {
			return times, fmt.Errorf("deleting %s: %w", key, err)
		}
	}
	return times, nil
}

func runStore(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("store", flag.ContinueOnError)
	bucket := flags.String("bucket", "", "the bucket")
	prefix := flags.String("prefix", "", "the objects' prefix, ending in /")
	rounds := flags.Int("count", 300, "rounds of the four calls")
	out := flags.String("out", "store.json", "where the times go")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *bucket == "" || *prefix == "" || *rounds <= 0 {
		return errors.New("store needs -bucket, -prefix and a positive -count")
	}
	store, closer, err := adapters.NewObjectStore(ctx, adapters.ObjectStoreConfig{Bucket: *bucket})
	if err != nil {
		return err
	}
	began := time.Now()
	times, err := timeStore(ctx, platform.WallClock(), store, *prefix, *rounds)
	if closeErr := closer.Close(); closeErr != nil {
		slog.ErrorContext(ctx, "sproutfs-startbench: closing the object store", "error", closeErr)
	}
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(times, "", " ")
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "sproutfs-startbench: %d rounds of the store's calls in %s\n", *rounds, time.Since(began))
	return os.WriteFile(*out, encoded, 0o644)
}
