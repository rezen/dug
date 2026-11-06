package artifacthub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fake(t *testing.T, total int) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/packages/search":
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			w.Header().Set("Pagination-Total-Count", strconv.Itoa(total))
			var items []string
			for i := offset; i < min(offset+limit, total) && i < 100; i++ {
				items = append(items, fmt.Sprintf(`{"package_id":"id%d","normalized_name":"c%d","stars":%d,"ts":1700000000,"repository":{"name":"r"}}`, i, i, i))
			}
			fmt.Fprintf(w, `{"packages":[%s]}`, strings.Join(items, ","))
		case r.URL.Path == "/api/v1/packages/helm/r/chart/1.0.0":
			fmt.Fprint(w, `{"normalized_name":"chart","version":"1.0.0","content_url":"https://x/c.tgz","digest":"ab","repository":{"name":"r","url":"https://x"}}`)
		case r.URL.Path == "/api/v1/packages/helm/r/other":
			fmt.Fprint(w, `{"normalized_name":"mismatch","repository":{"name":"r"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Base: srv.URL + "/api/v1", Attempts: 1}
}

func TestSearchPaginates(t *testing.T) {
	c := fake(t, 100)
	var got []string
	total, err := c.Search(context.Background(), KindHelm, func(p Package) error { got = append(got, p.ID); return nil })
	if err != nil || total != 100 || len(got) != 100 {
		t.Fatalf("total=%d got=%d err=%v", total, len(got), err)
	}
}

func TestSearchDetectsEarlyEnd(t *testing.T) {
	c := fake(t, 150) // fake serves at most 100 items
	_, err := c.Search(context.Background(), KindHelm, func(Package) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "ended early") {
		t.Fatalf("err = %v", err)
	}
}

func TestSearchCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fake(t, 10).Search(ctx, KindHelm, func(Package) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestPackageVersion(t *testing.T) {
	c := fake(t, 0)
	p, err := c.PackageVersion(context.Background(), "r", "chart", "1.0.0")
	if err != nil || p.ContentURL != "https://x/c.tgz" || p.Digest != "ab" {
		t.Fatalf("p=%+v err=%v", p, err)
	}
	if _, err := c.PackageVersion(context.Background(), "r", "chart", "9.9.9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.Package(context.Background(), "r", "other"); err == nil {
		t.Fatal("accepted a mismatched document")
	}
}

func TestSearchFilterAndSpacing(t *testing.T) {
	var queries []string
	var times []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		times = append(times, time.Now())
		w.Header().Set("Pagination-Total-Count", "61")
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		fmt.Fprintf(w, `{"packages":[{"package_id":"id%d","normalized_name":"c","repository":{"name":"r"}}]}`, offset)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), Base: srv.URL + "/api/v1", Attempts: 1, SearchInterval: 30 * time.Millisecond}
	total, err := c.SearchFiltered(context.Background(), KindHelm, Filter{Repos: []string{"a", "b"}, Text: "x y", Deprecated: true}, func(Package) error { return nil })
	if err != nil || total != 61 || len(queries) != 2 {
		t.Fatalf("total=%d requests=%d err=%v", total, len(queries), err)
	}
	q, _ := url.ParseQuery(queries[1])
	if strings.Join(q["repo"], ",") != "a,b" || q.Get("ts_query_web") != "x y" || q.Get("deprecated") != "true" || q.Get("offset") != "60" {
		t.Errorf("query = %v", q)
	}
	if gap := times[1].Sub(times[0]); gap < 25*time.Millisecond {
		t.Errorf("search requests %s apart, want at least the interval", gap)
	}
	if _, err := c.SearchFiltered(context.Background(), KindHelm, Filter{Repos: []string{"a&b"}}, func(Package) error { return nil }); err == nil {
		t.Error("accepted a repository name that is not a path segment")
	}
	c = &Client{HTTP: srv.Client(), Base: srv.URL + "/api/v1", Attempts: 1}
	queries = nil
	if _, err := c.Search(context.Background(), KindHelm, func(Package) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(queries[0], "deprecated") || strings.Contains(queries[0], "repo=") {
		t.Errorf("plain Search must keep the default view: %s", queries[0])
	}
}

func TestValidation(t *testing.T) {
	for _, s := range []string{"", "..", "a/b", "a?b", "a b", "../x"} {
		if ValidSegment(s) == nil {
			t.Errorf("accepted segment %q", s)
		}
	}
	for _, v := range []string{"", "1/0", "1.0 ", "1?x", "../1"} {
		if ValidVersion(v) == nil {
			t.Errorf("accepted version %q", v)
		}
	}
	if ValidSegment("my-chart_2.x") != nil || ValidVersion("1.0.0-rc.1+b5") != nil {
		t.Error("rejected valid input")
	}
}
