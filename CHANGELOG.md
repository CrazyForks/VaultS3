# Changelog

All notable changes to VaultS3 are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows
semantic-ish versioning via git tags (`vMAJOR.MINOR.PATCH`).

## [Unreleased]

## [4.2.17] - 2026-06-29
### Fixed
- **Objects uploaded or deleted through the web dashboard were never replicated
  to peers (issues #10, #11).** Only writes via the S3 API enqueued
  replication events; the dashboard upload/delete handlers did not, so a user who
  added files through the UI saw `last synced: never` and zero objects on the
  target. The dashboard mutation paths (upload, single delete, bulk delete) now
  enqueue replication events through the same callback as the S3 API, for both
  push and active-active modes. Note: this also means the **target instance does
  not need replication enabled** — one-way push only requires replication on the
  source plus valid peer credentials on the target.

## [4.2.16] - 2026-06-29
### Fixed
- **Replication dashboard showed "No replication peers configured" despite peers
  being set in `vaults3.yaml` (issue #10).** The replication status endpoint built
  its peer list from status records instead of the configured peers, so a peer
  that hadn't replicated anything yet (no status record) was invisible — even
  though the worker had loaded it (`peers=N` in the log). It now lists the
  configured peers and enriches each with its live status, so a freshly-configured
  peer shows immediately (with zero activity until it syncs).

## [4.2.15] - 2026-06-29
### Added
- **Small-file packing (experimental, issue #7).** A new `packing` storage mode
  packs objects up to `max_object_size` into large append-only **volume** files —
  each object an independent zstd frame, with byte-offset locations in a BoltDB
  index — to avoid the per-file overhead (inodes, syscalls, disk blocks) of
  millions of tiny objects. Larger objects fall through to individual files.
  Deleted/overwritten objects leave dead space that is reclaimed by background
  **compaction** (configurable interval) or on demand via `POST /api/v1/compact`.
  Crash-safe (frames fsync'd before the index commit) and concurrency-safe
  (compare-and-swap repointing, read-lock during volume deletion). Off by default;
  configured under `packing:` in vaults3.yaml. Not yet composable with encryption
  or erasure coding (skipped, with a warning, if either is enabled). This is the
  packing half of #7; the codec half (gzip→zstd) is below.

### Changed
- **Object compression now uses Zstandard (zstd) instead of gzip (issue #7).**
  New objects are written with zstd — better compression ratio and speed.
  Objects written by older gzip builds are still read transparently (the codec is
  detected by magic number), so there is no migration and nothing breaks; data
  written while compression was off is passed through unchanged. The same 1GB
  decompressed-size cap (decompression-bomb protection) and excluded file types
  apply. (`klauspost/compress`, already in the dependency tree.)

## [4.2.12] - 2026-06-28
### Added
- **Sidebar version indicator (issue #8).** The dashboard sidebar now shows the
  running version (from `GET /api/v1/version`) with a subtle "update available"
  dot when a newer release exists, linking to the releases page — so it's obvious
  at a glance which version you're on.
- **Cancel a running migration (issue #8).** The Migrate page shows a Cancel
  button on in-progress jobs (`POST /api/v1/migrate/cancel`). Cancellation takes
  effect between objects — any in-flight object copy finishes first, so no
  partial objects are left behind — and the job ends in a `cancelled` state.
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
  sub-delimiters like `&` and `$` literal — but standard S3 clients (boto3,
  aws-cli, the AWS SDKs) percent-encode them strictly (`&`→`%26`, `$`→`%24`,
  space→`%20`, …). The signatures therefore didn't match → `SignatureDoesNotMatch`
  / `AccessDenied` for any key with special characters. This affected both
  directions and is now fixed everywhere the canonical URI is computed:
  - **Server** (`internal/s3` auth) — now validates with strict per-segment
    encoding, so standard S3 clients can read/write special-character keys.
  - **Migrate source client** (`internal/migrate`) — signs strictly, so
    migrating such keys from external S3 (the reported case) succeeds.
  - **Replication, FUSE, and CLI** clients — sign strictly too, so they keep
    working against the now-strict server.
  Keys without special characters are unaffected (strict == raw for them).
  Verified end-to-end live (boto3 PUT + cross-instance migration of a key with
  `&`, `$`, and spaces) plus regression tests on both the client and server sides.

## [4.2.10] - 2026-06-28
### Fixed
- **`ListObjectsV2` pagination was broken (no continuation token).** The handler
  set `IsTruncated` but never emitted a `NextContinuationToken`, and ignored an
  incoming `continuation-token` — so S3 clients (boto3, the AWS SDKs) could not
  page past the first response and never saw more than `max-keys` objects. The
  V2 handler now reads `continuation-token` and returns `NextContinuationToken`
  (an opaque cursor), so standard continuation-token pagination works to any
  depth. Verified end-to-end with boto3 across multi-page listings. (V1
  marker-based pagination already worked.)

### Changed
- **Listing now scales to very large buckets (millions of objects under one
  prefix).** `ListObjectsV2`/`V1` previously read the entire prefix range into
  memory and sorted it on every page — `O(n)` per page, which falls over at high
  object counts. Listing now seeks straight to the continuation marker in the
  sorted BoltDB index and reads only one page forward (`O(log n + page_size)`),
  with memory bounded by the page size. Page latency is flat (~0.7 ms for a
  1000-key page) — measured (not extrapolated) from 1,000 to 100,000,000 objects
  in a single prefix. All listing (versioned and non-versioned) now goes through
  this metadata index instead of an `O(n)` filesystem walk. See
  `docs/SCALING.md` §11.

## [4.2.9] - 2026-06-28
### Added
- **Bucket snapshots ("git-for-buckets")** — a new `internal/snapshot` package plus
  a dashboard panel on each bucket: capture the bucket's state (commit), diff it
  against the live bucket, and roll back (restore) in one click — git-style history
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
- **Cost estimator** — a dashboard "Cost" page (and `GET /api/v1/tco`) that
  estimates the monthly/yearly cost of your live stored data on AWS S3, Google
  Cloud Storage, Cloudflare R2, Backblaze B2, and Wasabi (storage + adjustable
  egress) against self-hosting with VaultS3 (egress-free, $0). Pricing rates come
  from the server; the egress slider recomputes instantly client-side.
### Changed
- **Migration is now streaming + resilient (issue #6).** The migrator streams each
  object straight from the source into the local engine instead of buffering the
  whole body in memory (no more OOM risk on large objects), and retries transient
  source failures (HTTP 5xx / 429 / network errors) with exponential backoff —
  while leaving permanent errors (4xx) to fail fast. Listing is retried too.

## [4.2.7] - 2026-06-28
### Added
- **Auto-update (opt-in)** — a new `internal/selfupdate` package checks GitHub
  Releases on a daily interval and surfaces a **dashboard banner** when a newer
  version is out (`GET /api/v1/version`). With `auto_update.apply: true` it also
  downloads the release for the running platform, **verifies its SHA-256 checksum**
  (refuses to install otherwise), atomically swaps the binary, and re-execs into
  the new version — never crossing a major version automatically. Updates only
  ever replace the binary; object data, metadata, and config are untouched. Skips
  self-apply inside Docker (use Watchtower — documented in the README). Configure
  under `auto_update:` in vaults3.yaml (disabled by default).

## [4.2.6] - 2026-06-28
### Added
- **Migrate from S3 (`internal/migrate`)** — import buckets and objects from any
  S3-compatible source (MinIO, AWS S3, Garage…) into VaultS3. A SigV4 source
  client (no AWS SDK) plus an async migrator with per-job progress, exposed via
  `POST /api/v1/migrate/test`, `POST /api/v1/migrate`, `GET /api/v1/migrate/jobs`
  and a dashboard wizard (Migrate page: connect → select buckets → live progress).
- **Dashboard semantic search** — the Search page now has a Keyword / Semantic
  toggle; Semantic mode queries the vector store and shows results ranked by
  cosine similarity (greys out with a hint when vector search is disabled).
- Settings page surfaces the Vector Search, Erasure Coding, and Clustering
  feature flags in its read-only status panel.

## [4.2.5] - 2026-06-28
### Added
- **Semantic / vector search (optional add-on)** — a new `internal/vector` package
  brings RAG-style retrieval into the single binary, with no external vector
  database. A dependency-free cosine-kNN index (persisted across restarts) is fed
  by any OpenAI-compatible `/v1/embeddings` endpoint (OpenAI, Ollama, llama.cpp,
  LM Studio, vLLM…), so users pick their own (often local, private) embedding
  model. Text objects are auto-embedded on upload (best-effort, off the request
  path); query via `POST /api/v1/vectors/query`, status via
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
  that previously had little or none — and the last seven untested packages, so
  **every `internal/` package now has tests**:
  - **erasure** — Reed-Solomon encode/reconstruct, lost-disk reads, and the
    background healer repairing degraded objects (0% → ~64%).
  - **cluster** — consistent-hash ring + failure-detector state machine, plus a
    real multi-node **Raft consensus** harness (in-memory transport): leader
    election, log replication, no-split-brain under network partition, and
    membership changes (14.9% → 22.5%).
  - **replication** — vector-clock causality/merge and all three conflict
    resolution strategies (last-writer-wins, largest-object, site-preference).
  - **tiering** (0% → ~39%), **backup** (0% → ~48%), **fuse** (0% → ~45%).
  - **metrics, lambda, batch, inventory, scanner, accesslog, dashboard** — baseline
    coverage for the remaining packages.
- `docs/BENCHMARKS.md` — reproducible benchmark methodology (the `/speedtest`
  endpoint, `warp` for comparative throughput, RSS measurement) + results template.
- README **Production Readiness** section (stable vs. beta paths) and a
  refreshed competitor comparison verified against June 2026 sources.
- `CONTRIBUTING.md`, `CHANGELOG.md`, and GitHub issue/PR templates.

### Fixed
- **Tiering deadlock (data-availability bug):** the background tier scan called
  `SetObjectTier` (a write transaction) from inside `IterateAllObjects` (a read
  transaction), which deadlocks BoltDB — the scan would hang forever the first
  time it tried to migrate any object to cold. `scan()` now collects candidates
  inside the read txn and migrates them after it closes. Found by the new
  tiering tests.

### Changed
- `internal/cluster`: extracted a `newNodeWithDeps` seam so the Raft transport
  and stores are injectable (enables the in-process consensus tests). The
  production `NewNode` path is unchanged (TCP transport + BoltDB).
- Competitor comparison table corrected: SeaweedFS now has a web admin UI and a
  working FUSE mount; MinIO's Community console was removed (2025) and the
  open-source repo archived (Feb 2026); added an "as of June 2026" qualifier.
- Stopped tracking build artifacts and logs in git (`vaults3-cli`,
  `bin/vaults3-cli`, `access.log`, `test-results/`); added `*.log` and
  `test-results/` to `.gitignore`.

## [4.2.3] - 2026-06-26
### Added
- `docs/SCALING.md` operations guide: multi-disk erasure coding, multi-node
  Raft cluster setup, and lost-disk / lost-server / quorum-loss runbooks.
### Fixed
- `POST /api/v1/heal` was a stub that only acked the request; it now invokes the
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

[Unreleased]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.2.17...HEAD
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
