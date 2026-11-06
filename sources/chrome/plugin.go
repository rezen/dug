package chrome

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/registry"
)

func PluginFactory(client *http.Client) plugin.Factory {
	return func(options map[string]string) (registry.Registry, error) {
		s := &Source{Client: client, Attempts: 4, ListingConcurrency: 8, BrowserVersion: DefaultBrowserVersion}
		for key, value := range options {
			switch key {
			case "ids":
				for _, id := range strings.Split(value, ",") {
					id = strings.TrimSpace(id)
					if !idPattern.MatchString(id) {
						return nil, fmt.Errorf("invalid Chrome extension ID %q", id)
					}
					s.IDs = append(s.IDs, id)
				}
			case "browser_version":
				if !versionPattern.MatchString(value) {
					return nil, fmt.Errorf("invalid browser_version")
				}
				s.BrowserVersion = value
			case "listing_concurrency":
				n, err := strconv.Atoi(value)
				if err != nil || n < 1 || n > 128 {
					return nil, fmt.Errorf("listing_concurrency must be 1–128")
				}
				s.ListingConcurrency = n
			case "details":
				enabled, err := strconv.ParseBool(value)
				if err != nil {
					return nil, fmt.Errorf("details must be true or false")
				}
				s.SkipDetails = !enabled
			default:
				return nil, fmt.Errorf("unknown Chrome option %q (supported: ids, browser_version, listing_concurrency, details)", key)
			}
		}
		return s, nil
	}
}
