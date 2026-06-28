# VaultS3 — Scaling & Operations Guide

This guide explains how to scale VaultS3 across **multiple disks** and **multiple
servers**, and provides step-by-step runbooks for recovering from a **lost disk**
or a **lost server**.

> **TL;DR for "4 disks per server, going horizontal":**
> Use **erasure coding** to spread data across the 4 disks on each server (survives
> disk loss), and a **Raft cluster** with `replica_count ≥ 2` to spread data across
> servers (survives server loss). These are two independent redundancy layers — use
> **both** for full protection.

---

## 1. Two independent redundancy layers

VaultS3 protects data at two different levels. Understand both before designing a deployment.

| Layer | Protects against | Mechanism | Config block |
|-------|------------------|-----------|--------------|
| **Erasure coding (EC)** | **Disk** failure *within one server* | Reed–Solomon shards striped across local disks | `erasure:` |
| **Clustering** | **Server/node** failure | Raft (metadata) + consistent-hash placement with N replicas (object data) across nodes | `cluster:` |

Key facts:

- **EC does not span servers.** Its shards live on the disks of a single node.
- **Cluster replicas do not protect a single disk.** A replica is a whole-object copy on
  another node. Within a node, disk redundancy still comes from EC (or RAID).
- **Metadata** (bucket/object index) lives in a local embedded BoltDB per node. In a
  cluster it is replicated via **Raft** and is durable only while a Raft **quorum**
  (majority of nodes) is alive.
- **Storage is shared-nothing.** Each node stores its own object data locally; there is
  no shared SAN/NFS requirement.

---

## 2. Choose your topology

| You have… | You want… | Use |
|-----------|-----------|-----|
| 1 server, multiple disks | Survive a disk failure | **Erasure coding** (Section 3) |
| Multiple servers, 1 disk each | Survive a server failure, scale capacity | **Cluster** with `replica_count ≥ 2` (Section 4) |
| Multiple servers, multiple disks each | Survive both disk **and** server failure | **EC + Cluster** (Section 5) ← *recommended for the 4-disk / multi-server case* |
| A second site / region | Disaster recovery, geo-redundancy | **Replication** (Section 8) |

---

## 3. Single server, multiple disks — Erasure coding

Erasure coding splits each object into `data_shards` data shards plus `parity_shards`
parity shards, then distributes them **round-robin** across the disks listed in
`data_dirs`. You can lose up to **`parity_shards`** disks and still reconstruct every object.

### Configuration

`configs/vaults3.yaml`:

```yaml
erasure:
  enabled: true
  data_shards: 4
  parity_shards: 2          # tolerates losing up to 2 disks
  block_size: 4194304       # 4 MB — objects smaller than this bypass EC
  data_dirs:                # one path per physical disk / mount point
    - /mnt/disk1
    - /mnt/disk2
    - /mnt/disk3
    - /mnt/disk4
    - /mnt/disk5
    - /mnt/disk6
  heal_interval_secs: 300   # background heal scan cadence
```

### Sizing rules

- Number of `data_dirs` should be **≥ `data_shards + parity_shards`** so each shard can
  land on its own disk. (Fewer disks still works — shards share disks round-robin — but
  you lose independent-failure protection.)
- **Usable capacity ≈ raw × `data_shards / (data_shards + parity_shards)`.**
  Example: `4 + 2` over six 4 TB disks = 24 TB raw → **~16 TB usable**, tolerates 2 disk losses.
- **Failure tolerance = `parity_shards`.** `4+2` survives 2 disks; `6+2` survives 2 of 8; `3+1` survives 1.
- Objects **smaller than `block_size`** are stored whole (no EC) for efficiency — they are
  protected by clustering replicas, not EC. Size `block_size` to your workload.

### How healing works

- A background **Healer** scans on the `heal_interval_secs` cadence, detects objects with
  missing/corrupt shards, and **reconstructs them from parity** onto a healthy disk —
  automatically and transparently.
- Reads of a *degraded* object (some shards missing, but ≤ `parity_shards`) succeed live by
  reconstructing on the fly; a warning is logged.
- You can trigger an **on-demand** heal pass with `POST /api/v1/heal?bucket=&prefix=`
  (both params optional — empty `bucket` scans all buckets). It runs the same
  reconstruction as the background healer and returns `202 heal initiated`. Use it to
  repair immediately after replacing a disk instead of waiting for the next interval.

---

## 4. Multiple servers — Raft cluster

A cluster gives you horizontal capacity and **survives losing a node**. Metadata is kept
strongly consistent by **HashiCorp Raft**; object data is placed across nodes using a
**consistent hash ring** with `replica_count` copies per object.

### Quorum sizing (read this first)

Raft needs a **majority** of nodes alive to elect a leader and accept writes.

| Cluster size | Tolerates losing | Notes |
|--------------|------------------|-------|
| 1 | 0 | Not HA |
| **3** | **1** | Minimum recommended HA |
| **5** | **2** | Recommended for production |
| 7 | 3 | Diminishing returns |

> **Always run an odd number of nodes** (3, 5, 7). An even count adds a node without
> adding fault tolerance and risks split-brain.

Set `replica_count` for object data independently of cluster size. For 3 nodes,
`replica_count: 3` keeps a full copy on every node (max durability); `replica_count: 2`
trades one copy for ~33% more usable capacity.

### Configuration — per node

Each node gets a unique `node_id`. **Exactly one** node bootstraps; the rest join it.

**Node 1 (bootstrap):**
```yaml
cluster:
  enabled: true
  node_id: "node-1"
  bind_addr: "0.0.0.0"
  raft_port: 9001
  api_port: 9000
  bootstrap: true                 # ONLY on the first node
  peers: []
  peer_apis: {}
  placement:
    replica_count: 3
    read_quorum: 2
    write_quorum: 2
    virtual_nodes: 128
  detector:
    probe_interval_secs: 5
    suspect_after: 3              # → "suspect" after 3 missed probes
    down_after: 6                # → "down" after 6 missed probes
    probe_timeout_secs: 3
  rebalance:
    max_bandwidth_mbps: 50       # throttle data movement on membership change
    batch_size: 100
```

**Node 2 / Node 3 (joiners):**
```yaml
cluster:
  enabled: true
  node_id: "node-2"              # unique per node
  bind_addr: "0.0.0.0"
  raft_port: 9001
  api_port: 9000
  bootstrap: false               # joiners must be false
  peers: ["node-1@<node1-host>:9001"]
  peer_apis:
    node-1: "<node1-host>:9000"
  placement: { replica_count: 3, read_quorum: 2, write_quorum: 2, virtual_nodes: 128 }
```

- `peers` entries use the format **`nodeID@host:raftPort`**.
- `peer_apis` maps **`nodeID → host:apiPort`** so nodes can proxy S3 requests to the data owner.
- **Optional — separate the cluster control plane from S3 traffic** (recommended for security):
  ```yaml
  server:
    internode_address: "10.0.0.11"   # private NIC
    internode_port: 9100             # cluster endpoints served here instead of the public port
  ```

### Bootstrapping the cluster

1. Start **node-1** (with `bootstrap: true`). Confirm it becomes leader:
   ```bash
   curl -s http://node1:9000/cluster/status | jq
   # → "state": "Leader"
   ```
2. Start **node-2** and **node-3** (`bootstrap: false`).
3. Join each new node to the leader:
   ```bash
   curl -X POST http://node1:9000/cluster/join \
     -H 'Content-Type: application/json' \
     -d '{"node_id":"node-2","addr":"<node2-host>:9001"}'
   ```
   (If you POST to a follower you get a `307` redirect to the leader — follow it.)
4. Verify all members are present and voting:
   ```bash
   curl -s http://node1:9000/cluster/status | jq '.servers'
   # each entry should show "suffrage": "Voter"
   ```

### Cluster API reference

| Action | Method & path | Body |
|--------|---------------|------|
| Status / membership | `GET /cluster/status` | — |
| Add a node | `POST /cluster/join` | `{"node_id":"...","addr":"host:raftPort"}` |
| Remove a node | `POST /cluster/leave` | `{"node_id":"..."}` |

---

## 5. Recommended: EC + Cluster (4 disks per server, multiple servers)

This is the configuration for the "4 disks per host, go horizontal" goal. Each node uses
EC across its local disks (disk-failure protection) **and** participates in the cluster
(server-failure protection).

Per node (`node_id` unique, `bootstrap: true` on the first only):

```yaml
storage:
  data_dir: "/mnt/disk1"          # primary; EC also uses the disks below
  metadata_dir: "/mnt/disk1/metadata"

erasure:
  enabled: true
  data_shards: 2
  parity_shards: 2                # any 2 of the 4 local disks can fail
  block_size: 4194304
  data_dirs: ["/mnt/disk1", "/mnt/disk2", "/mnt/disk3", "/mnt/disk4"]
  heal_interval_secs: 300

cluster:
  enabled: true
  node_id: "node-1"
  raft_port: 9001
  api_port: 9000
  bootstrap: true                 # first node only
  peers: []                       # joiners list the bootstrap peer
  placement:
    replica_count: 3              # a full object copy per node (3-node cluster)
    read_quorum: 2
    write_quorum: 2
    virtual_nodes: 128
```

**Result:** A 3-node cluster, each node EC-protected across 4 disks. You can lose **up to
2 disks on any node** *and* **one whole node** without data loss or downtime.

---

## 6. Monitoring & health

- **Cluster health:** `GET /cluster/status` → leader, member list, per-node suffrage.
- **Node liveness:** `GET /health`.
- **Metrics (Prometheus):** `GET /metrics` — watch for replication lag, heal activity, and
  per-node request distribution.
- **Failure detection** is automatic: the detector marks a peer `suspect` after
  `suspect_after` missed probes and `down` after `down_after`; the failover proxy then
  routes reads/writes to a healthy replica.

---

## 7. Recovery runbooks

### 7a. Recover from a **lost disk** (EC enabled)

**Symptom:** one mount point in `data_dirs` is failed/unmounted; objects with a shard on
that disk are *degraded* but still readable (as long as failures ≤ `parity_shards`).

1. **Confirm tolerance.** Ensure no more than `parity_shards` disks are down. If more are
   down than parity, those objects are unrecoverable from EC alone — restore from a cluster
   replica or backup instead.
2. **Replace the hardware.** Swap the failed disk and mount it at the **same path** listed in
   `data_dirs` (e.g. `/mnt/disk3`). Ensure ownership/permissions match the VaultS3 user.
3. **Let the healer rebuild.** The background healer detects missing shards and reconstructs
   them onto the restored disk on its `heal_interval_secs` cadence. To repair immediately,
   trigger an on-demand pass:
   ```bash
   curl -X POST 'http://<host>:9000/api/v1/heal'              # all buckets
   curl -X POST 'http://<host>:9000/api/v1/heal?bucket=my-bucket&prefix=logs/'
   ```
4. **Verify.** Re-read a sample of affected objects (no degraded-read warnings in logs) and
   confirm shard files are repopulating on the new disk.

> If you front the disks with hardware/software RAID instead of EC, follow your RAID
> controller's rebuild procedure; VaultS3 sees a single volume and needs no action.

### 7b. Recover from a **lost server / node** (cluster enabled)

**Symptom:** a node is unreachable; `cluster/status` shows it as `down`. As long as a Raft
**quorum survives**, the cluster keeps serving via the failover proxy and surviving replicas.

**Case A — node comes back (transient outage):**
1. Restart the VaultS3 process on the node with its **original `node_id`** and config
   (`bootstrap: false`). It rejoins, catches up via Raft, and the rebalancer re-syncs any
   data it missed (throttled by `rebalance.max_bandwidth_mbps`).
2. Verify with `GET /cluster/status` — the node returns to `Voter`/healthy.

**Case B — node is permanently dead (replacement):**
1. **Remove the dead node** from the cluster so it stops counting toward quorum:
   ```bash
   curl -X POST http://<leader>:9000/cluster/leave -d '{"node_id":"node-3"}'
   ```
2. **Provision a replacement** with a **new** `node_id` (e.g. `node-3b`), `bootstrap: false`,
   peers pointing at a current member.
3. **Join it:**
   ```bash
   curl -X POST http://<leader>:9000/cluster/join \
     -d '{"node_id":"node-3b","addr":"<new-host>:9001"}'
   ```
4. The rebalancer redistributes the lost node's share of objects onto the new member to
   restore `replica_count`. Watch `/metrics` and `/cluster/status` until balanced.

**Case C — quorum lost (majority of nodes down at once):**
- Writes are rejected and no leader can be elected until a majority is restored. **Recover/restart
  enough original nodes to regain majority** (their Raft logs and snapshots reconstruct state).
- If a majority is permanently lost, the cluster cannot self-recover — restore from **backup**
  (Section 9) or a **replication peer** (Section 8). This is why an odd node count and an
  off-cluster backup/replica matter.

---

## 8. Cross-site replication (disaster recovery)

For geo-redundancy or a warm standby at another site, replicate to a **peer VaultS3**
instance. (Replication targets are VaultS3-to-VaultS3 only — not arbitrary S3 endpoints.)

```yaml
replication:
  enabled: true
  mode: "push"                          # one-way async
  scan_interval_secs: 30
  max_retries: 5
  batch_size: 100
  peers:
    - name: "dr-site"
      url: "https://dr.example.com:9000"
      access_key: "..."
      secret_key: "..."
```

- **`push`** — async, queue-backed, one-directional to the peer(s). Triggered on object
  PUT/DELETE plus a periodic scan; retries with backoff.
- **`active-active`** — bidirectional multi-site sync with vector clocks and conflict
  resolution (`conflict_strategy`: `last-writer-wins` (default), `largest-object`,
  `site-preference`). Set a unique `site_id` per site.

Per-bucket replication rules support prefix/tag filters, delete-marker replication, and
replicating pre-existing objects.

---

## 9. Backup & restore

```yaml
backup:
  enabled: true
  targets:
    - { name: "nightly", type: "local", path: "/backups/vaults3" }
  schedule_cron: "0 2 * * *"   # minute hour — daily at 02:00
  retention_days: 30
  incremental: false           # true = only objects changed since last run
```

- Backs up **all buckets and objects** on a cron schedule (or on demand). Incremental mode
  copies only objects modified since the previous run.
- **Targets are local filesystem only** in the current build (no S3 target).
- **Restore is manual:** stop the server (or use a fresh instance), copy the backed-up
  object tree back into `data_dir`, and restart so the metadata store re-indexes. There is
  no built-in one-click restore yet.

---

## 10. Known limitations & caveats

- **Metadata durability = Raft quorum.** Lose a majority of nodes simultaneously and cluster
  state cannot self-recover; you must restore original nodes or fall back to backup/replica.
- **EC protects object data, not metadata.** Metadata redundancy comes from Raft (in a
  cluster) or from your `metadata_dir` backups (standalone).
- **EC shards do not span nodes**, and **cluster replicas do not protect a single disk** —
  combine both layers for full disk-*and*-node protection.
- **Backup:** local target only; restore is a manual file copy.
- **Replication:** VaultS3↔VaultS3 only; not to AWS S3 or other providers.
- **Config nit:** sample `heal_interval_secs: 300` vs. an older code default of 3600 — set it
  explicitly to avoid surprises.

---

## 11. Listing very large buckets (many objects under one prefix)

VaultS3 keeps every object's metadata in a sorted BoltDB index, so listing a
prefix is a **seek to the continuation marker + read one page** — `O(log n +
page_size)`. Page latency stays flat regardless of how many objects the bucket
holds, because the cost is the page, not the bucket:

| Objects under one prefix | `ListObjectsV2` page (1000 keys) |
|--------------------------|----------------------------------|
| 1,000                    | ~0.8 ms                          |
| 100,000                  | ~0.7 ms                          |
| 1,000,000                | ~0.7 ms                          |
| 10,000,000               | ~0.7 ms                          |
| 100,000,000              | ~0.7 ms                          |

Measured — not extrapolated — across five orders of magnitude: page latency is
flat from a thousand to **a hundred million** objects in a single prefix
(`go test -bench BenchmarkListLatestObjectsPage ./internal/metadata`; a single
mid-tier laptop core. The dominant cost is JSON-decoding the 1000-key page, not
the seek.)

Practical guidance for huge flat prefixes (tens of millions of keys):

- **Paginate** with the `ContinuationToken` / `StartAfter` you get back — each
  page is cheap. The per-page cap is the S3-standard 1000 keys; that's by design.
- This applies to **prefix** listing. Arbitrary substring/partial-name matching
  (`*foo*` anywhere in a key) is a different operation and is not served by the
  ordered index.
- Metadata footprint grows with object count (roughly a few hundred bytes per
  object in the BoltDB file). Budget disk for the metadata DB accordingly at the
  100M-object scale (tens of GB).

## 12. Quick reference

| Goal | Block | Key settings |
|------|-------|--------------|
| Survive disk loss (1 host) | `erasure` | `enabled`, `data_shards`/`parity_shards`, `data_dirs[]` |
| Survive node loss | `cluster` | `enabled`, odd node count, `placement.replica_count ≥ 2` |
| Both | `erasure` + `cluster` | EC per node + cluster across nodes |
| Geo / DR | `replication` | `mode`, `peers[]` |
| Point-in-time copies | `backup` | `targets[]`, `schedule_cron`, `incremental` |

| Endpoint | Purpose |
|----------|---------|
| `GET /cluster/status` | Leader, members, suffrage |
| `POST /cluster/join` | Add node `{node_id, addr}` |
| `POST /cluster/leave` | Remove node `{node_id}` |
| `GET /health` | Node liveness |
| `GET /metrics` | Prometheus metrics |
| `POST /api/v1/heal` | Trigger on-demand erasure heal (`?bucket=&prefix=`) |
