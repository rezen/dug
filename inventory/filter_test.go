package inventory

import (
	"context"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/rezen/dug/sdk/registry"
)

func TestFilterComparisons(t *testing.T) {
	ref := registry.Reference{Name: "example", Metadata: map[string]string{"legacy_downloads": "21", "rating": "94", "num_ratings": "120", "ratings_5": "100", "updated_at": "2025-01-01T00:00:00Z"}, Facts: &registry.Facts{
		Downloads:        &registry.DownloadStats{Total: registry.Int64(21)},
		VersionDownloads: &registry.DownloadStats{Week: registry.Int64(2)},
		Installs:         registry.Int64(50),
		Repository:       &registry.Repository{URL: "https://github.com/acme/widget", Source: registry.RepositoryDeclared},
		UpdatedAt:        registry.Time(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)),
		ReleasedAt:       registry.Time(time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)),
	}, Tags: []string{"privacy", "seo"}, Author: &registry.Author{Name: "Acme Inc", Username: "acme", URL: "https://profiles.example.org/acme"}}
	for spec, want := range map[string]bool{
		"tag = seo": true, "tag = SEO ": true, "tag != seo": false, "tag != other": true, "tag = s": false,
		"author = acme": true, `author.name = "Acme Inc"`: true, "author.name != acme": true, "author.id = 7": false, "author.url = https://profiles.example.org/acme": true,
		"": true, "downloads > 20 AND updated >= 2025-01-01": true,
		"downloads > 21": false, "downloads <= 21 AND rating >= 90": true,
		"downloads.total = 21": true, "downloads.week > 0": false, "version_downloads.week >= 2": true,
		"installs >= 50": true, "active_installs < 50": false,
		"ratings = 120": true, "name != other": true, "name = example": true,
		`updated < "2025-01-01"`: false, "number:ratings_5 >= 100": true,
		"number:missing != 0": false, "updated >= 2025-01-02": false,
		"released >= 2024-06-01": true, "released_at > 2024-06-01T12:00:00Z": false,
		"repository = https://github.com/acme/widget": true, `repository.url != "https://github.com/acme/widget"`: false,
		"repository.source = declared": true, "repository.commit != abc": false,
		"date:updated_at >= 2025-01-01": true, "number:legacy_downloads > 20": true,
	} {
		filter, err := ParseFilter(spec)
		if err != nil {
			t.Fatalf("%s: %v", spec, err)
		}
		got, err := filter.Match(ref)
		if err != nil || got != want {
			t.Errorf("%s: got %v %v, want %v", spec, got, err, want)
		}
	}
	for _, spec := range []string{"downloads > NaN", "released_at > yesterday", "downloads > 20 OR downloads < 10", "updated >= 2025-02-30", "typo > 1", "downloaded > 1", "downloads.lifetime > 1", "tag > a", "tag <= z", "author.handle = x", "rating > 1 AND", "name", `name = "unterminated`} {
		if _, err := ParseFilter(spec); err == nil {
			t.Errorf("accepted %q", spec)
		}
	}
	f, _ := ParseFilter("rating > 1")
	ref.Metadata["rating"] = "invalid"
	if _, err := f.Match(ref); err == nil {
		t.Fatal("malformed source metadata should fail")
	}
	f, _ = ParseFilter("downloads > 1")
	if ok, err := f.Match(registry.Reference{Name: "bare"}); ok || err != nil {
		t.Fatalf("reference without facts matched: %v %v", ok, err)
	}
}

func TestSelectionFiltersBeforeLimitAndCanResume(t *testing.T) {
	entries := func(ctx context.Context, emit func(registry.Reference) error) error {
		for _, v := range []int64{0, 30, 40, 50} {
			if err := emit(registry.Reference{Name: strconv.FormatInt(v, 10), Facts: &registry.Facts{Downloads: &registry.DownloadStats{Total: registry.Int64(v)}}}); err != nil {
				return err
			}
		}
		return nil
	}
	f, _ := ParseFilter("downloads > 20")
	selected := Select(entries, f, 2)
	for i := 0; i < 2; i++ {
		var got []string
		if err := selected(context.Background(), func(ref registry.Reference) error { got = append(got, ref.Name); return nil }); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, []string{"30", "40"}) {
			t.Fatal(got)
		}
	}
}
