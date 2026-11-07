package locals3

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

func TestEnsureServerReusesRunningServer(t *testing.T) {
	// Endpoint is a fixed constant, so exercise listening/ensureServer via a client
	// whose transport answers every request.
	up := &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Body: http.NoBody}, nil
	})}
	if err := ensureServer(context.Background(), up, "/root", func(string) error { return errors.New("started a second server") }); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureServerStartsWhenDown(t *testing.T) {
	down := &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})}
	started := ""
	if err := ensureServer(context.Background(), down, "/root", func(root string) error { started = root; return nil }); err != nil {
		t.Fatal(err)
	}
	if started != "/root" {
		t.Fatalf("started %q", started)
	}
	want := errors.New("rclone missing")
	if err := ensureServer(context.Background(), down, "/root", func(string) error { return want }); !errors.Is(err, want) {
		t.Fatalf("start error not returned: %v", err)
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReadinessRetriesAndCancels(t *testing.T) {
	attempts := 0
	// Refuse the first connections, then answer 403 like rclone does to unsigned requests.
	client := &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		attempts++
		if attempts < 3 {
			return nil, fmt.Errorf("connection refused")
		}
		return &http.Response{StatusCode: http.StatusForbidden, Body: http.NoBody}, nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitReady(ctx, client, "http://127.0.0.1:9000", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d", attempts)
	}
	down := &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) { return nil, errors.New("refused") })}
	cancel()
	if err := waitReady(ctx, down, "http://127.0.0.1:9000", time.Millisecond); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestBucketCreationAndReuseWithSignedRequests(t *testing.T) {
	var mu sync.Mutex
	buckets := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=dug/") {
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
