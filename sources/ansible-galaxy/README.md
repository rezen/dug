# Ansible Galaxy registry plugin

Independent Go module for archiving the collection tarballs published on
[Ansible Galaxy](https://galaxy.ansible.com). Its only Dug dependency is
`github.com/rezen/dug/sdk`; it does not link the host or the S3 engine.

```sh
go test ./...
go build -o dug-ansible-galaxy ./cmd/dug-ansible-galaxy
./dug-ansible-galaxy --listen 127.0.0.1:7787
```

The host connects with registry identity `ansible-galaxy` and that plugin URL.
Options:

| Option | Meaning |
| --- | --- |
| `collections` | Comma-separated fully qualified `namespace.name` collections |
| `namespaces` | Comma-separated namespaces; every published collection in each |
| `page_size` | Index and version-list page size, 1–100; default 100 (the API clamps larger limits) |
| `version_depth` | Positive integer, inclusive `YYYY-MM-DD` cutoff, or `unlimited`; default 1 |

`collections` and `namespaces` are mutually exclusive. With neither, the plugin
walks the whole public catalog of published collections. Unknown keys are errors.

## Listing and downloading everything

From the repository root, `just list ansible-galaxy` starts the plugin and local
rclone S3, prints the catalog, and stops the plugin. Extra arguments are host flags,
such as `--limit 20` or `--option namespaces=community`.

To download every collection tarball, run one command. `sync` lists, saves the
inventory, then downloads with a completion barrier between the stages. Docker
must be running for `local` mode.

```sh
just sync ansible-galaxy
```

Like `list`, it starts the plugin and local rclone S3 server and stops the plugin when done.
To manage the plugin yourself, run `just plugin ansible-galaxy` in one terminal
and `just local sync ansible-galaxy --plugin-url http://127.0.0.1:7787` in another.

By default only the highest version of each collection is archived. Add
`--version-depth unlimited` for every version of every collection, or a number or
`YYYY-MM-DD` cutoff for a bounded history. Discovery costs one request per emitted
version (see Enumeration), so an unlimited run is far larger than the roughly
half-hour depth-1 walk. Try `--list-limit 20` first.

Outside local mode, pass `--bucket`, `--endpoint` and credentials as for any
other source. Rerunning `sync` resumes: completed resources are skipped unless
`--refresh` is given. Narrow a run with `--option namespaces=...`, or order and
filter the download with `--order downloads.total` and
`--where 'downloads.total >= 100000'`.

## Enumeration

The index at `/api/v3/plugin/ansible/content/published/collections/index/` is
ordered by collection creation time, newest first, and accepts no sort order or
date filter. It reports a total, pages by offset, and returns site-relative
`next` links. The plugin follows only links on the API origin, fails on a
repeated page, drops duplicates by `namespace.name`, and fails when a walk ends
having seen fewer collections than the API reports. Because creation time never
changes, a collection published mid-walk shifts later offsets (a duplicate, which
is dropped) and cannot hide an existing one. A collection deleted mid-walk could
still shift a later one out of reach; the count check reports that as an error
and a rerun fixes it.

A collection or version that disappears between the index page and its own
request is skipped with a warning during a full walk. The same condition on an
explicitly selected collection is an error.

Version history: the highest version is always emitted first, then older
versions in the API's listing order (upload time, newest first), so a backport
release can follow a newer major. Integer depth caps the count per collection
and date depth keeps versions uploaded on or after the cutoff, using the
listing's timestamps so versions outside the cutoff cost no request.

Discovery pins the download URL, SHA-256 and size from each version's own
document, which is the only place Galaxy publishes them. That makes one request
per emitted version, each a few hundred KB of JSON uncompressed, so a full depth-1
walk is about 4,700 requests and roughly half an hour. Resolution makes no
network request and fails rather than substituting a newer version when
upstream removes one.

Not covered: Galaxy roles (a different catalog on API v1, installed from GitHub
repositories), unpublished, staging or rejected repositories, Automation Hub, and
private namespaces.

## Metadata

Typed facts, as listing-time snapshots attached to every version of a collection:

- `downloads.total`: Galaxy's lifetime download count for the collection. Galaxy
  publishes no trailing windows and no per-version count, so those stay absent.
- `updated_at`: when the collection last changed.
- `released_at`: the version's creation time on Galaxy (`galaxy-version-created-at`).
- `repository`: the repository declared in the collection's `galaxy.yml`, source
  `declared`, with the `git_commit_sha` the tarball records when present.

`tags` come from the collection's `galaxy.yml` tags and `author.username` is the
namespace. Registry-specific metadata: `deprecated` (`true` or `false`),
`requires_ansible`, `license` (comma-joined), `marks` (the API's version marks,
omitted when empty), `version_rank` (1 is the highest version) and
`release_date_source`. Galaxy publishes no ratings or install base.

Credentials and S3 configuration belong to the host. `DUG_PLUGIN_TOKEN`
optionally protects the Connect service. Galaxy needs no credentials for public
collections. Artifacts are served from the persisted `galaxy.ansible.com` URL,
which redirects first to a signed Galaxy content URL and then to S3; the
allowlist applies to the persisted URL, which must be the
`artifacts/NAMESPACE-NAME-VERSION.tar.gz` route for exactly the referenced
version. The SHA-256 is Galaxy's, verified by the host against the bytes; Galaxy's
own collection signatures are not checked.

Local development uses the workspace's unpublished SDK. To extract this source,
publish and pin an SDK version, move this directory, and update its module path
and command self-import. No host changes are needed.

API reference: <https://galaxy.ansible.com/api/> (the v3 `pulp_ansible` content
API; see <https://docs.ansible.com/ansible/latest/galaxy/dev_guide.html>).
