package registry_test

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/rezen/dug/sdk/registry"
)

// This in-memory example stands in for a registry's API. A real implementation
// pages through its catalog in Discover and opens authenticated downloads in
// Resolve's Blob.Open functions. It does not import the engine or storage.
type exampleRegistry struct{}

var _ registry.Registry = exampleRegistry{}

func (exampleRegistry) Discover(ctx context.Context, emit func(registry.Reference) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return emit(registry.Reference{Registry: "example", Name: "package", Version: "1.0"})
}

func (exampleRegistry) Resolve(ctx context.Context, ref registry.Reference) (registry.Artifact, error) {
	if err := ctx.Err(); err != nil {
		return registry.Artifact{}, err
	}
	if ref.Registry != "example" || ref.Name != "package" || ref.Version != "1.0" {
		return registry.Artifact{}, fmt.Errorf("unknown resource")
	}
	const content = "package contents"
	return registry.Artifact{
		Reference: ref,
		Revision:  ref.Version,
		Blobs: []registry.Blob{{
			Name:      "package.txt",
			MediaType: "text/plain",
			Size:      int64(len(content)),
			Open: func(transferCtx context.Context) (io.ReadCloser, error) {
				if err := transferCtx.Err(); err != nil {
					return nil, err
				}
				return io.NopCloser(strings.NewReader(content)), nil
			},
		}},
	}, nil
}

func ExampleRegistry() {
	var source registry.Registry = exampleRegistry{}
	ctx := context.Background()
	// Stage one completes before any resolution. Production callers persist
	// references with inventory.Create instead of holding them in memory.
	var entries []registry.Reference
	err := source.Discover(ctx, func(ref registry.Reference) error {
		entries = append(entries, ref)
		return nil
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	// Stage two consumes the saved entries without calling Discover again.
	process := func(ref registry.Reference) error {
		resolutionCtx, cancel := context.WithCancel(ctx)
		artifact, err := source.Resolve(resolutionCtx, ref)
		cancel() // Blob.Open must remain usable after resolution ends.
		if err != nil {
			return err
		}
		for _, blob := range artifact.Blobs {
			body, err := blob.Open(ctx)
			if err != nil {
				return err
			}
			data, readErr := io.ReadAll(body)
			closeErr := body.Close()
			if readErr != nil {
				return readErr
			}
			if closeErr != nil {
				return closeErr
			}
			fmt.Printf("%s@%s: %s\n", ref.Name, artifact.Revision, data)
		}
		return nil
	}
	for _, ref := range entries {
		if err := process(ref); err != nil {
			fmt.Println(err)
			return
		}
	}
	// Output: package@1.0: package contents
}
