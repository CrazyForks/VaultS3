import { apiFetch } from './client'

export interface BucketStat {
  name: string
  size: number
  objectCount: number
  maxSizeBytes?: number
  maxObjects?: number
}

export interface RequestMethodStat {
  method: string
  count: number
}

export interface Stats {
  totalBuckets: number
  totalObjects: number
  totalSize: number
  uptimeSeconds: number
  goroutines: number
  memoryMB: number
  buckets: BucketStat[]
  requestsByMethod: RequestMethodStat[]
  totalRequests: number
  totalErrors: number
  bytesIn: number
  bytesOut: number
}

export function getStats(): Promise<Stats> {
  return apiFetch<Stats>('/stats')
}

export interface DirUsage {
  path: string
  bytes: number
  files: number
  error?: string
}

/** VaultS3's own measured footprint, as opposed to the whole filesystem. */
export interface Usage {
  dirs: DirUsage[]
  bytes: number
  files: number
  scannedAt: string
  tookMs: number
}

export interface SystemInfo {
  version: string
  os: string
  arch: string
  dataDirs: string[]
  disk: { totalBytes: number; usedBytes: number; freeBytes: number }
  objectBytes: number
  objectCount: number
  bucketCount: number
  /** Absent until the first background scan finishes, or when scanning is off. */
  usage?: Usage
  usageScanning?: boolean
}

export function getSystemInfo(): Promise<SystemInfo> {
  return apiFetch<SystemInfo>('/system')
}

export interface NodeSystemInfo extends SystemInfo {
  nodeId?: string
  address?: string
  reachable?: boolean
  error?: string
}

export interface ClusterInfo {
  clustered: boolean
  nodeCount: number
  reachableNodes: number
  nodes: NodeSystemInfo[]
  totals: {
    disk: { totalBytes: number; usedBytes: number; freeBytes: number }
    objectBytes: number
    objectCount: number
    /** Summed across the nodes that have finished a scan (measuredNodes). */
    vaultBytes: number
    vaultFiles: number
    measuredNodes: number
  }
}

export function getClusterInfo(): Promise<ClusterInfo> {
  return apiFetch<ClusterInfo>('/cluster/info')
}
