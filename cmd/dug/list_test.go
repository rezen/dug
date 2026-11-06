package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rezen/dug/inventory"
	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/services/storage"
	"github.com/rezen/dug/services/storage/memstore"
)

func TestListShowsEntriesAndLocalInventory(t *testing.T) {
	handler, err := plugin.NewHandler(plugin.ServerOptions{
		Registry: "wordpress",
		Provider: registry.Provider{ID: "wordpress", Registries: []registry.Definition{{ID: "wordpress", Kind: "plugins"}}},
		Factory:  func(map[string]string) (registry.Registry, error) { return discoveryFixture{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	for _, format := range []string{"table", "jsonl", "summary"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			store := memstore.New()
			var out, logs bytes.Buffer
			args := []string{"list", "wordpress", "--bucket", "test", "--plugin-url", server.URL, "--limit", "2", "--inventory-dir", dir}
			if format != "table" {
				args = append(args, "--format", format)
			}
			if err := executeWithStore(context.Background(), args, &out, &logs, func(context.Context, storage.S3Options) (storage.ReadableStore, error) { return store, nil }); err != nil {
				t.Fatal(err)
			}
			files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
			if err != nil || len(files) != 1 {
				t.Fatalf("missing inventory: %v, %v", files, err)
			}
			data, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			var first struct {
				Header inventory.Header `json:"header"`
			}
			if err := json.NewDecoder(bytes.NewReader(data)).Decode(&first); err != nil {
				t.Fatal(err)
			}
			if first.Header.Provider != "wordpress" || first.Header.Kind != "plugins" {
				t.Fatalf("local inventory is missing catalog identity: %+v", first.Header)
			}
			switch format {
			case "table":
				lines := strings.Split(strings.TrimSpace(out.String()), "\n")
				if len(lines) != 3 || !strings.Contains(lines[0], "KIND") || !strings.Contains(lines[0], "PROVIDER") {
					t.Fatalf("expected header and two rows: %s", out.String())
				}
				for _, line := range lines[1:] {
					if fields := strings.Fields(line); strings.Join(fields, " ") != "wordpress plugins wordpress 1 example" {
						t.Fatalf("incorrect row: %s", line)
					}
				}
			case "jsonl":
				decoder := json.NewDecoder(&out)
				for range 2 {
					var row map[string]any
					if err := decoder.Decode(&row); err != nil {
						t.Fatal(err)
					}
					if row["name"] != "example" || row["provider"] != "wordpress" || row["kind"] != "plugins" || row["registry"] != "wordpress" {
						t.Fatalf("incorrect JSONL reference: %+v", row)
					}
				}
				var extra any
				if err := decoder.Decode(&extra); err != io.EOF {
					t.Fatalf("stdout contains non-entry data: %v", err)
				}
			case "summary":
				var summary inventory.Summary
				if err := json.NewDecoder(&out).Decode(&summary); err != nil {
					t.Fatal(err)
				}
				if summary.Entries != 2 || summary.LocalPath != files[0] || summary.Key == "" {
					t.Fatalf("incorrect summary: %+v", summary)
				}
			}
			if format != "summary" && (!strings.Contains(logs.String(), files[0]) || !strings.Contains(logs.String(), "s3://test/dug/inventories/sha256/")) {
				t.Fatalf("missing local/S3 paths: %s", logs.String())
			}
		})
	}
}

func TestListEscapesTerminalControls(t *testing.T) {
	var out bytes.Buffer
	output := listOutput{format: "table", stdout: &out}
	if err := output.entry(registry.Reference{Registry: "external", Name: "bad\n\x1b[31m", Version: "1"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") || strings.Count(out.String(), "\n") != 2 {
		t.Fatalf("upstream text changed terminal layout: %q", out.String())
	}
}
