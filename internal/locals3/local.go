// Package locals3 manages the persistent RustFS development container.
package locals3

import (
	"context"
	"encoding/json"
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
	Endpoint      = "http://127.0.0.1:9000"
	Region        = "us-east-1"
	Bucket        = "dug-e2e"
	containerName = "dug-rustfs"
	// The image is pinned so a local run cannot silently pick up a new release.
	image = "rustfs/rustfs:1.0.0"
)

var localCredentials = aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
	return aws.Credentials{AccessKeyID: "rustfsadmin", SecretAccessKey: "rustfsadmin", Source: "dug local"}, nil
})

// NewStore ensures RustFS and the selected bucket exist before creating a store.
// The container stays running after the command exits, retaining its bind mounts.
func NewStore(ctx context.Context, opts storage.S3Options, stderr io.Writer) (*storage.S3, error) {
	root, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	// Image pulls can take longer than readiness; both remain cancellable.
	startup, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	fmt.Fprintln(stderr, "dug local: ensuring RustFS is running")
	if err := ensureContainer(startup, root, runDocker); err != nil {
		return nil, err
	}
	ready, stop := context.WithTimeout(ctx, 60*time.Second)
	defer stop()
	client := services.NewDugHttpClient(services.WithTimeout(3 * time.Second))
	defer client.CloseIdleConnections()
	if err := waitReady(ready, client, Endpoint, time.Second); err != nil {
		return nil, fmt.Errorf("RustFS did not become ready (inspect with docker logs %s): %w", containerName, err)
	}
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

type dockerRunner func(context.Context, ...string) ([]byte, error)

func runDocker(ctx context.Context, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		var failure *exec.ExitError
		if errors.As(err, &failure) {
			return output, fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(string(failure.Stderr)))
		}
		return output, fmt.Errorf("docker %s: %w", args[0], err)
	}
	return output, nil
}

type containerInfo struct {
	State      struct{ Running bool }
	Config     struct{ Image string }
	Mounts     []struct{ Source, Destination string }
	HostConfig struct {
		PortBindings map[string][]struct{ HostIp, HostPort string }
	}
}

func inspectContainer(ctx context.Context, docker dockerRunner) (containerInfo, error) {
	var result []containerInfo
	data, err := docker(ctx, "container", "inspect", containerName)
	if err != nil {
		return containerInfo{}, err
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return containerInfo{}, err
	}
	if len(result) != 1 {
		return containerInfo{}, fmt.Errorf("expected one RustFS container")
	}
	return result[0], nil
}

func ensureContainer(ctx context.Context, root string, docker dockerRunner) error {
	if _, err := docker(ctx, "info"); err != nil {
		return fmt.Errorf("local mode needs Docker Engine or Docker Desktop running: %w", err)
	}
	info, err := inspectContainer(ctx, docker)
	if err != nil {
		for _, dir := range []string{"data", "logs"} {
			if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
				return err
			}
		}
		_, runErr := docker(ctx, "run", "-d", "--name", containerName,
			"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
			// RustFS ships fixed default credentials, so publish on loopback only.
			"-p", "127.0.0.1:9000:9000", "-p", "127.0.0.1:9001:9001",
			"-v", filepath.Join(root, "data")+":/data", "-v", filepath.Join(root, "logs")+":/logs", image)
		// Another local command may have created the same container concurrently.
		info, err = inspectContainer(ctx, docker)
		if err != nil {
			return fmt.Errorf("create RustFS container: %w", errors.Join(runErr, err))
		}
	}
	if info.Config.Image != image {
		return fmt.Errorf("container %s uses %q, expected %q; remove it with `docker rm -f %s` (data/ and logs/ are kept) so dug can recreate it", containerName, info.Config.Image, image, containerName)
	}
	for _, dir := range []string{"data", "logs"} {
		found := false
		for _, mount := range info.Mounts {
			if mount.Destination == "/"+dir && mount.Source == filepath.Join(root, dir) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("container %s does not mount %s/%s; run from its original directory or rename the existing container", containerName, root, dir)
		}
	}
	for _, port := range []string{"9000", "9001"} {
		found := false
		for _, binding := range info.HostConfig.PortBindings[port+"/tcp"] {
			if binding.HostPort != port {
				continue
			}
			if binding.HostIp != "127.0.0.1" {
				return fmt.Errorf("container %s publishes port %s on %q instead of 127.0.0.1, exposing its default credentials to the network; remove it with `docker rm -f %s` (data/ and logs/ are kept) so dug can recreate it bound to loopback", containerName, port, binding.HostIp, containerName)
			}
			found = true
		}
		if !found {
			return fmt.Errorf("container %s must publish port %s to host port %s", containerName, port, port)
		}
	}
	if !info.State.Running {
		if _, err := docker(ctx, "start", containerName); err != nil {
			return err
		}
	}
	return nil
}

func waitReady(ctx context.Context, client *http.Client, endpoint string, interval time.Duration) error {
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/health", nil)
		if err != nil {
			return err
		}
		response, err := client.Do(req)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
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
