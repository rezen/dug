package jenkins

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/registry"
)

// PluginFactory validates query options for every Connect call. Options are
// persisted in inventories, so they carry selection and tuning, never
// credentials. The update center is anonymous, so there are none to pass in.
func PluginFactory(client *http.Client) plugin.Factory {
	return func(options map[string]string) (registry.Registry, error) {
		s := &Source{Client: client, Attempts: 4}
		for key, value := range options {
			switch key {
			case "plugins":
				for _, name := range strings.Split(value, ",") {
					name = strings.TrimSpace(name)
					if err := validName(name); err != nil {
						return nil, err
					}
					s.Plugins = append(s.Plugins, name)
				}
			case "version_depth":
				if _, err := registry.ParseVersionDepth(value); err != nil {
					return nil, err
				}
				s.VersionDepth = value
			default:
				return nil, fmt.Errorf("unknown Jenkins option %q (supported: plugins, version_depth)", key)
			}
		}
		return s, nil
	}
}
