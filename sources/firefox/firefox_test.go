package firefox

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rezen/dug/sdk/registry"
)

func testVersion(id int64, date, base string) version {
	v := version{ID: id, Version: fmt.Sprint(id)}
	v.File.ID = id
	v.File.Created, _ = time.Parse(time.RFC3339, date)
	v.File.Hash = fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("xpi bytes")))
	v.File.Size = 9
	v.File.Status = "public"
	v.File.URL = base + "/file.xpi"
	return v
}

func TestVersionDepthMetadataAndPinnedDownload(t *testing.T) {
	var queries, downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/file.xpi" {
			downloads.Add(1)
			io.WriteString(w, "xpi bytes")
			return
		}
		queries.Add(1)
		base := "http://" + r.Host
		latest := testVersion(3, "2026-01-01T00:00:00Z", base)
		switch r.URL.Path {
		case "/api/v5/addons/addon/example/":
			a := addon{ID: 1, Slug: "example", Type: "extension", Current: latest, WeeklyDownloads: 123, Users: 456, Tags: []string{"Firefox57", "privacy"}}
			a.Ratings.Average = 4.5
			a.Ratings.Count = 10
			a.LastUpdated = latest.File.Created
			a.Authors = append(a.Authors, struct {
				ID       int64  `json:"id"`
				Name     string `json:"name"`
				Username string `json:"username"`
				URL      string `json:"url"`
			}{ID: 77, Name: "Example Dev", Username: "exampledev", URL: base + "/user/exampledev/"})
			// Categories are sent keyed by application, the older AMO shape.
			var encoded map[string]any
			raw, _ := json.Marshal(a)
			json.Unmarshal(raw, &encoded)
			encoded["categories"] = map[string][]string{"firefox": {"privacy-security", "Privacy"}, "android": {"privacy-security"}}
			json.NewEncoder(w).Encode(encoded)
		case "/api/v5/addons/addon/1/versions/":
			if r.URL.Query().Get("page") == "2" {
				json.NewEncoder(w).Encode(page[version]{Count: 3, Results: []version{testVersion(2, "2025-01-01T00:00:00Z", base), testVersion(1, "2020-01-01T00:00:00Z", base)}})
			} else {
				json.NewEncoder(w).Encode(page[version]{Count: 3, Next: base + "/api/v5/addons/addon/1/versions/?page=2", Results: []version{latest}})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	for depth, want := range map[string][]string{"1": {"3"}, "2": {"3", "2"}, "unlimited": {"3", "2", "1"}, "2025-01-01": {"3", "2"}, "2027-01-01": {}} {
		t.Run(depth, func(t *testing.T) {
			s := &Source{Client: server.Client(), Endpoint: server.URL + "/api/v5/", Slugs: []string{"example"}, VersionDepth: depth, Attempts: 1, DownloadHosts: []string{strings.TrimPrefix(server.URL, "http://")}}
			refs := []registry.Reference{}
			if err := s.Discover(context.Background(), func(ref registry.Reference) error { refs = append(refs, ref); return nil }); err != nil {
				t.Fatal(err)
			}
			got := []string{}
			for _, ref := range refs {
				got = append(got, ref.Version)
				if ref.Metadata["rating"] != "90" || ref.Metadata["num_ratings"] != "10" || ref.Metadata["weekly_downloads"] != "" || ref.Metadata["released_at"] != "" {
					t.Fatal(ref.Metadata)
				}
				f := ref.Facts
				if f == nil || f.Downloads == nil || f.Downloads.Week == nil || *f.Downloads.Week != 123 || f.Downloads.Total != nil || f.Installs == nil || f.ReleasedAt == nil || f.Repository != nil {
					t.Fatalf("facts: %+v", f)
				}
				var raw map[string]any
				if err := json.Unmarshal(ref.Raw, &raw); err != nil || raw["slug"] != "example" || raw["categories"] == nil {
					t.Fatalf("raw = %s (%v)", ref.Raw, err)
				}
				if !reflect.DeepEqual(ref.Tags, []string{"firefox57", "privacy", "privacy-security"}) || !reflect.DeepEqual(ref.Author, &registry.Author{Name: "Example Dev", Username: "exampledev", ID: "77", URL: server.URL + "/user/exampledev/"}) {
					t.Fatalf("tags=%v author=%+v", ref.Tags, ref.Author)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v want %v", got, want)
			}
			if len(refs) > 0 {
				before := queries.Load()
				a, err := s.Resolve(context.Background(), refs[0])
				if err != nil {
					t.Fatal(err)
				}
				body, err := a.Blobs[0].Open(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(body)
				body.Close()
				if err != nil || string(data) != "xpi bytes" {
					t.Fatalf("%s %v", data, err)
				}
				if queries.Load() != before {
					t.Fatal("download re-queried metadata")
				}
				refs[0].Attributes["sha256"] = "sha256:bad"
				if _, err := s.Resolve(context.Background(), refs[0]); err == nil {
					t.Fatal("accepted bad digest")
				}
			}
		})
	}
	if downloads.Load() != 4 {
		t.Fatal(downloads.Load())
	}
}

func TestSearchPartitionsCappedResultsAndPaginates(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		from, to := q.Get("created__gte"), q.Get("created__lt")
		if q.Get("app") != "firefox" || q.Get("type") != "extension" {
			t.Error("missing Firefox extension filter")
		}
		if strings.HasPrefix(from, "2000") && strings.HasPrefix(to, "2004") {
			json.NewEncoder(w).Encode(page[addon]{Count: 3})
			return
		}
		p := page[addon]{Count: 1, Results: []addon{{ID: 3}}}
		if strings.HasPrefix(from, "2000") {
			p.Count = 2
			p.Results = []addon{{ID: 1}}
			if q.Get("page") == "2" {
				p.Results = []addon{{ID: 2}}
			} else {
				q.Set("page", "2")
				p.Next = "http://" + r.Host + r.URL.Path + "?" + q.Encode()
			}
		}
		json.NewEncoder(w).Encode(p)
	}))
	defer server.Close()
	s := &Source{Client: server.Client(), Endpoint: server.URL + "/", searchWindow: 2, Attempts: 1}
	var got []int64
	err := s.search(context.Background(), time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2004, 1, 1, 0, 0, 0, 0, time.UTC), func(a addon) error { got = append(got, a.ID); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []int64{3, 1, 2}) || calls.Load() != 4 {
		t.Fatalf("got %v calls %d", got, calls.Load())
	}
}

func TestValidationAndCancellation(t *testing.T) {
	for _, opts := range []map[string]string{{"unknown": "x"}, {"slugs": "../bad"}, {"slugs": ""}, {"slugs": "a", "search": "b"}, {"page_size": "51"}, {"version_depth": "0"}} {
		if _, err := PluginFactory(nil)(opts); err == nil {
			t.Fatal(opts)
		}
	}
	s := &Source{Endpoint: "https://example.com/api/v5/"}
	if _, err := s.next("https://other.example/api/v5/?page=2", map[string]bool{}); err == nil {
		t.Fatal("cross-origin pagination accepted")
	}
	if _, err := s.next("/other/", map[string]bool{}); err == nil {
		t.Fatal("cross-path pagination accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Slugs = []string{"example"}
	if err := s.Discover(ctx, func(registry.Reference) error { return nil }); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestDownloadURLsMustUseMozillaHost(t *testing.T) {
	s := &Source{}
	ref := registry.Reference{Registry: "firefox", Name: "example", Version: "1", Attributes: map[string]string{"download_url": "http://169.254.169.254/latest/meta-data/", "sha256": "sha256:" + strings.Repeat("0", 64), "size": "1", "file_id": "1"}}
	if _, err := s.Resolve(context.Background(), ref); err == nil {
		t.Fatal("resolved an XPI URL off Mozilla's download host")
	}
	ref.Attributes["download_url"] = "https://addons.mozilla.org/firefox/downloads/file/1/example.xpi"
	if _, err := s.Resolve(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
}
