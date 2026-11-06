package inventory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/services/storage/memstore"
)

type lister struct {
	refs []registry.Reference
	err  error
}

func (l lister) Discover(ctx context.Context, emit func(registry.Reference) error) error {
	for _, ref := range l.refs {
		if err := emit(ref); err != nil {
			return err
		}
	}
	return l.err
}
func ref(name, installs string) registry.Reference {
	r := registry.Reference{Registry: "test", Name: name, Version: "1", Metadata: map[string]string{"active_installs": installs}}
	// Non-numeric values stay metadata-only so malformed number:KEY sorting can be tested.
	if n, err := strconv.ParseInt(installs, 10, 64); err == nil {
		r.Facts = &registry.Facts{Installs: registry.Int64(n)}
	}
	return r
}
func testOptions(t *testing.T) Options {
	t.Helper()
	opts := DefaultOptions()
	opts.TempDir = t.TempDir()
	opts.Attempts = 1
	return opts
}

func TestStageOnePublishesOnlyCompleteInventory(t *testing.T) {
	store := memstore.New()
	opts := testOptions(t)
	_, err := Create(context.Background(), lister{refs: []registry.Reference{ref("first", "1")}, err: errors.New("listing interrupted")}, store, Header{Registry: "test"}, opts)
	if err == nil || len(store.Objects()) != 0 {
		t.Fatalf("err=%v objects=%d", err, len(store.Objects()))
	}
	files, _ := os.ReadDir(opts.TempDir)
	if len(files) != 0 {
		t.Fatal("listing temp file leaked")
	}
}

func TestOrderingAndStableTies(t *testing.T) {
	store := memstore.New()
	opts := testOptions(t)
	opts.SortBufferBytes = 1 // force disk merge
	refs := []registry.Reference{ref("z", "2"), ref("a", "10"), ref("b", "10"), ref("missing", "")}
	summary, err := Create(context.Background(), lister{refs: refs}, store, Header{Registry: "test", Options: map[string]string{"query": "saved"}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	for orderSpec, want := range map[string][]string{
		"discovery": {"z", "a", "b", "missing"}, "-discovery": {"missing", "b", "a", "z"},
		"name": {"a", "b", "missing", "z"}, "popular": {"a", "b", "z", "missing"},
		"installs": {"z", "a", "b", "missing"}, "number:active_installs": {"z", "a", "b", "missing"},
	} {
		t.Run(orderSpec, func(t *testing.T) {
			order, err := ParseOrder(orderSpec)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := Open(context.Background(), store, summary.Key, order, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer snapshot.Close()
			if snapshot.Header.Options["query"] != "saved" || snapshot.Summary.Entries != 4 {
				t.Fatalf("snapshot=%+v", snapshot)
			}
			var got []string
			if err := snapshot.Entries(context.Background(), func(ref registry.Reference) error { got = append(got, ref.Name); return nil }); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("got=%v want=%v", got, want)
			}
		})
	}
}

func TestMultiPassExternalSort(t *testing.T) {
	store := memstore.New()
	opts := testOptions(t)
	opts.SortBufferBytes = 1
	var refs []registry.Reference
	for i := 69; i >= 0; i-- {
		refs = append(refs, ref(fmt.Sprintf("item-%03d", i), ""))
	}
	summary, err := Create(context.Background(), lister{refs: refs}, store, Header{Registry: "test"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	order, _ := ParseOrder("name")
	snapshot, err := Open(context.Background(), store, summary.Key, order, opts)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	if err := snapshot.Entries(context.Background(), func(ref registry.Reference) error {
		if ref.Name != fmt.Sprintf("item-%03d", count) {
			return fmt.Errorf("wrong order at %d: %s", count, ref.Name)
		}
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 70 {
		t.Fatal(count)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	files, _ := os.ReadDir(opts.TempDir)
	if len(files) != 0 {
		t.Fatal("sorting temp files leaked")
	}
}

func TestCorruptAndIncompleteInventoriesAreRejected(t *testing.T) {
	for _, kind := range []string{"checksum", "missing-footer", "count", "trailing", "wrong-registry"} {
		t.Run(kind, func(t *testing.T) {
			store := memstore.New()
			opts := testOptions(t)
			entry := ref("item", "1")
			if kind == "wrong-registry" {
				entry.Registry = "wrong"
			}
			values := []record{{Type: "header", Header: &Header{SchemaVersion: 1, Registry: "test"}}, {Type: "entry", Reference: &entry}}
			if kind != "missing-footer" {
				count := int64(1)
				if kind == "count" {
					count = 2
				}
				values = append(values, record{Type: "complete", Count: count})
			}
			if kind == "trailing" {
				values = append(values, record{Type: "entry", Reference: &entry})
			}
			var body strings.Builder
			for _, value := range values {
				json.NewEncoder(&body).Encode(value)
			}
			sum := sha256.Sum256([]byte(body.String()))
			key := keyPrefix + hex.EncodeToString(sum[:]) + ".jsonl"
			data := body.String()
			if kind == "checksum" {
				data = strings.Replace(data, "item", "edit", 1)
			}
			if err := store.Put(context.Background(), key, strings.NewReader(data), int64(len(data)), "application/x-ndjson"); err != nil {
				t.Fatal(err)
			}
			if snapshot, err := Open(context.Background(), store, key, Order{}, opts); err == nil {
				snapshot.Close()
				t.Fatal("accepted invalid inventory")
			}
			files, _ := os.ReadDir(opts.TempDir)
			if len(files) != 0 {
				t.Fatal("failed read leaked temp files")
			}
		})
	}
}

func TestLimitEmptyInventoryAndBadSortValues(t *testing.T) {
	store := memstore.New()
	opts := testOptions(t)
	for _, refs := range [][]registry.Reference{nil, {ref("a", "1"), ref("b", "2")}} {
		summary, err := Create(context.Background(), lister{refs: refs}, store, Header{Registry: "test", Limit: 1}, opts)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := Open(context.Background(), store, summary.Key, Order{}, opts)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Summary.Entries != int64(min(len(refs), 1)) {
			t.Fatal("wrong limited count")
		}
		snapshot.Close()
	}
	summary, err := Create(context.Background(), lister{refs: []registry.Reference{ref("a", "NaN")}}, store, Header{Registry: "test"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	order, _ := ParseOrder("number:active_installs")
	if snapshot, err := Open(context.Background(), store, summary.Key, order, opts); err == nil {
		snapshot.Close()
		t.Fatal("accepted nonfinite sorting value")
	}
	for _, spec := range []string{"downloads.lifetime", "facts:installs", "number:", "metadata:"} {
		if _, err := ParseOrder(spec); err == nil {
			t.Errorf("accepted order %q", spec)
		}
	}
}

type countingStore struct {
	*memstore.Store
	opens atomic.Int32
}

func (c *countingStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	c.opens.Add(1)
	return c.Store.Open(ctx, key)
}

func TestOversizedInventoryLineFailsWithoutRetry(t *testing.T) {
	store := &countingStore{Store: memstore.New()}
	entry := ref("item", "1")
	entry.Metadata["pad"] = strings.Repeat("x", maxRecordBytes)
	var body strings.Builder
	for _, value := range []record{{Type: "header", Header: &Header{SchemaVersion: 1, Registry: "test"}}, {Type: "entry", Reference: &entry}, {Type: "complete", Count: 1}} {
		json.NewEncoder(&body).Encode(value)
	}
	sum := sha256.Sum256([]byte(body.String()))
	key := keyPrefix + hex.EncodeToString(sum[:]) + ".jsonl"
	if err := store.Put(context.Background(), key, strings.NewReader(body.String()), int64(body.Len()), "application/x-ndjson"); err != nil {
		t.Fatal(err)
	}
	opts := testOptions(t)
	opts.Attempts = 3
	if snapshot, err := Open(context.Background(), store, key, Order{}, opts); err == nil {
		snapshot.Close()
		t.Fatal("accepted an oversized inventory record")
	}
	if store.opens.Load() != 1 {
		t.Fatalf("re-downloaded a permanently invalid inventory: %d opens", store.opens.Load())
	}
}

func TestDiscoveryOrderStreamsWithoutSortRuns(t *testing.T) {
	dir := t.TempDir()
	s := newSorter(dir, Order{}, 1) // a one-byte buffer would otherwise force a run per item
	defer s.close()
	for i := 0; i < 50; i++ {
		if err := s.add(context.Background(), ref(fmt.Sprint(i), ""), int64(i), 100); err != nil {
			t.Fatal(err)
		}
	}
	path, err := s.finish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.next != 1 {
		t.Fatalf("discovery order created %d files instead of streaming to one", s.next)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	for i := 0; ; i++ {
		var item sortItem
		if err := decoder.Decode(&item); errors.Is(err, io.EOF) {
			if i != 50 {
				t.Fatalf("streamed %d items", i)
			}
			return
		} else if err != nil {
			t.Fatal(err)
		}
		if item.Sequence != int64(i) {
			t.Fatalf("item %d has sequence %d", i, item.Sequence)
		}
	}
}
