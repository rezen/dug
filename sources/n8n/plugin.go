package n8n

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/services/npm"
)

var keywordPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)

// PluginFactory validates query options for every Connect call. Options are
// persisted in inventories, so they carry selection and tuning, never
// credentials. n8n needs none: npm and the keyword index are public.
func PluginFactory(client *http.Client) plugin.Factory {
	return func(options map[string]string) (registry.Registry, error) {
		s := &Source{Client: client, Attempts: 4}
		_, hasKeyword := options["keyword"]
		_, hasEnumerate := options["enumerate"]
		for key, value := range options {
			switch key {
			case "packages":
				for _, name := range strings.Split(value, ",") {
					name = strings.TrimSpace(name)
					if err := npm.ValidName(name); err != nil {
						return nil, err
					}
					s.Packages = append(s.Packages, name)
				}
			case "keyword":
				if !keywordPattern.MatchString(value) {
					return nil, fmt.Errorf("n8n keyword must be lowercase letters, digits, dot, underscore or hyphen, at most 100 characters")
				}
				s.Keyword = value
			case "enumerate":
				switch value {
				case EnumerateAll, EnumerateSearch, EnumerateEcosystems:
					s.Enumerate = value
				default:
					return nil, fmt.Errorf("n8n enumerate must be %s, %s or %s", EnumerateAll, EnumerateSearch, EnumerateEcosystems)
				}
			case "version_depth":
				if _, err := registry.ParseVersionDepth(value); err != nil {
					return nil, err
				}
				s.VersionDepth = value
			default:
				return nil, fmt.Errorf("unknown n8n option %q (supported: packages, keyword, enumerate, version_depth)", key)
			}
		}
		if len(s.Packages) > 0 && (hasKeyword || hasEnumerate) {
			return nil, fmt.Errorf("use packages, or keyword and enumerate, not both")
		}
		return s, nil
	}
}
