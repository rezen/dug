package oci

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/services/oci"
)

// PluginFactory validates query options for every Connect call. Options are
// persisted in inventories, so they carry selection and tuning, never
// credentials; registry credentials are read from the environment in
// cmd/dug-oci and passed in here. The distribution client, and with it the
// token cache, is shared by every call the factory serves.
func PluginFactory(client *http.Client, credentials map[string]oci.Credential) plugin.Factory {
	shared := &oci.Client{HTTP: client, Attempts: 4, Credentials: credentials}
	return func(options map[string]string) (registry.Registry, error) {
		s := &Source{OCI: shared}
		endpoints := oci.Endpoints()
		for key, value := range options {
			switch key {
			case "repos":
				list, err := splitList(key, value)
				if err != nil {
					return nil, err
				}
				for _, name := range list {
					host, repo, err := oci.ParseReference(name)
					if err != nil {
						return nil, err
					}
					if _, ok := endpoints[host]; !ok {
						return nil, unsupportedRegistry(host, endpoints)
					}
					s.Repos = append(s.Repos, host+"/"+repo)
				}
			case "namespaces", "registries":
				list, err := splitList(key, value)
				if err != nil {
					return nil, err
				}
				for _, name := range list {
					host, _, _ := strings.Cut(name, "/")
					ep, ok := endpoints[host]
					if !ok {
						return nil, unsupportedRegistry(host, endpoints)
					}
					if ep.Catalog == oci.CatalogNone {
						return nil, fmt.Errorf("oci %s has no catalog API; name its repositories with the repos option", host)
					}
					if key == "namespaces" {
						if _, ns, ok := strings.Cut(name, "/"); !ok || !oci.ValidRepository(ns) || strings.Contains(ns, "/") {
							return nil, fmt.Errorf("oci namespace %q must look like HOST/NAMESPACE, for example quay.io/coreos", name)
						}
						s.Namespaces = append(s.Namespaces, name)
					} else {
						if name != host {
							return nil, fmt.Errorf("oci registries entry %q must be a bare registry name such as quay.io; use namespaces for HOST/NAMESPACE", name)
						}
						s.Registries = append(s.Registries, name)
					}
				}
			case "platform":
				if err := ValidatePlatform(value); err != nil {
					return nil, err
				}
				s.Platform = value
			case "max_layer_size":
				n, err := strconv.ParseInt(value, 10, 64)
				if err != nil || n < 1 {
					return nil, fmt.Errorf("oci max_layer_size must be a positive number of bytes")
				}
				s.MaxLayerSize = n
			case "max_tags":
				n, err := strconv.Atoi(value)
				if err != nil || n < 1 || n > 100000 {
					return nil, fmt.Errorf("oci max_tags must be 1–100000")
				}
				s.MaxTags = n
			case "version_depth":
				if _, err := registry.ParseVersionDepth(value); err != nil {
					return nil, err
				}
				s.VersionDepth = value
			default:
				return nil, fmt.Errorf("unknown OCI option %q (supported: repos, namespaces, registries, platform, max_layer_size, max_tags, version_depth)", key)
			}
		}
		if len(s.Registries) > 0 && (len(s.Repos) > 0 || len(s.Namespaces) > 0) {
			return nil, fmt.Errorf("oci registries cannot be combined with repos or namespaces")
		}
		return s, nil
	}
}

func splitList(key, value string) ([]string, error) {
	var out []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("oci %s contains an empty entry", key)
		}
		out = append(out, item)
	}
	return out, nil
}

func unsupportedRegistry(host string, endpoints map[string]oci.Endpoint) error {
	names := make([]string, 0, len(endpoints))
	for name := range endpoints {
		names = append(names, name)
	}
	return fmt.Errorf("oci registry %q is not supported (supported: %s)", host, strings.Join(sortedStrings(names), ", "))
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
