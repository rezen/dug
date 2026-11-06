package registry

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestFactPathsResolveTypedValues(t *testing.T) {
	released := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ref := Reference{Facts: &Facts{
		Downloads:        &DownloadStats{Total: Int64(100), Week: Int64(7)},
		VersionDownloads: &DownloadStats{Total: Int64(3)},
		Installs:         Int64(50),
		Repository:       &Repository{URL: "https://github.com/acme/widget", Commit: "abc", Source: RepositoryRegistry},
		ReleasedAt:       Time(released),
	}}
	for path, want := range map[string]Fact{
		"downloads.total":         {Kind: FactNumber, Number: 100},
		"downloads.week":          {Kind: FactNumber, Number: 7},
		"version_downloads.total": {Kind: FactNumber, Number: 3},
		"installs":                {Kind: FactNumber, Number: 50},
		"repository.url":          {Kind: FactString, String: "https://github.com/acme/widget"},
		"repository.commit":       {Kind: FactString, String: "abc"},
		"repository.source":       {Kind: FactString, String: RepositoryRegistry},
		"released_at":             {Kind: FactDate, Date: released},
	} {
		got, ok := ref.Fact(path)
		if !ok || !reflect.DeepEqual(got, want) || FactKindOf(path) != want.Kind {
			t.Errorf("%s: got %+v ok=%v kind=%v, want %+v", path, got, ok, FactKindOf(path), want)
		}
	}
	for _, path := range []string{"downloads.month", "version_downloads.week", "updated_at", "repository.directory", "stars", "downloads", ""} {
		if _, ok := ref.Fact(path); ok {
			t.Errorf("%s: unexpectedly present", path)
		}
	}
	if _, ok := (Reference{}).Fact("installs"); ok {
		t.Error("nil facts resolved a value")
	}
	if FactKindOf("stars") != 0 || FactKindOf("downloads") != 0 {
		t.Error("metadata keys must not be typed fact paths")
	}
	if paths := FactPaths(); len(paths) != 24 || paths[0] != "author.email" || paths[5] != "downloads.day" {
		t.Errorf("paths=%v", paths)
	}
}

func TestFactsCloneIsDeep(t *testing.T) {
	original := &Facts{Downloads: &DownloadStats{Total: Int64(1)}, Installs: Int64(2), Repository: &Repository{URL: "https://example.org/r", Source: RepositoryDeclared}, UpdatedAt: Time(time.Unix(10, 0))}
	clone := original.Clone()
	*clone.Downloads.Total = 99
	*clone.Installs = 99
	clone.Repository.URL = "changed"
	*clone.UpdatedAt = time.Unix(20, 0)
	if *original.Downloads.Total != 1 || *original.Installs != 2 || original.Repository.URL != "https://example.org/r" || original.UpdatedAt.Unix() != 10 {
		t.Fatalf("clone shared memory with the original: %+v", original)
	}
	if (*Facts)(nil).Clone() != nil {
		t.Fatal("nil clone")
	}
}

func TestFactsValidate(t *testing.T) {
	if err := (*Facts)(nil).Validate(); err != nil {
		t.Fatal(err)
	}
	valid := &Facts{Downloads: &DownloadStats{Total: Int64(0)}, Installs: Int64(0), Repository: &Repository{URL: "https://example.org/r", Source: RepositoryAttested}, ReleasedAt: Time(time.Unix(1, 0))}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, invalid := range map[string]*Facts{
		"negative total":     {Downloads: &DownloadStats{Total: Int64(-1)}},
		"negative week":      {VersionDownloads: &DownloadStats{Week: Int64(-1)}},
		"negative installs":  {Installs: Int64(-1)},
		"zero released":      {ReleasedAt: &time.Time{}},
		"zero updated":       {UpdatedAt: &time.Time{}},
		"repository no url":  {Repository: &Repository{Source: RepositoryDeclared}},
		"repository source":  {Repository: &Repository{URL: "https://example.org/r", Source: "guessed"}},
		"repository missing": {Repository: &Repository{URL: "https://example.org/r"}},
	} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNormalizeRepositoryURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://github.com/Acme/Widget":            "https://github.com/Acme/Widget",
		"https://GitHub.com/acme/widget.git/":       "https://github.com/acme/widget",
		"git+https://github.com/acme/widget.git":    "https://github.com/acme/widget",
		"git+ssh://git@github.com/acme/widget.git":  "https://github.com/acme/widget",
		"ssh://git@gitlab.com/group/sub/widget.git": "https://gitlab.com/group/sub/widget",
		"git@github.com:acme/widget.git":            "https://github.com/acme/widget",
		"github:acme/widget":                        "https://github.com/acme/widget",
		"gitlab:acme/widget":                        "https://gitlab.com/acme/widget",
		"bitbucket:acme/widget":                     "https://bitbucket.org/acme/widget",
		"acme/widget":                               "https://github.com/acme/widget",
		"http://plugins.svn.wordpress.org/akismet/": "https://plugins.svn.wordpress.org/akismet",
		" https://github.com/acme/widget ":          "https://github.com/acme/widget",
	} {
		got, ok := NormalizeRepositoryURL(raw)
		if !ok || got != want {
			t.Errorf("%q: got %q ok=%v, want %q", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "widget", "https://github.com", "https://github.com/", "ftp://example.org/repo", "mailto:dev@example.org", "acme/widget/extra", "git@github.com"} {
		if got, ok := NormalizeRepositoryURL(raw); ok {
			t.Errorf("%q: accepted as %q", raw, got)
		}
	}
}

func TestFactsJSONOmitsAbsentFields(t *testing.T) {
	data, err := json.Marshal(Reference{Registry: "r", Name: "n", Version: "1", Facts: &Facts{Downloads: &DownloadStats{Week: Int64(5)}}})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"registry":"r","name":"n","version":"1","facts":{"downloads":{"week":5}}}` {
		t.Fatal(string(data))
	}
	var decoded Reference
	if err := json.Unmarshal(data, &decoded); err != nil || decoded.Facts.Downloads.Total != nil || *decoded.Facts.Downloads.Week != 5 {
		t.Fatalf("%+v %v", decoded.Facts, err)
	}
}
