# Researching a registry

Answer every question below with a real request before writing Go. Record
the answers as a facts block in your reply (the user reviews it) and keep the
raw responses in your scratchpad; trimmed copies become test payloads.

## Recipes

```sh
S=/path/to/scratchpad
# Headers and body separately; -L follows redirects, -D dumps headers.
curl -sS -L -D "$S/headers.txt" -o "$S/body.json" 'https://registry.example/api/items?size=2'
python3 -m json.tool "$S/body.json" | head -80

# Size of a large document without downloading it.
curl -sSIL 'https://registry.example/index.json' | grep -i 'content-length\|location\|HTTP/'

# Where a download URL really goes, and whether the host changes.
curl -sSIL 'https://registry.example/download/x-1.0.zip' | grep -i 'HTTP/\|location\|content-length\|content-type'

# Compute a digest to compare against what the API claims.
curl -sSL 'https://registry.example/download/x-1.0.zip' | shasum -a 256
```

Send a descriptive `User-Agent` if the registry asks for one; `httpx.Get`
sends `dug/0.1`.

## Questions

### Enumeration

- Is there a way to list the whole catalog: a paginated listing, a search with
  no query, a sitemap, a single index document, a database dump?
- What bounds it: page size limit, maximum offset, maximum result window per
  query, document size? Which field can split a capped window (creation date
  is ideal because it never changes)?
- Does the API report a total count, so an early end can be detected?
- Does it support cursors or `next` links, or only page numbers?
- How large is the catalog, and roughly how many requests would a full walk
  take?

### Items and versions

- What is the stable identifier (slug, numeric ID, `owner/name`)? Can it
  change? Does the API accept both a slug and an ID?
- Where is the list of versions? Is it paginated? Is it ordered? Does it
  include yanked, deleted, or prerelease versions, and are they marked?
- Per version: download URL, digest (which algorithm, hex or base64), size,
  publication time. Is any of these only available from a second request?
- Are versions immutable once published? Can a tag or "latest" alias move?

### Metadata

- Last update time for the item and release time per version, with their
  exact meaning (upload, approval, tag creation).
- Popularity: lifetime downloads, periodic downloads, active installs, stars,
  rating scale and count. Which of these does the catalog listing include
  without a per-item request?

### Bytes

- Which hosts serve downloads? Do URLs redirect to a CDN or mirror? The
  allowlist applies to the URL you persist, before redirects.
- Does the download need a token or an `Accept` header? Does the registry
  publish a digest you can verify against?
- Typical and maximum artifact sizes; the host caps blobs at 4 GiB by
  default.

### Access

- Rate limits, with headers that announce them. Primary and secondary limits
  (GitHub) need different waits.
- Required or recommended headers (API version, user agent with contact).
- Terms: is bulk retrieval or mirroring permitted? Note the URL of the policy.
- Anonymous versus authenticated access; what a token unlocks.

## Decide

Weigh the answers against what the corpus needs (`sources/README.md`,
"What a source must provide"):

- Enumerable public catalog with stable versioned downloads: build it.
- Artifacts on arbitrary third-party hosts (Joomla, Homebrew casks): tell the
  user; the fixed host allowlist does not fit, and an index-only mode is a
  design decision for them.
- Terms forbid bulk access (VS Code Marketplace, CurseForge): stop; add or
  confirm the entry under "Blocked or deferred".
- No listing at all and no third-party index: stop, or propose an explicit
  selector-only source and ask whether that is still useful.

## Facts block

Put this in your reply before scaffolding:

```
Registry:        Open VSX (open-vsx.org)
Catalog:         GET /api/-/search?size=100&offset=N&sortBy=timestamp; totalSize reported; offset cap ?
Item:            GET /api/{namespace}/{name}; allVersions map; files.download, files.sha256 (URL to a text file)
Versions:        GET /api/{namespace}/{name}/versions?size=N&offset=M, newest first
Download host:   open-vsx.org (302 to storage); persisted URL is the open-vsx.org one
Digest:          SHA-256, hex, in a side file fetched per version at listing time
Size:            not in the API; Size -1 unless a HEAD is acceptable
Metadata:        timestamp (updated_at), downloadCount (downloads), averageRating 0–5 (rating ×20), reviewCount (num_ratings)
Limits:          none documented; keep listing concurrency modest
Terms:           public API, no restriction found at <url>
History:         yes, version_depth supported
Not covered:     private/unlisted, Microsoft-only extensions
```
