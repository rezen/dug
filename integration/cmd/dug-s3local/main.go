// dug-s3local serves GoFakeS3 with a persistent Bolt backend for local E2E tests.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3bolt"
	bolt "go.etcd.io/bbolt"
)

func main() {
	address := flag.String("listen", "127.0.0.1:9000", "Loopback S3 listen address")
	path := flag.String("data", ".local/s3.db", "Persistent test database")
	bucket := flag.String("bucket", "dug-e2e", "Bucket to create if missing")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, *address, *path, *bucket); err != nil {
		slog.Error("local S3 failed", "error", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, address, path, bucket string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("test S3 server must bind to a loopback IP")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return err
	}
	defer db.Close()
	backend := s3bolt.New(db)
	if err := backend.CreateBucket(bucket); err != nil && !gofakes3.HasErrorCode(err, gofakes3.ErrBucketAlreadyExists) {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: gofakes3.New(backend).Server(), ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := server.Shutdown(cleanup); err != nil {
				server.Close()
			}
		case <-done:
		}
	}()
	slog.Info("GoFakeS3 test server listening (authentication is not enforced)", "address", listener.Addr(), "bucket", bucket, "data", path)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
