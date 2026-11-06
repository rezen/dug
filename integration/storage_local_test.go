package integration_test

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/rezen/dug/services/storage"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3bolt"
	bolt "go.etcd.io/bbolt"
)

func TestGoFakeS3MultipartPersistsAcrossRestart(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "local")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "local")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	path := filepath.Join(t.TempDir(), "s3.db")
	payload := bytes.Repeat([]byte("multipart fixture "), 400000)
	for run := 0; run < 2; run++ {
		db, err := bolt.Open(path, 0600, nil)
		if err != nil {
			t.Fatal(err)
		}
		backend := s3bolt.New(db)
		if run == 0 {
			if err := backend.CreateBucket("test-bucket"); err != nil {
				t.Fatal(err)
			}
		}
		server := httptest.NewServer(gofakes3.New(backend).Server())
		store, err := storage.NewS3(context.Background(), storage.S3Options{Bucket: "test-bucket", Endpoint: server.URL, PathStyle: true, PartSize: 5 << 20, UploadConcurrency: 2, HTTPClient: server.Client()})
		if err != nil {
			t.Fatal(err)
		}
		if run == 0 {
			if err := store.Put(context.Background(), "multipart", bytes.NewReader(payload), int64(len(payload)), "application/octet-stream"); err != nil {
				t.Fatal(err)
			}
		}
		body, err := store.Open(context.Background(), "multipart")
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(body)
		body.Close()
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("persisted bytes differ: %v", err)
		}
		server.Close()
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
