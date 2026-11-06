package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// defaultConfigFile is read from the working directory when --config and
// DUG_CONFIG are not set. It is optional.
const defaultConfigFile = "dug.yaml"

// fileConfig is a parsed dug.yaml. Top-level keys are flag names (bucket,
// concurrency, plugin-url, ...), plus "options" for plugin options and
// "registries" for per-registry overrides of the same keys:
//
//	bucket: artifact-corpus
//	concurrency: 8
//	registries:
//	  chrome:
//	    plugin-url: http://127.0.0.1:7782
//	    options: {ids: abc,def}
//
// Precedence is flag default, then environment, then this file, then the
// command line.
type fileConfig struct {
	path string
	root map[string]any
}

// loadConfig reads the file named by --config, DUG_CONFIG, or ./dug.yaml. Only
// the default file may be missing.
func loadConfig(args []string) (*fileConfig, error) {
	path, explicit := configPath(args)
	if path == "" {
		path = defaultConfigFile
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !explicit {
			return nil, nil
		}
		return nil, usageError{fmt.Errorf("read config: %w", err)}
	}
	root := map[string]any{}
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, usageError{fmt.Errorf("parse %s: %w", path, err)}
	}
	return &fileConfig{path: path, root: root}, nil
}

// configPath finds --config before flag parsing, because the file supplies
// flag defaults and so has to load first.
func configPath(args []string) (path string, explicit bool) {
	for i, arg := range args {
		if arg == "--" {
			break
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if name != "config" || !strings.HasPrefix(arg, "-") {
			continue
		}
		if !hasValue && i+1 < len(args) {
			value = args[i+1]
		}
		return value, true
	}
	if value := os.Getenv("DUG_CONFIG"); value != "" {
		return value, true
	}
	return "", false
}

// apply sets flag values from the file and returns the plugin options it
// defines. Flags the command line sets afterwards win. A nil config applies
// nothing.
func (c *fileConfig) apply(flags *flag.FlagSet, command, kind string, local bool) (map[string]string, error) {
	if c == nil {
		return nil, nil
	}
	options := map[string]string{}
	layers := []map[string]any{c.root}
	if registries, ok := c.root["registries"]; ok {
		table, ok := registries.(map[string]any)
		if !ok {
			return nil, misuse("%s: registries must be a map of registry name to settings", c.path)
		}
		if section, found := table[kind]; found {
			layer, ok := section.(map[string]any)
			if !ok {
				return nil, misuse("%s: registries.%s must be a map", c.path, kind)
			}
			layers = append(layers, layer)
		}
	}
	for depth, layer := range layers {
		keys := make([]string, 0, len(layer))
		for key := range layer {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value := layer[key]
			switch key {
			case "registries":
				if depth > 0 {
					return nil, misuse("%s: registries cannot be nested", c.path)
				}
			case "options":
				table, ok := value.(map[string]any)
				if !ok {
					return nil, misuse("%s: options must be a map of key to value", c.path)
				}
				for name, item := range table {
					options[name] = scalar(item)
				}
			default:
				if err := c.setFlag(flags, key, value, command, local); err != nil {
					return nil, err
				}
			}
		}
	}
	return options, nil
}

func (c *fileConfig) setFlag(flags *flag.FlagSet, key string, value any, command string, local bool) error {
	// Local mode pins the storage connection; a configured bucket or endpoint
	// would only trip its consistency check.
	switch key {
	case "bucket", "endpoint", "region", "path-style":
		if local {
			return nil
		}
	case "format", "inventory-dir":
		if command != "list" {
			return nil
		}
	}
	if key == "config" || key == "option" || flags.Lookup(key) == nil {
		return misuse("%s: unknown setting %q", c.path, key)
	}
	if err := flags.Set(key, scalar(value)); err != nil {
		return misuse("%s: %s: %v", c.path, key, err)
	}
	return nil
}

// scalar renders a YAML value the way the same setting is typed on the command
// line; lists become comma-separated, like --option slugs=a,b.
func scalar(value any) string {
	if items, ok := value.([]any); ok {
		parts := make([]string, len(items))
		for i, item := range items {
			parts[i] = fmt.Sprint(item)
		}
		return strings.Join(parts, ",")
	}
	return fmt.Sprint(value)
}
