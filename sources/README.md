# Registry sources

Each subdirectory is an independent Go module that serves the Connect registry
protocol for one upstream registry. Sources depend only on the public SDK; the
host owns storage, ordering, resume, and S3 credentials. See
[the plugin design](../docs/plugins.md) and
[module boundaries](../docs/monorepo.md).

| Source | Identity | Stability | Default listen | Version history | Notes |
| --- | --- | --- | --- | --- | --- |
| [wordpress](wordpress/) | `wordpress` | stable | `127.0.0.1:7781` | Yes, SVN tags | Plugin directory; downloads, ratings, active installs |
| [wordpress-themes](wordpress/) | `wordpress-themes` | stable | `127.0.0.1:7785` | Yes, SVN version directories | Theme directory served by the wordpress module; adds parent theme |
| [chrome](chrome/) | `chrome` | stable | `127.0.0.1:7782` | No | Public sitemap discovery; CRX pinned with SHA-256; store page gives users, rating count, category, developer |
| [firefox](firefox/) | `firefox` | stable | `127.0.0.1:7783` | Yes, AMO versions | Weekly downloads, daily users, ratings |
| [github](github/) | `github` | stable | `127.0.0.1:7784` | Yes, releases and tags | Release assets, default-branch snapshots, stars; `GITHUB_TOKEN` for quota and private repos |
| [n8n](n8n/) | `n8n` | beta | `127.0.0.1:7786` | Yes, npm versions | Community nodes (npm keyword `n8n-community-node-package`); monthly downloads; tarballs verified against npm's SHA-512; enumeration is a union of two indexes and can leave a gap |
| [ansible-galaxy](ansible-galaxy/) | `ansible-galaxy` | beta | `127.0.0.1:7787` | Yes, collection versions | Published collections only; lifetime downloads, tags, declared repository; SHA-256 and size pinned per version |
| [obsidian](obsidian/) | `obsidian` | beta | `127.0.0.1:7788` | Yes, GitHub releases | Community plugins; lifetime and per-version downloads from the catalog stats; main.js, manifest.json and styles.css per release, with GitHub's SHA-256 where it publishes one; needs `GITHUB_TOKEN` (about 10,000 plugins) |
| [helm](helm/) | `helm` | beta | `127.0.0.1:7789` | Yes, Artifact Hub versions | Charts from Artifact Hub's helm repositories; per-repo download host rule; SHA-256 pinned per version, no size; stars; OCI-hosted charts skipped |
| [joomla](joomla/) | `joomla` | beta | `127.0.0.1:7790` | Yes, update-manifest releases | Install from Web extensions whose install URL is an HTTPS update manifest (about 90 of 3,900); every release pinned with the manifest's SHA-256, SHA-384 or SHA-512; ratings, license; per-manifest download host rule |
| [oci](oci/) | `oci` | experimental | `127.0.0.1:7791` | Yes, tags | Docker Hub and Quay enumerated through their catalog APIs, GHCR and ECR Public by `repos`; tag pinned to its manifest digest; manifest, config and layers per selected platform; client in `sdk/services/oci` |
| [jenkins](jenkins/) | `jenkins` | beta | `127.0.0.1:7792` | Yes, update center versions | Update center plugins (`.hpi`); two static documents walk the whole catalog; SHA-256 pinned per version, size for current releases only; installs, health score, declared repository |

Run one locally with `just plugin <name> --listen 127.0.0.1:PORT`.

Each source also has a `live_test.go` (build tag `live`) that runs it against the
real provider for one small, stable slice of the catalog and asserts the
contract: discovery emits references, the facts, tags and author we map are
still populated, `Resolve` returns a revision and blobs, and the first blob
downloads and matches its declared size and digest. Run them with `just live`
or `just live sources/firefox`; `.github/workflows/live.yml` runs them daily,
one job per source, so a red job means the provider changed. Timeouts, 429 and
5xx skip rather than fail. `LIVETEST_DUMP=1` logs the first reference and its
blobs, which is the quickest way to see what changed. obsidian needs
`GITHUB_TOKEN`; the others work without it.

## What a source must provide

- **Discover**: an enumerable public catalog, streamed one versioned reference
  at a time with backpressure. Explicit selection options are welcome, but a
  source is only useful for corpus work if it can walk the whole catalog.
- **Resolve**: a stable revision per version and complete blob descriptors with
  download URLs restricted to the registry's own hosts. Upstream digests and
  sizes should be captured at listing time when the registry publishes them;
  the host verifies SHA-256 either way.
- **Facts** for ordering and filtering: fill `registry.Facts` with at least an
  updated timestamp, plus download counts, install base and the source
  repository where the registry publishes them, leaving unpublished fields
  nil. Set `Tags` from the registry's keywords, topics or categories through
  `registry.NormalizeTags`, and `Author` to the publishing account. Registry-
  specific metrics go in string metadata. Missing facts sort last.
- **Options** that are rejected when unknown, carry no credentials, and are
  safe to persist in an inventory. Credentials stay in the plugin process.
- **Stability** declared in code: each `registry.Definition` in the source's
  `Provider()` sets `Stability` to `registry.Experimental`, `registry.Beta` or
  `registry.Stable`, and `dug-NAME` refuses to start without one. Experimental
  means options, facts and coverage may change without notice; beta works end
  to end but is young or has known gaps; stable keeps options and facts
  compatible between releases. The level is sent in the handshake, shown as
  `stability` in `--format jsonl` output, and `dug` warns for experimental
  sources.
- **Release version** per source, tagged `sources/NAME/vX.Y.Z` (the wordpress
  module covers both catalogs). `scripts/build.sh` stamps it into `dug-NAME`
  from `git describe`; untagged builds report `devel`. `dug-NAME --version`
  prints it, the handshake carries it as `source_version`, and `dug` logs it
  when it connects. It is separate from the wire `ProtocolVersion`.
- **Version history** where the registry exposes it, so `--version-depth` can
  collect the releases around a suspicious change.

## Todos

The corpus exists so artifacts can be security reviewed: malware triage,
vulnerability analysis, supply-chain diffing between versions, and preserving
copies after upstream takedown. Candidates are weighed by install base,
privilege at runtime, how little the registry reviews uploads, enumerability,
stable versioned downloads with hashes, and terms that permit bulk retrieval.
API facts below were checked on 2026-11-06.

### High priority

- [x] **GitHub Actions**. Runs with CI credentials in millions of workflows and
  GitHub does not review tags. No listing API exists: discover via an explicit
  `repos` option, a scrape of Marketplace search, or a third-party index such
  as ecosyste.ms. Resolve a tag to its commit SHA as the revision and archive
  the tarball from `codeload.github.com`; GitHub publishes no digest, so the
  host computes it. Pin moving major tags such as `v4` at listing time.
  Metadata: stars, `pushed_at`. Repeated inventories detect retagging of the
  kind used in the March 2025 tj-actions compromise. GitHub is moving actions
  into GHCR as immutable OCI packages; once that is generally available the
  OCI source below covers them.
  Covered by the [github](github/) source: `repos` or `owner` selection,
  `tags=true` or `refs=v4` pinned to commit SHAs, release assets, and
  quota-free archives from `codeload.github.com` for public repositories.
- [x] **OCI registries** (Docker Hub, GHCR, Quay, ECR Public). Built as
  `sources/oci` over the shared client in `sdk/services/oci` (checked
  2026-11-10). Blobs are the index, the selected image manifests, configs and
  layers; revision is the pinned manifest or index digest. Options select the
  platform (`linux/amd64` by default, or `all` with attestations) and cap layer
  size. Different from the note above: Docker Hub cannot list every namespace
  (only `library` is a catalog; `search` is windowed and was left out), Quay
  does list every public repository without a namespace, GHCR and ECR deny
  `/v2/_catalog` even with a token, so both take `repos`, and their tag lists
  are lexical, so recency comes from each image's creation time. Docker Hub
  allows 100 anonymous manifest pulls per hour per IP. Cosign signature
  indexes (`sha256-<digest>` tags) have no platform and are archived whole.
  Not built: GitHub-packages or ECR gallery seeding, the referrers API, and
  Helm's `oci://` charts (Helm still skips them; the client is ready for it).
- [ ] **npm**. Install scripts make it the primary malware registry. The legacy
  replication feed was replaced in May 2025 and no longer inlines documents, so
  discover via the changes feed plus per-package registry documents, or via an
  external index. The package document carries every version's tarball URL,
  integrity hash, and publish time; download counts come from
  `api.npmjs.org`. Scope by downloads or recency; the full catalog is tens of
  terabytes.
- [x] **n8n community nodes**. A scoped view of npm: packages carry the
  `n8n-community-node-package` keyword and are resolved like any npm package.
  They load into the n8n process and can read every credential the instance
  holds. n8n verifies a subset for its catalog (`api.n8n.io/api/community-nodes`
  lists only those, so it cannot enumerate), but self-hosted instances install
  any matching package without review.
  Covered by the [n8n](n8n/) source, built standalone rather than as a
  `keywords` option on an npm source, over the shared npm client in
  `sdk/services/npm`. Checked 2026-11-06, and different from the note above:
  npm search reports the keyword's full total (14,244) but serves only its first
  5,000 results (later offsets repeat the top of the ranking instead of failing),
  and keyword queries combine only with AND, so the window cannot be split into
  exact complements. The ecosyste.ms index (`/keywords/KEYWORD`, `per_page`
  1000, `Link` headers) pages through all it knows, 13,807 npm packages, but lags
  npm. The source takes the union (14,265 names), verifies each against npm, and
  warns about any remaining gap. npm publishes SHA-512 only, no SHA-256 or
  size. A future npm source can reuse `sdk/services/npm`; Homebridge and Node-RED
  nodes are the same shape and work today through the `keyword` option.
- [ ] **Agent skills and plugins** (Claude Code, Codex, and other agents).
  A skill is a directory with a `SKILL.md` plus scripts and resources that an
  agent executes with the developer's permissions; nobody reviews them and
  they are also a prompt-injection surface. There is no single registry.
  Claude Code installs plugins from marketplaces, which are git repositories
  with a marketplace manifest; the official one is
  `anthropics/claude-plugins-official` and community marketplaces are added
  by repo. Anthropic's reference skills live in `anthropics/skills`. Codex
  ships `openai/skills`, split into system, curated, and experimental
  directories, and its installer pulls from that catalog or any GitHub repo
  path. Directories such as `skills.sh` install from GitHub repos as well.
  Discover by walking marketplace manifests, catalog repos, and directory
  listings; resolve each plugin or skill directory as a GitHub tarball at a
  commit SHA, which is the revision. Few are tagged, so history is commit
  history. Metadata: repo stars, marketplace plugin versions. Builds on the
  GitHub tags-and-releases client.
- [ ] **MCP servers**. The official MCP Registry at
  `registry.modelcontextprotocol.io` publishes `server.json` entries that point
  at npm, PyPI, NuGet, or OCI packages. It is an enumerator, not a host: use it
  as a seed list for the npm, PyPI, NuGet, and OCI sources and record the
  registry entry as metadata.
- [ ] **PyPI**. Simple index (PEP 691 JSON) for names; per-project JSON for
  every release file with SHA-256, size, and upload time. The XML-RPC changelog
  and RSS feeds give recency for new-release polling. Mirroring is tolerated;
  send a descriptive user agent.
- [ ] **VS Code extensions via Open VSX**. IDE extensions run with developer
  privileges and the registry was hit by GlassWorm in October 2025. Public
  search API, no auth; per-extension JSON lists versions, VSIX URL, and a
  SHA-256 file. Metadata: download count, rating, timestamp. Coverage is
  partial: Open VSX only has extensions whose publishers also release there,
  so Microsoft's own extensions and many single-marketplace publishers are
  missing. The Microsoft VS Code Marketplace is deferred below for
  terms-of-use reasons.
- [ ] **JetBrains Marketplace**. Same privilege model as Open VSX. Public
  `searchPlugins` API, per-plugin update list, and download endpoint; metadata
  includes downloads, rating, and dates.

### Medium priority

- [x] **Jenkins plugins**. Implemented as `jenkins` over
  `updates.jenkins.io/current/` (checked 2026-11-11: 2,131 plugins, 38,234
  versions). `update-center.actual.json` (3.5 MB) holds the current release of
  every plugin with SHA-256, size, install count and health; `plugin-versions.json`
  (24 MB) holds every version with SHA-256 but no size. Digests are base64.
  Downloads redirect to `get.jenkins.io` and then a mirror; only the stable
  `updates.jenkins.io` URL is pinned. The signed update center also lists
  security `warnings` and `deprecations`, which are read into `security_warnings` and `deprecated` metadata. Both documents'
  signatures are verified (SHA-512 and RSA, chaining to the two update center
  roots embedded from Jenkins core) before anything is read.
- [ ] **PowerShell Gallery**. NuGet v2 feed of `.nupkg` modules with install-time
  scripts; history of typosquatting.
- [x] **Ansible Galaxy collections**. API v3 with tarballs, SHA-256, and version
  history. Runs as root across fleets. Implemented as `ansible-galaxy` over the
  `published` pulp repository (checked 2026-11-06: 4,675 collections, listed
  newest first, 100 per page, no sort or date filter). Digest and size exist
  only in the per-version document, so listing costs one request per emitted
  version. Downloads go through two redirects (a signed Galaxy URL, then S3);
  only the stable `galaxy.ansible.com` artifact URL is pinned. Galaxy roles (API
  v1, 37,883 of them, installed from GitHub) are a separate catalog and belong
  to a seed option on the github source.
- [ ] **Terraform / OpenTofu registry**. Providers are binaries with published
  `SHA256SUMS`; modules redirect to git hosts and are not registry hosted.
- [ ] **dbt packages**. Macros and models that dbt executes against the
  warehouse with the runner's credentials; hooks and run-operations can issue
  arbitrary SQL. The dbt Hub at `hub.getdbt.com` has a static JSON API:
  `api/v1/index.json` lists every package, `api/v1/{org}/{name}.json` lists
  its versions and latest, and `api/v1/{org}/{name}/{version}.json` gives a
  codeload tarball URL, a mirror on `public.cdn.getdbt.com`, and a SHA-1.
  Inclusion is a one-time pull request; new versions are picked up from
  GitHub releases without review. Published timestamps are epoch zero and
  there is no popularity metadata, so ordering is discovery or name. The
  catalog is small, a few hundred packages, so a full walk is cheap.
- [x] **Helm charts**. Charts deploy workloads and RBAC into clusters.
  Implemented as `helm` over the Artifact Hub API through
  `sdk/services/artifacthub`. Checked 2026-11-06, and different from the note
  above: the search lists 18,942 helm packages (18,517 without the deprecated
  ones it hides by default), 60 per page with no offset cap, and it answers a
  sustained rate above about one request per second with a bare 429, so
  searches are spaced two seconds apart (a full index walk is about 310
  requests, ten minutes). The package document carries the content URL and
  SHA-256 digest of the current version, but older versions' URLs and digests
  exist only in per-version documents, so history costs one request per
  version. Artifact Hub reports stars in search results only, and no downloads
  or install base. There is no `index.yaml` fetch: Artifact Hub already passes
  on the repository index's digest, and archives downloaded in the live probe
  matched it.
  Chart URLs are on the repository's own host (`github.io` index, GitHub
  release assets, a vendor domain) or GitHub release hosts, and about a tenth of
  charts, including Bitnami's classic repository, now point at `oci://`
  registries. Those are skipped with a count until the OCI source exists.
  Resolve accepts a download host equal to the repository URL's host or a
  GitHub release host, over HTTPS to a public DNS name; that rule narrows but
  cannot eliminate what an edited inventory could make the plugin fetch.
- [ ] **NuGet**. The catalog API is an append-only log of every version with
  timestamps and SHA-512; the flat container serves `.nupkg`. Best bulk
  ergonomics of any registry. Download totals come from the search API.
- [ ] **crates.io**. Daily database dump and sparse index; crates served from
  `static.crates.io`. Crawler policy wants a contact user agent and about one
  API request per second.
- [ ] **RubyGems**. Data dumps, specs index, per-gem API with SHA-256.
- [ ] **Go modules**. `index.golang.org` feeds every module version with
  timestamps; the proxy serves zips. No popularity metadata.
- [ ] **Homebrew**. Bottles are prebuilt binaries that developers run with
  their own privileges, and they are registry hosted: `formulae.brew.sh`
  serves static `formula.json` and `cask.json` with no auth, each formula
  lists bottle URLs on `ghcr.io` with a SHA-256 per platform, and the
  analytics endpoints give 30, 90, and 365 day install counts. Only the
  current bottle is advertised, so there is no version history. Casks point at
  vendor download URLs with a SHA-256, so treat them as index-only or allow
  per-reference hosts. Bottle fetches need the OCI source's anonymous GHCR
  token flow.
- [ ] **Drupal.org**. Project and release API with tarballs on `ftp.drupal.org`.
- [ ] **Joomla**. The Joomla Extensions Directory at `extensions.joomla.org` is
  an index, not a host: each listing links to the developer's site, many
  extensions are paid, and the Install from Web installer plugin reads the
  same directory through a Joomla-run endpoint. A source can enumerate the
  directory and follow free download links, but the ZIPs come from arbitrary
  developer hosts, which breaks the fixed download-host allowlist. Needs a
  per-reference host rule or an index-only mode that records links without
  fetching. Lower confidence than the other CMS entries.
- [ ] **Thunderbird add-ons**. AMO-compatible API v4 at
  `services.addons.thunderbird.net`; parameterize the firefox source.
- [x] **Obsidian community plugins**. Built as `sources/obsidian` (checked
  2026-11-06). The catalog is `community-plugins.json` (10,035 entries) in
  `obsidianmd/obsidian-releases`, with lifetime and per-version downloads in
  `community-plugin-stats.json`. Each version is a GitHub release whose
  assets are `main.js`, `manifest.json` and optional `styles.css`. Different
  from the note above: GitHub now publishes a SHA-256 `digest` on newer release
  assets (older ones have none), and the module uses the shared client in
  `sdk/services/github`, which the GitHub source also uses, rather than
  seeding `sources/github`. Plugins run unsandboxed in an Electron app and
  Obsidian reviews only the initial submission, not updates. Themes
  (`community-css-themes.json`) are not covered yet.
- [ ] **F-Droid**. A single `index-v2.json` lists every app and version with APK
  URL, SHA-256, size, and `lastUpdated`. The only open Android channel.
- [ ] **Snap Store**. Public find and download API with digests; weak review and
  a history of wallet scams.
- [ ] **Hugging Face**. Models, datasets, and Spaces are git repositories with
  a public API: the list endpoints page by cursor and sort by downloads, likes,
  or last modified; the tree endpoint with `recursive=true` returns every file
  with size and, for LFS files, a SHA-256; files are fetched through resolve
  URLs. Revision is the commit SHA the API reports. Pickle-based model files
  and Space application code are the review targets. Needs a size cap and a
  file-pattern option, since LFS weights run to many gigabytes. Gated and
  private repos are skipped; anonymous rate limits are low, so a token in the
  plugin process is practical.
- [ ] **Modrinth**. Public v2 API with SHA-1 and SHA-512 per file and CDN
  downloads. Consumer scale but a real malware history.
- [ ] **Chocolatey**. The community repository is a NuGet v2 OData feed at
  `community.chocolatey.org/api/v2` with version history, download counts, and
  `.nupkg` downloads; moderation is partly automated. Most community packages
  are PowerShell install scripts that fetch the vendor installer at install
  time, so the archive captures the script and its pinned checksum rather than
  the binary. The feed has usage limits and asks heavy consumers to mirror, so
  listing concurrency must stay low.

### Blocked or deferred

- **VS Code Marketplace**. Terms limit API use to Visual Studio products; Open
  VSX covers most of the same code.
- **Edge Add-ons**. CRX download by ID works, but there is no listing API and
  the sitemap and robots paths return the store web app. Enumeration would mean
  scraping search results.
- **Google Play, App Store, Safari, Figma, Atlassian Cloud, Shopify,
  Salesforce**. No public artifact downloads.
- **CurseForge**. Requires an API key and restrictive terms.
- **Packagist, winget, Raycast, Sublime Package Control, HACS**. The
  registry only indexes; artifacts live in GitHub releases or tags.
  A generic GitHub tags-and-releases source, which the GitHub Actions work would
  produce, covers these better than per-registry adapters.
