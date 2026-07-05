# Changelog

All notable changes to VaultS3 are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows
semantic-ish versioning via git tags (`vMAJOR.MINOR.PATCH`).

## [Unreleased]

## [4.4.4] - 2026-07-05
### Fixed
- **S3 clients that omit the space after commas in the SigV4 Authorization header
  now authenticate** (issue #22). The header parser split on `", "` only, so clients
  like WinSCP and S3 Browser, which send `Credential=...,SignedHeaders=...,Signature=...`
  without spaces, failed with "missing auth parameters" and a 403. The parser now
  accepts commas with or without surrounding whitespace, per the SigV4 spec.
- **Dashboard IAM actions no longer error with "The string did not match the expected
  pattern"** (issue #23). Attaching a policy to a user, adding a user to a group, and
  attaching a policy to a group returned HTTP 200 with an empty body, and the dashboard
  parsed that empty body as JSON (which throws on Safari/WebKit). Those actions now
  return 204 No Content, and the dashboard tolerates empty success bodies.

## [4.4.3] - 2026-07-05
### Added
- **Login page improvements.** A remember-me option, a show/hide toggle for the secret
  key, and a dark-mode toggle on the login screen. When remember-me is left unchecked
  the session token is now kept only for the tab session and cleared when the tab
  closes, instead of always persisting. Contributed by @idpcks in #21.

## [4.4.2] - 2026-07-05
### Added
- **File browser grid view.** The object browser has a new grid layout with file-type
  icons, toggleable with the existing list view from the toolbar. The choice is
  remembered per browser. Contributed by @idpcks in #20.
- **Collapsible dashboard sidebar.** The desktop sidebar can collapse to an icon rail
  to give the content area more room. Contributed by @idpcks in #20.

### Fixed
- **Dragging an empty folder onto the upload dropzone no longer hangs.** It now reports
  that no files were found instead of spinning. Contributed by @idpcks in #20.
- **Dark-mode theme toggle icon is visible again.** It used an invalid Tailwind size
  class (`w-4.5`) that rendered it at zero size. Contributed by @idpcks in #20.

## [4.4.1] - 2026-07-02
### Added
- **Migration source presets in the dashboard.** The Migrate wizard now has a source
  type dropdown (MinIO, SeaweedFS, Garage, Ceph, AWS S3, Cloudflare R2, Wasabi,
  Backblaze B2, or any S3-compatible) that pre-fills the endpoint hint and the SigV4
  region, most importantly Garage's non-default region. The migrator already read any
  S3-compatible source, so this is discoverability, not new migration logic. Verified
  live against a real SeaweedFS S3 gateway and a real Garage cluster.

## [4.4.0] - 2026-07-02

A correctness, WORM, and stability release from a real-world test pass (boto3
against the core S3 API, advanced features, and the compression/encryption/packing
engines) plus an audit of the high-risk packages. Every fix has a regression test.

### Security
- **Object lock (WORM) is now enforced on delete.** The non-versioned delete path
  never checked retention or legal hold, so an object under a COMPLIANCE,
  legal-hold, or non-bypassed GOVERNANCE lock could be permanently deleted. Deletes
  of locked objects are now refused (with governance bypass honored), on both the
  retention API and the inline `x-amz-object-lock-*` PUT headers.
- **SigV4 auth no longer buffers the whole request body in memory.** Signature
  building read the entire upload into RAM (even for `UNSIGNED-PAYLOAD`, where the
  hash was discarded), so any caller with a valid access key could exhaust memory
  and every large upload was buffered rather than streamed. The client signed
  content hash is now used directly and the body streams through.
- **Bucket quota can no longer be undercut via `X-Amz-Decoded-Content-Length`.** An
  aws-chunked client could declare a tiny size to pass admission and then stream a
  much larger object. Quota is re-checked against the real decoded size.

### Fixed
- **CompleteMultipartUpload could destroy an existing object.** On the default
  (non-encrypted) path, assembly wrote straight to the final object path and removed
  it on a missing part, truncating or deleting whatever was already stored there,
  non-atomically, and shadowing packed-engine objects. Completion now assembles into
  a temp file and writes through the engine, so it is atomic, wrapper-aware
  (compression, encryption, packing), and never touches the target until the new
  object is fully assembled.
- **Range (206) responses no longer carry a whole-object checksum header.** Modern
  SDKs (boto3 1.36+, aws-cli v2) validate `x-amz-checksum-*` against the bytes they
  receive, so a whole-object checksum made every range download fail. The header is
  now emitted only on full (200) responses.
- **S3 Select now returns a proper AWS event stream.** It previously wrote raw
  CSV/JSON, which no S3 SDK can parse (they fail on the event-stream prelude
  checksum). Results are now framed as Records, Stats, and End messages with CRCs,
  and `CAST(col AS TYPE)` in predicates is supported.
- **Object lock buckets now behave like AWS.** Creating a bucket with object lock
  enabled auto-enables versioning (required for object lock), inline lock headers are
  applied on every PutObject path, and `GetObjectLockConfiguration` reports the true
  state (404 when object lock is not configured) instead of always claiming Enabled.
- **Dashboard bucket size/count no longer drift.** Version promote and delete now
  adjust the cached counters by the correct delta, and the one-time backfill reads
  the metadata index atomically, which is correct for versioned, compressed, and
  encrypted buckets (an engine filesystem walk counted on-disk bytes and skipped
  versioned data).
- **Third-party-signed presigned URLs with spaces now verify.** The presigned
  canonical query used Go's `+` for spaces instead of RFC 3986 `%20`, so a URL signed
  by boto3/aws-cli whose query carried a space (for example a
  `response-content-disposition` filename) failed verification.
- **`x-amz-meta-*` metadata keys are returned lowercased**, matching AWS, rather than
  Title-Cased.
- **Cluster: a node no longer routes to a dead peer after a restart.** The reverse
  proxy cache was keyed by node ID and never invalidated when a node's address
  changed, so it kept forwarding to the old address forever. The cache entry is now
  dropped when the address changes or the node leaves.
- **Backups can no longer run twice concurrently.** The scheduler used a
  load-then-store check instead of a compare-and-swap, so two triggers (or a trigger
  racing the ticker) could both start and write the same target directory.
- **Small-file packing: reads no longer fail during a volume roll.** `readFrame`
  released the lock between capturing the active file handle and reading it, so a
  concurrent roll could close the handle mid-read. The read now holds the lock and
  falls back to opening the sealed volume by path.
- **In-flight upload temp files (`.vaults3-tmp-*`) are excluded** from object listing
  and bucket-size walks.

### Added
- **ListObjectsV2 delimiter support.** The V2 listing now honors `delimiter` and
  returns `CommonPrefixes`, so folder-style browsing works for aws-cli, SDK
  paginators, and the dashboard file browser. The grouping is done at the sorted
  metadata index so it stays O(page) for large prefixes.

## [4.3.1] - 2026-06-30
### Fixed
- **CRITICAL: `aws-chunked` (streaming) uploads were stored corrupted.** Modern AWS
  SDKs (boto3/botocore 1.36+, aws-cli, aws-sdk-js v3) default to flexible checksums
  and, when the transport supports it, notably **HTTP/2, which Go negotiates for
  any TLS listener**, stream the body with `Content-Encoding: aws-chunked` and
  `x-amz-content-sha256: STREAMING-…-PAYLOAD`. VaultS3 didn't decode that framing,
  so the chunk-size headers + trailing checksum were written into the object itself
  (a 100-byte PUT stored as 142 bytes). Net effect: **uploads over HTTPS from recent
  SDKs were silently corrupted.** The request body is now de-chunked centrally
  before any handler reads it (covers PutObject, multipart UploadPart, POST). SigV4
  is unaffected (streaming modes sign the `STREAMING-…` literal, not the body).
  Verified over HTTPS with boto3 (0 B, 5 MB), aws-cli (incl. 60 MB multipart), and
  boto3 multipart, all byte-for-byte. HTTP path unchanged.
### Added
- **Separate port for the Dashboard vs the S3 API (issue #18).** Set
  `server.console_port` (e.g. `9001`) to serve the Web UI + its `/api/v1/` on a
  dedicated listener, leaving the S3 API on `server.port`, so each can have its
  own firewall rules, TLS, and reverse proxy (MinIO-style). Default `0` keeps
  everything on one port (unchanged). Env: `VAULTS3_CONSOLE_PORT` /
  `VAULTS3_CONSOLE_ADDRESS`.

## [4.3.0] - 2026-06-30
### Added
- **Per-bucket encryption keys (opt-in).** For bucket-per-tenant deployments, each
  bucket can now be encrypted with its own key that is **not shared** with other
  tenants, or opt out and stay plaintext. Enable with `encryption.per_bucket: true`
  (the configured `key` becomes a master KEK). A bucket provisions its own data key
  the first time it opts into SSE via `PUT /{bucket}?encryption`. Uses envelope
  encryption (KEK-wrapped per-bucket data keys, AES-256-GCM), supports key **rotation**
  and **crypto-shredding**, and keeps reading objects written before the switch via
  `encryption.legacy_key`. Managed from the dashboard's bucket page (enable / rotate /
  shred) and the `/api/v1/buckets/{b}/encryption` endpoints. See
  `docs/design/per-bucket-encryption.md`. Transparent to S3 clients. Opt-out buckets
  stay plaintext.
- **SSE-C (customer-provided encryption keys).** Operator-blind per-object encryption:
  clients pass `x-amz-server-side-encryption-customer-*` headers. The server
  encrypts/decrypts with the supplied key and stores only the key's MD5 (never the
  key). Wrong/missing key is rejected on GET/HEAD. (PUT/GET/HEAD on the non-versioned
  path.)
### Fixed
- **Multipart uploads now respect encryption.** `CompleteMultipartUpload` wrote the
  assembled object straight to disk, bypassing the encryption layer, so multipart
  (i.e. large) objects were stored **plaintext** even in encrypted buckets. The
  assembled object is now written through the engine, so per-bucket and SSE-S3/KMS
  encryption cover multipart objects too. (Non-encrypted deployments keep the fast
  direct path.)
- **Presigned URLs from standard S3 clients were rejected (`SignatureDoesNotMatch`).**
  The presigned-URL verifier encoded the canonical request path with a function
  that escaped `/` to `%2F`, while header-auth was already fixed (issue #9) to
  preserve slashes. Since every key path has slashes, presigned GET/PUT URLs from
  boto3 / aws-cli / the SDKs always failed. Now uses the per-segment path encoder,
  matching header auth, presigned GET/PUT verified end-to-end (incl. keys with
  `&`, `$`, spaces).
- **Object browser slow + capped on large buckets (issue #16 follow-up).** Two
  bugs in the dashboard file browser (`/api/v1/objects`):
  - *Backend:* for **non-versioned** buckets the listing fell back to a full
    `filepath.Walk` of the bucket **plus an MD5 hash of every file's contents** on
    every page request, so browsing a 500k-object bucket took minutes. It now
    reads the BoltDB metadata index (seek to page, O(pageSize)) like the S3 API
    already does, ~1.5ms per page regardless of bucket size.
  - *Frontend:* the browser fetched only the first page and ignored the
    `truncated`/continuation cursor, so only the first ~200 objects were ever
    visible. It now pulls 1,000 per request with a **Load more** control (server
    cursor `nextStartAfter`, folder roll-ups de-duplicated across pages), so the
    whole bucket is reachable.
  - *Folder-heavy buckets:* folders were rolled up **client-side** from a flat page,
    so a bucket with thousands of folders surfaced only a handful per page. Listing
    now collapses folders **server-side** (`ListLatestObjectsDelimited`) and seeks
    past each folder's contents, a folder level returns up to ~1,000 folders per
    page and is O(folders) instead of O(objects). Measured: a 5,000-folder bucket
    lists in 5 pages (~1.8ms/page) instead of hundreds.

## [4.2.22] - 2026-06-30
### Fixed
- **Slow dashboard pages with large buckets (issue #16).** The Home/Buckets/Stats/
  Cost pages computed storage + object count by walking the entire bucket on the
  filesystem (`BucketSize` → `filepath.Walk`) on **every** request, so cost scaled
  with object count, ~13s per page load at 1M objects (reproduced locally). They
  now read **maintained per-bucket counters** kept in the metadata store and
  updated incrementally on every write (put/overwrite/delete), so reads are O(1)
  regardless of object count. Existing data is backfilled with a single one-time
  walk on first load after upgrade, then never walked again. Measured: 12.8s →
  **0.4ms** at 1M objects, counts exact.

## [4.2.21] - 2026-06-29
### Added
- **Helm chart: Deployment mode + existing PVCs for backup/restore (issue #15).**
  A new `controller.kind` value selects `StatefulSet` (default) or `Deployment`
  (single-node), and `persistence.data.existingClaim` / `persistence.metadata.existingClaim`
  let you mount pre-created PVCs, e.g. claims restored from a Velero or k8up
  backup. Deployment-mode PVCs are annotated `helm.sh/resource-policy: keep` so
  they survive uninstall. Verified end-to-end on kind: write data → uninstall
  (PVCs kept) → reinstall with `existingClaim` → data intact. Deployment mode is
  guarded to single-node (incompatible with `cluster.enabled`/multi-replica).
- **Helm chart auto-clustering (Beta, issue #12 follow-up).** With
  `cluster.enabled=true` and `replicaCount>=3`, the StatefulSet now auto-forms a
  Raft cluster, pod-0 bootstraps as the initial leader and the rest auto-join it,
  with no manual bootstrap/join steps. A pod that restarts with a new IP
  re-announces itself automatically (the Raft server ID is the stable pod name.
  the address is the current pod IP). New `VAULTS3_CLUSTER_ENABLED/BOOTSTRAP/
  JOIN_ADDR/PEERS` env overrides drive the per-pod config, and a node-initiated
  `AutoJoin` (retry + leader-redirect) makes pod start order irrelevant.
- **Cluster metadata is now replicated across nodes via Raft consensus (Beta).**
  The API and S3 handlers depend on a `metadata.StoreAPI` interface. When
  clustering is on, the server injects a `DistributedStore` that commits every
  metadata write (bucket/object/version/IAM/…, all 58 command types) through the
  Raft log, so all nodes converge. Writes are accepted on **any** node: a write
  landing on a follower is transparently forwarded to the leader (new
  `/cluster/apply` endpoint), so there is no "write only to the leader" rule.
  Reads stay local. The data-placement hash ring tracks **live Raft membership**
  (it previously only saw statically-configured peers, so auto-clustered nodes
  placed object data inconsistently). Object reads proxy to the owning node across
  the cluster. **Dashboard** uploads place each file on its hash owner and
  downloads/deletes proxy to the owner, so the web UI is consistent with the S3
  path. Inter-node endpoints (`/cluster/join` `/leave` `/apply`) are authenticated
  with a **shared cluster secret** (the chart reuses the admin secret key).
  Verified end-to-end on a 3-node kind cluster: bucket create/delete on the leader
  **and** on a follower (via forwarding) replicate to every node. An object PUT on
  one node is byte-for-byte readable from another. A dashboard upload on one node
  is downloadable from another. 60 concurrent writes across all nodes are visible
  with full integrity from every node. Killing the leader elects a new one and
  writes continue. The recovered node rejoins and catches up to data written while
  it was down. Unauthenticated inter-node calls are rejected.
  **Beta:** clustering is functional but newer/less battle-tested than single-node
  + erasure coding, validate against your workload before trusting it as the only
  copy of critical data.

## [4.2.20] - 2026-06-29
### Security
- **Rebuilt on the patched Go 1.26.3 toolchain and updated `golang.org/x/*`
  dependencies to clear standard-library and dependency CVEs in the published
  Docker image.** The image was being built with an outdated Go 1.25.x toolchain
  (a stale `golang:1.25-alpine` base served from the CI build cache), which
  `govulncheck` flagged for 14 reachable stdlib vulnerabilities plus 2 in
  `golang.org/x/net`. Bumped the builder to `golang:1.26-alpine`, the CI/release
  Go to 1.26, `go.mod` to `go 1.26.0` (`toolchain go1.26.3`), and
  `x/net`→v0.56.0 / `x/crypto`→v0.53.0 / `x/text`→v0.38.0 / `x/sys`→v0.46.0.
  Reachable vulnerabilities drop from 16 to 2 (the last two are fixed only in the
  not-yet-released Go 1.26.4 and will clear automatically on the next rebuild).
  No application code changed.

### Added
- **S3 migration now carries over bucket policies and tags (IAM/policies
  migration).** Previously migration copied only buckets and objects. The access
  policy and tag set on each source bucket were left behind. Migration now fetches
  the source bucket's policy (`GET /{bucket}?policy`) and tags
  (`GET /{bucket}?tagging`) and applies them locally, so access control survives
  the move. Best-effort and standard-S3, works against MinIO, AWS S3, Garage, or
  any S3-compatible source. A bucket with no policy/tags (404) is not an error.
  The migration job now reports a `policies` count, surfaced in the dashboard.
  User/access-key migration is intentionally out of scope (it relies on each
  vendor's proprietary admin API, not the portable S3 API).

## [4.2.19] - 2026-06-29
### Fixed
- **S3 migration now preserves each object's original metadata instead of
  stamping today's date (issue #13).** Migrated objects kept their content but were
  written with `LastModified = now`, so a migration looked like everything was
  created on migration day, breaking lifecycle rules, sort-by-date, and audit
  trails. Migration now carries over the source's original modified time, user
  metadata (`x-amz-meta-*`), and content headers (Content-Encoding/Disposition/
  Cache-Control/Language), and stamps the on-disk file mtime to match so every
  surface (dashboard, S3 `HEAD`/`GET`/`ListObjectsV2`) reflects the real date.
  Because VaultS3's migrator writes directly to its own store (not via PutObject),
  it can preserve the original date where `mc mirror --preserve` structurally
  cannot. Also fixed: the migrator now disables transparent response
  decompression, so gzip-encoded source objects are copied verbatim rather than
  silently decoded while keeping their `Content-Encoding: gzip` header.

## [4.2.18] - 2026-06-29
### Added
- **Kubernetes deployment (issue #12).** A Helm chart (`deploy/helm/vaults3/`) and
  a no-Helm plain-manifest quickstart (`deploy/k8s/quickstart.yaml`). Deploys
  VaultS3 as a StatefulSet with admin keys from a Secret, `vaults3.yaml` from a
  ConfigMap, persistent volumes for `/data` and `/metadata`, liveness/readiness
  probes on `/health` and `/ready`, a non-root securityContext, and opt-in Ingress
  and Prometheus ServiceMonitor. Validated with `helm lint` + `kubeconform` and
  deployed end-to-end on a live cluster (StatefulSet rollout, bound PVCs, probes,
  Secret-injected credentials, and data surviving a pod restart).

## [4.2.17] - 2026-06-29
### Fixed
- **Objects uploaded or deleted through the web dashboard were never replicated
  to peers (issues #10, #11).** Only writes via the S3 API enqueued
  replication events. The dashboard upload/delete handlers did not, so a user who
  added files through the UI saw `last synced: never` and zero objects on the
  target. The dashboard mutation paths (upload, single delete, bulk delete) now
  enqueue replication events through the same callback as the S3 API, for both
  push and active-active modes. Note: this also means the **target instance does
  not need replication enabled**, one-way push only requires replication on the
  source plus valid peer credentials on the target.

## [4.2.16] - 2026-06-29
### Fixed
- **Replication dashboard showed "No replication peers configured" despite peers
  being set in `vaults3.yaml` (issue #10).** The replication status endpoint built
  its peer list from status records instead of the configured peers, so a peer
  that hadn't replicated anything yet (no status record) was invisible, even
  though the worker had loaded it (`peers=N` in the log). It now lists the
  configured peers and enriches each with its live status, so a freshly-configured
  peer shows immediately (with zero activity until it syncs).

## [4.2.15] - 2026-06-29
### Added
- **Small-file packing (experimental, issue #7).** A new `packing` storage mode
  packs objects up to `max_object_size` into large append-only **volume** files, 
  each object an independent zstd frame, with byte-offset locations in a BoltDB
  index, to avoid the per-file overhead (inodes, syscalls, disk blocks) of
  millions of tiny objects. Larger objects fall through to individual files.
  Deleted/overwritten objects leave dead space that is reclaimed by background
  **compaction** (configurable interval) or on demand via `POST /api/v1/compact`.
  Crash-safe (frames fsync'd before the index commit) and concurrency-safe
  (compare-and-swap repointing, read-lock during volume deletion). Off by default.
  configured under `packing:` in vaults3.yaml. Not yet composable with encryption
  or erasure coding (skipped, with a warning, if either is enabled). This is the
  packing half of #7. The codec half (gzip→zstd) is below.

### Changed
- **Object compression now uses Zstandard (zstd) instead of gzip (issue #7).**
  New objects are written with zstd, better compression ratio and speed.
  Objects written by older gzip builds are still read transparently (the codec is
  detected by magic number), so there is no migration and nothing breaks. Data
  written while compression was off is passed through unchanged. The same 1GB
  decompressed-size cap (decompression-bomb protection) and excluded file types
  apply. (`klauspost/compress`, already in the dependency tree.)

## [4.2.12] - 2026-06-28
### Added
- **Sidebar version indicator (issue #8).** The dashboard sidebar now shows the
  running version (from `GET /api/v1/version`) with a subtle "update available"
  dot when a newer release exists, linking to the releases page, so it's obvious
  at a glance which version you're on.
- **Cancel a running migration (issue #8).** The Migrate page shows a Cancel
  button on in-progress jobs (`POST /api/v1/migrate/cancel`). Cancellation takes
  effect between objects, any in-flight object copy finishes first, so no
  partial objects are left behind, and the job ends in a `cancelled` state.
  Starting an identical migration (same source + buckets) while one is already
  running is now rejected, so accidental double-clicks no longer spawn parallel
  copies (the Migrate button also disables while that source is busy).

### Changed
- **Docker images and `make build` now embed the build version** (`-ldflags -X
  main.version`), so the sidebar version indicator and `-version` show the real
  release (e.g. `v4.2.12`) instead of `dev`. Previously only the GitHub Release
  binaries injected it, so Docker/source builds reported `dev`.

## [4.2.11] - 2026-06-28
### Fixed
- **Object keys with `&`, `$`, or spaces broke SigV4 auth (issue #9).** VaultS3
  built the SigV4 canonical URI from the raw request path, which leaves
  sub-delimiters like `&` and `$` literal, but standard S3 clients (boto3,
  aws-cli, the AWS SDKs) percent-encode them strictly (`&`→`%26`, `$`→`%24`,
  space→`%20`, …). The signatures therefore didn't match → `SignatureDoesNotMatch`
  / `AccessDenied` for any key with special characters. This affected both
  directions and is now fixed everywhere the canonical URI is computed:
  - **Server** (`internal/s3` auth), now validates with strict per-segment
    encoding, so standard S3 clients can read/write special-character keys.
  - **Migrate source client** (`internal/migrate`), signs strictly, so
    migrating such keys from external S3 (the reported case) succeeds.
  - **Replication, FUSE, and CLI** clients, sign strictly too, so they keep
    working against the now-strict server.
  Keys without special characters are unaffected (strict == raw for them).
  Verified end-to-end live (boto3 PUT + cross-instance migration of a key with
  `&`, `$`, and spaces) plus regression tests on both the client and server sides.

## [4.2.10] - 2026-06-28
### Fixed
- **`ListObjectsV2` pagination was broken (no continuation token).** The handler
  set `IsTruncated` but never emitted a `NextContinuationToken`, and ignored an
  incoming `continuation-token`, so S3 clients (boto3, the AWS SDKs) could not
  page past the first response and never saw more than `max-keys` objects. The
  V2 handler now reads `continuation-token` and returns `NextContinuationToken`
  (an opaque cursor), so standard continuation-token pagination works to any
  depth. Verified end-to-end with boto3 across multi-page listings. (V1
  marker-based pagination already worked.)

### Changed
- **Listing now scales to very large buckets (millions of objects under one
  prefix).** `ListObjectsV2`/`V1` previously read the entire prefix range into
  memory and sorted it on every page, `O(n)` per page, which falls over at high
  object counts. Listing now seeks straight to the continuation marker in the
  sorted BoltDB index and reads only one page forward (`O(log n + page_size)`),
  with memory bounded by the page size. Page latency is flat (~0.7 ms for a
  1000-key page), measured (not extrapolated) from 1,000 to 100,000,000 objects
  in a single prefix. All listing (versioned and non-versioned) now goes through
  this metadata index instead of an `O(n)` filesystem walk. See
  `docs/SCALING.md` §11.

## [4.2.9] - 2026-06-28
### Added
- **Bucket snapshots ("git-for-buckets")**: a new `internal/snapshot` package plus
  a dashboard panel on each bucket: capture the bucket's state (commit), diff it
  against the live bucket, and roll back (restore) in one click, git-style history
  built on object versioning, with no external stack (vs. lakeFS, which needs a
  separate server + database). Restore re-points version pointers (no data
  deleted), so it resurrects deleted objects and is itself reversible. API under
  `/api/v1/buckets/{bucket}/snapshots`. Requires bucket versioning.

### Fixed
- The dashboard is now **version-aware** for object operations on versioned
  buckets: uploads create versions, downloads/zips resolve the latest version,
  and deletes write a delete marker (recoverable) instead of failing. Previously
  these used the unversioned path and broke on versioned buckets.

## [4.2.8] - 2026-06-28
### Added
- **Cost estimator**: a dashboard "Cost" page (and `GET /api/v1/tco`) that
  estimates the monthly/yearly cost of your live stored data on AWS S3, Google
  Cloud Storage, Cloudflare R2, Backblaze B2, and Wasabi (storage + adjustable
  egress) against self-hosting with VaultS3 (egress-free, $0). Pricing rates come
  from the server. The egress slider recomputes instantly client-side.
### Changed
- **Migration is now streaming + resilient (issue #6).** The migrator streams each
  object straight from the source into the local engine instead of buffering the
  whole body in memory (no more OOM risk on large objects), and retries transient
  source failures (HTTP 5xx / 429 / network errors) with exponential backoff, 
  while leaving permanent errors (4xx) to fail fast. Listing is retried too.

## [4.2.7] - 2026-06-28
### Added
- **Auto-update (opt-in)**: a new `internal/selfupdate` package checks GitHub
  Releases on a daily interval and surfaces a **dashboard banner** when a newer
  version is out (`GET /api/v1/version`). With `auto_update.apply: true` it also
  downloads the release for the running platform, **verifies its SHA-256 checksum**
  (refuses to install otherwise), atomically swaps the binary, and re-execs into
  the new version, never crossing a major version automatically. Updates only
  ever replace the binary. Object data, metadata, and config are untouched. Skips
  self-apply inside Docker (use Watchtower, documented in the README). Configure
  under `auto_update:` in vaults3.yaml (disabled by default).

## [4.2.6] - 2026-06-28
### Added
- **Migrate from S3 (`internal/migrate`)**: import buckets and objects from any
  S3-compatible source (MinIO, AWS S3, Garage…) into VaultS3. A SigV4 source
  client (no AWS SDK) plus an async migrator with per-job progress, exposed via
  `POST /api/v1/migrate/test`, `POST /api/v1/migrate`, `GET /api/v1/migrate/jobs`
  and a dashboard wizard (Migrate page: connect → select buckets → live progress).
- **Dashboard semantic search**: the Search page now has a Keyword / Semantic
  toggle. Semantic mode queries the vector store and shows results ranked by
  cosine similarity (greys out with a hint when vector search is disabled).
- Settings page surfaces the Vector Search, Erasure Coding, and Clustering
  feature flags in its read-only status panel.

## [4.2.5] - 2026-06-28
### Added
- **Semantic / vector search (optional add-on)**: a new `internal/vector` package
  brings RAG-style retrieval into the single binary, with no external vector
  database. A dependency-free cosine-kNN index (persisted across restarts) is fed
  by any OpenAI-compatible `/v1/embeddings` endpoint (OpenAI, Ollama, llama.cpp,
  LM Studio, vLLM…), so users pick their own (often local, private) embedding
  model. Text objects are auto-embedded on upload (best-effort, off the request
  path). Query via `POST /api/v1/vectors/query`, status via
  `GET /api/v1/vectors/status`. Configure under `vector:` in vaults3.yaml
  (disabled by default).

### Fixed
- **Conditional writes are now atomic.** `If-Match` / `If-None-Match` on PutObject
  previously checked the precondition and wrote in separate steps (a TOCTOU race):
  concurrent `If-None-Match: *` creates to the same key could all succeed,
  breaking the compare-and-swap guarantee that makes conditional writes usable for
  lock files and Iceberg-style commits. Writes carrying a conditional header now
  hold a per-key striped lock across the check-and-write, so exactly one create
  wins. Regression test spins up 16 concurrent creators and asserts 1×200 + 15×412.

## [4.2.4] - 2026-06-28
### Added
- Fault-injection / consensus test coverage for the data-durability subsystems
  that previously had little or none, and the last seven untested packages, so
  **every `internal/` package now has tests**:
  - **erasure**: Reed-Solomon encode/reconstruct, lost-disk reads, and the
    background healer repairing degraded objects (0% → ~64%).
  - **cluster**: consistent-hash ring + failure-detector state machine, plus a
    real multi-node **Raft consensus** harness (in-memory transport): leader
    election, log replication, no-split-brain under network partition, and
    membership changes (14.9% → 22.5%).
  - **replication**: vector-clock causality/merge and all three conflict
    resolution strategies (last-writer-wins, largest-object, site-preference).
  - **tiering** (0% → ~39%), **backup** (0% → ~48%), **fuse** (0% → ~45%).
  - **metrics, lambda, batch, inventory, scanner, accesslog, dashboard**: baseline
    coverage for the remaining packages.
- `docs/BENCHMARKS.md`, reproducible benchmark methodology (the `/speedtest`
  endpoint, `warp` for comparative throughput, RSS measurement) + results template.
- README **Production Readiness** section (stable vs. beta paths) and a
  refreshed competitor comparison verified against June 2026 sources.
- `CONTRIBUTING.md`, `CHANGELOG.md`, and GitHub issue/PR templates.

### Fixed
- **Tiering deadlock (data-availability bug):** the background tier scan called
  `SetObjectTier` (a write transaction) from inside `IterateAllObjects` (a read
  transaction), which deadlocks BoltDB, the scan would hang forever the first
  time it tried to migrate any object to cold. `scan()` now collects candidates
  inside the read txn and migrates them after it closes. Found by the new
  tiering tests.

### Changed
- `internal/cluster`: extracted a `newNodeWithDeps` seam so the Raft transport
  and stores are injectable (enables the in-process consensus tests). The
  production `NewNode` path is unchanged (TCP transport + BoltDB).
- Competitor comparison table corrected: SeaweedFS now has a web admin UI and a
  working FUSE mount. MinIO's Community console was removed (2025) and the
  open-source repo archived (Feb 2026). Added an "as of June 2026" qualifier.
- Stopped tracking build artifacts and logs in git (`vaults3-cli`,
  `bin/vaults3-cli`, `access.log`, `test-results/`). Added `*.log` and
  `test-results/` to `.gitignore`.

## [4.2.3] - 2026-06-26
### Added
- `docs/SCALING.md` operations guide: multi-disk erasure coding, multi-node
  Raft cluster setup, and lost-disk / lost-server / quorum-loss runbooks.
### Fixed
- `POST /api/v1/heal` was a stub that only acked the request. It now invokes the
  erasure healer (`Healer.Heal(bucket, prefix)`) asynchronously. (issue #5)

## [4.2.2] - 2026-06-16
### Security
- Removed esbuild from the dependency tree (Dependabot #16, GHSA-gv7w-rqvm-qjhr)
  by upgrading `vite` 6→8 and `@vitejs/plugin-react` 4→6. Vite 8 uses the
  Rolldown bundler instead of esbuild.

## [4.2.1] - 2026-06-06
### Security
- Bumped `react-router-dom` 7.13.0 → 7.17.0, clearing 6 Dependabot alerts
  (turbo-stream RCE, RSC/Location XSS, manifest/single-fetch DoS, open redirect).

## [4.2.0] - 2026-05-31
### Security
- Bumped `postcss` 8.5.6 → 8.5.15 (Dependabot, dev dependency).

## [4.1.0] - 2026-04-02
### Fixed
- Four dashboard bugs: bucket stats drift, empty file browser listing, search
  result timestamps showing 1970, and a `/dashboard/buckets/` redirect loop.
- Versioned `ListObjectsV2`/`V1` returning empty results for versioned buckets.

## [4.0.0] - 2026-02-28
### Added
- "Change Admin Credentials" feature in the dashboard Settings page.
- Distributed/enterprise features: erasure coding, Raft clustering,
  active-active replication, tiering, and backup.

## [3.0.0] - 2026-02-28
### Added
- SSE-KMS encryption, AMQP/PostgreSQL event notifications, and Parquet support
  for S3 Select.

## [2.0.0] - 2026-02-28
### Added
- Expanded S3 API surface and dashboard features.

## [1.0.0] - 2026-02-25
### Added
- First public release: S3-compatible object storage server with built-in web
  dashboard, CLI, versioning, WORM, notifications, full-text search, FUSE mount,
  and multi-platform release binaries + Docker images.

[Unreleased]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.4...HEAD
[4.4.4]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.3...v4.4.4
[4.4.3]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.2...v4.4.3
[4.4.2]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.1...v4.4.2
[4.4.1]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.0...v4.4.1
[4.4.0]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.3.1...v4.4.0
[4.3.1]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.3.0...v4.3.1
[4.3.0]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.23...v4.3.0
[4.2.23]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.22...v4.2.23
[4.2.22]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.21...v4.2.22
[4.2.21]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.20...v4.2.21
[4.2.20]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.19...v4.2.20
[4.2.19]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.18...v4.2.19
[4.2.18]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.17...v4.2.18
[4.2.17]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.16...v4.2.17
[4.2.16]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.15...v4.2.16
[4.2.15]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.12...v4.2.15
[4.2.12]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.11...v4.2.12
[4.2.11]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.10...v4.2.11
[4.2.10]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.9...v4.2.10
[4.2.9]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.8...v4.2.9
[4.2.8]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.7...v4.2.8
[4.2.7]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.6...v4.2.7
[4.2.6]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.5...v4.2.6
[4.2.5]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.4...v4.2.5
[4.2.4]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.3...v4.2.4
[4.2.3]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.2...v4.2.3
[4.2.2]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.1...v4.2.2
[4.2.1]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.0...v4.2.1
[4.2.0]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.1.0...v4.2.0
[4.1.0]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.0.0...v4.1.0
[4.0.0]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v3.0.0...v4.0.0
[3.0.0]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v2.0.0...v3.0.0
[2.0.0]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v1.0.0...v2.0.0
[1.0.0]: https://github.com/Kodiqa-Solutions/VaultS3/releases/tag/v1.0.0
