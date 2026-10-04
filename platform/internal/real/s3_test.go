package real_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/internal/real"
)

const s3TestBucket = "sproutfs-test"

// asS3 gives gofakes3 two behaviours of S3 it lacks. A DELETE carrying
// If-Match removes the object only while its ETag is that one, and fails with
// 412 otherwise; every request is served one at a time, so the check and the
// delete are one step. And a ranged GET carries no checksum of the whole
// object, which a client would check against the range and fail.
type asS3 struct {
	mu   sync.Mutex
	next http.Handler
}

// rangedReply drops the whole object's checksums from a ranged GET's reply.
type rangedReply struct {
	http.ResponseWriter
	written bool
}

func (r *rangedReply) WriteHeader(status int) {
	if !r.written {
		r.written = true
		for name := range r.Header() {
			if strings.HasPrefix(strings.ToLower(name), "x-amz-checksum") {
				r.Header().Del(name)
			}
		}
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *rangedReply) Write(data []byte) (int, error) {
	if !r.written {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(data)
}

func (c *asS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.Method == http.MethodGet && r.Header.Get("Range") != "" {
		w = &rangedReply{ResponseWriter: w}
		// A suffix longer than the object is the whole object over HTTP, and
		// gofakes3 refuses it.
		if suffix, found := strings.CutPrefix(r.Header.Get("Range"), "bytes=-"); found {
			head := httptest.NewRecorder()
			c.next.ServeHTTP(head, httptest.NewRequestWithContext(r.Context(), http.MethodHead, r.URL.String(), nil))
			length, lengthErr := strconv.ParseInt(head.Header().Get("Content-Length"), 10, 64)
			want, wantErr := strconv.ParseInt(suffix, 10, 64)
			if lengthErr == nil && wantErr == nil && length > 0 && want > length {
				r.Header.Set("Range", "bytes=0-"+strconv.FormatInt(length-1, 10))
			}
		}
	}
	if ifMatch := r.Header.Get("If-Match"); r.Method == http.MethodDelete && ifMatch != "" {
		head := httptest.NewRecorder()
		c.next.ServeHTTP(head, httptest.NewRequestWithContext(r.Context(), http.MethodHead, r.URL.String(), nil))
		if head.Code == http.StatusNotFound {
			http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", http.StatusNotFound)
			return
		}
		if head.Header().Get("ETag") != ifMatch {
			http.Error(w, "<Error><Code>PreconditionFailed</Code></Error>", http.StatusPreconditionFailed)
			return
		}
		r.Header.Del("If-Match")
	}
	c.next.ServeHTTP(w, r)
}

// newFakeS3 serves an empty bucket from an emulator and returns a store over
// it, opened the way a deployment opens one: through the ambient AWS
// configuration, here the environment alone, with the emulator's endpoint.
func newFakeS3(t *testing.T) platform.ObjectStore { return newFakeS3Behind(t, nil) }

// newFakeS3Behind is newFakeS3 with front, where it is not nil, in front of
// the emulator.
func newFakeS3Behind(t *testing.T, front func(http.Handler) http.Handler) platform.ObjectStore {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket(s3TestBucket); err != nil {
		t.Fatal(err)
	}
	var handler http.Handler = &asS3{next: gofakes3.New(backend).Server()}
	if front != nil {
		handler = front(handler)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	isolated := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(isolated, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(isolated, "credentials"))
	t.Setenv("AWS_ACCESS_KEY_ID", "sproutfs")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sproutfs")
	t.Setenv("AWS_REGION", "us-east-1")
	client, err := real.NewS3Client(t.Context(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := real.NewS3ObjectStore(client, s3TestBucket, "cluster")
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestS3ObjectStoreConformance(t *testing.T) {
	runObjectStoreConformance(t, newFakeS3, listsInOrder)
}

// The same conformance against a real bucket, which is what says the adapter
// agrees with S3 rather than with an emulator. It runs when
// SPROUTFS_TEST_S3_BUCKET names a bucket the ambient AWS credentials may
// write: a general purpose bucket, or a directory bucket of S3 Express One
// Zone (bucket--use1-az4--x-s3), which the store refuses to list. Each run
// writes under a prefix of its own and deletes what it wrote.
func TestS3ObjectStoreConformanceOnABucket(t *testing.T) {
	bucket := os.Getenv("SPROUTFS_TEST_S3_BUCKET")
	if bucket == "" {
		t.Skip("SPROUTFS_TEST_S3_BUCKET is unset")
	}
	client, err := real.NewS3Client(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	lists := listsInOrder
	if real.IsS3DirectoryBucket(bucket) {
		lists = refusesListing
	}
	runObjectStoreConformance(t, func(t *testing.T) platform.ObjectStore {
		prefix := "sproutfs-conformance/" + filepath.Base(t.TempDir()) + "/"
		store, err := real.NewS3ObjectStore(client, bucket, prefix)
		if err != nil {
			t.Fatal(err)
		}
		// The cleanup lists through the client, which a directory bucket
		// answers under a prefix that ends in a slash, in no order.
		t.Cleanup(func() {
			// The test's own context is cancelled before its cleanups run.
			ctx := context.WithoutCancel(t.Context())
			pages := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: &bucket, Prefix: &prefix})
			for pages.HasMorePages() {
				page, err := pages.NextPage(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				for _, object := range page.Contents {
					if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: object.Key}); err != nil {
						t.Error(err)
					}
				}
			}
		})
		return store
	}, lists)
}

// sessionRecorder answers a client as S3 Express One Zone does: CreateSession
// with a session's credentials, and every other request with an empty 200.
// It keeps each request it was sent.
type sessionRecorder struct {
	mu       sync.Mutex
	requests []*http.Request
}

func (r *sessionRecorder) Do(request *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.requests = append(r.requests, request)
	r.mu.Unlock()
	body := ""
	if _, session := request.URL.Query()["session"]; session {
		body = `<CreateSessionResult><Credentials><SessionToken>zonal-session</SessionToken>` +
			`<SecretAccessKey>zonal-secret</SecretAccessKey><AccessKeyId>zonal-key</AccessKeyId>` +
			`<Expiration>2999-01-01T00:00:00Z</Expiration></Credentials></CreateSessionResult>`
	}
	header := http.Header{}
	header.Set("ETag", `"opaque"`)
	return &http.Response{StatusCode: http.StatusOK, Header: header, Request: request,
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}, nil
}

// A directory bucket needs nothing of the store but its name: the client
// makes a session at the bucket's zone and writes through it, and a
// create-if-absent PUT carries its condition there as it does anywhere. A
// listing is refused before any request.
func TestADirectoryBucketIsWrittenThroughASessionAtItsZone(t *testing.T) {
	recorder := &sessionRecorder{}
	client := s3.New(s3.Options{Region: "us-east-1", HTTPClient: recorder,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "regional-key", SecretAccessKey: "regional-secret"}, nil
		})})
	const bucket = "sproutfs-hot--use1-az4--x-s3"
	store, err := real.NewS3ObjectStore(client, bucket, "run")
	if err != nil {
		t.Fatal(err)
	}
	key, err := platform.NewObjectKey("vm/part-0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(t.Context(), platform.PutRequest{Key: key, Body: strings.NewReader("part"), Size: 4,
		Conditions: platform.PutConditions{IfNoneMatch: true}}); err != nil {
		t.Fatal(err)
	}
	var sent []string
	for _, request := range recorder.requests {
		sent = append(sent, fmt.Sprintf("%s %s%s session=%q if-none-match=%q", request.Method, request.URL.Host,
			request.URL.RequestURI(), request.Header.Get("X-Amz-S3session-Token"), request.Header.Get("If-None-Match")))
	}
	want := []string{
		`GET sproutfs-hot--use1-az4--x-s3.s3express-use1-az4.us-east-1.amazonaws.com/?session= session="" if-none-match=""`,
		`PUT sproutfs-hot--use1-az4--x-s3.s3express-use1-az4.us-east-1.amazonaws.com/run/vm/part-0?x-id=PutObject session="zonal-session" if-none-match="*"`,
	}
	if strings.Join(sent, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sent\n%s\nwant\n%s", strings.Join(sent, "\n"), strings.Join(want, "\n"))
	}
	prefix, err := platform.NewObjectPrefix("vm/")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(t.Context(), platform.ListRequest{Prefix: prefix}); !errors.Is(err, real.ErrUnorderedListing) {
		t.Fatalf("list error = %v, want ErrUnorderedListing", err)
	}
	if len(recorder.requests) != 2 {
		t.Fatalf("the listing sent %d requests, want none", len(recorder.requests)-2)
	}
}
