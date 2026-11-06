package locals3

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

func inspectJSON(root string, running bool, hostIP string) []byte {
	data, _ := json.Marshal([]any{map[string]any{
		"State": map[string]any{"Running": running}, "Config": map[string]any{"Image": image},
		"Mounts":     []any{map[string]string{"Source": filepath.Join(root, "data"), "Destination": "/data"}, map[string]string{"Source": filepath.Join(root, "logs"), "Destination": "/logs"}},
		"HostConfig": map[string]any{"PortBindings": map[string]any{"9000/tcp": []any{map[string]string{"HostIp": hostIP, "HostPort": "9000"}}, "9001/tcp": []any{map[string]string{"HostIp": hostIP, "HostPort": "9001"}}}},
	}})
	return data
}

func TestContainerLifecycle(t *testing.T) {
	for _, initial := range []string{"missing", "stopped", "running", "concurrent"} {
		t.Run(initial, func(t *testing.T) {
			root := t.TempDir()
			exists, running := initial == "stopped" || initial == "running", initial == "running"
			runs, starts := 0, 0
			docker := func(_ context.Context, args ...string) ([]byte, error) {
				switch args[0] {
				case "info":
					return nil, nil
				case "container":
					if !exists {
						return nil, errors.New("no such container")
					}
					return inspectJSON(root, running, "127.0.0.1"), nil
				case "run":
					runs++
					exists, running = true, true
					// The image ships fixed credentials: publish on loopback only, from a pinned tag.
					if !slices.Contains(args, "127.0.0.1:9000:9000") || !slices.Contains(args, "127.0.0.1:9001:9001") || slices.Contains(args, "9000:9000") || args[len(args)-1] != image || strings.HasSuffix(image, ":latest") {
						t.Errorf("unsafe container arguments: %v", args)
					}
					if initial == "concurrent" {
						return nil, errors.New("name already in use")
					}
					return nil, nil
				case "start":
					starts++
					running = true
					return nil, nil
				}
				return nil, fmt.Errorf("unexpected docker call %v", args)
			}
			if err := ensureContainer(context.Background(), root, docker); err != nil {
				t.Fatal(err)
			}
			if err := ensureContainer(context.Background(), root, docker); err != nil {
				t.Fatal(err)
			}
			wantRuns, wantStarts := 0, 0
			if initial == "missing" || initial == "concurrent" {
				wantRuns = 1
			}
			if initial == "stopped" {
				wantStarts = 1
			}
			if runs != wantRuns || starts != wantStarts {
				t.Fatalf("run=%d start=%d", runs, starts)
			}
		})
	}
}

func TestRefusesContainerExposedBeyondLoopback(t *testing.T) {
	for _, hostIP := range []string{"", "0.0.0.0"} {
		root := t.TempDir()
		err := ensureContainer(context.Background(), root, func(_ context.Context, args ...string) ([]byte, error) {
			switch args[0] {
			case "info":
				return nil, nil
			case "container":
				return inspectJSON(root, true, hostIP), nil
			}
			t.Fatalf("unexpected mutation: %v", args)
			return nil, nil
		})
		if err == nil || !strings.Contains(err.Error(), "docker rm -f") {
			t.Fatalf("accepted a container published on %q: %v", hostIP, err)
		}
	}
}

func TestDockerUnavailable(t *testing.T) {
	calls := 0
	err := ensureContainer(context.Background(), t.TempDir(), func(context.Context, ...string) ([]byte, error) {
		calls++
		return nil, errors.New("daemon unavailable")
	})
	if err == nil || !strings.Contains(err.Error(), "Docker Desktop running") || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}

func TestContainerCreationFailure(t *testing.T) {
	err := ensureContainer(context.Background(), t.TempDir(), func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "info" {
			return nil, nil
		}
		if args[0] == "run" {
			return nil, errors.New("port already allocated")
		}
		return nil, errors.New("no such container")
	})
	if err == nil || !strings.Contains(err.Error(), "port already allocated") {
		t.Fatalf("lost startup failure: %v", err)
	}
}

func TestRefusesUnrelatedContainer(t *testing.T) {
	for _, config := range []string{
		`[{"Config":{"Image":"other/image"}}]`,
		`[{"Config":{"Image":"rustfs/rustfs:latest"}}]`,
		`[{"Config":{"Image":"` + image + `"},"Mounts":[{"Source":"/other/data","Destination":"/data"}]}]`,
	} {
		err := ensureContainer(context.Background(), t.TempDir(), func(_ context.Context, args ...string) ([]byte, error) {
			if args[0] == "info" {
				return nil, nil
			}
			if args[0] != "container" {
				t.Fatalf("unexpected mutation: %v", args)
			}
			return []byte(config), nil
		})
		if err == nil {
			t.Fatal("accepted unrelated container")
		}
	}
}

func TestReadinessRetriesAndCancels(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitReady(ctx, server.Client(), server.URL, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := waitReady(ctx, server.Client(), server.URL, time.Millisecond); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestBucketCreationAndReuseWithSignedRequests(t *testing.T) {
	var mu sync.Mutex
	buckets := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=rustfsadmin/") {
			t.Error("missing local credentials")
		}
		name := strings.Trim(r.URL.Path, "/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodHead:
			if !buckets[name] {
				w.WriteHeader(http.StatusNotFound)
			}
		case http.MethodPut:
			buckets[name] = true
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	client := s3.New(s3.Options{Region: Region, BaseEndpoint: aws.String(server.URL), UsePathStyle: true, Credentials: localCredentials, HTTPClient: server.Client()})
	for i := 0; i < 2; i++ {
		if err := ensureBucket(context.Background(), client, "test-local"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.HeadBucket(context.Background(), &s3.HeadBucketInput{Bucket: aws.String("test-local")}); err != nil {
		t.Fatal(err)
	}
}

type bucketFailure struct {
	head, create error
	creates      int
}

func (b *bucketFailure) HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	return nil, b.head
}
func (b *bucketFailure) CreateBucket(context.Context, *s3.CreateBucketInput, ...func(*s3.Options)) (*s3.CreateBucketOutput, error) {
	b.creates++
	return nil, b.create
}
func TestBucketErrors(t *testing.T) {
	for _, code := range []string{"AccessDenied", "NotFound"} {
		b := &bucketFailure{head: &smithy.GenericAPIError{Code: code}, create: errors.New("creation failed")}
		err := ensureBucket(context.Background(), b, "test-local")
		if err == nil {
			t.Fatal("ignored error")
		}
		if code == "AccessDenied" && b.creates != 0 {
			t.Fatal("attempted creation after forbidden response")
		}
		if code == "NotFound" && b.creates != 1 {
			t.Fatal("did not attempt creation")
		}
	}
	b := &bucketFailure{head: &smithy.GenericAPIError{Code: "NotFound"}, create: &smithy.GenericAPIError{Code: "BucketAlreadyOwnedByYou"}}
	if err := ensureBucket(context.Background(), b, "test-local"); err != nil {
		t.Fatal(err)
	}
}
