package invstats

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const header = `{"type":"header","header":{"schema_version":1,"registry":"demo","created_at":"2026-01-02T03:04:05Z"}}` + "\n"

func write(t *testing.T, dir, name string, lines ...string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func entry(name, version, attrs, facts, meta string) string {
	return `{"type":"entry","reference":{"registry":"demo","namespace":"ns","name":"` + name + `","version":"` + version + `","attributes":{` + attrs + `},"metadata":{` + meta + `},"facts":{` + facts + `}}}`
}

func TestScanTotalsAndRankings(t *testing.T) {
	dir := t.TempDir()
	lines := []string{strings.TrimSpace(header)}
	// a has two versions: size counts both, ranking counts the package once.
	lines = append(lines,
		entry("a", "2", `"size":"100"`, `"downloads":{"total":50},"installs":7`, ""),
		entry("a", "1", `"size":"40"`, `"downloads":{"total":50},"installs":7`, ""),
		entry("b", "1", ``, `"downloads":{"total":900}`, `"stars":"12"`),
		entry("c", "1", `"size":"zero"`, `"downloads":{"total":0}`, ""),
	)
	for i := range 12 {
		lines = append(lines, entry("p"+string(rune('a'+i)), "1", ``, `"downloads":{"total":`+string(rune('1'+i%9))+`}`, ""))
	}
	lines = append(lines, `{"type":"complete","count":16,"sha256":"abc"}`)
	sum, err := Scan(write(t, dir, "x.jsonl", lines...))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Registry != "demo" || !sum.Complete {
		t.Fatalf("registry=%q complete=%v", sum.Registry, sum.Complete)
	}
	if sum.Entries != 16 || sum.Packages != 15 {
		t.Fatalf("entries=%d packages=%d", sum.Entries, sum.Packages)
	}
	if sum.SizedEntries != 2 || sum.UnsizedEntries != 14 || sum.KnownBytes != 140 {
		t.Fatalf("sized=%d unsized=%d bytes=%d", sum.SizedEntries, sum.UnsizedEntries, sum.KnownBytes)
	}
	byKey := map[string]Ranking{}
	for _, r := range sum.Rankings {
		byKey[r.Key] = r
	}
	d := byKey["downloads"]
	if len(d.Items) != TopN || d.Items[0].Name != "b" || d.Items[0].Value != 900 {
		t.Fatalf("downloads ranking = %+v", d.Items)
	}
	if got := byKey["installs"].Items; len(got) != 1 || got[0].Name != "a" || got[0].Value != 7 {
		t.Fatalf("installs ranking = %+v", got)
	}
	if got := byKey["stars"].Items; len(got) != 1 || got[0].Value != 12 {
		t.Fatalf("stars ranking = %+v", got)
	}
}

func TestScanFlagsIncomplete(t *testing.T) {
	dir := t.TempDir()
	// Declared count disagrees with the entries present.
	p := write(t, dir, "x.jsonl", strings.TrimSpace(header), entry("a", "1", ``, ``, ``), `{"type":"complete","count":2}`)
	sum, err := Scan(p)
	if err != nil || sum.Complete {
		t.Fatalf("mismatched count: complete=%v err=%v", sum.Complete, err)
	}
	p = write(t, dir, "y.jsonl", strings.TrimSpace(header), entry("a", "1", ``, ``, ``))
	if sum, err = Scan(p); err != nil || sum.Complete {
		t.Fatalf("no completion record: complete=%v err=%v", sum.Complete, err)
	}
}

func TestScanRejectsMissingHeaderAndBadJSON(t *testing.T) {
	dir := t.TempDir()
	if _, err := Scan(write(t, dir, "a.jsonl", entry("a", "1", ``, ``, ``))); err == nil {
		t.Fatal("missing header accepted")
	}
	if _, err := Scan(write(t, dir, "b.jsonl", strings.TrimSpace(header), `{not json`)); err == nil {
		t.Fatal("bad JSON accepted")
	}
}

func TestDirKeepsOnlyNewestRunPerRegistry(t *testing.T) {
	dir := t.TempDir()
	run := func(registry, created string) []string {
		return []string{`{"type":"header","header":{"schema_version":1,"registry":"` + registry + `","created_at":"` + created + `"}}`, entry("a", "1", ``, ``, ``), `{"type":"complete","count":1}`}
	}
	write(t, dir, "old.jsonl", run("demo", "2026-01-01T00:00:00Z")...)
	write(t, dir, "new.jsonl", run("demo", "2026-03-01T00:00:00Z")...)
	write(t, dir, "mid.jsonl", run("demo", "2026-02-01T00:00:00Z")...)
	write(t, dir, "other.jsonl", run("other", "2026-01-15T00:00:00Z")...)
	write(t, dir, "bad.jsonl", `{oops`)
	d := &Dir{Path: dir}
	for range 2 { // second pass exercises the cache
		sums, skipped, err := d.Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(sums) != 2 || sums[0].ID != "new" || sums[1].ID != "other" || len(skipped) != 1 {
			t.Fatalf("sums=%+v skipped=%v", sums, skipped)
		}
	}
	if _, _, err := (&Dir{Path: filepath.Join(dir, "missing")}).Load(); err == nil {
		t.Fatal("missing directory accepted")
	}
}
