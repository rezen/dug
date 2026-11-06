package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/services/storage/memstore"
)

func TestCreateRetainsCompleteLocalInventory(t *testing.T) {
	opts := testOptions(t)
	opts.LocalDir = filepath.Join(t.TempDir(), "inventories")
	seen := 0
	opts.OnEntry = func(registry.Reference) error { seen++; return nil }
	store := memstore.New()
	summary, err := Create(context.Background(), lister{refs: []registry.Reference{ref("one", "1"), ref("two", "2")}}, store,
		Header{Registry: "test", Provider: "example", Kind: "packages", Limit: 1}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(summary.LocalPath) || seen != 1 || summary.Entries != 1 {
		t.Fatalf("summary=%+v seen=%d", summary, seen)
	}
	local, err := os.ReadFile(summary.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(local, store.Objects()[summary.Key+"/"+partName(1)]) {
		t.Fatal("local inventory differs from the uploaded part")
	}
	if filepath.Base(summary.LocalPath) != path.Base(summary.Key)+".jsonl" {
		t.Fatal("local filename does not match the stored inventory id")
	}
	decoder := json.NewDecoder(bytes.NewReader(local))
	for _, kind := range []string{"header", "entry"} {
		var value record
		if err := decoder.Decode(&value); err != nil || value.Type != kind {
			t.Fatalf("expected %s record, got %+v (%v)", kind, value, err)
		}
		if kind == "header" && (value.Header.Provider != "example" || value.Header.Kind != "packages") {
			t.Fatalf("missing catalog identity: %+v", value.Header)
		}
	}
	files, _ := os.ReadDir(opts.LocalDir)
	if len(files) != 1 {
		t.Fatal("temporary inventory was not cleaned up")
	}
	snapshot, err := Open(context.Background(), store, summary.Key, Order{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	if snapshot.Header.Provider != "example" || snapshot.Header.Kind != "packages" {
		t.Fatal("opening an inventory lost catalog identity")
	}
}

func TestFailedListingDoesNotRetainLocalInventory(t *testing.T) {
	for _, failure := range []string{"discovery", "output"} {
		t.Run(failure, func(t *testing.T) {
			opts := testOptions(t)
			opts.LocalDir = t.TempDir()
			interrupted := errors.New("interrupted")
			source := lister{refs: []registry.Reference{ref("one", "1")}}
			if failure == "discovery" {
				source.err = interrupted
			} else {
				opts.OnEntry = func(registry.Reference) error { return interrupted }
			}
			store := memstore.New()
			summary, err := Create(context.Background(), source, store, Header{Registry: "test"}, opts)
			if !errors.Is(err, interrupted) || summary.LocalPath != "" || len(store.Objects()) != 0 {
				t.Fatalf("failed listing published an inventory: %+v, %v", summary, err)
			}
			files, _ := os.ReadDir(opts.LocalDir)
			if len(files) != 0 {
				t.Fatal("incomplete local inventory leaked")
			}
		})
	}
}
