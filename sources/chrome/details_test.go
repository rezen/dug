package chrome

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rezen/dug/sdk/registry"
)

func TestStorePageDetails(t *testing.T) {
	id := strings.Repeat("a", 32)
	record := `AF_initDataCallback({key: 'ds:0', data:[["` + id + `","https://lh3.example/icon","Fixture Blocker",4.5,3797,"https://lh3.example/img","Blocks <things>.",null,null,1,null,["make_chrome_yours/privacy",null,22],1,1,22000000,null,"https://lh3.example/x"]], sideChannel: {}});`
	text := `<a class="x" href="./category/extensions/make_chrome_yours/privacy" jslog="1">Privacy &amp; Security</a>21,000,000 users</div><span class="y">3.8K ratings</span><li><div class="QDHp8e">Offered by</div><div>Raymond Hill (gorhill)</div></li>`
	var page atomic.Value
	var hits atomic.Int32
	page.Store("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/detail/") {
			hits.Add(1)
			if r.URL.Query().Get("hl") != "en" || strings.TrimPrefix(r.URL.Path, "/detail/") != id {
				t.Errorf("store page request %s", r.URL)
			}
			switch body := page.Load().(string); body {
			case "":
				http.NotFound(w, r)
			case "fail":
				w.WriteHeader(http.StatusInternalServerError)
			default:
				io.WriteString(w, body)
			}
			return
		}
		fmt.Fprintf(w, `<gupdate><app appid="%s" status="ok"><updatecheck status="ok" version="1" codebase="http://%s/%s.crx" hash_sha256="%s" size="1"/></app></gupdate>`, id, r.Host, id, strings.Repeat("0", 64))
	}))
	defer server.Close()
	source := func() *Source {
		return &Source{Client: server.Client(), UpdateURL: server.URL, DetailURL: server.URL + "/detail/", IDs: []string{id}, Attempts: 1, DownloadHosts: []string{strings.TrimPrefix(server.URL, "http://")}}
	}
	discover := func(t *testing.T, s *Source) registry.Reference {
		t.Helper()
		var refs []registry.Reference
		if err := s.Discover(context.Background(), func(ref registry.Reference) error { refs = append(refs, ref); return nil }); err != nil {
			t.Fatal(err)
		}
		if len(refs) != 1 {
			t.Fatalf("refs=%d", len(refs))
		}
		return refs[0]
	}
	author := &registry.Author{Name: "Raymond Hill (gorhill)"}
	tags := []string{"make_chrome_yours", "privacy"}

	page.Store("<html>" + record + text + "</html>")
	ref := discover(t, source())
	if ref.Facts == nil || ref.Facts.Installs == nil || *ref.Facts.Installs != 22000000 {
		t.Fatalf("record user count not used: %+v", ref.Facts)
	}
	var raw struct {
		UpdateXML string `json:"update_xml"`
		StoreItem []any  `json:"store_item"`
	}
	if err := json.Unmarshal(ref.Raw, &raw); err != nil || !strings.Contains(raw.UpdateXML, "<gupdate>") || len(raw.StoreItem) < 3 || raw.StoreItem[2] != "Fixture Blocker" {
		t.Fatalf("raw = %s (%v)", ref.Raw, err)
	}
	want := map[string]string{"store_url": "https://chromewebstore.google.com/detail/" + id, "rating": "90", "rating_average": "4.5", "num_ratings": "3797", "title": "Fixture Blocker", "category": "Privacy & Security"}
	if !reflect.DeepEqual(ref.Metadata, want) || !reflect.DeepEqual(ref.Tags, tags) || !reflect.DeepEqual(ref.Author, author) {
		t.Fatalf("metadata=%v tags=%v author=%+v", ref.Metadata, ref.Tags, ref.Author)
	}

	page.Store("<html>" + text + "</html>")
	ref = discover(t, source())
	if err := json.Unmarshal(ref.Raw, &raw); err != nil || !strings.Contains(raw.UpdateXML, "<gupdate>") {
		t.Fatalf("raw without a store record = %s (%v)", ref.Raw, err)
	}
	if ref.Facts == nil || ref.Facts.Installs == nil || *ref.Facts.Installs != 21000000 || ref.Metadata["num_ratings"] != "3800" || ref.Metadata["rating"] != "" || ref.Metadata["title"] != "" || !reflect.DeepEqual(ref.Tags, tags) || !reflect.DeepEqual(ref.Author, author) {
		t.Fatalf("text fallback: facts=%+v metadata=%v tags=%v author=%+v", ref.Facts, ref.Metadata, ref.Tags, ref.Author)
	}

	for name, body := range map[string]string{"store shell": "<html><title>Chrome Web Store</title></html>", "missing page": ""} {
		page.Store(body)
		ref = discover(t, source())
		if ref.Facts != nil || ref.Tags != nil || ref.Author != nil || len(ref.Metadata) != 1 {
			t.Fatalf("%s: facts=%+v metadata=%v tags=%v author=%+v", name, ref.Facts, ref.Metadata, ref.Tags, ref.Author)
		}
	}

	page.Store("fail")
	if err := source().Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil {
		t.Fatal("store page server error did not stop listing")
	}

	before := hits.Load()
	s := source()
	s.SkipDetails = true
	discover(t, s)
	if hits.Load() != before {
		t.Fatal("details=false still fetched the store page")
	}
}
