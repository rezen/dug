package helm

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/registry"
)

// PluginFactory validates query options for every Connect call. Options are
// persisted in inventories, so they carry selection and tuning, never
// credentials; Artifact Hub needs none.
func PluginFactory(client *http.Client) plugin.Factory {
	return func(options map[string]string) (registry.Registry, error) {
		s := &Source{Client: client, Attempts: 4}
		for key, value := range options {
			switch key {
			case "charts":
				for _, name := range strings.Split(value, ",") {
					name = strings.TrimSpace(name)
					if _, _, err := splitChart(name); err != nil {
						return nil, err
					}
					s.Charts = append(s.Charts, name)
				}
			case "repos":
				for _, name := range strings.Split(value, ",") {
					name = strings.TrimSpace(name)
					if err := validPart("repository", name); err != nil {
						return nil, err
					}
					s.Repos = append(s.Repos, name)
				}
			case "search":
				if value == "" || utf8.RuneCountInString(value) > 100 {
					return nil, fmt.Errorf("helm search must be 1–100 characters")
				}
				s.Search = value
			case "version_depth":
				if _, err := registry.ParseVersionDepth(value); err != nil {
					return nil, err
				}
				s.VersionDepth = value
			case "listing_concurrency":
				n, err := strconv.Atoi(value)
				if err != nil || n < 1 || n > maxConcurrency {
					return nil, fmt.Errorf("helm listing_concurrency must be 1–%d", maxConcurrency)
				}
				s.Concurrency = n
			default:
				return nil, fmt.Errorf("unknown Helm option %q (supported: charts, repos, search, version_depth, listing_concurrency)", key)
			}
		}
		if len(s.Charts) > 0 && (len(s.Repos) > 0 || s.Search != "") {
			return nil, fmt.Errorf("use charts, or repos and search, not both")
		}
		return s, nil
	}
}
