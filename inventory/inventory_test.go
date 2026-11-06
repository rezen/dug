package inventory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/rezen/dug/internal/layout"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	opts.PartEntries = 1
	summary, err := Create(context.Background(), lister{refs: []registry.Reference{ref("first", "1")}, err: errors.New("listing interrupted")}, store, Header{Registry: "test"}, opts)
	if _, committed := store.Objects()[summary.Key+"/"+completeName]; err == nil || committed {
		t.Fatalf("err=%v committed=%v objects=%v", err, committed, keys(store))
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

// jsonl encodes records as JSONL lines.
func jsonl(values ...record) string {
	var body strings.Builder
	for _, value := range values {
		line, _ := json.Marshal(value)
		body.Write(line)
		body.WriteByte('\n')
	}
	return body.String()
}

// buildInventory stores the given part bodies plus a matching complete.json and
// returns the inventory key. tamper may change the completion or the bodies
// after the completion was computed.
func buildInventory(t *testing.T, store *memstore.Store, parts []string, tamper func(*Completion, []string)) string {
	t.Helper()
	id, err := layout.NewULID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	key, err := layout.InventoryKey("test", id)
	if err != nil {
		t.Fatal(err)
	}
	completion := Completion{SchemaVersion: 1, Registry: "test", CreatedAt: time.Now().UTC()}
	for i, body := range parts {
		sum := sha256.Sum256([]byte(body))
		entries := int64(strings.Count(body, `"type":"entry"`))
		completion.Entries += entries
		completion.Parts = append(completion.Parts, Part{Name: partName(i + 1), Size: int64(len(body)), Entries: entries, SHA256: hex.EncodeToString(sum[:])})
	}
	if tamper != nil {
		tamper(&completion, parts)
	}
	put := func(name, body string) {
		if err := store.Put(context.Background(), key+"/"+name, strings.NewReader(body), int64(len(body)), "application/x-ndjson"); err != nil {
			t.Fatal(err)
		}
	}
	for i, body := range parts {
		put(partName(i+1), body)
	}
	data, _ := json.Marshal(completion)
	put(completeName, string(data))
	return key
}

func TestCorruptAndIncompleteInventoriesAreRejected(t *testing.T) {
	header := record{Type: "header", Header: &Header{SchemaVersion: 1, Registry: "test"}}
	entry := ref("item", "1")
	wrong := ref("item", "1")
	wrong.Registry = "wrong"
	good := jsonl(header, record{Type: "entry", Reference: &entry})
	for name, c := range map[string]struct {
		parts  []string
		tamper func(*Completion, []string)
	}{
		"checksum":       {[]string{good}, func(_ *Completion, p []string) { p[0] = strings.Replace(p[0], "item", "edit", 1) }},
		"count":          {[]string{good}, func(c *Completion, _ []string) { c.Entries = 2 }},
		"part-count":     {[]string{good}, func(c *Completion, _ []string) { c.Parts[0].Entries = 5 }},
		"part-size":      {[]string{good}, func(c *Completion, _ []string) { c.Parts[0].Size++ }},
		"wrong-registry": {[]string{jsonl(header, record{Type: "entry", Reference: &wrong})}, nil},
		"stray-complete": {[]string{good + jsonl(record{Type: "complete", Count: 1})}, nil},
		"late-header":    {[]string{good, jsonl(header)}, nil},
		"missing-part":   {[]string{good}, func(c *Completion, _ []string) { c.Parts = append(c.Parts, Part{Name: partName(2)}) }},
		"part-name":      {[]string{good}, func(c *Completion, _ []string) { c.Parts[0].Name = "../part-000001.jsonl" }},
		"no-header":      {[]string{jsonl(record{Type: "entry", Reference: &entry})}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			store := memstore.New()
			opts := testOptions(t)
			key := buildInventory(t, store, c.parts, c.tamper)
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

func TestInventoryWithoutCompletionIsPartial(t *testing.T) {
	store := memstore.New()
	opts := testOptions(t)
	opts.PartEntries = 2
	boom := errors.New("listing interrupted")
	refs := []registry.Reference{ref("a", "1"), ref("b", "2"), ref("c", "3"), ref("d", "4"), ref("e", "5")}
	summary, err := Create(context.Background(), lister{refs: refs, err: boom}, store, Header{Registry: "test"}, opts)
	if !errors.Is(err, boom) || summary.Key == "" {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	// Five entries at two per part: two full parts uploaded, the fifth still buffered.
	if _, ok := store.Objects()[summary.Key+"/"+partName(1)]; !ok {
		t.Fatalf("no part survived: %v", keys(store))
	}
	if _, ok := store.Objects()[summary.Key+"/"+completeName]; ok {
		t.Fatal("interrupted listing wrote complete.json")
	}
	if _, err := Open(context.Background(), store, summary.Key, Order{}, opts); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("expected ErrIncomplete, got %v", err)
	}
	opts.AllowPartial = true
	snapshot, err := Open(context.Background(), store, summary.Key, Order{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	var names []string
	snapshot.Entries(context.Background(), func(r registry.Reference) error { names = append(names, r.Name); return nil })
	if !snapshot.Partial || strings.Join(names, ",") != "a,b,c,d" {
		t.Fatalf("partial=%v names=%v", snapshot.Partial, names)
	}
}

func TestPartsUploadWhileListing(t *testing.T) {
	store := memstore.New()
	opts := testOptions(t)
	opts.PartEntries = 2
	var refs []registry.Reference
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		refs = append(refs, ref(n, "1"))
	}
	summary, err := Create(context.Background(), lister{refs: refs}, store, Header{Registry: "test"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	// Header + 2 entries, 2 entries, 1 entry: three parts, plus complete.json.
	for _, name := range []string{partName(1), partName(2), partName(3), completeName} {
		if _, ok := store.Objects()[summary.Key+"/"+name]; !ok {
			t.Fatalf("missing %s: %v", name, keys(store))
		}
	}
	if _, ok := store.Objects()[summary.Key+"/"+partName(4)]; ok || len(store.Objects()) != 4 {
		t.Fatalf("unexpected objects: %v", keys(store))
	}
	snapshot, err := Open(context.Background(), store, summary.Key, Order{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	if snapshot.Summary.Entries != 5 || snapshot.Partial {
		t.Fatalf("%+v", snapshot)
	}
}

func TestFailedPartUploadStopsListing(t *testing.T) {
	opts := testOptions(t)
	opts.PartEntries = 1
	store := &failingPutStore{Store: memstore.New(), failAfter: 1}
	summary, err := Create(context.Background(), lister{refs: []registry.Reference{ref("a", "1"), ref("b", "2"), ref("c", "3")}}, store, Header{Registry: "test"}, opts)
	if err == nil || summary.Key == "" {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	if _, ok := store.Objects()[summary.Key+"/"+completeName]; ok {
		t.Fatal("complete.json written after a failed part upload")
	}
}

type failingPutStore struct {
	*memstore.Store
	failAfter int
	puts      int
}

func (f *failingPutStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, mediaType string) error {
	f.puts++
	if f.puts > f.failAfter {
		return errors.New("s3 unavailable")
	}
	return f.Store.Put(ctx, key, body, size, mediaType)
}

func keys(s *memstore.Store) []string {
	var out []string
	for k := range s.Objects() {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
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
	mem := store.Store
	key := buildInventory(t, mem, []string{jsonl(record{Type: "header", Header: &Header{SchemaVersion: 1, Registry: "test"}}, record{Type: "entry", Reference: &entry})}, nil)
	opts := testOptions(t)
	opts.Attempts = 3
	if snapshot, err := Open(context.Background(), store, key, Order{}, opts); err == nil {
		snapshot.Close()
		t.Fatal("accepted an oversized inventory record")
	}
	// One attempt: complete.json and the offending part, each opened once.
	if store.opens.Load() != 2 {
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
