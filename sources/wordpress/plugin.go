package wordpress

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	registryplugin "github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/registry"
)

// PluginFactory exposes the query options of one WordPress.org directory
// without coupling the host CLI to this adapter. Upstream credentials and HTTP
// transport stay in the plugin process.
func PluginFactory(client *http.Client, kind Kind) registryplugin.Factory {
	return func(options map[string]string) (registry.Registry, error) {
		dir, err := kind.directory()
		if err != nil {
			return nil, err
		}
		source := &Source{Client: client, Kind: kind, PageSize: 100, Attempts: 4}
		for key, value := range options {
			switch key {
			case "version_depth":
				if _, err := registry.ParseVersionDepth(value); err != nil {
					return nil, err
				}
				source.VersionDepth = value
			case "browse":
				if !slices.Contains(dir.browse, value) {
					return nil, fmt.Errorf("browse must be one of %s", strings.Join(dir.browse, ", "))
				}
				source.Browse = value
			case "search":
				source.Search = value
			case "slugs":
				for _, slug := range strings.Split(value, ",") {
					slug = strings.TrimSpace(slug)
					if slug == "" {
						return nil, fmt.Errorf("slugs must contain nonempty comma-separated %s slugs", dir.item)
					}
					source.Slugs = append(source.Slugs, slug)
				}
			case "page_size":
				pageSize, err := strconv.Atoi(value)
				if err != nil || pageSize < 1 || pageSize > 250 {
					return nil, fmt.Errorf("page_size must be between 1 and 250")
				}
				source.PageSize = pageSize
			default:
				return nil, fmt.Errorf("unknown WordPress option %q (supported: search, slugs, page_size, browse, version_depth)", key)
			}
		}
		if len(source.Slugs) > 0 && source.Search != "" {
			return nil, fmt.Errorf("use search or slugs, not both")
		}
		return source, nil
	}
}
