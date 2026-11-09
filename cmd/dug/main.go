package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/rezen/dug/engine"
	"github.com/rezen/dug/internal/locals3"
	"github.com/rezen/dug/inventory"
	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/services"
	"github.com/rezen/dug/services/storage"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := execute(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "dug:", err)
		var misuse usageError
		if errors.As(err, &misuse) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// usageError marks caller mistakes such as bad flags or invalid combinations.
// main exits with status 2 for them, so scripts can tell misuse from a run
// that started and failed (status 1).
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

func misuse(format string, args ...any) error { return usageError{fmt.Errorf(format, args...)} }

const usage = `dug — archive registry resources using Connect plugins

Usage:
  dug local COMMAND REGISTRY [flags]  (automatically start a local rclone S3 server)
  dug list REGISTRY --bucket BUCKET [flags]
  dug download REGISTRY --bucket BUCKET --inventory KEY --order ORDER [flags]
  dug sync REGISTRY --bucket BUCKET --order ORDER [flags]

Stage 1 (list): print entries; save the inventory locally and in S3.
Stage 2 (download): sort that inventory and download without listing again.
sync runs both stages with a completion barrier between them.
sync reuses the last completed inventory for the same registry, --option values
and --list-limit; --rediscover (or --inventory KEY) overrides. With no prior run
it lists first. Listing uploads the inventory in parts as it runs and commits it
with complete.json; if it is interrupted, download --inventory KEY --allow-partial
uses the saved parts. list and sync record that last run in S3 under <registry>/<type>/last_run/.

List output: --format table (default), jsonl (references), or summary (JSON).
Local inventories: --inventory-dir .local/inventories (default).

local wraps any command with automatic local rclone S3 startup and bucket creation.
Requires Docker running; defaults to bucket dug-e2e at http://127.0.0.1:9000.
It uses local credentials and data/logs in the working directory.
The registry plugin must be running separately.

Orders: discovery (default), name, updated, popular, downloads, rating, released,
        a fact path (downloads.total, downloads.week, version_downloads.total,
        installs, released_at, updated_at, repository.url), metadata:KEY, number:KEY.
Prefix '-' for descending order. Concurrent workers finish independently.

Download selection (package-wide downloads): --where 'downloads >= 100000 AND installs >= 10000'
Filters take the same fact paths plus author (author.username, .name, .id,
.url), tag (= or != membership), rating, num_ratings, name, version, registry,
namespace, metadata:KEY, number:KEY and date:KEY.
downloads means package-wide totals; version_downloads.total is version-specific.
--where filters download/sync, not list; list --limit keeps discovery entries.
sync applies --limit after filtering and ordering; --list-limit bounds discovery.
Plugin history: --version-depth 3 | 2025-01-01 | unlimited (list/sync).

WordPress plugins: dug-wordpress (default URL http://127.0.0.1:7781)
  --option search=TEXT
  --option slugs=akismet,wordpress-seo
  --option page_size=100
  --option browse=popular

WordPress themes: dug-wordpress-themes (use --plugin-url http://127.0.0.1:7785)
  --option slugs=astra,hello-elementor
  --option browse=popular

Chrome: dug-chrome (use --plugin-url http://127.0.0.1:7782)
  --option ids=EXTENSION_ID,EXTENSION_ID
  --option listing_concurrency=8
  --option browser_version=150.0.0.0
  --option details=false   (skip store pages: no installs, rating, tags, author)

Firefox: dug-firefox (use --plugin-url http://127.0.0.1:7783)
  --option slugs=ublock-origin,darkreader
  --option search=TEXT
  --option page_size=50

GitHub: dug-github (use --plugin-url http://127.0.0.1:7784; set GITHUB_TOKEN)
  --option repos=OWNER/NAME,OWNER/NAME
  --option owner=ORG_OR_USER
  --option search=QUERY
  --option snapshot=fallback|always|only|never
  --option refs=v4,main
  --option tags=true

n8n community nodes: dug-n8n (use --plugin-url http://127.0.0.1:7786)
  --option packages=n8n-nodes-evolution-api,@elevenlabs/n8n-nodes-elevenlabs
  --option keyword=n8n-community-node-package
  --option enumerate=all|search|ecosystems

Ansible Galaxy: dug-ansible-galaxy (use --plugin-url http://127.0.0.1:7787)
  --option collections=community.general,amazon.aws
  --option namespaces=community,ansible
  --option page_size=100

Obsidian: dug-obsidian (use --plugin-url http://127.0.0.1:7788)
  --option ids=ID,ID        Obsidian plugin ids (default: the whole community catalog)

Helm charts: dug-helm (use --plugin-url http://127.0.0.1:7789)
  --option charts=prometheus-community/kube-prometheus-stack,argo/argo-cd
  --option repos=prometheus-community,argo
  --option search=TEXT

Joomla: dug-joomla (use --plugin-url http://127.0.0.1:7790)
  --option slugs=akeeba-backup,admin-tools
  --option ids=80,1737
  --option categories=23,20

Events (list/sync/download): discovered, downloaded, skipped and failed events are
queued and delivered in batches by background subscribers, so they never slow the run.
  --events-db FILE          record events in SQLite (table "events")
  --events-webhook URL      POST {"events": [...]} batches; set DUG_EVENTS_WEBHOOK_SECRET
                            to sign bodies in the X-Dug-Signature header (HMAC-SHA256)
If a subscriber falls more than --events-buffer events behind, events are dropped
and counted in a final warning; S3 remains the durable record.

Run a command with --help for transfer and storage options.
`

type optionsFlag map[string]string

func (s *optionsFlag) String() string { return fmt.Sprint(map[string]string(*s)) }
func (s *optionsFlag) Set(value string) error {
	key, value, ok := strings.Cut(value, "=")
	if !ok || key == "" {
		return fmt.Errorf("plugin options must use key=value")
	}
	if *s == nil {
		*s = make(map[string]string)
	}
	if _, exists := (*s)[key]; exists {
		return fmt.Errorf("duplicate plugin option %q", key)
	}
	(*s)[key] = value
	return nil
}

func execute(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return executeWithStore(ctx, args, stdout, stderr, func(ctx context.Context, opts storage.S3Options) (storage.ReadableStore, error) {
		if len(args) > 0 && args[0] == "local" {
			return locals3.NewStore(ctx, opts, stderr)
		}
		return storage.NewS3(ctx, opts)
	})
}

func executeWithStore(ctx context.Context, args []string, stdout, stderr io.Writer, newStore func(context.Context, storage.S3Options) (storage.ReadableStore, error)) error {
	local := len(args) > 0 && args[0] == "local"
	if local {
		args = args[1:]
	}
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" || args[0] == "-h" {
		_, err := io.WriteString(stdout, usage)
		return err
	}
	if len(args) < 2 || (args[0] != "sync" && args[0] != "list" && args[0] != "download") {
		return misuse("expected 'list REGISTRY', 'download REGISTRY' or 'sync REGISTRY'; run dug --help")
	}
	command, kind := args[0], args[1]
	flags := flag.NewFlagSet(command+" "+kind, flag.ContinueOnError)
	flags.SetOutput(stderr)
	opts := engine.DefaultOptions()
	flags.IntVar(&opts.Concurrency, "concurrency", opts.Concurrency, "Concurrent resource workers")
	flags.IntVar(&opts.Downloads, "downloads", opts.Downloads, "Global concurrent blob transfers (download + upload)")
	flags.IntVar(&opts.QueueSize, "queue-size", opts.QueueSize, "Bounded ordered download queue size")
	flags.IntVar(&opts.Attempts, "attempts", opts.Attempts, "Attempts per transfer/resolution and HTTP request")
	flags.DurationVar(&opts.Timeout, "timeout", opts.Timeout, "Deadline per resolution or blob transfer attempt")
	flags.Int64Var(&opts.MaxBlobBytes, "max-blob-bytes", opts.MaxBlobBytes, "Maximum size of one blob in bytes")
	flags.StringVar(&opts.TempDir, "temp-dir", "", "Directory for bounded temporary blob files")
	flags.BoolVar(&opts.Refresh, "refresh", false, "Reprocess completed resources (existing content hashes are still reused)")
	flags.BoolVar(&opts.FailFast, "fail-fast", false, "Stop on the first resource failure")
	limit := flags.Int("limit", 0, "Maximum matching versions to download; for list, maximum listed entries (0 = unlimited)")
	listLimit := flags.Int("list-limit", 0, "Maximum inventory entries for sync (0 = full catalog)")
	where := flags.String("where", "", "Filter joined by AND, e.g. downloads > 20 AND updated >= 2025-01-01")
	versionDepth := flags.String("version-depth", "", "Stage-one version selection: positive integer, YYYY-MM-DD cutoff, or unlimited")
	inventoryOpts := inventory.DefaultOptions()
	inventoryKey := flags.String("inventory", "", "Saved inventory key for stage two (relative to S3 prefix)")
	flags.BoolVar(&inventoryOpts.AllowPartial, "allow-partial", false, "download/sync with --inventory: use an inventory whose listing was interrupted (no complete.json), covering only what was saved")
	rediscover := flags.Bool("rediscover", false, "sync: list the catalog again instead of reusing the last run's inventory")
	orderSpec := flags.String("order", "discovery", "Download order: discovery, name, updated, popular, downloads, rating, released, a fact path such as downloads.week, metadata:KEY, number:KEY")
	listFormat := "table"
	if command == "list" {
		flags.StringVar(&listFormat, "format", "table", "List output: table, jsonl (references), or summary (JSON)")
		flags.StringVar(&inventoryOpts.LocalDir, "inventory-dir", ".local/inventories", "Directory for completed local JSONL inventories")
	}
	flags.Int64Var(&inventoryOpts.MaxBytes, "max-inventory-bytes", inventoryOpts.MaxBytes, "Maximum serialized inventory size")
	flags.IntVar(&inventoryOpts.PartEntries, "inventory-part-entries", inventoryOpts.PartEntries, "Entries per uploaded inventory part; parts upload while listing runs so an interrupted listing keeps what it saved")
	flags.IntVar(&inventoryOpts.SortBufferBytes, "sort-buffer-bytes", inventoryOpts.SortBufferBytes, "Approximate encoded metadata bytes per sorting run")
	quiet := flags.Bool("quiet", false, "Suppress progress and per-resource completion/skip logs (failures are still logged)")
	var eventOpts eventSettings
	eventOpts.register(flags)
	s3opts := storage.S3Options{}
	bucket, endpoint, region := os.Getenv("DUG_BUCKET"), os.Getenv("DUG_S3_ENDPOINT"), envOr("AWS_REGION", "us-east-1")
	if local {
		bucket, endpoint, region = locals3.Bucket, locals3.Endpoint, locals3.Region
	}
	flags.StringVar(&s3opts.Bucket, "bucket", bucket, "S3 bucket (local mode creates it; otherwise must exist)")
	flags.StringVar(&s3opts.Endpoint, "endpoint", endpoint, "S3-compatible endpoint URL (or DUG_S3_ENDPOINT)")
	flags.StringVar(&s3opts.Region, "region", region, "S3 signing region")
	flags.StringVar(&s3opts.Prefix, "prefix", "dug", "S3 key prefix")
	flags.BoolVar(&s3opts.PathStyle, "path-style", true, "Use path-style S3 addressing")
	flags.IntVar(&s3opts.UploadConcurrency, "upload-concurrency", 4, "Parallel S3 parts per active blob upload")
	flags.Int64Var(&s3opts.PartSize, "part-size", 16<<20, "S3 multipart part size in bytes (minimum 5242880)")
	pluginURL := flags.String("plugin-url", envOr("DUG_PLUGIN_URL", "http://127.0.0.1:7781"), "Connect registry plugin URL (or DUG_PLUGIN_URL)")
	var pluginOptions optionsFlag
	flags.Var(&pluginOptions, "option", "Plugin-specific key=value option (repeatable)")
	flags.String("config", "", "YAML settings file (default ./dug.yaml or DUG_CONFIG); flags override it")
	config, err := loadConfig(args[2:])
	if err != nil {
		return err
	}
	configOptions, err := config.apply(flags, command, kind, local)
	if err != nil {
		return err
	}
	if err := flags.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return usageError{err}
	}
	if flags.NArg() > 0 {
		return misuse("unexpected positional arguments: %v", flags.Args())
	}
	// Options given with --option win over the same key in the config file. A
	// saved inventory carries its own options, so the file's do not apply.
	for key, value := range configOptions {
		if command == "download" || *inventoryKey != "" {
			break
		}
		if _, exists := pluginOptions[key]; !exists {
			if pluginOptions == nil {
				pluginOptions = make(optionsFlag)
			}
			pluginOptions[key] = value
		}
	}
	if command == "list" {
		if listFormat != "table" && listFormat != "jsonl" && listFormat != "summary" {
			return misuse("list format must be table, jsonl, or summary")
		}
		if inventoryOpts.LocalDir == "" {
			return misuse("inventory-dir must not be empty")
		}
	}
	if local && (s3opts.Endpoint != locals3.Endpoint || s3opts.Region != locals3.Region || !s3opts.PathStyle) {
		return misuse("local mode requires endpoint %s, region %s, and path-style addressing", locals3.Endpoint, locals3.Region)
	}
	if *limit < 0 || *listLimit < 0 {
		return misuse("limits must be nonnegative")
	}
	// Each package validates its own limits; running those checks here fails
	// before any listing or storage work starts.
	inventoryOpts.TempDir, inventoryOpts.Attempts, inventoryOpts.Timeout = opts.TempDir, opts.Attempts, opts.Timeout
	for _, err := range []error{opts.Validate(), inventoryOpts.Validate(), s3opts.Validate(), eventOpts.validate()} {
		if err != nil {
			return usageError{err}
		}
	}
	order, err := inventory.ParseOrder(*orderSpec)
	if err != nil {
		return usageError{err}
	}
	filter, err := inventory.ParseFilter(*where)
	if err != nil {
		return usageError{err}
	}
	if command == "list" && (*where != "" || *listLimit != 0) {
		return misuse("list uses --limit; --where and --list-limit are download/sync options")
	}
	reusing := command == "download" || (command == "sync" && *inventoryKey != "")
	if *rediscover && (command != "sync" || *inventoryKey != "") {
		return misuse("--rediscover only applies to sync without --inventory")
	}
	if reusing && (*versionDepth != "" || *listLimit != 0) {
		return misuse("version depth and list limit belong to stage one")
	}
	if *versionDepth != "" {
		if _, err := registry.ParseVersionDepth(*versionDepth); err != nil {
			return usageError{err}
		}
		if pluginOptions == nil {
			pluginOptions = make(optionsFlag)
		}
		if _, exists := pluginOptions["version_depth"]; exists {
			return misuse("specify --version-depth or --option version_depth, not both")
		}
		pluginOptions["version_depth"] = *versionDepth
	}
	if command == "download" {
		if *inventoryKey == "" {
			return misuse("download requires --inventory from stage one")
		}
	} else if *inventoryKey != "" && command == "list" {
		return misuse("--inventory is only valid with download or sync")
	}
	if reusing && len(pluginOptions) != 0 {
		return misuse("an explicit inventory uses its saved plugin options; --option belongs to stage one")
	}
	client := services.NewDugHttpClient(services.WithConnections(max(64, opts.Downloads)))
	defer client.CloseIdleConnections()
	s3opts.HTTPClient = services.NewDugHttpClient(
		services.WithConnections(max(64, opts.Downloads*s3opts.UploadConcurrency)),
		services.WithTimeout(opts.Timeout),
	)
	defer s3opts.HTTPClient.CloseIdleConnections()
	store, err := newStore(ctx, s3opts)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	bus, err := eventOpts.start(client, logger)
	if err != nil {
		return err
	}
	defer closeBus(bus, logger)
	var remote *plugin.Client
	// sync defaults to the last completed inventory for this query; only
	// --rediscover (or no prior run) lists the catalog again.
	if command == "sync" && *inventoryKey == "" && !*rediscover {
		run, found, err := inventory.LoadLastRun(ctx, store, kind, pluginOptions, *listLimit, inventoryOpts)
		if err != nil {
			return fmt.Errorf("load last run: %w", err)
		}
		if found {
			*inventoryKey = run.InventoryKey
			logger.Info("reusing last run; pass --rediscover to list again", "key", run.InventoryKey, "entries", run.Entries, "created_at", run.CreatedAt)
		}
	}
	if command == "list" || (command == "sync" && *inventoryKey == "") {
		remote, err = plugin.NewClient(ctx, client, *pluginURL, plugin.ClientOptions{Registry: kind, Options: pluginOptions, Token: os.Getenv("DUG_PLUGIN_TOKEN")})
		if err != nil {
			return err
		}
		listingLimit := *listLimit
		if command == "list" {
			listingLimit = *limit
		}
		provider := remote.Provider()
		definition, _ := provider.Registry(kind)
		output := &listOutput{format: listFormat, stdout: stdout, stderr: stderr, provider: provider}
		stopListing := func() {}
		if command == "list" {
			inventoryOpts.OnEntry = output.entry
		} else if !*quiet {
			var listed atomic.Int64
			inventoryOpts.OnEntry = func(registry.Reference) error { listed.Add(1); return nil }
			logger.Info("listing started", "registry", kind, "limit", listingLimit)
			stopListing = startProgress(logger, "listing", func() []any { return []any{"entries", listed.Load()} })
		}
		if bus != nil {
			inventoryOpts.OnEntry = publishDiscovery(bus, inventoryOpts.OnEntry)
		}
		header := inventory.Header{Registry: kind, Provider: provider.ID, Kind: definition.Kind, Options: pluginOptions, Limit: listingLimit}
		summary, err := inventory.Create(ctx, remote, store, header, inventoryOpts)
		stopListing()
		if err != nil {
			if summary.Key != "" {
				logger.Warn("listing interrupted; saved parts remain", "key", summary.Key, "entries", summary.Entries, "resume_with", "download --inventory KEY --allow-partial")
			}
			return fmt.Errorf("stage one listing: %w", err)
		}
		// The inventory is already durable; a missing pointer only costs a
		// re-listing next time, so it must not fail the run.
		if err := inventory.SaveLastRun(ctx, store, header, summary, inventoryOpts); err != nil {
			logger.Warn("record last run failed", "key", summary.Key, "error", err.Error())
		}
		if command == "list" {
			return output.finish(summary, s3opts)
		}
		*inventoryKey = summary.Key
		logger.Info("inventory complete", "key", summary.Key, "entries", summary.Entries)
	}
	snapshot, err := inventory.Open(ctx, store, *inventoryKey, order, inventoryOpts)
	if err != nil {
		return fmt.Errorf("load inventory: %w", err)
	}
	defer snapshot.Close()
	if snapshot.Partial {
		logger.Warn("using a partial inventory: its listing was interrupted, so entries after the last saved part are missing", "key", *inventoryKey, "entries", snapshot.Summary.Entries)
	}
	if snapshot.Header.Registry != kind {
		return fmt.Errorf("inventory belongs to registry %q, not %q", snapshot.Header.Registry, kind)
	}
	if remote == nil {
		remote, err = plugin.NewClient(ctx, client, *pluginURL, plugin.ClientOptions{Registry: kind, Options: snapshot.Header.Options, Token: os.Getenv("DUG_PLUGIN_TOKEN")})
		if err != nil {
			return err
		}
	}
	var done, failed atomic.Int64
	opts.OnEvent = func(event engine.Event) {
		publishDownload(bus, event)
		args := []any{"registry", event.Reference.Registry, "name", event.Reference.Name, "version", event.Reference.Version}
		if event.Error != nil {
			failed.Add(1)
			logger.Error(event.Status, append(args, "error", event.Error.Error())...)
			return
		}
		done.Add(1)
		if !*quiet {
			logger.Info(event.Status, append(args, "key", event.Key)...)
		}
	}
	if !*quiet {
		logger.Info("download started", "inventory", *inventoryKey, "inventory_entries", snapshot.Summary.Entries, "order", *orderSpec)
		defer startProgress(logger, "downloading", func() []any {
			return []any{"processed", done.Load(), "failed", failed.Load()}
		})()
	}
	report, runErr := engine.Download(ctx, inventory.Select(snapshot.Entries, filter, *limit), remote, store, opts)
	result := struct {
		InventoryKey string `json:"inventory_key"`
		Order        string `json:"order"`
		Where        string `json:"where,omitempty"`
		Limit        int    `json:"limit"`
		engine.Report
	}{*inventoryKey, *orderSpec, *where, *limit, report}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return errors.Join(runErr, err)
	}
	return runErr
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
