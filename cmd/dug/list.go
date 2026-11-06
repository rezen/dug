package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/rezen/dug/inventory"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/services/storage"
)

// listOutput streams rows without retaining an unbounded catalog in memory.
type listOutput struct {
	format         string
	stdout, stderr io.Writer
	header         bool
	provider       registry.Provider
}

func (output *listOutput) entry(ref registry.Reference) error {
	definition, _ := output.provider.Registry(ref.Registry)
	switch output.format {
	case "summary":
		return nil
	case "jsonl":
		return json.NewEncoder(output.stdout).Encode(struct {
			registry.Reference
			Provider string `json:"provider,omitempty"`
			Kind     string `json:"kind,omitempty"`
		}{ref, output.provider.ID, definition.Kind})
	default:
		if !output.header {
			if err := output.row("NAME", "VERSION", "PROVIDER", "KIND", "REGISTRY"); err != nil {
				return err
			}
			output.header = true
		}
		provider, kind := output.provider.ID, definition.Kind
		if provider == "" {
			provider = "-"
		}
		if kind == "" {
			kind = "-"
		}
		return output.row(ref.Name, ref.Version, provider, kind, ref.Registry)
	}
}

func (output *listOutput) row(name, version, provider, kind, registry string) error {
	// Keep upstream control characters from changing terminal output.
	field := func(value string) string {
		quoted := strconv.Quote(value)
		return quoted[1 : len(quoted)-1]
	}
	// Names can be arbitrarily long; put them last so they cannot shift the
	// provider/kind columns that distinguish catalogs.
	_, err := fmt.Fprintf(output.stdout, "%-12s %-12s %-20s %-16s %s\n", field(provider), field(kind), field(registry), field(version), field(name))
	return err
}

func (output *listOutput) finish(summary inventory.Summary, opts storage.S3Options) error {
	if output.format == "summary" {
		return json.NewEncoder(output.stdout).Encode(summary)
	}
	key := summary.Key
	if prefix := strings.Trim(opts.Prefix, "/"); prefix != "" {
		key = prefix + "/" + key
	}
	_, err := fmt.Fprintf(output.stderr, "Listed %d entries.\nInventory file: %s\nS3 inventory: s3://%s/%s\n", summary.Entries, summary.LocalPath, opts.Bucket, key)
	return err
}
