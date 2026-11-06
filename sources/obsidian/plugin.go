package obsidian

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/registry"
	ghsvc "github.com/rezen/dug/sdk/services/github"
)

// PluginFactory validates query options for every Connect call. Options are
// persisted in inventories, so they carry selection and tuning, never
// credentials. The GitHub endpoint and token belong to the plugin process.
func PluginFactory(client *http.Client, endpoint, token string) plugin.Factory {
	return func(options map[string]string) (registry.Registry, error) {
		s := &Source{Client: client, Attempts: 4, GitHubEndpoint: endpoint, Token: token}
		for key, value := range options {
			switch key {
			case "ids":
				for _, id := range strings.Split(value, ",") {
					id = strings.TrimSpace(id)
					if err := validID(id); err != nil {
						return nil, err
					}
					s.IDs = append(s.IDs, id)
				}
			case "version_depth":
				if _, err := registry.ParseVersionDepth(value); err != nil {
					return nil, err
				}
				s.VersionDepth = value
			default:
				return nil, fmt.Errorf("unknown Obsidian option %q (supported: ids, version_depth)", key)
			}
		}
		if _, err := (&ghsvc.Client{Endpoint: endpoint, Token: token}).API(); err != nil {
			return nil, err
		}
		return s, nil
	}
}
