package real

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/semistrict/sproutfs/platform"
)

// s3DefaultPageSize is the page size used when a listing requests no limit.
// It is the service's own maximum and default for MaxKeys.
const s3DefaultPageSize = 1000

// S3ObjectStore adapts Amazon S3 to the platform seam. The object's ETag is the
// validator, passed back as S3 returned it. For an object written in one PUT,
// which is every object this store writes, it is the MD5 of the body, so two
// writes of the same bytes share one. A compare-and-set on it is therefore a
// compare-and-set on the object's bytes. That is what every caller means by
// one: a control record is the whole of its state, and an immutable object
// carries the digest of its bytes.
type S3ObjectStore struct {
	client *s3.Client
	bucket string
	prefix string
}

func NewS3ObjectStore(client *s3.Client, bucket, prefix string) (*S3ObjectStore, error) {
	if client == nil || bucket == "" {
		return nil, fmt.Errorf("s3 object store: client and bucket are required")
	}
	prefix = strings.TrimLeft(prefix, "/")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &S3ObjectStore{client: client, bucket: bucket, prefix: prefix}, nil
}

// NewS3Client opens an S3 client with the ambient AWS configuration: the
// environment, the shared config files, or the instance's role. A non-empty
// endpoint points the client at an S3-compatible server instead, addressed by
// path rather than by a bucket's own host name, which is what an emulator
// answers to.
func NewS3Client(ctx context.Context, endpoint string) (*s3.Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading the AWS configuration: %w", err)
	}
	return s3.NewFromConfig(cfg, func(options *s3.Options) {
		// A ranged read carries no checksum to validate, which is every read
		// of a part's member; the client would log that on each one.
		options.DisableLogOutputChecksumValidationSkipped = true
		if endpoint != "" {
			options.BaseEndpoint = aws.String(endpoint)
			options.UsePathStyle = true
		}
	}), nil
}

func (s *S3ObjectStore) Head(ctx context.Context, key platform.ObjectKey) (platform.ObjectMetadata, error) {
	if key.IsZero() {
		return platform.ObjectMetadata{}, platform.ErrInvalidObjectKey
	}
	output, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: s.name(key)})
	if err != nil {
		return platform.ObjectMetadata{}, normalizeS3Error(err)
	}
	return platform.ObjectMetadata{
		Key:          key,
		Size:         aws.ToInt64(output.ContentLength),
		LastModified: aws.ToTime(output.LastModified),
		ETag:         platform.ETag(aws.ToString(output.ETag)),
		Attributes:   platform.CloneAttributes(output.Metadata),
	}, nil
}

func (s *S3ObjectStore) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	if request.Key.IsZero() {
		return platform.GetResult{}, platform.ErrInvalidObjectKey
	}
	input := &s3.GetObjectInput{Bucket: &s.bucket, Key: s.name(request.Key)}
	if request.Range != nil {
		if err := request.Range.Validate(); err != nil {
			return platform.GetResult{}, err
		}
		if request.Range.Suffix > 0 {
			input.Range = aws.String(fmt.Sprintf("bytes=-%d", request.Range.Suffix))
		} else {
			input.Range = aws.String(fmt.Sprintf("bytes=%d-%d", request.Range.Offset,
				request.Range.Offset+request.Range.Length-1))
		}
	}
	output, err := s.client.GetObject(ctx, input)
	if err != nil {
		return platform.GetResult{}, normalizeS3Error(err)
	}
	length := aws.ToInt64(output.ContentLength)
	// A ranged reply says how long the whole object is only in its
	// Content-Range; a whole one, including a suffix at least as long as the
	// object, is the object.
	size := length
	if output.ContentRange != nil {
		size, err = s3ObjectSize(*output.ContentRange)
		if err != nil {
			output.Body.Close()
			return platform.GetResult{}, err
		}
	}
	return platform.GetResult{
		Metadata: platform.ObjectMetadata{
			Key:          request.Key,
			Size:         size,
			LastModified: aws.ToTime(output.LastModified),
			ETag:         platform.ETag(aws.ToString(output.ETag)),
			Attributes:   platform.CloneAttributes(output.Metadata),
		},
		ContentLength: length,
		Body:          output.Body,
	}, nil
}

// s3ObjectSize reads the complete length out of a Content-Range of the form
// "bytes first-last/complete".
func s3ObjectSize(contentRange string) (int64, error) {
	_, complete, found := strings.Cut(contentRange, "/")
	size, err := strconv.ParseInt(complete, 10, 64)
	if !found || err != nil || size < 0 {
		return 0, fmt.Errorf("S3 replied with Content-Range %q, which names no object size", contentRange)
	}
	return size, nil
}

// Put writes one object in one PUT. The ETag S3 returns is the only
// metadata it returns: the result carries no modification time.
func (s *S3ObjectStore) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	if err := request.Validate(); err != nil {
		return platform.PutResult{}, err
	}
	input := &s3.PutObjectInput{
		Bucket: &s.bucket,
		Key:    s.name(request.Key),
		// A section reader seeks, so the client can sign the body and send it
		// again on a retry without buffering it.
		Body:          io.NewSectionReader(request.Body, 0, request.Size),
		ContentLength: aws.Int64(request.Size),
		Metadata:      platform.CloneAttributes(request.Attributes),
	}
	if request.ContentType != "" {
		input.ContentType = aws.String(request.ContentType)
	}
	switch {
	case request.Conditions.IfNoneMatch:
		input.IfNoneMatch = aws.String("*")
	case request.Conditions.IfMatch != nil:
		input.IfMatch = aws.String(string(*request.Conditions.IfMatch))
	}
	output, err := s.client.PutObject(ctx, input)
	if err != nil {
		err = normalizeS3Error(err)
		// S3 answers a replacement of an object that is not there with 404,
		// not 412. The validator names no live object, so its condition
		// cannot hold, as it cannot in every other store.
		if input.IfMatch != nil && errors.Is(err, platform.ErrNotFound) {
			err = errors.Join(platform.ErrPrecondition, err)
		}
		return platform.PutResult{}, err
	}
	return platform.PutResult{Metadata: platform.ObjectMetadata{
		Key:        request.Key,
		Size:       request.Size,
		ETag:       platform.ETag(aws.ToString(output.ETag)),
		Attributes: platform.CloneAttributes(request.Attributes),
	}}, nil
}

// Delete removes an object. S3 reports an unconditional delete of an absent
// object as a success, as this seam does.
func (s *S3ObjectStore) Delete(ctx context.Context, request platform.DeleteRequest) error {
	if request.Key.IsZero() {
		return platform.ErrInvalidObjectKey
	}
	input := &s3.DeleteObjectInput{Bucket: &s.bucket, Key: s.name(request.Key)}
	if request.IfMatch != nil {
		input.IfMatch = aws.String(string(*request.IfMatch))
	}
	_, err := s.client.DeleteObject(ctx, input)
	return normalizeS3Error(err)
}

func (s *S3ObjectStore) List(ctx context.Context, request platform.ListRequest) (platform.ListResult, error) {
	if err := request.Validate(); err != nil {
		return platform.ListResult{}, err
	}
	pageSize := int32(s3DefaultPageSize)
	if request.Limit > 0 {
		pageSize = min(request.Limit, s3DefaultPageSize)
	}
	input := &s3.ListObjectsV2Input{
		Bucket:  &s.bucket,
		Prefix:  aws.String(s.prefix + request.Prefix.String()),
		MaxKeys: aws.Int32(pageSize),
	}
	if request.ContinuationToken != "" {
		input.ContinuationToken = aws.String(request.ContinuationToken)
	}
	output, err := s.client.ListObjectsV2(ctx, input)
	if err != nil {
		return platform.ListResult{}, normalizeS3Error(err)
	}
	objects := make([]platform.ObjectMetadata, 0, len(output.Contents))
	for _, object := range output.Contents {
		name := aws.ToString(object.Key)
		if !strings.HasPrefix(name, s.prefix) {
			return platform.ListResult{}, fmt.Errorf("S3 listing returned key outside configured prefix: %q", name)
		}
		relativeKey := strings.TrimPrefix(name, s.prefix)
		if relativeKey == "" {
			continue
		}
		key, keyErr := platform.NewObjectKey(relativeKey)
		if keyErr != nil {
			return platform.ListResult{}, fmt.Errorf("invalid S3 object key %q: %w", name, keyErr)
		}
		// A listing carries no user metadata; Head reads it.
		objects = append(objects, platform.ObjectMetadata{
			Key:          key,
			Size:         aws.ToInt64(object.Size),
			LastModified: aws.ToTime(object.LastModified),
			ETag:         platform.ETag(aws.ToString(object.ETag)),
		})
	}
	next := ""
	if aws.ToBool(output.IsTruncated) {
		next = aws.ToString(output.NextContinuationToken)
	}
	return platform.ListResult{Objects: objects, NextContinuationToken: next}, nil
}

func (s *S3ObjectStore) name(key platform.ObjectKey) *string {
	return aws.String(s.prefix + key.String())
}

// normalizeS3Error maps an S3 reply to the seam's errors by its HTTP status,
// which is what S3 documents its conditions by.
func normalizeS3Error(err error) error {
	if err == nil {
		return nil
	}
	var response *awshttp.ResponseError
	if !errors.As(err, &response) {
		return err
	}
	switch response.HTTPStatusCode() {
	case http.StatusNotFound:
		return errors.Join(platform.ErrNotFound, err)
	case http.StatusPreconditionFailed:
		return errors.Join(platform.ErrPrecondition, err)
	case http.StatusRequestedRangeNotSatisfiable:
		return errors.Join(platform.ErrInvalidRange, err)
	// A conditional write that raced another conditional write to the same
	// key. S3 asks for the request to be retried, and a retry meets whichever
	// write won.
	case http.StatusConflict, http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return errors.Join(platform.ErrUnavailable, err)
	default:
		return err
	}
}

var _ platform.ObjectStore = (*S3ObjectStore)(nil)
