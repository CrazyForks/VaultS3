# Changelog

All notable changes to VaultS3 are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows
semantic-ish versioning via git tags (`vMAJOR.MINOR.PATCH`).

## [Unreleased]

## [4.4.36] - 2026-07-24
### Fixed
- **Compressed reads now stream, so GET time-to-first-byte no longer scales with
  object size** (issue #38). With compression enabled, `CompressedEngine` read the
  entire object into memory and decompressed it before emitting the first byte, so
  TTFB grew with object size (~4 ms/MiB on slower disks, e.g. ~330 ms for 64 MiB)
  even though pod-to-pod latency was sub-millisecond. Reads now wrap the stored blob
  in a streaming zstd/gzip decoder and report `Content-Length` from the size recorded
  in the container (zstd frame header, gzip trailing ISIZE) without materializing the
  object. Measured TTFB is now flat (~8 ms) from 1 MiB to 64 MiB. `Range`/`partNumber`
  reads (which seek) still buffer on demand, and blobs stored while compression was
  off stream through untouched. Note: whole-object encryption (SSE-S3/SSE-KMS/
  per-bucket) still buffers on read to verify the GCM tag before releasing plaintext;
  streaming that needs a chunked-cipher format and is tracked separately.

## [4.4.35] - 2026-07-22
### Fixed
- **Cluster read-after-write miss root-caused and fixed: bucket listings are now
  served by the Raft leader** (issue #37). The reported symptom was a `HEAD`/stat
  right after a `PUT` returning "Object not found" on a multi-node cluster while a
  `GET` for the same key succeeded. Tracing the client showed `mc stat` (and warp's
  verification) issues a `ListObjectsV2` *before* the `HEAD`, and reports the object
  missing if that list comes back empty. Object `GET`/`HEAD` are owner-routed and
  wait out replication with a per-key barrier, but a listing is bucket-wide with no
  single key to wait on: it was answered from a follower whose Raft FSM had not yet
  applied the just-committed write (the leader commits on a quorum and does not block
  on lagging followers), so the new key was absent from the list. Listings
  (`ListObjects` v1/v2, `ListObjectVersions`, and bucket sub-resource reads) are now
  forwarded to the current leader, which has every committed write applied, giving
  read-your-writes for enumerations. This uses only the leader identity Raft already
  tracks (no inter-node read-index RPC, which earlier attempts showed is unreliable
  under this topology). In-progress multipart listing (`?uploads`) stays node-local
  (issue #32) and object `GET`/`HEAD` keep owner routing.
- **`VAULTS3_TRACE_READS=1` now also logs `HEAD` 404s.** It previously logged only
  `GET` misses, so the operation that actually failed was invisible to the trace.

## [4.4.34] - 2026-07-21
### Fixed
- **`/cluster/ownership` diagnostic now reaches its handler regardless of path
  shape** (issue #37 diagnosis). It was registered only as the exact path
  `/cluster/ownership`, so a trailing slash (`/cluster/ownership/`), a path-style
  call (`/cluster/ownership/<bucket>/<key>`), or an LB that appends a slash fell
  through to the S3 bucket handler and returned `NoSuchBucket`/`AccessDenied` — which
  looked like the ownership view was reading a different store than the S3 path, when
  in fact the request never hit the endpoint. The handler is now registered for both
  the exact path and the subtree and accepts the bucket/key from either the query
  string or the path, so all call shapes return the JSON ownership view. (The exact
  query form `?bucket=&key=` already worked on v4.4.33.)

## [4.4.33] - 2026-07-20
### Added
- **`GET /cluster/ownership?bucket=&key=` diagnostic endpoint** to localize the
  residual issue #37 read-after-write miss on a large cluster. From the responding
  pod's own view it returns the key's `owner`, the data `holders`, where a request
  `would_proxy_to`, and whether this pod holds the key's metadata/data locally
  (`meta_present_local`, `data_present_local`), plus the pod's `ring_members`.
  `curl` it against every pod for the same key: if they disagree on `owner`, the
  placement ring is inconsistent across pods (the miss cause); if they agree but the
  owner's `data_present_local` is false, it's data placement; if only
  `meta_present_local` lags, it's replication. Read-only and gated by the cluster
  secret (`X-Cluster-Secret`) when one is configured, since it is served on the
  public S3 port. On a healthy cluster every pod agrees on the owner, metadata is
  present everywhere, and data is present only on the owner.

## [4.4.32] - 2026-07-20
### Fixed
- **Cluster reads are no longer served by a node that holds no data** (issue #37).
  With `replica_count = 1` an object's data lives on exactly one node. Routing forced
  the candidate set to two nodes "for failover", so when a per-node failure detector
  marked the true owner down — including a false positive from a stale/unreachable
  probe address — a `GET`/`HEAD` could be answered by the second-ranked node (or
  handled locally) even though it holds no data, returning a phantom `Object not
  found` for a live object that then "reappeared" on a retry routed elsewhere. This
  is the read-after-write miss reported on a 12-node cluster that three prior
  read-path fixes (v4.4.25–v4.4.31) did not reach, because the miss happens in
  routing, before the consistent read runs. `ShouldProxy` now routes strictly within
  the actual data-holder set (the first `replica_count` nodes): a read is served
  locally only if this node holds the data, otherwise it is forwarded to the first
  healthy holder, and if every holder is marked down it is still forwarded to the
  primary owner rather than answered from an empty shard — so a falsely-down owner
  still serves the read, and a genuinely-down single copy returns an honest upstream
  error instead of a misleading 404. Legitimate failover with `replica_count >= 2`
  (where other nodes really do hold the data) is unchanged. Unit-covered; happy-path
  read-your-writes verified at 0 misses on a local multi-node cluster.

## [4.4.31] - 2026-07-20
### Added
- **Opt-in read-404 cause tracing for cluster reads** (`VAULTS3_TRACE_READS=1`), to
  diagnose the residual issue #37 read-after-write miss on a large cluster. When
  enabled, every `GET`/`HEAD` that returns `404` logs whether the cause is
  `meta_nil` (the object's Raft-replicated metadata hasn't arrived on this node — a
  replication/consistency lag the consistent read waits out) or `data_missing` (the
  metadata is present but this node has no data file — the read was served by a node
  that isn't the shard owner, a routing problem), plus which node proxied it here.
  On a local cluster (5 and 9 nodes, injected latency) the v4.4.29 read-your-writes
  path reproduces 0 misses, so this trace is aimed at capturing the actual cause on
  the reporter's 12-node topology. Off by default, zero overhead.

## [4.4.30] - 2026-07-20
### Added
- **Opt-in per-hop latency tracing for cluster reads** (`VAULTS3_TRACE_FORWARD=1`),
  to diagnose issue #38 (a large fixed GET time-to-first-byte in some clustered
  deployments). When enabled, every proxied request logs an `httptrace` breakdown of
  the upstream hop — DNS resolution time, TCP connect time, whether the keep-alive
  connection was reused, and upstream time-to-first-byte — so the ~fixed overhead can
  be attributed to the extra pod hop, a slow DNS lookup (e.g. Kubernetes `ndots`
  search expansion), a cold connection setup, or the owner's disk read. Off by
  default with zero overhead. The forward path itself measures ~2ms end-to-end
  (owner and forwarded reads alike) on a local multi-node cluster, so the fixed cost
  reported in #38 originates in the deployment environment, not the request path;
  this trace pinpoints where. Keep-alive to the owner is reused across requests once
  a response body is fully read.

## [4.4.29] - 2026-07-20
### Fixed
- **Cluster read-your-writes no longer depends on an inter-node RPC** (issue #37,
  thanks @kesavkolla). v4.4.27's read-side barrier asked the leader for its applied
  index over `/cluster/readindex` (derived via the same hardcoded-port address that
  was already wrong behind a split proxy in issue #29); when that call didn't reach
  the leader the barrier silently no-opped, so a `GET`/`HEAD` right after a `PUT`
  returned `Object not found` on a follower until replication caught up (~500ms).
  The consistent read now simply **polls the local store** until the just-written
  key replicates in through normal Raft (which reaches every node) or a 2s timeout
  elapses — no RPC, no derived addresses, so it is robust regardless of proxy or
  network topology. The leader (authoritative) never waits, the write path stays
  fast, and a genuine miss still returns `404` after the wait. Validated on a
  latency-injected multi-node cluster: 0 misses on tight sequential `PUT`→`GET` and
  on 5000 objects at `--concurrent=128`. For resilience to node/pod churn (when the
  placement ring is briefly reconciling), run `placement.replica_count: 2` so more
  than one node holds each object's data.

## [4.4.28] - 2026-07-20
### Fixed
- **`CreateBucket` on an existing bucket returns a clean `409 BucketAlreadyExists`.**
  In cluster mode the error message leaked the internal forwarding error
  (`cluster: leader rejected forwarded write (500): ...`), and any `CreateBucket`
  error was mapped to `BucketAlreadyExists` — so a genuine failure (e.g. an
  unreachable leader) would be misreported. The handler now checks existence first
  and returns a clean AWS-style message, treats only an "already exists" error as
  `409`, and surfaces other failures as `500 InternalError`.

## [4.4.27] - 2026-07-19
### Changed
- **Cluster read-your-writes reworked to a read-side barrier** (issue #37). v4.4.25
  made a follower block on its own FSM before acking every write; that added a
  replication round-trip to every write and collapsed throughput under high
  concurrency (a `warp` GET benchmark at `--concurrent=64` still saw `Object not
  found` and unstable writes). The barrier moved to the read path: writes are fast
  again (no per-write wait), and a GET/HEAD that misses on the owner catches up to
  the leader and re-checks before returning (`GetObjectMetaConsistent`), so a read
  right after a write still sees it. The barrier only fires on the rare read that
  races a write; a genuine miss still returns `404`. Bucket-existence checks
  already used this barrier-on-miss.

## [4.4.26] - 2026-07-19
### Security
- **The `X-Forwarded-Prefix` header is no longer trusted by default** (issue #36
  hardening, thanks @arthurvmdantas). v4.4.22–24 auto-detected the reverse-proxy
  subpath from that client-supplied header when `base_path` was unset. That was not
  an auth bypass (SigV4 still requires the secret key; the dashboard value is
  sanitized and same-origin), but as defense-in-depth it is now opt-in: set
  `server.trust_forwarded_prefix: true` (env `VAULTS3_TRUST_FORWARDED_PREFIX`) to
  honor the header. By default only `server.base_path` is used, so a spoofed header
  can't influence the served base or signature verification. `base_path` always
  wins regardless of this flag.

## [4.4.25] - 2026-07-19
### Fixed
- **Cluster read-your-writes: `GET`/`HEAD` after a `PUT` no longer returns
  `Object not found`** (issue #37). Writes commit through Raft on the leader, but a
  follower served reads from local state that could lag the committed log, so a
  fast read-after-write on a follower missed the object. The node that handles a
  write now waits for its own state machine to apply the entry (the leader returns
  the committed index; the follower blocks on an FSM-tracked applied index) before
  acking, so a follow-up read — which routes to the same owner — sees it.
- **Cluster bucket visibility.** `BucketExists` now does a barrier-on-miss (catch
  up to the leader and re-check) so a write right after `CreateBucket` no longer
  spuriously gets `Bucket does not exist`. The barrier only costs a round-trip on
  a miss, never on a hit.
- **Placement ring reconciles to Raft membership every 1s** (was 3s), shrinking
  the window where two nodes disagree about a key's owner during membership churn.
- **A down / OOM-looping shard owner fails fast.** The cluster reverse proxy gained
  a short dial timeout + response-header timeout, so a request to an unreachable
  owner returns `502` quickly instead of hanging (large-object streaming after the
  headers is unaffected).
### Added
- **`replica_count ≥ 2` now replicates object data across nodes** (issue #37). With
  `placement.replica_count > 1`, a written object's data is streamed to the other
  nodes in its replica set, so a node loss no longer makes its objects
  unavailable — `GET` failover (which already tries replicas when the primary is
  down) now finds a copy. Replication is best-effort and asynchronous (it never
  blocks or fails the client write, and streams from the engine without buffering
  the whole object), so it provides eventual redundancy rather than synchronous
  write-quorum durability; pair it with erasure coding for disk-loss protection.
  `replica_count: 1` (default) is unchanged.
- Documented the cluster consistency model and per-pod memory sizing in
  docs/SCALING.md.

## [4.4.24] - 2026-07-19
### Fixed
- **S3 API works behind a reverse-proxy subpath** (issue #36 follow-up). With the
  dashboard hosted under a subpath (e.g. `/sistemas/s3-nac`), S3 requests returned
  `403 AccessDenied: signature mismatch`. SigV4 signs the request URI, so a client
  pointed at the proxied endpoint signs `/<base>/bucket/key`, but the proxy strips
  the prefix and VaultS3 verified the signature over the bare `/bucket/key`. The
  authenticator now reconstructs the signed path from `server.base_path` (or the
  proxy's `X-Forwarded-Prefix` header) before verifying, so signatures match. With
  no base path (default) verification is byte-for-byte unchanged, and a spoofed
  prefix can't bypass auth — it only changes the expected signature.

## [4.4.23] - 2026-07-18
### Fixed
- **Reverse-proxy subpath now works under the dashboard's CSP** (issue #36
  follow-up). v4.4.22 exposed the base path via an inline `<script>`, but the
  dashboard's own Content-Security-Policy (`default-src 'self'`, no
  `script-src 'unsafe-inline'`) blocks inline scripts, so the SPA router never
  picked up the base and client-side routing broke behind a subpath (assets loaded
  fine). The base path is now published as a `<meta name="vaults3-base">` tag,
  which CSP does not restrict, and the frontend reads it from there.

## [4.4.22] - 2026-07-18
### Added
- **Host the dashboard under a reverse-proxy subpath** (issue #36). The dashboard
  hardcoded absolute `/dashboard/` and `/api/v1/` paths, so it couldn't be served
  behind a proxy at, say, `https://example.com/vaults3/dashboard/`. A new
  `server.base_path` (env `VAULTS3_BASE_PATH`, e.g. `/vaults3`) makes the server
  rewrite the served `index.html` — asset URLs and a runtime base the SPA reads
  for its router basename and API base — so everything resolves under the subpath.
  When `base_path` is unset the server also auto-detects the proxy's
  `X-Forwarded-Prefix` header (a forwarded prefix is sanitized to safe path
  characters). Default is empty, so a normal root deployment is unchanged.

## [4.4.21] - 2026-07-17
### Fixed
- **Dashboard file browser now shows folder dates** (issue #35 follow-up). v4.4.20
  made the API return a Last-Modified for folders, but the dashboard's file list
  still hardcoded `-` in the Modified column for folder rows, so the date never
  showed. The column now renders the folder's date (falling back to `-` only when
  genuinely absent). This works on existing buckets — the date is computed from
  the folder's contents at list time, so no re-migration is needed.

## [4.4.20] - 2026-07-16
### Changed
- **Folder listings now carry a Last-Modified date** (issue #35). S3 "folders"
  (common prefixes) have no timestamp in the standard, so listings returned them
  dateless and clients (e.g. rclone) filled in a fake date. VaultS3 already had
  the real date in hand while collapsing a folder — the folder's directory-marker
  object (its date preserved on migration) or, failing that, its first child — but
  discarded it. It now surfaces that date: `ListObjectsV2` adds a `<LastModified>`
  to each `<CommonPrefixes>` entry (a backward-compatible extension — standard S3
  clients ignore the extra element), and the dashboard file browser shows folder
  dates instead of blanks. Whether a given third-party client displays the folder
  date is up to that client.

## [4.4.19] - 2026-07-16
### Fixed
- **Deleted objects no longer linger as phantoms in a cluster** (issue #34). After
  a successful delete, a HEAD/GET from a client whose connection landed on a
  different node could still return the object — a HEAD came back `200` with null
  `Last-Modified`/`ETag` and a stale `Content-Length`. Two causes, both fixed:
  - **Metadata is now the single source of truth (correctness).** `HeadObject` and
    `GetObject` no longer fall back to "is there a data file on disk?" when an
    object's metadata is gone. A delete removes metadata cluster-wide (via Raft),
    so HEAD/GET now return `404 NoSuchKey` consistently on every node, even if a
    data file lingers.
  - **Orphan data files are now reaped (disk reclamation).** Writes land on a
    single node, but a past ring/primary change can leave an orphan copy on
    another node. A delete now broadcasts a best-effort, cluster-secret-authed
    object-delete to every node so the orphan's disk is reclaimed. Correctness
    does not depend on it — the metadata fix already prevents phantom reads.
  Single-node behavior is unchanged.

## [4.4.18] - 2026-07-15
### Fixed
- **Concurrent multipart uploads no longer fail with 404 `NoSuchUpload` in a
  cluster** (issue #32). In-progress multipart upload metadata was written through
  Raft, but reads were served from the local store. On a follower, a part uploaded
  immediately after `CreateMultipartUpload` could hit the node before it had
  applied its own forwarded create, so `UploadPart` returned `404 NoSuchUpload` —
  frequently under high concurrency (e.g. `rclone --transfers=600
  --s3-upload-concurrency 8`), never for sequential/low-concurrency uploads. Since
  every request for an object already routes to the same owner node and its part
  data is stored on that node's local disk, multipart metadata is now kept on the
  node-local store too (co-located, no replication lag). The final assembled
  object is still written through Raft so it is visible cluster-wide. Single-node
  behavior is unchanged.

## [4.4.17] - 2026-07-15
### Added
- **Cluster operations in `vaults3-cli` and the admin API** (issue #31). New
  `vaults3-cli cluster` subcommands — `status`, `join`, `leave`, `drain`,
  `undrain`, `rebalance`, `decommission` — backed by admin-authenticated
  `/api/v1/cluster/*` endpoints, so cluster membership no longer needs raw curl.
  - **Drain**: a node can be told to stop accepting writes (S3 object PUT/POST/
    DELETE return `503 SlowDown`) while continuing to serve reads, so it can be
    taken down for maintenance or evacuated. Drain a specific node by ID from any
    node (forwarded over the cluster channel) or the node you connect to.
  - **Rebalance**: trigger the background pass that moves objects to their correct
    hash-ring owner after membership changes.
  - **Decommission**: guided drain + rebalance for replacing a server (removal is
    left to an explicit `cluster leave` after you confirm data has moved).
  - Adding a member in Kubernetes is already automatic — scaling the StatefulSet
    replicas auto-joins the new pod; documented in docs/SCALING.md.

## [4.4.16] - 2026-07-12
### Fixed
- **Large multipart uploads no longer fail with `MalformedXML` on completion**
  (issue #26). The `CompleteMultipartUpload` request body was read through a 256KB
  limit, which silently truncated the part list for large objects (a 100GB upload
  is thousands of parts, and even ~17GB at the default 8MB part size exceeds it).
  The XML then failed to parse, so S3 clients (aws-cli, rclone, s3cmd) reported
  `MalformedXML: Could not parse request body` when finishing a multi-GB upload.
  The cap is raised to comfortably hold the full S3 maximum of 10,000 parts.

## [4.4.15] - 2026-07-12
### Fixed
- **Dashboard uploads now report storage failures instead of silently failing**
  (issue #26). A large upload that failed mid-write (for example a full data disk)
  was swallowed: the handler skipped the file, wrote no log, and still returned
  HTTP 200 with an empty result, so the browser showed a bare "upload failed" with
  nothing in the server logs. Each failed file is now logged with the real reason
  and returned to the client (the dashboard surfaces it, e.g. `write object: no
  space left on device`), and the request returns a 5xx when any file failed. Note
  for very large objects: a single browser POST holds the whole transfer with no
  resume, so an S3 client that does multipart upload (aws-cli, rclone, s3cmd) is
  the robust path for multi-GB files.

## [4.4.14] - 2026-07-10
### Fixed
- **Cluster capacity now gathers peer info over the cluster channel** (issue #29).
  The coordinator built the rollup by logging in to each peer's dashboard `/api/v1`
  API, which is unreachable peer-to-peer in split-`console_port` or proxied
  (Kubernetes + Envoy) deployments — every remote node showed as unreachable while
  only the node serving the request appeared. Nodes now expose their capacity on a
  new cluster-secret-authenticated `/cluster/sysinfo` endpoint (served next to
  `/cluster/status`), and the coordinator fetches it over the same peer addresses
  the placement proxy already uses for S3 forwarding — no dashboard login, no
  console-port dependency. Response is assembled server-side.

## [4.4.12] - 2026-07-10
### Fixed
- **Cluster capacity now reports *why* a node is unreachable** (issue #29). The
  rollup silently marked peers unreachable when the login to fetch their info
  failed (it did not check the login response status). Each unreachable node now
  carries an error reason (shown in the dashboard and `vaults3-cli info`), e.g. a
  peer HTTP 403 (its `peer_apis` address is not serving the dashboard API, often a
  split `console_port` or the S3 port) versus a connection refused. `vaults3-cli
  info`'s own login error is likewise clearer: a 403 means the endpoint is not
  serving `/api/v1`, and a 401 means the root admin key is required (not an IAM key).

## [4.4.11] - 2026-07-10
### Fixed
- **`vaults3-cli object ls` now lists past 1000 objects and shows a folder view**
  (issue #30). It was capped at a single 1000-key page (the continuation token was
  ignored) and always listed flat. It now follows the pagination cursor to list
  everything, and by default shows a `mc ls`-style view: immediate objects plus
  folders (`CommonPrefixes`), with `--recursive` for the full nested listing.

## [4.4.10] - 2026-07-09
### Added
- **Cluster-wide capacity overview.** `GET /api/v1/cluster/info` aggregates every
  node's version and on-disk capacity into a cluster total plus a per-node
  breakdown (unreachable nodes are marked, not fatal), the multi-node equivalent
  of `mc admin info`. The dashboard Stats capacity panel and `vaults3-cli info` now
  show the cluster totals and per-node rows when clustered, and fall back to the
  single-node view otherwise.

## [4.4.9] - 2026-07-09
### Added
- **Server and storage-capacity overview.** A new `GET /api/v1/system` endpoint
  reports the version, data directories, on-disk capacity (total / used / free,
  aggregated across the distinct filesystems backing the data, cold-tier, and
  erasure directories), and logical object usage. The dashboard Stats page shows a
  capacity bar, and `vaults3-cli info` prints the same overview. This is the
  single-node answer to "how much capacity is there and how much is occupied"
  (a lightweight equivalent of the capacity numbers `mc admin info` shows).

## [4.4.8] - 2026-07-09
### Added
- **Lifecycle rule to abort incomplete multipart uploads** (issue #28). A bucket
  lifecycle rule can now expire abandoned multipart uploads (from killed or failed
  clients) after a number of days, via the standard S3
  `AbortIncompleteMultipartUpload` / `DaysAfterInitiation` element (works with
  `aws s3api`, `mc`, and boto3) and via a field in the dashboard lifecycle editor.
  A rule may now specify only this action, with no object expiration. The lifecycle
  worker that enforces it now also deletes the uploaded part files from disk, not
  just the upload metadata, so the space is actually reclaimed.

## [4.4.7] - 2026-07-09
### Fixed
- **Large-file migration no longer times out** (issue #26). The migration source
  client used a single total request timeout that also capped reading the response
  body, so any object that took longer than the timeout to download failed with
  "context deadline exceeded ... while reading body". The client now bounds only
  connect, TLS, and time-to-first-byte, letting a large object body (tens of GB)
  stream for as long as it needs.
- **Large-file dashboard uploads no longer fail, and folder uploads keep their
  structure** (issue #26). The upload handler buffered the entire request body to
  a temp file before writing to storage, which failed for very large files when
  the temp dir filled. It now streams each part straight to storage (no temp
  buffering, no double copy). It also preserves the relative folder path in the
  filename instead of flattening subfolders to the base name.

## [4.4.6] - 2026-07-08
### Fixed
- **Directory-marker objects (keys ending in `/`) no longer corrupt folders or
  break migration and s3fs.** Zero-byte "folder" objects created by s3fs, MinIO,
  and folder uploads were stored as regular files, which then blocked every child
  object under that prefix and failed with `mkdir ...: not a directory` (ENOTDIR).
  Such keys are now stored as real directories so children nest correctly, read
  back as empty objects, and delete cleanly. This affects all storage engines
  (plain, compressed, encrypted, per-bucket, KMS, erasure). Despite the report
  naming FreeBSD, the bug was OS-agnostic.

## [4.4.5] - 2026-07-07
### Added
- **Migration is now resumable and parallel** (issue #24). A migration that stops
  (restart or crash) no longer re-copies the whole bucket when restarted: objects
  already present at the destination with the same size are skipped, so it continues
  where it left off. Objects within a bucket are also copied with a bounded worker
  pool (configurable, default 8) instead of one at a time, so large buckets migrate
  much faster. The job now reports a `skipped` count alongside `copied`/`failed`.

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

[Unreleased]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.36...HEAD
[4.4.36]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.35...v4.4.36
[4.4.35]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.34...v4.4.35
[4.4.34]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.33...v4.4.34
[4.4.33]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.32...v4.4.33
[4.4.32]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.31...v4.4.32
[4.4.31]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.30...v4.4.31
[4.4.30]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.29...v4.4.30
[4.4.29]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.28...v4.4.29
[4.4.28]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.27...v4.4.28
[4.4.27]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.26...v4.4.27
[4.4.26]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.25...v4.4.26
[4.4.25]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.24...v4.4.25
[4.4.24]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.23...v4.4.24
[4.4.23]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.22...v4.4.23
[4.4.22]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.21...v4.4.22
[4.4.21]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.20...v4.4.21
[4.4.20]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.19...v4.4.20
[4.4.19]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.18...v4.4.19
[4.4.18]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.17...v4.4.18
[4.4.17]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.16...v4.4.17
[4.4.16]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.15...v4.4.16
[4.4.15]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.14...v4.4.15
[4.4.14]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.12...v4.4.14
[4.4.12]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.11...v4.4.12
[4.4.11]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.10...v4.4.11
[4.4.10]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.9...v4.4.10
[4.4.9]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.8...v4.4.9
[4.4.8]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.7...v4.4.8
[4.4.7]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.6...v4.4.7
[4.4.6]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.5...v4.4.6
[4.4.5]: https://github.com/Kodiqa-Solutions/VaultS3/compare/v4.4.4...v4.4.5
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
