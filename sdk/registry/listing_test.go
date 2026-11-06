package registry

import (
	"reflect"
	"testing"
)

func TestNormalizeTags(t *testing.T) {
	got := NormalizeTags([]string{" Security ", "seo", "SEO", "", "go", "security"})
	if !reflect.DeepEqual(got, []string{"go", "security", "seo"}) {
		t.Fatal(got)
	}
	if NormalizeTags(nil) != nil || NormalizeTags([]string{"", "  "}) != nil {
		t.Fatal("empty input must normalize to nil")
	}
	if err := ValidateTags(got); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string][]string{"upper": {"SEO"}, "space": {" seo"}, "empty": {""}, "unsorted": {"seo", "go"}, "duplicate": {"go", "go"}} {
		if err := ValidateTags(bad); err == nil {
			t.Errorf("%s: accepted %v", name, bad)
		}
	}
	ref := Reference{Tags: got}
	if !ref.HasTag("seo") || !ref.HasTag(" SEO ") || ref.HasTag("se") || (Reference{}).HasTag("seo") {
		t.Fatal("tag membership")
	}
}

func TestAuthorValidateAndClone(t *testing.T) {
	if err := (*Author)(nil).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (&Author{}).Validate(); err == nil {
		t.Fatal("empty author accepted")
	}
	for _, a := range []Author{{Name: "n"}, {Username: "u"}, {ID: "1"}, {URL: "https://example.org/u"}, {Email: "u@example.org"}} {
		if err := a.Validate(); err != nil {
			t.Errorf("%+v: %v", a, err)
		}
	}
	original := &Author{Username: "acme", ID: "7"}
	clone := original.Clone()
	clone.Username = "other"
	if original.Username != "acme" || (*Author)(nil).Clone() != nil {
		t.Fatal("clone")
	}
}

func TestValidateListingCoversFactsTagsAndAuthor(t *testing.T) {
	ok := Reference{Facts: &Facts{Installs: Int64(1)}, Tags: []string{"a", "b"}, Author: &Author{Username: "u"}}
	if err := ok.ValidateListing(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]Reference{
		"facts":  {Facts: &Facts{Installs: Int64(-1)}},
		"tags":   {Tags: []string{"b", "a"}},
		"author": {Author: &Author{}},
		"raw":    {Raw: []byte("<xml/>")},
	} {
		if err := bad.ValidateListing(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, ok := ok.Fact("author.username"); !ok {
		t.Fatal("author path not addressable")
	}
	if _, ok := (Reference{Author: &Author{Name: "n"}}).Fact("author.username"); ok {
		t.Fatal("empty author field reported present")
	}
}
