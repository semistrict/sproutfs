package real

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"cloud.google.com/go/storage"
	"github.com/semistrict/sproutfs/platform"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// gcsDefaultPageSize is the page size used when a listing requests no limit.
// It matches the service's own default for maxResults.
const gcsDefaultPageSize = 1000

// GCSObjectStore adapts Google Cloud Storage to the platform seam. The object
// generation, in decimal, is the ETag: it is the validator GCS conditions
// accept, it changes on every write, and it is never reused for a key.
type GCSObjectStore struct {
	client *storage.Client
	bucket string
	prefix string
}

func NewGCSObjectStore(client *storage.Client, bucket, prefix string) (*GCSObjectStore, error) {
	if client == nil || bucket == "" {
		return nil, fmt.Errorf("gcs object store: client and bucket are required")
	}
	prefix = strings.TrimLeft(prefix, "/")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &GCSObjectStore{client: client, bucket: bucket, prefix: prefix}, nil
}

// NewGCSClient opens a storage client. An empty endpoint uses the ambient
// Google credentials — on GCE, the instance's service account. A non-empty one
// points the client at an emulator and sends no credentials, which is what the
// client library's own STORAGE_EMULATOR_HOST override does; passing the
// endpoint explicitly keeps a deployment from depending on process environment.
// An emulator also reads through the JSON API: object downloads otherwise go to
// the XML API's own host, which an emulator does not answer for.
func NewGCSClient(ctx context.Context, endpoint string) (*storage.Client, error) {
	if endpoint == "" {
		return storage.NewClient(ctx)
	}
	address, err := gcsEmulatorEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	return storage.NewClient(ctx,
		option.WithoutAuthentication(),
		option.WithEndpoint(address),
		storage.WithJSONReads(),
	)
}

// gcsEmulatorEndpoint turns an emulator host into the JSON API base URL the
// client expects, supplying the scheme and the service path when they are
// missing, exactly as the library does for STORAGE_EMULATOR_HOST.
func gcsEmulatorEndpoint(endpoint string) (string, error) {
	parsed := &url.URL{Scheme: "http", Host: endpoint}
	if strings.Contains(endpoint, "://") {
		value, err := url.Parse(endpoint)
		if err != nil {
			return "", fmt.Errorf("gcs endpoint %q: %w", endpoint, err)
		}
		parsed = value
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("gcs endpoint %q has no host", endpoint)
	}
	parsed.Path = "storage/v1/"
	return parsed.String(), nil
}

func (g *GCSObjectStore) Head(ctx context.Context, key platform.ObjectKey) (platform.ObjectMetadata, error) {
	if key.IsZero() {
		return platform.ObjectMetadata{}, platform.ErrInvalidObjectKey
	}
	attributes, err := g.object(key).Attrs(ctx)
	if err != nil {
		return platform.ObjectMetadata{}, normalizeGCSError(err)
	}
	return platform.ObjectMetadata{
		Key:          key,
		Size:         attributes.Size,
		LastModified: attributes.Updated,
		ETag:         gcsETag(attributes.Generation),
		Attributes:   platform.CloneAttributes(attributes.Metadata),
	}, nil
}

func (g *GCSObjectStore) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	if request.Key.IsZero() {
		return platform.GetResult{}, platform.ErrInvalidObjectKey
	}
	// A negative offset with no length is how the client library asks for a
	// suffix: it sends the object's last that many bytes, and the service
	// answers with the whole object when there are fewer than that.
	offset, length := int64(0), int64(-1)
	if request.Range != nil {
		if err := request.Range.Validate(); err != nil {
			return platform.GetResult{}, err
		}
		if request.Range.Suffix > 0 {
			offset = -request.Range.Suffix
		} else {
			offset, length = request.Range.Offset, request.Range.Length
		}
	}
	reader, err := g.object(request.Key).NewRangeReader(ctx, offset, length)
	if err != nil {
		return platform.GetResult{}, normalizeGCSError(err)
	}
	// A download carries no user metadata: the client library exposes only the
	// object's own attributes on a reader. Callers that need attributes read
	// them from Head or from the result of the Put that wrote them.
	return platform.GetResult{
		Metadata: platform.ObjectMetadata{
			Key:          request.Key,
			Size:         reader.Attrs.Size,
			LastModified: reader.Attrs.LastModified,
			ETag:         gcsETag(reader.Attrs.Generation),
		},
		ContentLength: reader.Remain(),
		Body:          reader,
	}, nil
}

func (g *GCSObjectStore) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	if err := request.Validate(); err != nil {
		return platform.PutResult{}, err
	}
	object := g.object(request.Key)
	switch {
	case request.Conditions.IfNoneMatch:
		object = object.If(storage.Conditions{DoesNotExist: true})
	case request.Conditions.IfMatch != nil:
		generation, err := gcsGeneration(*request.Conditions.IfMatch)
		if err != nil {
			return platform.PutResult{}, err
		}
		object = object.If(storage.Conditions{GenerationMatch: generation})
	}

	// An upload is abandoned by cancelling its context, so it gets one of its
	// own: a body that fails half way must not leave the request in flight.
	uploadCtx, abandonUpload := context.WithCancel(ctx)
	defer abandonUpload()
	writer := object.NewWriter(uploadCtx)
	// The writer buffers a chunk at a time, so an object smaller than the
	// library's default costs one request and one buffer of its own size rather
	// than 16 MiB of buffer for a few hundred kilobytes of index.
	writer.ChunkSize = uploadChunkSize(request.Size)
	writer.ContentType = request.ContentType
	writer.Metadata = platform.CloneAttributes(request.Attributes)
	if _, err := io.Copy(writer, io.NewSectionReader(request.Body, 0, request.Size)); err != nil {
		abandonUpload()
		return platform.PutResult{}, fmt.Errorf("write GCS upload body: %w", normalizeGCSError(err))
	}
	if err := writer.Close(); err != nil {
		return platform.PutResult{}, normalizeGCSError(err)
	}
	attributes := writer.Attrs()
	return platform.PutResult{Metadata: platform.ObjectMetadata{
		Key:          request.Key,
		Size:         attributes.Size,
		LastModified: attributes.Updated,
		ETag:         gcsETag(attributes.Generation),
		Attributes:   platform.CloneAttributes(request.Attributes),
	}}, nil
}

// uploadChunkSize is the buffer one GCS upload takes: enough for the whole
// object when it is small, rounded up to the chunk granularity the library
// requires, and the library's own default for anything larger.
func uploadChunkSize(size int64) int {
	if size <= 0 || size >= defaultUploadChunkSize {
		return defaultUploadChunkSize
	}
	chunks := (size + uploadChunkGranularity - 1) / uploadChunkGranularity
	return int(chunks * uploadChunkGranularity)
}

const (
	// uploadChunkGranularity is the multiple GCS resumable uploads require.
	uploadChunkGranularity = googleapi.MinUploadChunkSize
	// defaultUploadChunkSize is what the library takes when nothing says
	// otherwise.
	defaultUploadChunkSize = 16 << 20
)

func (g *GCSObjectStore) Delete(ctx context.Context, request platform.DeleteRequest) error {
	if request.Key.IsZero() {
		return platform.ErrInvalidObjectKey
	}
	object := g.object(request.Key)
	if request.IfMatch != nil {
		generation, err := gcsGeneration(*request.IfMatch)
		if err != nil {
			return err
		}
		object = object.If(storage.Conditions{GenerationMatch: generation})
	}
	err := normalizeGCSError(object.Delete(ctx))
	// An unconditional delete of an absent object is a success elsewhere in
	// this seam; GCS reports it as 404.
	if request.IfMatch == nil && errors.Is(err, platform.ErrNotFound) {
		return nil
	}
	return err
}

func (g *GCSObjectStore) List(ctx context.Context, request platform.ListRequest) (platform.ListResult, error) {
	if err := request.Validate(); err != nil {
		return platform.ListResult{}, err
	}
	pageSize := gcsDefaultPageSize
	if request.Limit > 0 {
		pageSize = int(request.Limit)
	}
	query := &storage.Query{Prefix: g.prefix + request.Prefix.String()}
	pager := iterator.NewPager(g.client.Bucket(g.bucket).Objects(ctx, query), pageSize, request.ContinuationToken)
	var page []*storage.ObjectAttrs
	nextToken, err := pager.NextPage(&page)
	if err != nil {
		return platform.ListResult{}, normalizeGCSError(err)
	}
	objects := make([]platform.ObjectMetadata, 0, len(page))
	for _, attributes := range page {
		if !strings.HasPrefix(attributes.Name, g.prefix) {
			return platform.ListResult{}, fmt.Errorf("GCS listing returned key outside configured prefix: %q", attributes.Name)
		}
		relativeKey := strings.TrimPrefix(attributes.Name, g.prefix)
		if relativeKey == "" {
			continue
		}
		key, keyErr := platform.NewObjectKey(relativeKey)
		if keyErr != nil {
			return platform.ListResult{}, fmt.Errorf("invalid GCS object key %q: %w", attributes.Name, keyErr)
		}
		objects = append(objects, platform.ObjectMetadata{
			Key:          key,
			Size:         attributes.Size,
			LastModified: attributes.Updated,
			ETag:         gcsETag(attributes.Generation),
			Attributes:   platform.CloneAttributes(attributes.Metadata),
		})
	}
	return platform.ListResult{Objects: objects, NextContinuationToken: nextToken}, nil
}

func (g *GCSObjectStore) object(key platform.ObjectKey) *storage.ObjectHandle {
	return g.client.Bucket(g.bucket).Object(g.prefix + key.String())
}

func gcsETag(generation int64) platform.ETag {
	return platform.ETag(strconv.FormatInt(generation, 10))
}

// gcsGeneration reads back an ETag this store issued. A validator that is not
// one cannot name a live generation, so the condition it carries cannot hold:
// that is a failed precondition, not a malformed request.
func gcsGeneration(etag platform.ETag) (int64, error) {
	generation, err := strconv.ParseInt(string(etag), 10, 64)
	if err != nil || generation <= 0 {
		return 0, fmt.Errorf("GCS condition carries a foreign validator %q: %w", string(etag), platform.ErrPrecondition)
	}
	return generation, nil
}

func normalizeGCSError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, storage.ErrObjectNotExist) || errors.Is(err, storage.ErrBucketNotExist) {
		return errors.Join(platform.ErrNotFound, err)
	}
	var apiError *googleapi.Error
	if !errors.As(err, &apiError) {
		return err
	}
	switch apiError.Code {
	case http.StatusNotFound:
		return errors.Join(platform.ErrNotFound, err)
	case http.StatusPreconditionFailed, http.StatusNotModified:
		return errors.Join(platform.ErrPrecondition, err)
	// A range that starts past the end of the object. It says the object is not
	// what the caller believed it to be, which is corruption to reckon with
	// rather than a service that might answer differently in a moment.
	case http.StatusRequestedRangeNotSatisfiable:
		return errors.Join(platform.ErrInvalidRange, err)
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return errors.Join(platform.ErrUnavailable, err)
	default:
		return err
	}
}

var _ platform.ObjectStore = (*GCSObjectStore)(nil)
