package storage

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// This fixture exercises real SDK signing, endpoint/addressing, multipart wire
// requests and aborts without requiring cloud credentials.
func TestS3WireProtocol(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	var mu sync.Mutex
	objects := map[string][]byte{}
	parts := map[int][]byte{}
	var aborted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.HasPrefix(r.URL.Path, "/bucket/prefix/") {
			t.Errorf("wrong path: %s", r.URL.Path)
			w.WriteHeader(400)
			return
		}
		if !strings.Contains(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
			t.Error("missing request signature")
			w.WriteHeader(403)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/bucket/prefix/")
		query := r.URL.Query()
		switch {
		case r.Method == http.MethodGet:
			data, ok := objects[key]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Write(data)
		case r.Method == http.MethodHead:
			if key == "forbidden" {
				w.WriteHeader(403)
				return
			}
			if data, ok := objects[key]; ok {
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				return
			}
			w.WriteHeader(404)
		case r.Method == http.MethodPost && query.Has("uploads"):
			parts = map[int][]byte{}
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, `<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>`+key+`</Key><UploadId>test-upload</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut && query.Get("uploadId") != "":
			if key == "fail" {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(400)
				io.WriteString(w, `<Error><Code>InvalidRequest</Code><Message>part rejected</Message></Error>`)
				return
			}
			n, _ := strconv.Atoi(query.Get("partNumber"))
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			parts[n] = data
			w.Header().Set("ETag", fmt.Sprintf(`"part-%d"`, n))
		case r.Method == http.MethodPost && query.Get("uploadId") != "":
			var complete struct {
				Parts []struct {
					Number int `xml:"PartNumber"`
				} `xml:"Part"`
			}
			if err := xml.NewDecoder(r.Body).Decode(&complete); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			var data []byte
			for _, part := range complete.Parts {
				data = append(data, parts[part.Number]...)
			}
			objects[key] = data
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, `<CompleteMultipartUploadResult><Bucket>bucket</Bucket><Key>`+key+`</Key><ETag>"complete"</ETag></CompleteMultipartUploadResult>`)
		case r.Method == http.MethodDelete && query.Get("uploadId") != "":
			aborted = true
			w.WriteHeader(204)
		case r.Method == http.MethodPut:
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			objects[key] = data
			w.Header().Set("ETag", `"object"`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	store, err := NewS3(context.Background(), S3Options{Bucket: "bucket", Prefix: "/prefix/", Endpoint: server.URL, Region: "us-east-1", PathStyle: true, UploadConcurrency: 2, PartSize: 5 << 20, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if exists, _, err := store.Stat(context.Background(), "absent"); err != nil || exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
	if _, err := store.Open(context.Background(), "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open of a missing key = %v, want ErrNotFound", err)
	}
	if _, _, err := store.Stat(context.Background(), "forbidden"); err == nil {
		t.Fatal("403 must not be treated as absent")
	}
	for _, size := range []int{0, 100, 11 << 20} {
		key := fmt.Sprint(size)
		payload := bytes.Repeat([]byte("x"), size)
		if err := store.Put(context.Background(), key, bytes.NewReader(payload), int64(size), "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
		if exists, size, err := store.Stat(context.Background(), key); err != nil || !exists || size != int64(len(payload)) {
			t.Fatalf("exists=%v size=%d err=%v", exists, size, err)
		}
		mu.Lock()
		equal := bytes.Equal(payload, objects[key])
		mu.Unlock()
		if !equal {
			t.Fatalf("payload mismatch for size %d", size)
		}
		body, err := store.Open(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(body)
		body.Close()
		if err != nil || !bytes.Equal(data, payload) {
			t.Fatalf("S3 read mismatch for size %d: %v", size, err)
		}
	}
	payload := bytes.Repeat([]byte("x"), 11<<20)
	if err := store.Put(context.Background(), "fail", bytes.NewReader(payload), int64(len(payload)), "application/octet-stream"); err == nil {
		t.Fatal("expected upload failure")
	}
	mu.Lock()
	defer mu.Unlock()
	if !aborted {
		t.Fatal("failed multipart upload was not aborted")
	}
	if _, exists := objects["fail"]; exists {
		t.Fatal("partial object was published")
	}
}
