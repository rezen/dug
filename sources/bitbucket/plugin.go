package bitbucket

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
// credentials; the access token is read from the environment in
// cmd/dug-bitbucket and passed in here.
func PluginFactory(client *http.Client, token string) plugin.Factory {
	return func(options map[string]string) (registry.Registry, error) {
		s := &Source{Client: client, Token: token, Attempts: 4, Snapshot: SnapshotFallback}
		for key, value := range options {
			switch key {
			case "workspaces":
				for _, workspace := range strings.Split(value, ",") {
					workspace = strings.TrimSpace(workspace)
					if !validSlug(workspace) {
						return nil, fmt.Errorf("invalid Bitbucket workspace %q", workspace)
					}
					s.Workspaces = append(s.Workspaces, workspace)
				}
			case "repos":
				for _, full := range strings.Split(value, ",") {
					full = strings.TrimSpace(full)
					if _, _, err := ParseRepo(full); err != nil {
						return nil, err
					}
					s.Repos = append(s.Repos, full)
				}
			case "forks":
				enabled, err := strconv.ParseBool(value)
				if err != nil {
					return nil, fmt.Errorf("bitbucket forks must be true or false")
				}
				s.Forks = enabled
			case "snapshot":
				switch value {
				case SnapshotFallback, SnapshotAlways, SnapshotOnly, SnapshotNever:
					s.Snapshot = value
				default:
					return nil, fmt.Errorf("bitbucket snapshot must be fallback, always, only, or never")
				}
			case "version_depth":
				if _, err := registry.ParseVersionDepth(value); err != nil {
					return nil, err
				}
				s.VersionDepth = value
			default:
				return nil, fmt.Errorf("unknown Bitbucket option %q (supported: workspaces, repos, forks, snapshot, version_depth)", key)
			}
		}
		if err := s.validate(); err != nil {
			return nil, err
		}
		return s, nil
	}
}
