package wordpress

import "testing"

func TestPluginOptions(t *testing.T) {
	factory := PluginFactory(nil, Plugins)
	for _, options := range []map[string]string{{"unknown": "x"}, {"page_size": "251"}, {"page_size": "bad"}, {"slugs": "a,"}, {"search": "a", "slugs": "b"}, {"browse": "featured"}} {
		if _, err := factory(options); err == nil {
			t.Fatalf("accepted invalid options %v", options)
		}
	}
	for _, options := range []map[string]string{nil, {"search": "shop"}, {"slugs": "akismet, wordpress-seo", "page_size": "25"}, {"browse": "popular"}} {
		if _, err := factory(options); err != nil {
			t.Fatalf("rejected options %v: %v", options, err)
		}
	}
}

func TestThemeOptions(t *testing.T) {
	factory := PluginFactory(nil, Themes)
	for _, options := range []map[string]string{{"unknown": "x"}, {"browse": "garbage"}, {"slugs": ""}} {
		if _, err := factory(options); err == nil {
			t.Fatalf("accepted invalid options %v", options)
		}
	}
	for _, options := range []map[string]string{nil, {"browse": "featured"}, {"slugs": "twentytwentyfour,astra", "version_depth": "unlimited"}} {
		source, err := factory(options)
		if err != nil {
			t.Fatalf("rejected options %v: %v", options, err)
		}
		if source.(*Source).Kind != Themes {
			t.Fatalf("factory lost the directory: %+v", source)
		}
	}
	if _, err := PluginFactory(nil, Kind("widgets"))(nil); err == nil {
		t.Fatal("accepted an unknown directory")
	}
	if Plugins.Registry() != "wordpress" || Themes.Registry() != "wordpress-themes" || Kind("").Registry() != "wordpress" {
		t.Fatalf("identities: %q %q %q", Plugins.Registry(), Themes.Registry(), Kind("").Registry())
	}
}
