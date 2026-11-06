package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/rezen/dug/sdk/httpx"
	"github.com/rezen/dug/sdk/services"
	"golang.org/x/sync/errgroup"
)

type S3Options struct {
	Bucket, Prefix, Region, Endpoint string
	PathStyle                        bool
	UploadConcurrency                int
	PartSize                         int64
	HTTPClient                       *http.Client
	Credentials                      aws.CredentialsProvider
}

type S3 struct {
	client         *s3.Client
	bucket, prefix string
	concurrency    int
	partSize       int64
}

// Validate reports invalid settings. NewS3 calls it; hosts may call it first
// so bad flags fail before any transport or client is built.
func (o S3Options) Validate() error {
	if o.Bucket == "" {
		return fmt.Errorf("S3 bucket is required")
	}
	if o.UploadConcurrency < 1 || o.PartSize < 5*1024*1024 || o.PartSize > 5<<30 {
		return fmt.Errorf("upload concurrency must be positive and part size between 5 MiB and 5 GiB")
	}
	if o.Endpoint != "" {
		if err := httpx.ValidateURL(o.Endpoint); err != nil {
			return fmt.Errorf("S3 endpoint: %w", err)
		}
	}
	return nil
}

func NewS3(ctx context.Context, opts S3Options) (*S3, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	if opts.Region == "" {
		opts.Region = "us-east-1"
	}
	options := []func(*config.LoadOptions) error{config.WithRegion(opts.Region)}
	if opts.Credentials != nil {
		options = append(options, config.WithCredentialsProvider(opts.Credentials))
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = services.DefaultDugHttpClient()
	}
	options = append(options, config.WithHTTPClient(opts.HTTPClient))
	cfg, err := config.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = opts.PathStyle
		if opts.Endpoint != "" {
			o.BaseEndpoint = aws.String(opts.Endpoint)
		}
		// Optional AWS-specific checksums are not supported by every S3 backend.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	return &S3{client: client, bucket: opts.Bucket, prefix: strings.Trim(opts.Prefix, "/"), concurrency: opts.UploadConcurrency, partSize: opts.PartSize}, nil
}

func (s *S3) key(key string) string {
	if s.prefix == "" {
		return key
	}
	return s.prefix + "/" + key
}

// Stat uses HEAD so callers can compare the stored size with what they expect;
// an object is not trusted just because its key exists.
func (s *S3) Stat(ctx context.Context, key string) (bool, int64, error) {
	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.key(key))})
	if err == nil {
		if head.ContentLength == nil {
			return true, -1, nil
		}
		return true, *head.ContentLength, nil
	}
	if isNotFound(err) {
		return false, -1, nil
	}
	return false, -1, err
}

func isNotFound(err error) bool {
	var api smithy.APIError
	return errors.As(err, &api) && (api.ErrorCode() == "NotFound" || api.ErrorCode() == "NoSuchKey" || api.ErrorCode() == "404")
}

func (s *S3) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.key(key))})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return nil, err
	}
	return result.Body, nil
}

func (s *S3) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, contentType string) (resultErr error) {
	if size < 0 {
		return fmt.Errorf("object size must be known")
	}
	bucket, objectKey := aws.String(s.bucket), aws.String(s.key(key))
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if size <= s.partSize {
		_, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: objectKey, Body: body, ContentLength: aws.Int64(size), ContentType: aws.String(contentType)})
		return err
	}
	// Each part reads a section of the same file. This avoids allocating
	// concurrency * part-size buffers and remains seekable for SDK retries.
	reader, ok := body.(io.ReaderAt)
	if !ok {
		reader = &seekerAt{body: body}
	}
	partSize := max(s.partSize, (size-1)/10000+1)
	if partSize > 5<<30 {
		return fmt.Errorf("object exceeds multipart capacity")
	}
	created, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: bucket, Key: objectKey, ContentType: aws.String(contentType)})
	if err != nil {
		return err
	}
	if created.UploadId == nil || *created.UploadId == "" {
		return fmt.Errorf("S3 returned an empty multipart upload ID")
	}
	completed := false
	defer func() {
		if completed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := s.client.AbortMultipartUpload(cleanup, &s3.AbortMultipartUploadInput{Bucket: bucket, Key: objectKey, UploadId: created.UploadId})
		if err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("abort multipart upload: %w", err))
		}
	}()
	count := int((size-1)/partSize + 1)
	parts := make([]types.CompletedPart, count)
	group, partCtx := errgroup.WithContext(ctx)
	group.SetLimit(s.concurrency)
	for i := 0; i < count; i++ {
		if partCtx.Err() != nil {
			break
		}
		group.Go(func() error {
			offset := int64(i) * partSize
			length := min(partSize, size-offset)
			number := int32(i + 1)
			part, err := s.client.UploadPart(partCtx, &s3.UploadPartInput{Bucket: bucket, Key: objectKey, UploadId: created.UploadId, PartNumber: aws.Int32(number), ContentLength: aws.Int64(length), Body: io.NewSectionReader(reader, offset, length)})
			if err != nil {
				return err
			}
			if part.ETag == nil || *part.ETag == "" {
				return fmt.Errorf("S3 returned an empty ETag for part %d", number)
			}
			parts[i] = types.CompletedPart{ETag: part.ETag, PartNumber: aws.Int32(number)}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err = s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: bucket, Key: objectKey, UploadId: created.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
	if err != nil {
		return err
	}
	completed = true
	return nil
}

// Compatibility fallback for custom seekable readers without ReaderAt. Disk
// files and bytes.Reader implement ReaderAt directly and do not take this lock.
type seekerAt struct {
	mu   sync.Mutex
	body io.ReadSeeker
}

func (s *seekerAt) ReadAt(p []byte, offset int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.body.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	return io.ReadFull(s.body, p)
}
