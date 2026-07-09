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

export interface SystemInfo {
  version: string
  os: string
  arch: string
  dataDirs: string[]
  disk: { totalBytes: number; usedBytes: number; freeBytes: number }
  objectBytes: number
  objectCount: number
  bucketCount: number
}

export function getSystemInfo(): Promise<SystemInfo> {
  return apiFetch<SystemInfo>('/system')
}
