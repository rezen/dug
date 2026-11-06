package wordpress

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rezen/dug/sdk/registry"
)

func TestRatingsAndVersionDepth(t *testing.T) {
	var reports atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PROPFIND" {
			w.WriteHeader(207)
			fmt.Fprint(w, `<multistatus xmlns="DAV:">`)
			for _, v := range []string{"3.0", "2.10", "2.9"} {
				fmt.Fprintf(w, `<response><href>/example/tags/%s/</href><propstat><status>HTTP/1.1 200 OK</status><prop><creationdate>2026-01-01T00:00:00Z</creationdate><version-name>100</version-name></prop></propstat></response>`, v)
			}
			fmt.Fprint(w, `</multistatus>`)
			return
		}
		if r.Method == "REPORT" {
			reports.Add(1)
			date := "2025-01-01T00:00:00Z"
			if strings.Contains(r.URL.Path, "2.9") {
				date = "2020-01-01T00:00:00Z"
			}
			fmt.Fprintf(w, `<S:log-report xmlns:S="svn:"><S:log-item><S:date>%s</S:date></S:log-item></S:log-report>`, date)
			return
		}
		for _, field := range []string{"downloaded", "rating", "ratings", "num_ratings", "versions", "tags", "author", "author_profile"} {
			if r.URL.Query().Get("request[fields]["+field+"]") != "1" {
				t.Errorf("missing %s", field)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"slug": "example", "version": "3.0", "download_link": "http://" + r.Host + "/3.0.zip", "downloaded": 12345, "rating": 94, "num_ratings": 5, "ratings": map[string]int{"5": 4, "4": 1}, "last_updated": "2026-01-01 1:00pm GMT", "tags": map[string]string{"seo": "SEO", "Performance": "Performance"}, "author": `<a href="https://profiles.wordpress.org/ExampleDev/">Example &amp; <strong>Dev</strong></a>`, "author_profile": "https://profiles.wordpress.org/ExampleDev/", "versions": map[string]string{"2.9": "http://" + r.Host + "/2.9.zip", "2.10": "http://" + r.Host + "/2.10.zip", "3.0": "http://" + r.Host + "/3.0.zip", "trunk": "http://" + r.Host + "/trunk.zip"}})
	}))
	defer server.Close()
	for spec, want := range map[string][]string{"2": {"3.0", "2.10"}, "unlimited": {"3.0", "2.10", "2.9"}, "2025-01-01": {"3.0", "2.10"}, "2027-01-01": {}} {
		t.Run(spec, func(t *testing.T) {
			s := &Source{Client: server.Client(), Endpoint: server.URL, SVNEndpoint: server.URL, PageSize: 100, Slugs: []string{"example"}, VersionDepth: spec, Attempts: 1, DownloadHosts: testHosts(server)}
			got := []string{}
			err := s.Discover(context.Background(), func(ref registry.Reference) error {
				got = append(got, ref.Version)
				if ref.Metadata["downloads"] != "" || ref.Metadata["rating"] != "94" || ref.Metadata["num_ratings"] != "5" || ref.Metadata["ratings_5"] != "4" || ref.Metadata["ratings"] == "" {
					t.Errorf("metadata: %v", ref.Metadata)
				}
				if ref.Facts == nil || ref.Facts.Downloads == nil || ref.Facts.Downloads.Total == nil || *ref.Facts.Downloads.Total != 12345 || ref.Facts.Repository == nil || ref.Facts.Repository.URL != "https://plugins.svn.wordpress.org/example" {
					t.Errorf("facts: %+v", ref.Facts)
				}
				if !reflect.DeepEqual(ref.Tags, []string{"performance", "seo"}) || !reflect.DeepEqual(ref.Author, &registry.Author{Name: "Example & Dev", Username: "exampledev", URL: "https://profiles.wordpress.org/ExampleDev/"}) {
					t.Errorf("tags=%v author=%+v", ref.Tags, ref.Author)
				}
				if spec == "2025-01-01" && (ref.Facts.ReleasedAt == nil || ref.Facts.ReleasedAt.Format(time.RFC3339) != "2025-01-01T00:00:00Z" || ref.Metadata["release_date_source"] != "svn-tag-creation") {
					t.Errorf("date: %+v %v", ref.Facts, ref.Metadata)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got=%v want=%v", got, want)
			}
		})
	}
	if reports.Load() != 3 {
		t.Fatalf("expected creation history for three candidate tags, got %d", reports.Load())
	}
}

func TestVersionDepthValidationAndDefault(t *testing.T) {
	for _, spec := range []string{"0", "-1", "2025-02-30", "many"} {
		if _, err := registry.ParseVersionDepth(spec); err == nil {
			t.Errorf("accepted %q", spec)
		}
	}
	s := &Source{}
	versions, err := s.selectVersions(context.Background(), item{Version: "1", DownloadLink: "https://example.com/current.zip"})
	if err != nil || len(versions) != 1 || versions[0].Version != "1" {
		t.Fatalf("%v %v", versions, err)
	}
	for _, options := range []map[string]string{{"version_depth": "0"}, {"browse": "garbage"}} {
		if _, err := PluginFactory(nil, Plugins)(options); err == nil {
			t.Fatalf("accepted %v", options)
		}
	}
}

// Theme releases live directly under the slug in SVN, so date depth must list
// the slug collection itself and report on each version directory.
func TestThemeVersionDirectories(t *testing.T) {
	var propfind, reports []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "PROPFIND":
			propfind = append(propfind, r.URL.Path)
			w.WriteHeader(207)
			fmt.Fprint(w, `<multistatus xmlns="DAV:"><response><href>/example/</href><propstat><status>HTTP/1.1 200 OK</status><prop><creationdate>2026-01-01T00:00:00Z</creationdate><version-name>100</version-name></prop></propstat></response>`)
			for _, v := range []string{"1.1", "1.0"} {
				fmt.Fprintf(w, `<response><href>/example/%s/</href><propstat><status>HTTP/1.1 200 OK</status><prop><creationdate>2026-01-01T00:00:00Z</creationdate><version-name>100</version-name></prop></propstat></response>`, v)
			}
			fmt.Fprint(w, `</multistatus>`)
		case "REPORT":
			reports = append(reports, r.URL.Path)
			date := "2025-06-01T00:00:00Z"
			if strings.HasSuffix(r.URL.Path, "/1.0/") {
				date = "2024-01-01T00:00:00Z"
			}
			fmt.Fprintf(w, `<S:log-report xmlns:S="svn:"><S:log-item><S:date>%s</S:date></S:log-item></S:log-report>`, date)
		default:
			if r.URL.Query().Get("action") != "theme_information" {
				t.Errorf("action=%q", r.URL.Query().Get("action"))
			}
			json.NewEncoder(w).Encode(map[string]any{"slug": "example", "version": "1.1", "download_link": "http://" + r.Host + "/theme/example.1.1.zip", "versions": map[string]string{"1.0": "http://" + r.Host + "/theme/example.1.0.zip", "1.1": "http://" + r.Host + "/theme/example.1.1.zip"}})
		}
	}))
	defer server.Close()
	s := &Source{Kind: Themes, Client: server.Client(), Endpoint: server.URL, SVNEndpoint: server.URL, PageSize: 100, Slugs: []string{"example"}, VersionDepth: "2025-01-01", Attempts: 1, DownloadHosts: testHosts(server)}
	var got []string
	err := s.Discover(context.Background(), func(ref registry.Reference) error {
		if ref.Registry != "wordpress-themes" || ref.Facts == nil || ref.Facts.ReleasedAt == nil || ref.Facts.Repository.URL != "https://themes.svn.wordpress.org/example" {
			t.Errorf("reference: %+v", ref)
		}
		got = append(got, ref.Version)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"1.1"}) {
		t.Fatalf("versions=%v", got)
	}
	if !reflect.DeepEqual(propfind, []string{"/example/"}) {
		t.Fatalf("PROPFIND paths=%v", propfind)
	}
	sort.Strings(reports)
	if !reflect.DeepEqual(reports, []string{"/example/1.0/", "/example/1.1/"}) {
		t.Fatalf("REPORT paths=%v", reports)
	}
}
