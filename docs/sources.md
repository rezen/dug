# Sources

Each registry is served by its own plugin executable; see [plugin design](plugins.md) for the protocol. Start the plugin in its own terminal, then point `dug` at it with `--plugin-url`.

## WordPress plugins

```sh
./bin/dug-wordpress --listen 127.0.0.1:7781
```

Options: `search`, `slugs`, `page_size` (default 100, range 1–250), `browse` (`updated`, `popular`, `new`), and `version_depth`. The plugin independently retries upstream HTTP requests up to four times. Discovery defaults to current public releases; deleted/private plugins and removed versions are excluded. Directory pagination is a live view, so a changing directory can produce duplicates or omissions during long scans; rerun to reconcile. ZIPs are stored intact and are not extracted. See the [WordPress module](../sources/wordpress/README.md).

## WordPress themes

The theme directory is a second registry served by the same source module.
Themes run as PHP in every request of the sites that activate them and account
for a large share of WordPress CVEs. Start the theme plugin in its own terminal:

```sh
./bin/dug-wordpress-themes --listen 127.0.0.1:7785

# In another terminal with the host S3 environment configured:
./bin/dug sync wordpress-themes --plugin-url http://127.0.0.1:7785 \
  --option slugs=astra,hello-elementor --version-depth 2

# Save the full current catalog; updated and new walk every public theme.
./bin/dug list wordpress-themes --plugin-url http://127.0.0.1:7785 --option page_size=250

./bin/dug download wordpress-themes --plugin-url http://127.0.0.1:7785 \
  --inventory INVENTORY_KEY --order popular \
  --where 'downloads > 1000 AND rating >= 80' --limit 100
```

The `wordpress-themes` plugin accepts the same options as `wordpress`: `slugs`,
`search`, `page_size` (1–250), `browse`, and version depth. `browse` takes
`updated` (default), `popular`, `new`, or `featured`; `popular` lists only themes
with active installs and `featured` is a small curated set. Facts and metadata
match the plugin directory (`downloads.total`, `installs`, `updated_at` and the
theme's SVN `repository.url` as facts; `rating` 0–100, `num_ratings`, `ratings_1`
through `ratings_5` as metadata) plus `template`, the parent theme
slug of a child theme, so `metadata:template != ""` selects child themes. Theme
releases are version directories under the slug in the theme SVN rather than
tags; date depth reads their creation history the same way. Plugin and theme
slugs are separate namespaces, so the two registries never share inventories or
completion manifests. See the [WordPress module](../sources/wordpress/README.md).

## Chrome extensions

Start the second plugin in another terminal:

```sh
./bin/dug-chrome --listen 127.0.0.1:7782

# Explicit IDs: metadata inventory first, then archived CRX files.
./bin/dug sync chrome --plugin-url http://127.0.0.1:7782 \
  --option ids=ghbmnnjooekpmoecnnnilnnbdlolhkhi

# Without ids, enumerate the public Web Store sitemap.
./bin/dug list chrome --plugin-url http://127.0.0.1:7782 \
  --option listing_concurrency=8 --limit 100
```

Options: `ids` (comma separated), `listing_concurrency` (default 8, range 1–128),
`browser_version` (default `150.0.0.0`), and `details` (default `true`). The
browser version selects the compatible release returned by Google's update
service; this is not a historical version archive. Listing pins the version, CRX
URL, expected SHA-256, and size. Downloads use the pinned descriptor and verify
the bytes against those values. If a saved URL becomes unavailable, the download
fails; list again to refresh it.

With `details` on, listing also reads each item's Web Store page, one extra
request per item. The user count becomes the `installs` fact, so `--order
popular` and `--where 'installs >= 100000'` work for Chrome; the store category
becomes tags (`tag = privacy`); the "Offered by" developer becomes the author;
and the rating, rating count (`num_ratings`), `title` and `category` name are
metadata. Items whose page is gone keep only their CRX facts. `details=false`
skips the page fetch for a faster, facts-free listing.

Sitemap discovery covers publicly indexed Web Store CRX entries and can include
themes. It does not guarantee every extension, private/unlisted items, or regional
availability. Duplicate IDs are suppressed. Entries explicitly reported as
unavailable by the update service are skipped with plugin warnings during sitemap
listing; explicit IDs instead fail. Other metadata failures stop listing. The
plugin keeps a set of seen IDs in memory, reads one sitemap at a time (64 MiB
uncompressed limit), and bounds concurrent update requests. CRX signatures are not
independently validated; the upstream SHA-256 verifies transfer integrity.

## Firefox extensions

```sh
./bin/dug-firefox --listen 127.0.0.1:7783

# In another terminal with the host S3 environment configured:
./bin/dug sync firefox --plugin-url http://127.0.0.1:7783 \
  --option slugs=ublock-origin,darkreader --version-depth 2

# Save a public catalog inventory; omit --limit to enumerate the catalog.
./bin/dug list firefox --plugin-url http://127.0.0.1:7783 --limit 100

./bin/dug download firefox --plugin-url http://127.0.0.1:7783 \
  --inventory INVENTORY_KEY --order=-downloads.week \
  --where 'rating >= 80 AND installs > 1000' --limit 100
```

The `firefox` plugin accepts `slugs` (also numeric IDs/GUIDs), `search`, `page_size`
(default/max 50), and version depth. Public search windows are capped by Mozilla;
discovery splits creation-date ranges to traverse larger catalogs. A window that
cannot be split further fails rather than silently omitting entries. The live
catalog can still change during listing. Only public Firefox extensions and
available public versions are included, not themes or private releases.

Integer depth includes the current release, then remaining versions in API order.
Date depth uses the inclusive UTC XPI file creation date. Discovery pins IDs, URLs,
SHA-256 and sizes; downloads do not look up a newer release. XPIs are ZIP-format
archives and are stored intact. Expected hashes verify bytes; signature verification
is separate. Firefox publishes a trailing seven-day download count and average
daily users, never a lifetime total, so its facts carry `downloads.week` and
`installs` while `downloads.total` stays absent. `rating` is normalized to 0–100,
with the original 0–5 `rating_average` also preserved as metadata. See the
[Firefox module](../sources/firefox/README.md).

## GitHub repositories

```sh
GITHUB_TOKEN=ghp_... ./bin/dug-github --listen 127.0.0.1:7784

# Latest release of each repository: source archive plus uploaded release assets.
./bin/dug sync github --plugin-url http://127.0.0.1:7784 \
  --option repos=cli/cli,junegunn/fzf

# Every repository an organization owns, three releases deep. Repositories
# without releases contribute a default-branch snapshot instead.
./bin/dug list github --plugin-url http://127.0.0.1:7784 \
  --option owner=actions --version-depth 3

# GitHub Actions: every tag, pinned to the commit it points at today, so a
# later listing shows if v4 was retargeted.
./bin/dug sync github --plugin-url http://127.0.0.1:7784 \
  --option repos=actions/checkout --option snapshot=never --option tags=true \
  --version-depth unlimited

./bin/dug download github --plugin-url http://127.0.0.1:7784 \
  --inventory INVENTORY_KEY --order=-number:stars \
  --where 'metadata:kind = release AND metadata:prerelease = false' --limit 100
```

The `github` plugin needs exactly one of `repos` (`owner/name` list), `owner`
(user or organization login), or `search` (repository search query, partitioned
by creation date past GitHub's 1,000-result cap). Versions are published releases
in API order, newest first; `version_depth` counts them or applies a
publication-date cutoff. Listing resolves each release tag to its commit SHA and
pins that SHA, the release ID, and the uploaded assets (ID, name, size, digest).
Resolution derives download URLs from those validated fields, so the source
archive `NAME-SHA.zip` and every asset are fetched exactly as listed even if the
tag later moves. Asset SHA-256 digests are verified when GitHub records them;
source archives carry no upstream hash and are hashed by the host. Public
github.com repositories download from `codeload.github.com` and
`github.com/OWNER/NAME/releases/download/` without a token or API quota; private
and Enterprise repositories download through API routes with the token.

`snapshot` controls default-branch heads: `fallback` (default) snapshots only
repositories without releases, `always` adds one to every repository, `only`
skips releases, and `never` leaves release-less repositories out. A snapshot's
version is the branch name and its revision the commit SHA, so an unchanged head
is skipped on the next run. `refs` snapshots extra branches or tags per
repository, and `tags=true` archives every remaining tag in API order (`kind`
`tag`; `version_depth` bounds tags separately, and a date cutoff uses commit
dates). `assets=false` keeps only source archives; `forks=true` includes
forks when enumerating an owner. Metadata includes `stars`, `forks`, `language`,
`license`, `archived`, `private`, `kind`, and `prerelease`. Facts carry
`released_at`, `updated_at` (the last push), `repository.url` with the archived
commit, and per-release asset downloads as `version_downloads.total`; GitHub has
no package-wide download count, so `downloads` is absent.

Set `GITHUB_TOKEN` in the plugin environment. Unauthenticated access is limited to
60 API requests per hour and public repositories; listing costs a few requests
per repository and release. The token is sent only to the API origin over HTTPS or loopback and is
never persisted. The plugin waits out primary and secondary rate limits. `--api`
targets GitHub Enterprise Server. See the [GitHub module](../sources/github/README.md).

## Obsidian

```sh
export GITHUB_TOKEN=...
./bin/dug-obsidian --listen 127.0.0.1:7788 &
./bin/dug list obsidian --plugin-url http://127.0.0.1:7788 --option ids=obsidian-git,dataview --option version_depth=3
```

The `obsidian` plugin reads the community catalog (`community-plugins.json`
and `community-plugin-stats.json` from `obsidianmd/obsidian-releases`) and
archives each plugin's GitHub releases: `main.js`, `manifest.json` and
`styles.css`. `ids` selects plugins; without it the whole catalog is walked,
skipping plugins whose repository or releases are gone (an explicit id that is
missing is an error). `version_depth` counts releases or applies a publication
date cutoff. It sorts by `--order downloads` (lifetime installs) or `updated`.
Digests come from GitHub when it records them; otherwise the host hashes the
bytes. GitHub keeps the same release ID if a maintainer replaces an asset, so
an unchanged revision does not prove unchanged bytes. The GitHub client is
shared with the `github` plugin through `sdk/services/github`.

## Ansible Galaxy collections

```sh
./bin/dug-ansible-galaxy --listen 127.0.0.1:7787

# In another terminal with the host S3 environment configured:
./bin/dug sync ansible-galaxy --plugin-url http://127.0.0.1:7787 \
  --option collections=community.general,amazon.aws --version-depth 2

# Save the whole public catalog inventory; omit --limit to enumerate it all.
./bin/dug list ansible-galaxy --plugin-url http://127.0.0.1:7787 --limit 100

./bin/dug download ansible-galaxy --plugin-url http://127.0.0.1:7787 \
  --inventory INVENTORY_KEY --order=-downloads.total \
  --where 'downloads.total > 100000' --limit 100
```

The `ansible-galaxy` plugin accepts `collections` (comma-separated `namespace.name`),
`namespaces` (every collection of those namespaces; not combinable with
`collections`), `page_size` (default/max 100), and version depth. With no selector
it walks the whole public catalog of published collections (about 4,700), a hundred
per request, newest collection first. A walk that sees fewer collections than the API
reports fails instead of returning a partial catalog. Ansible Galaxy roles are a
different catalog hosted on GitHub and are not included, nor are unpublished or
Automation Hub collections.

Integer depth includes the highest version, then the remaining versions in the order
the API lists them (newest upload first). Date depth uses the inclusive UTC version
creation date. Discovery makes one request per emitted version because the digest
and size exist only in the per-version document, so a full depth-1 walk costs
roughly 4,700 requests and about half an hour. It pins the download URL, SHA-256 and
size; downloads never look up a newer release. Archives are `.tar.gz` collection
tarballs, stored intact; the host verifies the pinned digest, and Galaxy's own
signatures are not checked. Galaxy publishes a lifetime download total for the
collection (`downloads.total`, repeated on every version), update and release times,
a declared source repository, tags, and a `deprecated` flag in metadata; it publishes
no ratings or install base. See the [Ansible Galaxy module](../sources/ansible-galaxy/README.md).

## Jenkins plugins

```sh
./bin/dug-jenkins --listen 127.0.0.1:7792

# In another terminal with the host S3 environment configured:
./bin/dug sync jenkins --plugin-url http://127.0.0.1:7792 \
  --option plugins=git,workflow-cps --version-depth 3

# Save the whole update center inventory; omit --limit to enumerate it all.
./bin/dug list jenkins --plugin-url http://127.0.0.1:7792 --limit 100

./bin/dug download jenkins --plugin-url http://127.0.0.1:7792 \
  --inventory INVENTORY_KEY --order=-installs --where 'installs > 10000' --limit 100
```

The `jenkins` plugin reads the Jenkins update center. It accepts `plugins`
(comma-separated short names) and version depth. With no selector it walks every
plugin in `update-center.actual.json` (about 2,100), which is two requests in total:
the 3.5 MB current-release document and, for any depth above 1, the 24 MB
`plugin-versions.json`. Depth 1 skips the larger document. The current release is
emitted first, then older versions newest release first; date depth uses the
version's release time. Plugins the update center does not list (suspended, removed
or hosted elsewhere) are not included. Both documents' signatures are verified
against the Jenkins update center roots embedded in the plugin before any of the
content is used, so a tampered or stale mirror copy fails the listing.

Discovery pins the download URL and SHA-256 for every version, and the size for each
plugin's current release only, since older versions have none. Downloads redirect to
`get.jenkins.io` and a mirror, so only the `updates.jenkins.io` URL is pinned and the
host verifies the digest. Archives are `.hpi` files, stored intact. The update center
publishes install counts (`installs`), release times, a declared source repository,
labels as tags, and in metadata a health score, the minimum Jenkins core, deprecation,
and `security_warnings` (the IDs of Jenkins security advisories that match the
version; absent when none match, and filterable as `metadata:security_warnings`); it publishes
no download counts or ratings. See the [Jenkins module](../sources/jenkins/README.md).

## Bitbucket Cloud repositories

```sh
BITBUCKET_TOKEN=... ./bin/dug-bitbucket --listen 127.0.0.1:7793

# In another terminal with the host S3 environment configured:
./bin/dug sync bitbucket --plugin-url http://127.0.0.1:7793 \
  --option workspaces=atlassian --version-depth 3

./bin/dug list bitbucket --plugin-url http://127.0.0.1:7793 \
  --option repos=atlassian/aui --option snapshot=always
```

The `bitbucket` plugin archives repositories from the workspaces (tenants) you name,
or the repositories you list. Bitbucket Cloud has no public catalog to walk, so a
selector is required. A workspace walk lists repositories oldest first, skips forks
unless `forks=true`, and emits each repository's tags newest commit first as
versions; a repository with no tags gets a snapshot of its main branch. Every
reference pins the commit, so a moved tag shows up as a new artifact on the next
inventory. Archives are `.tar.gz` files from `bitbucket.org`; Bitbucket publishes no
digest, so the host computes it. Set `BITBUCKET_TOKEN` for private repositories and
for quota: anonymous requests are limited to 60 per hour per IP. See the
[Bitbucket module](../sources/bitbucket/README.md).

## n8n community nodes

```sh
./bin/dug-n8n --listen 127.0.0.1:7786

# In another terminal with the host S3 environment configured:
./bin/dug sync n8n --plugin-url http://127.0.0.1:7786 \
  --option packages=n8n-nodes-evolution-api,@elevenlabs/n8n-nodes-elevenlabs --version-depth 3

# Save the keyword's inventory; omit --limit to enumerate it all.
./bin/dug list n8n --plugin-url http://127.0.0.1:7786 --limit 100

./bin/dug download n8n --plugin-url http://127.0.0.1:7786 \
  --inventory INVENTORY_KEY --order=-downloads.month \
  --where 'downloads.month > 10000' --limit 100
```

A community node is an npm package with the `n8n-community-node-package` keyword.
n8n loads it into its own process, where it can read every credential the instance
holds, and a self-hosted instance installs any matching package without review. The
`n8n` plugin accepts `packages` (npm names, scoped or not; not combinable with the
next two), `keyword` (default `n8n-community-node-package`, which also fits other
npm-hosted plugin ecosystems), `enumerate`, and version depth.

npm cannot walk a keyword to the end. Its search reports the full total (about
14,200 for this keyword) but serves only the first 5,000 results; later offsets
silently repeat the top of the ranking. The ecosyste.ms keyword index pages through
everything it knows, but its sync lags npm, so it misses newer packages and still lists
removed ones. By default (`enumerate=all`) the plugin takes the union of the two,
checks every name against npm, skips what npm no longer serves or what lost the
keyword (with a warning), and logs a warning when it finds fewer packages than npm
search reports, so a gap is never silent. In a live check the union held 14,265 names
against npm's 14,244; the count of live packages it recovers is only known at the
end of a full walk. `enumerate=search` or `ecosystems` uses one index alone.

Integer depth emits the current version (the `latest` dist-tag) first, then the rest
newest published first; date depth uses the inclusive UTC publish time. A full
depth-1 walk makes one package-document request per package plus one count request
per scoped package (about 3,500), sequentially; it has not been timed end to end.
Package documents are capped at 32 MiB, and a package over it fails the listing.
npm publishes SHA-512 (SHA-1 for old versions), never SHA-256 or a size, so the
reference pins npm's hash, the plugin checks it while the tarball streams, and the
host computes SHA-256. Tarballs are `.tgz` files from `registry.npmjs.org` only,
stored intact. Facts are `downloads.month`, `updated_at`, `released_at`, a declared
repository, keyword tags, and the account that published each version; metadata
adds `install_script`, `deprecated` and `provenance` when true. npm has no lifetime
total, weekly count, ratings or install base. See the
[n8n module](../sources/n8n/README.md).


## Helm charts

```sh
./bin/dug-helm --listen 127.0.0.1:7789

# In another terminal with the host S3 environment configured:
./bin/dug sync helm --plugin-url http://127.0.0.1:7789 \
  --option charts=prometheus-community/kube-prometheus-stack,argo/argo-cd --version-depth 3

# Every chart in some repositories, or the whole catalog (about ten minutes to list).
./bin/dug list helm --plugin-url http://127.0.0.1:7789 --option repos=prometheus-community,argo
./bin/dug list helm --plugin-url http://127.0.0.1:7789 --limit 1000

./bin/dug download helm --plugin-url http://127.0.0.1:7789 \
  --inventory INVENTORY_KEY --order=-number:stars --limit 100
```

The `helm` plugin indexes charts through [Artifact Hub](https://artifacthub.io). It
accepts `charts` (`REPOSITORY/CHART`), `repos`, `search` and `version_depth`; `charts`
cannot be combined with the other two. With no selector it walks the whole helm
catalog, deprecated charts included (about 18,900 packages), sorted by last update.
Artifact Hub throttles bulk search, so searches are spaced two seconds apart and a
full walk takes about ten minutes before any chart is fetched. Charts updated
mid-walk jump to the front of that ordering, so the front is read again afterwards to
catch them. A walk that still comes up far short of the reported total fails; a small
gap is reported as a warning.

Artifact Hub hosts no chart bytes. Each version is downloaded from its repository's
own host, so Resolve accepts a download URL only over HTTPS to a public DNS name that
is the repository URL's host or a GitHub release host. Digests are the SHA-256 the
repository's `index.yaml` published, passed on by Artifact Hub; the archive size is not
published. Charts whose URL is an `oci://` registry (about a tenth, including
Bitnami's) are skipped with a count until an OCI source covers them, as are versions
with no digest or on an unaccepted host. Integer depth emits the current version and
then the version list's order (newest first); older versions each cost one request
because their URLs and digests exist only in per-version documents. Artifact Hub
reports stars in search results only (so `--order=-number:stars` needs a walk, not
`charts`) and no download counts or install base. See the
[Helm module](../sources/helm/README.md).

## OCI registries

```sh
./bin/dug-oci --listen 127.0.0.1:7791

# In another terminal with the host S3 environment configured:
./bin/dug sync oci --plugin-url http://127.0.0.1:7791 \
  --option repos=docker.io/alpine,quay.io/coreos/etcd,ghcr.io/jqlang/jq --version-depth 3

# Every official Docker Hub image for two platforms, or every public Quay repository.
./bin/dug list oci --plugin-url http://127.0.0.1:7791 \
  --option namespaces=docker.io/library --option platform=all
./bin/dug list oci --plugin-url http://127.0.0.1:7791 --option registries=quay.io --limit 1000

./bin/dug download oci --plugin-url http://127.0.0.1:7791 \
  --inventory INVENTORY_KEY --order=-downloads --limit 100
```

The `oci` plugin speaks the OCI distribution API to Docker Hub, Quay, GHCR and ECR
Public through the shared client in `sdk/services/oci`. Docker Hub and Quay enumerate
through their catalog APIs; GHCR and ECR deny `/v2/_catalog`, so name their repositories
with `repos`. With no selector it walks Docker Hub's official `library` namespace and
every public Quay repository. A reference is one tag, named `HOST/REPOSITORY` and pinned
to the manifest or index digest the tag pointed at; resolution fetches that digest and
archives the index, the image manifests for the selected `platform` (`linux/amd64` by
default, `all` includes attestations), their configs and layers. `max_layer_size` omits
big layers with a warning. Listing makes no manifest requests, but resolution does, and
Docker Hub allows 100 anonymous pulls per hour per IP, so a large run needs
`DOCKER_HUB_USERNAME` and `DOCKER_HUB_TOKEN`. See the
[OCI module](../sources/oci/README.md).
