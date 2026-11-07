// Command dug-web serves a read-only dashboard over the inventories that
// `dug list` keeps locally: entry counts, the download size where registries
// publish one, and the top packages by popularity.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/rezen/dug/internal/invstats"
)

//go:embed index.html
var indexHTML []byte

type response struct {
	Dir         string             `json:"dir"`
	Inventories []invstats.Summary `json:"inventories"`
	Skipped     []string           `json:"skipped,omitempty"`
}

func handler(dir *invstats.Dir) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/inventories", func(w http.ResponseWriter, r *http.Request) {
		sums, skipped, err := dir.Load()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if sums == nil {
			sums = []invstats.Summary{}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(response{Dir: dir.Path, Inventories: sums, Skipped: skipped})
	})
	return mux
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "dug-web:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("dug-web", flag.ContinueOnError)
	addr := flags.String("addr", "127.0.0.1:7790", "Listen address; the page has no authentication, so keep it on loopback unless fronted by something that has")
	path := flags.String("inventory-dir", ".local/inventories", "Directory of local JSONL inventories written by `dug list`")
	if err := flags.Parse(args); err != nil {
		return err
	}
	dir := &invstats.Dir{Path: *path}
	if _, _, err := dir.Load(); err != nil {
		return fmt.Errorf("inventory directory: %w", err)
	}
	srv := &http.Server{Addr: *addr, Handler: handler(dir), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("serving %s on http://%s", *path, *addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
