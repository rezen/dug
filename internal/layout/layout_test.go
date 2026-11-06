package layout

import (
	"strings"
	"testing"
	"time"

	"github.com/rezen/dug/sdk/registry"
)

func TestArtifactDir(t *testing.T) {
	for _, c := range []struct {
		ref  registry.Reference
		want string
	}{
		{registry.Reference{Registry: "wordpress", Name: "akismet", Version: "5.3"}, "wordpress/plugins/artifacts/akismet/5.3"},
		{registry.Reference{Registry: "wordpress-themes", Name: "astra", Version: "4"}, "wordpress/themes/artifacts/astra/4"},
		{registry.Reference{Registry: "github", Name: "cli/cli", Version: "v1/rc"}, "github/repos/artifacts/cli/cli/v1%2Frc"},
		{registry.Reference{Registry: "unlisted", Name: "x", Version: "1"}, "unlisted/packages/artifacts/x/1"},
	} {
		got, err := ArtifactDir(c.ref)
		if err != nil || got != c.want {
			t.Errorf("%+v: got %q, %v; want %q", c.ref, got, err, c.want)
		}
	}
	for _, ref := range []registry.Reference{
		{Registry: "wordpress", Name: "a/../b", Version: "1"},
		{Registry: "wordpress", Name: "a//b", Version: "1"},
		{Registry: "wordpress", Name: "a", Version: ".."},
		{Registry: "wordpress", Name: "", Version: "1"},
		{Registry: "..", Name: "a", Version: "1"},
	} {
		if got, err := ArtifactDir(ref); err == nil {
			t.Errorf("%+v accepted as %q", ref, got)
		}
	}
}

func TestInventoryKeyRoundTrip(t *testing.T) {
	id, err := NewULID(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	key, err := InventoryKey("wordpress-themes", id)
	if err != nil || key != "wordpress/themes/inventories/"+id {
		t.Fatalf("%q %v", key, err)
	}
	dir, got, ok := ParseInventoryKey(key)
	if !ok || dir != "wordpress/themes" || got != id {
		t.Fatalf("%q %q %v", dir, got, ok)
	}
	const valid = "01JC8ZQ4M0AAAAAAAAAAAAAAAA"
	for _, bad := range []string{"inventories/" + valid, "../x/inventories/" + valid, "a/b/c/inventories/" + valid, "a/b/inventories/" + valid + ".jsonl", "a/b/inventories/" + valid + "/part-000001.jsonl", "a/b/inventories/sha256/ab.jsonl", "a/%2e%2e/inventories/" + valid, "a/b/inventories/" + strings.ToLower(valid)} {
		if _, _, ok := ParseInventoryKey(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestULIDsSortByTime(t *testing.T) {
	base := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var previous string
	for i := 0; i < 5; i++ {
		id, err := NewULID(base.Add(time.Duration(i) * time.Millisecond))
		if err != nil || !ValidULID(id) {
			t.Fatalf("%q %v", id, err)
		}
		if id <= previous {
			t.Fatalf("%q does not sort after %q", id, previous)
		}
		previous = id
	}
	// Known vector: the timestamp of 2026-11-06T12:00:00Z encodes in the first 10 characters.
	first, _ := NewULID(base)
	again, _ := NewULID(base)
	if first[:10] != again[:10] || first == again {
		t.Fatalf("timestamp prefix or randomness wrong: %s %s", first, again)
	}
}

func TestLastRunPrefix(t *testing.T) {
	if p, err := LastRunPrefix("wordpress"); err != nil || p != "wordpress/plugins/last_run/" {
		t.Fatalf("%q %v", p, err)
	}
}
