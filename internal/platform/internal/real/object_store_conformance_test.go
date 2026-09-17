package real_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/semistrict/sproutfs/internal/platform"
)

// runObjectStoreConformance exercises the conditional-write contract every
// platform.ObjectStore adapter owes its callers. newStore returns an empty
// store; it is called once per subtest so one case cannot see another's keys.
func runObjectStoreConformance(t *testing.T, newStore func(*testing.T) platform.ObjectStore) {
	t.Helper()

	t.Run("create_if_absent_rejects_an_existing_object", func(t *testing.T) {
		store := newStore(t)
		key := objectKey(t, "control")
		created, err := store.Put(t.Context(), putRequest(key, "one", platform.PutConditions{IfNoneMatch: true}))
		if err != nil {
			t.Fatal(err)
		}
		if created.Metadata.ETag == "" {
			t.Fatal("create returned an empty ETag")
		}
		_, err = store.Put(t.Context(), putRequest(key, "two", platform.PutConditions{IfNoneMatch: true}))
		if !errors.Is(err, platform.ErrPrecondition) {
			t.Fatalf("second create error = %v, want ErrPrecondition", err)
		}
		if got := getBody(t, store, key); got != "one" {
			t.Fatalf("object = %q, want \"one\"", got)
		}
	})

	t.Run("head_and_get_report_the_validator_put_returned", func(t *testing.T) {
		store := newStore(t)
		key := objectKey(t, "control")
		request := putRequest(key, "payload", platform.PutConditions{})
		request.Attributes = map[string]string{"committed-lsn": "7"}
		written, err := store.Put(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		if written.Metadata.Key != key || written.Metadata.Size != 7 {
			t.Fatalf("put metadata = %#v, want key %q and size 7", written.Metadata, key.String())
		}
		if got := written.Metadata.Attributes["committed-lsn"]; got != "7" {
			t.Fatalf("put attributes = %q, want 7", got)
		}
		head, err := store.Head(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		if head.ETag != written.Metadata.ETag {
			t.Fatalf("head ETag = %q, want %q", head.ETag, written.Metadata.ETag)
		}
		if head.Key != key || head.Size != 7 {
			t.Fatalf("head metadata = %#v, want key %q and size 7", head, key.String())
		}
		if got := head.Attributes["committed-lsn"]; got != "7" {
			t.Fatalf("head attributes = %q, want 7", got)
		}
		result, err := store.Get(t.Context(), platform.GetRequest{Key: key})
		if err != nil {
			t.Fatal(err)
		}
		defer result.Body.Close()
		if result.Metadata.ETag != written.Metadata.ETag {
			t.Fatalf("get ETag = %q, want %q", result.Metadata.ETag, written.Metadata.ETag)
		}
		if result.ContentLength != 7 || result.Metadata.Size != 7 {
			t.Fatalf("get size/content length = %d/%d, want 7/7", result.Metadata.Size, result.ContentLength)
		}
		body, err := io.ReadAll(result.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "payload" {
			t.Fatalf("body = %q, want \"payload\"", string(body))
		}
	})

	t.Run("if_match_replaces_only_the_current_validator", func(t *testing.T) {
		store := newStore(t)
		key := objectKey(t, "control")
		first, err := store.Put(t.Context(), putRequest(key, "one", platform.PutConditions{IfNoneMatch: true}))
		if err != nil {
			t.Fatal(err)
		}
		second, err := store.Put(t.Context(), putRequest(key, "two", platform.PutConditions{IfMatch: &first.Metadata.ETag}))
		if err != nil {
			t.Fatal(err)
		}
		if second.Metadata.ETag == first.Metadata.ETag {
			t.Fatalf("replacement kept ETag %q", first.Metadata.ETag)
		}
		_, err = store.Put(t.Context(), putRequest(key, "three", platform.PutConditions{IfMatch: &first.Metadata.ETag}))
		if !errors.Is(err, platform.ErrPrecondition) {
			t.Fatalf("stale replacement error = %v, want ErrPrecondition", err)
		}
		if got := getBody(t, store, key); got != "two" {
			t.Fatalf("object = %q, want \"two\"", got)
		}
		head, err := store.Head(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		if head.ETag != second.Metadata.ETag {
			t.Fatalf("head ETag = %q, want %q", head.ETag, second.Metadata.ETag)
		}
	})

	t.Run("if_match_fails_on_an_absent_object", func(t *testing.T) {
		store := newStore(t)
		key := objectKey(t, "control")
		created, err := store.Put(t.Context(), putRequest(key, "one", platform.PutConditions{IfNoneMatch: true}))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(t.Context(), platform.DeleteRequest{Key: key}); err != nil {
			t.Fatal(err)
		}
		_, err = store.Put(t.Context(), putRequest(key, "two", platform.PutConditions{IfMatch: &created.Metadata.ETag}))
		if !errors.Is(err, platform.ErrPrecondition) {
			t.Fatalf("replacement of a deleted object = %v, want ErrPrecondition", err)
		}
	})

	// A range that starts past the end of an object is what a reader asking for
	// a member of a truncated part does. It is a corrupt object rather than a
	// service that is unwell, so the seam has to name it: a caller that cannot
	// tell the two apart retries a read that can never succeed.
	t.Run("a_ranged_get_past_the_end_is_an_invalid_range", func(t *testing.T) {
		store := newStore(t)
		key := objectKey(t, "segment")
		if _, err := store.Put(t.Context(), putRequest(key, "0123456789", platform.PutConditions{})); err != nil {
			t.Fatal(err)
		}
		result, err := store.Get(t.Context(), platform.GetRequest{
			Key:   key,
			Range: &platform.ByteRange{Offset: 16, Length: 4},
		})
		if err == nil {
			result.Body.Close()
			t.Fatal("a range past the end of the object was accepted")
		}
		if !errors.Is(err, platform.ErrInvalidRange) {
			t.Fatalf("get past the end = %v, want ErrInvalidRange", err)
		}
	})

	t.Run("a_ranged_get_reports_the_whole_object_size", func(t *testing.T) {
		store := newStore(t)
		key := objectKey(t, "segment")
		if _, err := store.Put(t.Context(), putRequest(key, "0123456789", platform.PutConditions{})); err != nil {
			t.Fatal(err)
		}
		result, err := store.Get(t.Context(), platform.GetRequest{
			Key:   key,
			Range: &platform.ByteRange{Offset: 2, Length: 3},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer result.Body.Close()
		if result.Metadata.Size != 10 || result.ContentLength != 3 {
			t.Fatalf("size/content length = %d/%d, want 10/3", result.Metadata.Size, result.ContentLength)
		}
		body, err := io.ReadAll(result.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "234" {
			t.Fatalf("body = %q, want \"234\"", string(body))
		}
	})

	// A reader of a part's tail knows what the tail can hold but not how
	// long the part is, so it asks for the last so many bytes. A suffix longer
	// than the object is the whole object, as it is over HTTP.
	t.Run("a_suffix_get_reads_the_tail", func(t *testing.T) {
		store := newStore(t)
		key := objectKey(t, "part")
		if _, err := store.Put(t.Context(), putRequest(key, "0123456789", platform.PutConditions{})); err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			name   string
			suffix int64
			want   string
		}{
			{name: "a tail", suffix: 4, want: "6789"},
			{name: "the whole object", suffix: 10, want: "0123456789"},
			{name: "one longer than the object", suffix: 64, want: "0123456789"},
		} {
			result, err := store.Get(t.Context(), platform.GetRequest{
				Key: key, Range: &platform.ByteRange{Suffix: test.suffix}})
			if err != nil {
				t.Fatalf("%s suffix of %d bytes: %v", test.name, test.suffix, err)
			}
			body, err := io.ReadAll(result.Body)
			result.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != test.want {
				t.Fatalf("%s suffix of %d bytes read %q, want %q", test.name, test.suffix, body, test.want)
			}
			if result.ContentLength != int64(len(test.want)) || result.Metadata.Size != 10 {
				t.Fatalf("%s suffix reported length/size %d/%d, want %d/10",
					test.name, result.ContentLength, result.Metadata.Size, len(test.want))
			}
		}
	})

	t.Run("list_pages_the_prefix_in_order", func(t *testing.T) {
		store := newStore(t)
		want := []string{
			"logs/segment-0",
			"logs/segment-1",
			"logs/segment-2",
			"logs/segment-3",
			"logs/segment-4",
		}
		for _, name := range want {
			if _, err := store.Put(t.Context(), putRequest(objectKey(t, name), name, platform.PutConditions{})); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := store.Put(t.Context(), putRequest(objectKey(t, "images/page-0"), "other", platform.PutConditions{})); err != nil {
			t.Fatal(err)
		}
		prefix, err := platform.NewObjectPrefix("logs/")
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		var pages int
		token := ""
		for {
			result, listErr := store.List(t.Context(), platform.ListRequest{
				Prefix:            prefix,
				ContinuationToken: token,
				Limit:             2,
			})
			if listErr != nil {
				t.Fatal(listErr)
			}
			pages++
			if pages > len(want) {
				t.Fatalf("listing did not terminate after %d pages, keys so far %v", pages, got)
			}
			for _, object := range result.Objects {
				got = append(got, object.Key.String())
				if object.Size != int64(len(object.Key.String())) {
					t.Fatalf("size of %q = %d, want %d", object.Key.String(), object.Size, len(object.Key.String()))
				}
				if object.ETag == "" {
					t.Fatalf("listing gave %q an empty ETag", object.Key.String())
				}
			}
			token = result.NextContinuationToken
			if token == "" {
				break
			}
		}
		if pages != 3 {
			t.Fatalf("pages = %d, want 3", pages)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("keys = %v, want %v", got, want)
		}
	})

	t.Run("delete_removes_the_object_and_forgives_a_missing_one", func(t *testing.T) {
		store := newStore(t)
		key := objectKey(t, "control")
		if err := store.Delete(t.Context(), platform.DeleteRequest{Key: key}); err != nil {
			t.Fatalf("delete of a missing object = %v, want nil", err)
		}
		if _, err := store.Put(t.Context(), putRequest(key, "one", platform.PutConditions{})); err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(t.Context(), platform.DeleteRequest{Key: key}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Head(t.Context(), key); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("head after delete = %v, want ErrNotFound", err)
		}
		if err := store.Delete(t.Context(), platform.DeleteRequest{Key: key}); err != nil {
			t.Fatalf("second delete = %v, want nil", err)
		}
	})

	t.Run("head_and_get_report_a_missing_object", func(t *testing.T) {
		store := newStore(t)
		key := objectKey(t, "absent")
		if _, err := store.Head(t.Context(), key); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("head error = %v, want ErrNotFound", err)
		}
		if _, err := store.Get(t.Context(), platform.GetRequest{Key: key}); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("get error = %v, want ErrNotFound", err)
		}
	})
}

func putRequest(key platform.ObjectKey, body string, conditions platform.PutConditions) platform.PutRequest {
	return platform.PutRequest{
		Key:         key,
		Body:        strings.NewReader(body),
		Size:        int64(len(body)),
		ContentType: "application/octet-stream",
		Conditions:  conditions,
	}
}

func getBody(t *testing.T, store platform.ObjectStore, key platform.ObjectKey) string {
	t.Helper()
	result, err := store.Get(t.Context(), platform.GetRequest{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Body.Close()
	body, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func objectKey(t *testing.T, value string) platform.ObjectKey {
	t.Helper()
	key, err := platform.NewObjectKey(value)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
