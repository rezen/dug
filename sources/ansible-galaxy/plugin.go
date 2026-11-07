package ansiblegalaxy

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
// credentials; Galaxy needs none for public collections.
func PluginFactory(client *http.Client) plugin.Factory {
	return func(options map[string]string) (registry.Registry, error) {
		s := &Source{Client: client, Attempts: 4}
		for key, value := range options {
			switch key {
			case "collections":
				for _, fqcn := range strings.Split(value, ",") {
					fqcn = strings.TrimSpace(fqcn)
					if _, _, err := splitName(fqcn); err != nil {
						return nil, err
					}
					s.Collections = append(s.Collections, fqcn)
				}
			case "namespaces":
				for _, namespace := range strings.Split(value, ",") {
					namespace = strings.TrimSpace(namespace)
					if err := validPart(namespace); err != nil {
						return nil, err
					}
					s.Namespaces = append(s.Namespaces, namespace)
				}
			case "page_size":
				n, err := strconv.Atoi(value)
				if err != nil || n < 1 || n > maxPageSize {
					return nil, fmt.Errorf("ansible-galaxy page_size must be 1–%d", maxPageSize)
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
					return nil, fmt.Errorf("ansible-galaxy listing_concurrency must be 1–16")
				}
				s.Concurrency = n
			default:
				return nil, fmt.Errorf("unknown Ansible Galaxy option %q (supported: collections, namespaces, page_size, version_depth, listing_concurrency)", key)
			}
		}
		if len(s.Collections) > 0 && len(s.Namespaces) > 0 {
			return nil, fmt.Errorf("ansible-galaxy collections and namespaces are mutually exclusive")
		}
		return s, nil
	}
}
