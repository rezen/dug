// Package locals3 manages a persistent local rclone S3 server over data/.
package locals3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/rezen/dug/sdk/services"
	"github.com/rezen/dug/services/storage"
)

const (
	Endpoint  = "http://127.0.0.1:9000"
	Region    = "us-east-1"
	Bucket    = "dug-e2e"
	accessKey = "dug"
	secretKey = "dug"
)

var localCredentials = aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
	return aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey, Source: "dug local"}, nil
})

// NewStore ensures the rclone server and the selected bucket exist before creating a store.
// The server stays running after the command exits; objects are plain files under data/<bucket>/.
func NewStore(ctx context.Context, opts storage.S3Options, stderr io.Writer) (*storage.S3, error) {
	root, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client := services.NewDugHttpClient(services.WithTimeout(3 * time.Second))
	defer client.CloseIdleConnections()
	fmt.Fprintln(stderr, "dug local: ensuring rclone S3 server is running")
	if err := ensureServer(startup, client, root, startRclone); err != nil {
		return nil, err
	}
	if err := waitReady(startup, client, Endpoint, 200*time.Millisecond); err != nil {
		return nil, fmt.Errorf("rclone did not become ready (see logs/rclone.log): %w", err)
	}
	ready := startup
	s3client := s3.New(s3.Options{
		Region: Region, BaseEndpoint: aws.String(Endpoint), UsePathStyle: true,
		Credentials: localCredentials, HTTPClient: client,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	if err := ensureBucket(ready, s3client, opts.Bucket); err != nil {
		return nil, fmt.Errorf("prepare local bucket %q: %w", opts.Bucket, err)
	}
	opts.Credentials = localCredentials
	fmt.Fprintf(stderr, "dug local: %s/%s ready\n", Endpoint, opts.Bucket)
	return storage.NewS3(ctx, opts)
}

// serverStarter launches the detached rclone server; tests replace it.
type serverStarter func(root string) error

func startRclone(root string) error {
	binary, err := exec.LookPath("rclone")
	if err != nil {
		return fmt.Errorf("local mode needs rclone on PATH (https://rclone.org/install/, e.g. brew install rclone): %w", err)
	}
	for _, dir := range []string{"data", "logs"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			return err
		}
	}
	logFile, err := os.OpenFile(filepath.Join(root, "logs", "rclone.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	// Loopback only: the credentials are fixed and publicly known.
	cmd := exec.Command(binary, "serve", "s3", filepath.Join(root, "data"),
		"--addr", strings.TrimPrefix(Endpoint, "http://"), "--auth-key", accessKey+","+secretKey)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = detachedAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start rclone: %w", err)
	}
	// The server outlives this command, like the container it replaces.
	return cmd.Process.Release()
}

// listening reports whether anything already answers HTTP at the endpoint.
func listening(ctx context.Context, client *http.Client, endpoint string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	response, err := client.Do(req)
	if err != nil {
		return false
	}
	response.Body.Close()
	return true
}

func ensureServer(ctx context.Context, client *http.Client, root string, start serverStarter) error {
	if listening(ctx, client, Endpoint) {
		return nil
	}
	return start(root)
}

// waitReady polls until the server answers any HTTP request. rclone has no
// health endpoint and rejects unsigned requests, so a 403 still means ready.
func waitReady(ctx context.Context, client *http.Client, endpoint string, interval time.Duration) error {
	for !listening(ctx, client, endpoint) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	return nil
}

type bucketAPI interface {
	HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	CreateBucket(context.Context, *s3.CreateBucketInput, ...func(*s3.Options)) (*s3.CreateBucketOutput, error)
}

func ensureBucket(ctx context.Context, client bucketAPI, name string) error {
	_, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(name)})
	if err == nil {
		return nil
	}
	var api smithy.APIError
	if !errors.As(err, &api) || (api.ErrorCode() != "NotFound" && api.ErrorCode() != "NoSuchBucket" && api.ErrorCode() != "404") {
		return err
	}
	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name)})
	if errors.As(err, &api) && api.ErrorCode() == "BucketAlreadyOwnedByYou" {
		return nil
	}
	return err
}
