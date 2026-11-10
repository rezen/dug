// dug-oci runs the OCI registries source as an independent Connect plugin.
package main

import (
	"net/http"
	"os"

	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/services/oci"
	"github.com/rezen/dug/sdk/services/pluginhost"
	source "github.com/rezen/dug/sources/oci"
)

// credentials reads optional registry logins from the environment. Anonymous
// access works everywhere; a login mainly raises Docker Hub's hourly pull
// allowance. They never travel in options, inventories or references.
func credentials() map[string]oci.Credential {
	out := map[string]oci.Credential{}
	for registry, vars := range map[string][2]string{
		"docker.io": {"DOCKER_HUB_USERNAME", "DOCKER_HUB_TOKEN"},
		"quay.io":   {"QUAY_USERNAME", "QUAY_TOKEN"},
		"ghcr.io":   {"GHCR_USERNAME", "GHCR_TOKEN"},
	} {
		if secret := os.Getenv(vars[1]); secret != "" {
			out[registry] = oci.Credential{Username: os.Getenv(vars[0]), Secret: secret}
		}
	}
	return out
}

func main() {
	creds := credentials()
	pluginhost.Main(pluginhost.Command{
		Provider:       source.Provider(),
		Registry:       source.Identity,
		DefaultAddress: "127.0.0.1:7791",
		Factory: func(client *http.Client) plugin.Factory {
			return source.PluginFactory(client, creds)
		},
	})
}
