package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dug.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func configFlags() (*flag.FlagSet, *int, *string) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	concurrency := flags.Int("concurrency", 1, "")
	pluginURL := flags.String("plugin-url", "default", "")
	flags.String("bucket", "", "")
	flags.String("format", "table", "")
	return flags, concurrency, pluginURL
}

func TestConfigAppliesTopLevelAndRegistrySettings(t *testing.T) {
	path := writeConfig(t, `
concurrency: 8
plugin-url: http://top
options: {shared: a, ids: [x, y]}
registries:
  chrome:
    plugin-url: http://chrome
    options: {shared: b}
`)
	config, err := loadConfig([]string{"--config", path})
	if err != nil {
		t.Fatal(err)
	}
	flags, concurrency, pluginURL := configFlags()
	options, err := config.apply(flags, "sync", "chrome", false)
	if err != nil {
		t.Fatal(err)
	}
	if *concurrency != 8 || *pluginURL != "http://chrome" {
		t.Fatalf("concurrency=%d plugin-url=%q", *concurrency, *pluginURL)
	}
	if options["shared"] != "b" || options["ids"] != "x,y" {
		t.Fatalf("options = %v", options)
	}
	// Other registries only see the top-level settings.
	flags, _, pluginURL = configFlags()
	if _, err := config.apply(flags, "sync", "wordpress", false); err != nil || *pluginURL != "http://top" {
		t.Fatalf("plugin-url=%q err=%v", *pluginURL, err)
	}
}

func TestConfigCommandLineOverridesFile(t *testing.T) {
	path := writeConfig(t, "concurrency: 8\n")
	config, err := loadConfig([]string{"--config=" + path})
	if err != nil {
		t.Fatal(err)
	}
	flags, concurrency, _ := configFlags()
	if _, err := config.apply(flags, "sync", "chrome", false); err != nil {
		t.Fatal(err)
	}
	if err := flags.Parse([]string{"--concurrency", "2"}); err != nil {
		t.Fatal(err)
	}
	if *concurrency != 2 {
		t.Fatalf("concurrency = %d, want the command line value", *concurrency)
	}
}

func TestConfigSkipsInapplicableKeys(t *testing.T) {
	path := writeConfig(t, "bucket: mine\nformat: jsonl\n")
	config, err := loadConfig([]string{"--config", path})
	if err != nil {
		t.Fatal(err)
	}
	flags, _, _ := configFlags()
	if _, err := config.apply(flags, "sync", "chrome", true); err != nil {
		t.Fatal(err)
	}
	if got := flags.Lookup("bucket").Value.String(); got != "" {
		t.Fatalf("local mode bucket = %q, want it untouched", got)
	}
	if got := flags.Lookup("format").Value.String(); got != "table" {
		t.Fatalf("sync format = %q, want it untouched", got)
	}
}

func TestConfigRejectsBadFiles(t *testing.T) {
	for name, body := range map[string]string{
		"unknown key":     "bukket: x\n",
		"bad value":       "concurrency: many\n",
		"options not map": "options: [a]\n",
		"bad registries":  "registries: [a]\n",
		"nested":          "registries:\n  chrome:\n    registries: {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			config, err := loadConfig([]string{"--config", writeConfig(t, body)})
			if err != nil {
				t.Fatal(err)
			}
			flags, _, _ := configFlags()
			_, err = config.apply(flags, "sync", "chrome", false)
			var usage usageError
			if !errors.As(err, &usage) {
				t.Fatalf("err = %v, want usage error", err)
			}
		})
	}
}

func TestConfigMissingFile(t *testing.T) {
	t.Chdir(t.TempDir())
	if config, err := loadConfig(nil); config != nil || err != nil {
		t.Fatalf("default file is optional: config=%v err=%v", config, err)
	}
	if _, err := loadConfig([]string{"--config", "nope.yaml"}); err == nil {
		t.Fatal("an explicit config that is missing must fail")
	}
}
