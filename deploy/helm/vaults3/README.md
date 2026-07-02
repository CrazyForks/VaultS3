# VaultS3 Helm Chart

Deploy [VaultS3](https://github.com/Kodiqa-Solutions/VaultS3), a lightweight,
S3-compatible object store with a built-in dashboard, to Kubernetes.

VaultS3 runs as a **StatefulSet** with persistent volumes for object data
(`/data`) and BoltDB metadata (`/metadata`). A single port (`9000`) serves the
S3 API, the web dashboard (`/dashboard/`), Prometheus metrics (`/metrics`), and
the `/health` / `/ready` probe endpoints.

## Install

```bash
# From a local checkout of the repo:
helm install vaults3 ./deploy/helm/vaults3 \
  --namespace vaults3 --create-namespace \
  --set auth.secretKey="$(openssl rand -hex 20)"
```

Get the admin credentials and reach the dashboard:

```bash
kubectl -n vaults3 get secret vaults3 -o jsonpath='{.data.access-key}' | base64 -d; echo
kubectl -n vaults3 get secret vaults3 -o jsonpath='{.data.secret-key}' | base64 -d; echo
kubectl -n vaults3 port-forward svc/vaults3 9000:9000
# open http://localhost:9000/dashboard/
```

## Key values

| Key | Default | Description |
|-----|---------|-------------|
| `replicaCount` | `1` | Keep at 1 unless using Raft clustering (Beta). |
| `image.repository` | `eniz1806/vaults3` | Image repo. |
| `image.tag` | `""` | Defaults to the chart `appVersion`. Pin for reproducibility. |
| `auth.accessKey` | `vaults3-admin` | Admin access key (injected via Secret → env). |
| `auth.secretKey` | `vaults3-secret-change-me` | Admin secret. **Change it**, or set empty to auto-generate. |
| `auth.existingSecret` | `""` | Use your own Secret (keys `access-key`, `secret-key`). |
| `config` | single-node config | The `vaults3.yaml` mounted at `/etc/vaults3/`. Replace to enable encryption/replication/erasure/etc. |
| `existingConfigMap` | `""` | Use your own ConfigMap (key `vaults3.yaml`). |
| `controller.kind` | `StatefulSet` | `StatefulSet` (default. Required for clustering/multi-replica) or `Deployment` (single-node, standalone PVCs). |
| `persistence.enabled` | `true` | Keep enabled for real use. |
| `persistence.data.size` | `50Gi` | Object-data PVC size. |
| `persistence.data.existingClaim` | `""` | Mount a pre-existing data PVC (Deployment mode), e.g. a restored backup. |
| `persistence.metadata.size` | `5Gi` | Metadata PVC size. |
| `persistence.metadata.existingClaim` | `""` | Mount a pre-existing metadata PVC (Deployment mode). |
| `service.type` | `ClusterIP` | Use `LoadBalancer` or an Ingress to expose. |
| `ingress.enabled` | `false` | Enable + set `hosts` to expose via Ingress. For large uploads set `nginx.ingress.kubernetes.io/proxy-body-size: "0"`. |
| `serviceMonitor.enabled` | `false` | Prometheus-Operator scraping of `/metrics`. |
| `resources` | 100m/128Mi → 1/512Mi | VaultS3 is light (<80MB RAM typical). |
| `extraEnv` | `[]` | Extra env vars (`VAULTS3_LOG_LEVEL`, `VAULTS3_DOMAIN`, `VAULTS3_ENCRYPTION_KEY`, …). |

See [`values.yaml`](./values.yaml) for the full list.

## Enabling features

VaultS3 features (encryption, compression, replication, erasure coding, tiering,
etc.) are driven by `vaults3.yaml`. Override the `config` value with your own:

```bash
helm install vaults3 ./deploy/helm/vaults3 -n vaults3 --create-namespace \
  --set-file config=./my-vaults3.yaml \
  --set auth.secretKey="$(openssl rand -hex 20)"
```

Keep `storage.data_dir: /data` and `storage.metadata_dir: /metadata` (the chart
forces these via env vars anyway, so they always land on the PVCs).

## Clustering (Beta)

Set `cluster.enabled=true` with an **odd `replicaCount` ≥ 3** and the chart
auto-forms a Raft cluster: pod-0 bootstraps as the initial leader and the other
pods auto-join it over stable headless-service DNS, no manual bootstrap/join
steps. Raft state lives on the metadata PVC, and a node re-joins automatically
after a restart (its identity is the StatefulSet DNS name, not its pod IP).

```bash
helm install vaults3 ./deploy/helm/vaults3 -n vaults3 --create-namespace \
  --set cluster.enabled=true --set replicaCount=3 \
  --set auth.secretKey="$(openssl rand -hex 20)"

# verify the cluster has a leader + all members
kubectl -n vaults3 exec vaults3-0 -- wget -qO- http://localhost:9000/cluster/status
```

Metadata writes (buckets, objects, IAM, …) are committed through Raft consensus,
so all nodes converge, and a write to **any** node works (a write to a follower
is transparently forwarded to the leader). Reads are served locally.

> **Beta.** Clustering is functional but newer and less battle-tested than
> single-node + erasure coding. For maximum production durability today, prefer a
> **single node with erasure coding** (disk redundancy), validate clustering
> against your workload before trusting it as the only copy of critical data. See
[`docs/SCALING.md`](https://github.com/Kodiqa-Solutions/VaultS3/blob/main/docs/SCALING.md)
for the redundancy trade-offs.

| Cluster value | Default | Description |
|---|---|---|
| `cluster.enabled` | `false` | Auto-form a Raft cluster across the replicas (Beta). |
| `cluster.raftPort` | `9001` | Port for inter-node Raft traffic. |

## Backups & restore

VaultS3 keeps object **data** on `/data` (plain files) and **metadata** in a
BoltDB file on `/metadata`, and they reference each other.

**Backing up the PVCs** (Velero, k8up, CSI snapshots, …):

- Back up **`/data` and `/metadata` together, from the same point in time.** A
  snapshot of one paired with a mismatched copy of the other can leave dangling
  references. Atomic **CSI volume snapshots** are preferable to a live file copy.
  if you must file-copy, quiescing writes (or a brief downtime) gives the cleanest
  result. BoltDB is crash-consistent, so a live snapshot is usually recoverable.
- StatefulSet PVCs (`data-<release>-0`, `metadata-<release>-0`) are backed up by
  Velero/k8up like any other PVC, clustering is not required to do so.

**Restoring into a known PVC** (the easiest restore workflow): run in
**Deployment mode** and point the chart at the restored claims:

```bash
helm install vaults3 ./deploy/helm/vaults3 -n vaults3 \
  --set controller.kind=Deployment \
  --set persistence.data.existingClaim=restored-data \
  --set persistence.metadata.existingClaim=restored-metadata \
  --set auth.secretKey="$(openssl rand -hex 20)"
```

In Deployment mode the chart's standalone PVCs carry a `helm.sh/resource-policy:
keep` annotation, so they survive `helm uninstall` and can be re-attached on
reinstall.

**App-level alternative:** VaultS3 also has a built-in backup (full/incremental)
and bucket snapshots, which sidestep the cross-volume-consistency concern, see
the main README.

## Uninstall

```bash
helm uninstall vaults3 -n vaults3
# PVCs are retained by design — delete them explicitly to wipe data:
kubectl -n vaults3 delete pvc -l app.kubernetes.io/name=vaults3
```
