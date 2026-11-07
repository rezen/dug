package firefox

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/registry"
)

func validIdentifier(value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\?# \t\n\r") {
		return fmt.Errorf("invalid Firefox add-on identifier %q", value)
	}
	return nil
}

func PluginFactory(client *http.Client) plugin.Factory {
	return func(options map[string]string) (registry.Registry, error) {
		s := &Source{Client: client, PageSize: 50, Attempts: 4}
		for key, value := range options {
			switch key {
			case "slugs":
				for _, slug := range strings.Split(value, ",") {
					slug = strings.TrimSpace(slug)
					if err := validIdentifier(slug); err != nil {
						return nil, err
					}
					s.Slugs = append(s.Slugs, slug)
				}
			case "search":
				if utf8.RuneCountInString(value) > 100 {
					return nil, fmt.Errorf("firefox search is limited to 100 characters")
				}
				s.Search = value
			case "page_size":
				n, err := strconv.Atoi(value)
				if err != nil || n < 1 || n > 50 {
					return nil, fmt.Errorf("firefox page_size must be 1–50")
				}
				s.PageSize = n
			case "version_depth":
				if _, err := registry.ParseVersionDepth(value); err != nil {
					return nil, err
				}
				s.VersionDepth = value
			case "listing_concurrency":
				n, err := strconv.Atoi(value)
				if err != nil || n < 1 || n > 16 {
					return nil, fmt.Errorf("firefox listing_concurrency must be 1–16")
				}
				s.Concurrency = n
			default:
				return nil, fmt.Errorf("unknown Firefox option %q (supported: slugs, search, page_size, version_depth, listing_concurrency)", key)
			}
		}
		if len(s.Slugs) > 0 && s.Search != "" {
			return nil, fmt.Errorf("use search or slugs, not both")
		}
		return s, nil
	}
}
