# VaultS3 Benchmarks

This page describes **how to benchmark VaultS3** reproducibly and provides a
results template. The numbers tables are intentionally left as placeholders, 
fill them in from a controlled run on your own hardware. **Do not cite numbers
you haven't measured.** Throughput depends heavily on disk, CPU, network, object
size, and concurrency, so a number without its methodology is meaningless.

> If you run these and want to contribute results, open a PR adding a row with
> your hardware spec, that's far more credible than vendor-claimed figures.

---

## 1. Built-in drive benchmark (`/speedtest`)

VaultS3 ships a quick single-object drive benchmark. It writes then reads a 64 MB
object through the storage engine and reports throughput. It measures **local
disk + engine overhead only**, no network, no concurrency, no S3 protocol.

```bash
# Get a dashboard JWT first (admin login), then:
curl -s -X POST http://localhost:9000/api/v1/speedtest \
  -H "Authorization: Bearer $TOKEN" | jq
```

```json
{
  "writeThroughputMBps": 0.0,
  "readThroughputMBps": 0.0,
  "duration": "0s"
}
```

Use this for a fast "is my disk healthy?" check, **not** for comparisons against
other systems, it doesn't exercise the S3 API path or concurrent clients.

---

## 2. Comparative S3 throughput (recommended: `warp`)

For apples-to-apples comparisons against MinIO / SeaweedFS / Garage, drive all
systems with the same S3 benchmark tool, on the same machine, with the same
object size and concurrency. [`warp`](https://github.com/minio/warp) is the
standard.

```bash
# Example: mixed read/write, 4 KB–10 MB objects, 20 concurrent clients, 60s.
warp mixed \
  --host=localhost:9000 \
  --access-key=vaults3-admin \
  --secret-key=vaults3-secret-change-me \
  --obj.size=1MiB \
  --concurrent=20 \
  --duration=60s \
  --bucket=bench
```

Run the identical command against each system (only `--host`/keys change) and
record the reported GET/PUT throughput and latency percentiles.

`s3-bench` and `hyperfine`-wrapped `aws s3 cp` loops are reasonable alternatives.
the key is **same tool, same flags, same box** for every system under test.

---

## 3. Memory footprint

VaultS3's headline claim is low RAM. Measure resident set size (RSS) under a
steady workload, not at idle:

```bash
# Native process:
ps -o rss= -p "$(pgrep -f vaults3)" | awk '{printf "%.1f MB\n", $1/1024}'

# Docker:
docker stats --no-stream --format '{{.MemUsage}}' vaults3
```

Capture RAM **while `warp` is running**, so the number reflects real load.

---

## 4. Results template

Fill these in from your own controlled run. Replace every `TBD`. State the
methodology so the numbers are reproducible.

**Environment:** `TBD` (CPU, cores, RAM, disk type, OS, filesystem, network)
**Tool / workload:** `warp mixed, 1 MiB objects, 20 concurrent, 60s` (or your own)
**Date / versions:** VaultS3 `TBD`, MinIO `TBD`, SeaweedFS `TBD`, Garage `TBD`

| Metric | VaultS3 | MinIO | SeaweedFS | Garage |
|---|---|---|---|---|
| GET throughput (MB/s) | TBD | TBD | TBD | TBD |
| PUT throughput (MB/s) | TBD | TBD | TBD | TBD |
| GET p99 latency (ms) | TBD | TBD | TBD | TBD |
| PUT p99 latency (ms) | TBD | TBD | TBD | TBD |
| RSS under load (MB) | TBD | TBD | TBD | TBD |
| Idle RSS (MB) | TBD | TBD | TBD | TBD |

---

## 5. Reporting honestly

- Always publish the **hardware, tool, flags, and date** alongside the numbers.
- Run each system several times and report medians, not best-case cherry-picks.
- If VaultS3 loses on a metric, say so, credibility compounds. The pitch is
  "lightweight and batteries-included in one binary," and RAM/footprint is where
  that shows. Raw peak throughput on big iron is not the claim.
