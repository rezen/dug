package oci

import "github.com/rezen/dug/sdk/registry"

// Provider describes the catalogs implemented by this adapter. Kind "images"
// covers container images and the other artifacts OCI registries carry.
func Provider() registry.Provider {
	return registry.Provider{
		ID:         Identity,
		Registries: []registry.Definition{{ID: Identity, Kind: "images", Stability: registry.Experimental}},
	}
}
