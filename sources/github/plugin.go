package github

import (
	"fmt"
	ghsvc "github.com/rezen/dug/sdk/services/github"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/registry"
)

// PluginFactory validates query options for each call. The API endpoint and
// token belong to the plugin process and never travel as options.
func PluginFactory(client *http.Client, endpoint, token string) plugin.Factory {
	return func(options map[string]string) (registry.Registry, error) {
		s := &Source{Client: client, Endpoint: endpoint, Token: token, Attempts: 4, Assets: true, Snapshot: SnapshotFallback}
		for key, value := range options {
			switch key {
			case "repos":
				for _, full := range strings.Split(value, ",") {
					full = strings.TrimSpace(full)
					if _, _, err := ghsvc.ParseRepo(full); err != nil {
						return nil, err
					}
					s.Repos = append(s.Repos, full)
				}
			case "owner":
				if !ghsvc.ValidOwner(value) {
					return nil, fmt.Errorf("invalid GitHub owner %q", value)
				}
				s.Owner = value
			case "search":
				if strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > 256 {
					return nil, fmt.Errorf("github search must be 1–256 characters")
				}
				if strings.Contains(strings.ToLower(value), "created:") {
					return nil, fmt.Errorf("github search must not contain created:; discovery partitions creation dates itself")
				}
				s.Search = value
			case "forks", "assets", "tags":
				enabled, err := strconv.ParseBool(value)
				if err != nil {
					return nil, fmt.Errorf("github %s must be true or false", key)
				}
				switch key {
				case "forks":
					s.Forks = enabled
				case "assets":
					s.Assets = enabled
				default:
					s.Tags = enabled
				}
			case "snapshot":
				switch value {
				case SnapshotFallback, SnapshotAlways, SnapshotOnly, SnapshotNever:
					s.Snapshot = value
				default:
					return nil, fmt.Errorf("github snapshot must be fallback, always, only, or never")
				}
			case "refs":
				for _, ref := range strings.Split(value, ",") {
					ref = strings.TrimSpace(ref)
					if err := validRef(ref); err != nil {
						return nil, err
					}
					s.Refs = append(s.Refs, ref)
				}
			case "version_depth":
				if _, err := registry.ParseVersionDepth(value); err != nil {
					return nil, err
				}
				s.VersionDepth = value
			default:
				return nil, fmt.Errorf("unknown GitHub option %q (supported: repos, owner, search, forks, assets, snapshot, refs, tags, version_depth)", key)
			}
		}
		if err := s.validate(); err != nil {
			return nil, err
		}
		return s, nil
	}
}
