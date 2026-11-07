package joomla

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/registry"
)

// PluginFactory validates query options for every Connect call. Options are
// persisted in inventories, so they carry selection and tuning, never
// credentials; the Install from Web catalog needs none.
func PluginFactory(client *http.Client) plugin.Factory {
	return func(options map[string]string) (registry.Registry, error) {
		s := &Source{Client: client, Attempts: 4}
		for key, value := range options {
			switch key {
			case "slugs":
				list, err := splitList("slugs", value)
				if err != nil {
					return nil, err
				}
				for _, slug := range list {
					if err := validSlug(slug); err != nil {
						return nil, err
					}
				}
				s.Slugs = list
			case "ids":
				list, err := splitList("ids", value)
				if err != nil {
					return nil, err
				}
				for _, id := range list {
					if err := validID("extension ID", id); err != nil {
						return nil, err
					}
				}
				s.IDs = list
			case "categories":
				list, err := splitList("categories", value)
				if err != nil {
					return nil, err
				}
				for _, id := range list {
					if err := validID("category ID", id); err != nil {
						return nil, err
					}
				}
				s.Categories = list
			case "version_depth":
				depth, err := registry.ParseVersionDepth(value)
				if err != nil {
					return nil, err
				}
				if !depth.Since.IsZero() {
					return nil, errDateDepth
				}
				s.VersionDepth = value
			case "listing_concurrency":
				n, err := strconv.Atoi(value)
				if err != nil || n < 1 || n > 16 {
					return nil, fmt.Errorf("joomla listing_concurrency must be 1–16")
				}
				s.Concurrency = n
			default:
				return nil, fmt.Errorf("unknown Joomla option %q (supported: slugs, ids, categories, version_depth, listing_concurrency)", key)
			}
		}
		if len(s.IDs) > 0 && (len(s.Slugs) > 0 || len(s.Categories) > 0) {
			return nil, fmt.Errorf("use joomla ids alone, or slugs and categories together, not both")
		}
		return s, nil
	}
}

// splitList parses a comma-separated option into trimmed, non-empty values.
func splitList(name, value string) ([]string, error) {
	var out []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("joomla %s has an empty entry", name)
		}
		out = append(out, item)
	}
	return out, nil
}
