# Jenkins registry plugin

Independent Go module for archiving the plugins (`.hpi`) published through the
[Jenkins update center](https://updates.jenkins.io). Its only Dug dependency is
`github.com/rezen/dug/sdk`; it does not link the host or the S3 engine.

```sh
go test ./...
go build -o dug-jenkins ./cmd/dug-jenkins
./dug-jenkins --listen 127.0.0.1:7792
```

The host connects with registry identity `jenkins` and that plugin URL.
Options:

| Option | Meaning |
| --- | --- |
| `plugins` | Comma-separated plugin short names such as `git,workflow-cps`; an unknown name is an error |
| `version_depth` | Positive integer, inclusive `YYYY-MM-DD` cutoff, or `unlimited`; default `1` |

With no `plugins`, discovery walks every plugin in the update center (2,131 and
38,234 versions on 2026-11-11). It makes two requests: `current/update-center.actual.json`
(3.5 MB, the current release of every plugin) and, only when `version_depth` is
above 1, `current/plugin-versions.json` (24 MB, every version). Each is capped at
32 MiB and 128 MiB. The current release is emitted first, then older versions
newest release first, because version strings are not semver. Discovery pins the
download URL and SHA-256 per version, and the size for current releases only; the
update center publishes no size for older ones, so those blobs carry size -1 and the
host verifies the digest. Resolution makes no request.

Two historical entries (`lucene-search` 5.1, `servicenow-devops` 1.35) are keyed by a
normalized version but carry another version string and URL (`5.01`, `1.35.0`); the
plugin uses the entry's own version, as its download URL does.

Not covered: plugins the update center does not list and the per-core update center
directories (`/2.440/` and similar).

Both documents are signed, and the signature is verified before anything in them is
read, so a bad mirror or a tampered copy fails the listing. The signed bytes are the
document without its top-level `signature` member, written compactly with keys in
document order (`signature.go` reproduces that form). The member's certificate must
chain to one of the two Jenkins update center roots embedded in `roots/` (copied
from Jenkins core; see its README for provenance and fingerprints), and its
SHA-512 digest and RSA signature must match. The legacy SHA-1 pair is ignored. The
check fails closed: unsigned, altered, reordered, untrusted or expired is an error,
and it is retryable, since a mirror can briefly serve a stale copy. The signing
certificate expires 2027-01-14; the update center rotates it, but a listing fails
if upstream ever serves an expired one. This protects the metadata, which includes
every pinned digest; the artifacts themselves are covered by those digests.

Metadata and facts, all listing-time snapshots attached to every version of a plugin
except where noted:

- `installs`: the install count Jenkins controllers report (`popularity`), so
  `--order popular` works. The update center publishes no download counts or
  ratings, so `downloads` and `rating` orders have nothing to sort on.
- `updated_at`: the current release time. `released_at`: that version's own release
  time, also the basis of date `version_depth`.
- `repository`: the plugin's `scm` link, declared and unverified.
- Tags are the plugin's labels; the author is its first listed developer.
- `security_warnings`: comma-separated advisory IDs (`SECURITY-258`) from the update
  center's `warnings` list whose version pattern matches this version; absent when
  none do. It is a snapshot of what Jenkins knows at listing time, so re-list to
  refresh it. Core advisories are ignored.
- `deprecated` (`true` or `false`, the list is exhaustive) and `deprecation_url`.
- `health_score` (0–100), `required_core` (per version), `version_rank` (1 is the
  current release) and `release_date_source` are string metadata.

Credentials and S3 configuration belong to the host. `DUG_PLUGIN_TOKEN` optionally
protects the Connect service. The update center needs no credentials. Downloads are
pinned to `updates.jenkins.io`, which redirects to `get.jenkins.io` and then to a
mirror; the digest comes from the update center and the host verifies it.

Local development uses the workspace's unpublished SDK. To extract this source,
publish and pin an SDK version, move this directory, and update its module path
and command self-import. No host changes are needed.

Terms: checked 2026-11-11, none of `updates.jenkins.io`, the update-center2
README or the mirrors page states a rate limit or a policy on automated or bulk
downloads, and none forbids it. The project welcomes download mirrors through an
allowlisted rsync program (<https://www.jenkins.io/download/mirrors/>), which this
plugin is not. The update center page warns that many files on the site "are
intended for internal use by the Jenkins project and may change at any time", so
the daily live test is what notices a layout change. Discovery makes two requests
and downloads only what is selected; keep `--concurrency` modest.

`just e2e-jenkins` runs a live stage-one and stage-two pass against a local S3
(git and credentials, two versions each), checks every stored blob against its
manifest, and re-runs to confirm resume skips everything.

API reference: <https://github.com/jenkins-infra/update-center2> (the generator of
`update-center.json` and `plugin-versions.json`) and <https://updates.jenkins.io>.
